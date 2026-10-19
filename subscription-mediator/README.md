# subscription-mediator

Bridges **WSO2 API Portal 1.0.0** webhook events to the **WSO2 API Platform
Gateway 1.2.0** Controller management REST API.

Part of the [API Portal → API Platform Gateway subscription
bridge](../README.md), which has the end-to-end walkthrough. This README covers
the service itself.

| Component | Tested against | Documentation |
|---|---|---|
| WSO2 API Manager | **4.7.0** | [apim.docs.wso2.com](https://apim.docs.wso2.com/en/latest/) |
| WSO2 API Platform Gateway | **1.2.0** | [API Gateway 1.2.0](https://wso2.com/api-platform/docs/api-gateway/1.2.0/overview/) · [`gateway/v1.2.0`](https://github.com/wso2/api-platform/releases/tag/gateway%2Fv1.2.0) |
| WSO2 API Portal | **1.0.0** | [API Portal 1.0.0](https://wso2.com/api-platform/docs/api-portal/1.0.0/overview/) · [`api-portal/v1.0.0`](https://github.com/wso2/api-platform/releases/tag/api-portal%2Fv1.0.0) |

Both wire contracts it depends on — the portal's webhook envelope and the
gateway's management REST API — are specific to those versions. Another release
may move a field or a path, and this service will not notice; it will just fail
the event.

> **This is built for a demo.** Not a WSO2 product, no warranty. The endpoints are
> unauthenticated and plaintext, and `configs/config.toml` holds secrets in the
> clear (unless you keep them in the environment — see Configuration).
> Run it where only the portal and you can reach it. Don't deploy it as-is, and don't
> commit your filled-in `configs/config.toml`.

## Why this exists

A gateway attached to the Platform API holds a WebSocket to it and receives
subscription, API-key and plan events over that channel. A gateway attached to
**WSO2 API Manager 4.7.0** does not — it has no connection to the API Portal's
Platform API, so nothing tells it that a developer just subscribed to an API or
revoked a key in the portal.

The API Portal doesn't talk to a gateway at all. It delivers a signed HTTP POST
to whatever URL you register as a webhook subscriber, and leaves it to the
receiver to do something useful. This service is that receiver:

```
  WSO2 API Portal 1.0.0                 WSO2 API Platform Gateway 1.2.0
  (subscriptions, API keys)             (APIs synced from WSO2 APIM 4.7.0)
        │                                            ▲
        │  POST, HMAC-SHA256 signed,                 │  management REST API
        │  sensitive fields AES-256-GCM              │  (HTTP basic auth)
        ▼                                            │
  ┌──────────────────────────────────────────────────┴──┐
  │ subscription-mediator                               │
  │  verify → decrypt → apply → answer                  │
  └─────────────────────────────────────────────────────┘
```

The gateway keeps getting its API definitions from API Manager. This service
only adds the subscription and credential state the portal owns.

It keeps no state of its own. Each delivery is applied to the gateway before
the portal gets its answer, and every lookup reads the gateway: a subscription
by the portal's subscription ID (which the gateway subscription is created
under, as its `externalRefId`), an API key by the portal's key ID (its
`externalRefId`).

---

## Quick start

Needs Docker with Compose v2. Run from this directory.

Generate a webhook secret, then supply it and the gateway credentials. Either
edit **`configs/config.toml`**, which ships with the required values blank:

```bash
openssl rand -base64 48    # paste into [webhook] secret
```

```toml
[webhook]
secret = "<the generated secret>"

[gateway]
base_url = "http://host.docker.internal:9090/api/management/v1"
username = "<gateway-admin-username>"
password = "<gateway-admin-password>"
```

…or leave the file as shipped and put them in a `.env` beside
`docker-compose.yaml`. Environment variables override the file, compose passes
these through, and `.env` is git-ignored — so nothing secret is ever staged.
Start from the tracked template, `cp .env.example .env`, and fill it in:

```bash
SM_WEBHOOK_SECRET=<the generated secret>
SM_GATEWAY_USERNAME=<gateway-admin-username>
SM_GATEWAY_PASSWORD=<gateway-admin-password>
SM_GATEWAY_BASE_URL=http://host.docker.internal:9090/api/management/v1
```

The mediator calls the gateway's management API on port `9090`. The gateway
1.2.0 controller serves it over plain HTTP only; it has no TLS listener of its
own. When the gateway sits behind a TLS-terminating proxy or ingress, use that
`https` address instead, and when its certificate is self-signed or from a
private CA, copy the CA certificate to `certs/gateway-ca.crt` beside
`docker-compose.yaml` (compose mounts `certs/` at `/app/certs`) and set
`ca_file = "/app/certs/gateway-ca.crt"`. See
[Reaching the gateway](#reaching-the-gateway-outbound).

The second way is the better habit, and the one the
[walkthrough](../README.md#7-start-the-subscription-mediator) uses.

Then:

```bash
make build
docker compose up -d
docker compose logs -f
```

A healthy start logs:

```
msg="the gateway management API is reachable and the credentials were accepted"
msg="subscription-mediator is ready"
msg="listening for API Portal webhooks" address=0.0.0.0:8085 path=/devportal/events
```

If the gateway isn't up yet you get a warning instead of a failure. Deliveries
fail with `502` until it answers.

### Register it with the API Portal

An admin registers it in the portal's settings
([Configure webhooks](https://wso2.com/api-platform/docs/api-portal/1.0.0/admin-settings/webhook-integration/)):
open `https://<portal-host>/api-portal/<org-handle>/settings`, choose
**Webhooks** under **Integrations**, then **Add webhook**, and fill in:

| Field | Value |
|---|---|
| Name | `Subscription Mediator` |
| Target URL | `http://<host-reachable-from-the-portal>:8085/devportal/events` |
| Secret | the same secret as `[webhook] secret` |
| Timeout (ms) | `15000` |
| Events | **Select all** under **Subscriptions** and under **API keys** |
| Enabled | checked |

Use the **same** secret. Without a matching secret the portal omits the
subscription token and API key value from its deliveries entirely, and the
mediator has nothing to give the gateway.

The same thing over the portal's REST API, with an admin token:

```bash
curl -k -X POST "https://localhost:9543/api-portal/api/v0.9/webhook-subscribers" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "displayName": "Subscription Mediator",
    "targetUrl": "http://<host-reachable-from-the-portal>:8085/devportal/events",
    "secret": "<the same secret from configs/config.toml>",
    "events": ["subscription.*", "apikey.*"],
    "timeoutMs": 15000
  }'
```

Subscribers belong to one organization, and the portal delivers an event only to
its own organization's subscribers. With more than one organization, register
the mediator in each, all with the same secret: the mediator verifies every
delivery against its one `[webhook] secret`.

The mediator answers only once the gateway reflects the event, so `timeoutMs`
has to cover the gateway calls: a create can take a few (find the API, the plan,
then create). The portal's default of `5000` is tight; past it the portal marks
the delivery `FAILED` even if the mediator finishes the job.

Plain `http://` is fine — the portal doesn't validate the scheme and delivers to
HTTP targets (its own delivery tests do exactly that). If you want TLS, put a
tunnel or proxy in front; the mediator itself serves plaintext only.

### Check it

```bash
docker compose ps
curl -s localhost:8085/status | jq
```

Then subscribe to an API in the **API Portal UI** and confirm it landed:

```bash
curl -su "<gateway-admin-username>:<gateway-admin-password>" \
  http://localhost:9090/api/management/v1/subscriptions | jq
```

Subscribing, generating and revoking keys, suspending a subscription and changing
its plan are all API Portal UI actions — each publishes a webhook this service
turns into a gateway call. [`agent-verification.md`](../agent-verification.md)
exercises every one of them with expected results.

---

## Commands

```bash
make test                       # run the tests
make build                      # build the image (also tagged for docker compose)
make build-and-push-multiarch \
  DOCKER_REGISTRY=<registry> VERSION=<x.y.z>   # linux/amd64 + linux/arm64, pushed
make help                       # every target and the variables they take
```

Publishing to the dev registry is normally left to CI — see [Publishing the
image](#publishing-the-image).

Everything else is plain Compose:

```bash
docker compose up -d       # start; also picks up a freshly built image
docker compose logs -f
docker compose ps
docker compose down
```

The container runs as UID/GID `10001`, not root, and writes nothing to disk.

`configs/config.toml` is bind-mounted, so a configuration change needs only
`docker compose restart` — no rebuild.

After changing the **source**: `make build && docker compose up -d`.
(`docker compose up -d --build` in one step is *not* equivalent — it rebuilds the
image but leaves the running container on the old one.)

A missing or invalid setting makes the mediator refuse to start. The container
retries a few times, then stays `Exited` with the reason as its last log line:

```
subscription-mediator: /app/config.toml: [webhook] secret is required — it verifies
each delivery and decrypts the subscription token, and must match the secret
registered with the API Portal subscriber
```

### Updating dependencies

Run these from `subscription-mediator/`. First see what is out of date:

```bash
go list -m -u all | grep '\['        # module, current version, [newest version]
```

Then upgrade every dependency to its newest minor or patch release, and run
the same checks CI runs:

```bash
go get -u ./...                      # direct and indirect dependencies
go mod tidy                          # CI fails if go.mod or go.sum is untidy
go vet ./...
go test -race ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

To move a single module instead, run `go get <module>@latest` (or
`@<version>`), then `go mod tidy`. `go get -u` never crosses a major
version: a new major (`/v3`, for example) is a different import path and
means changing the imports by hand.

The Go toolchain is separate. `go get -u` doesn't touch it. To move to a new
Go release (see <https://go.dev/dl/>), change both of these to the same
version in one commit. CI's setup-go reads `go.mod`.

- the `go` line in `go.mod`, e.g. `go mod edit -go=1.26.9`
- the `golang:<version>-alpine` tag in the `Dockerfile`

Then rebuild the image (`make build`) to check the Docker build too.

---

## Publishing the image

[`.github/workflows/subscription-mediator-dev.yaml`](../.github/workflows/subscription-mediator-dev.yaml)
publishes a multi-arch image (linux/amd64, linux/arm64) to the **dev** Docker
repository in JFrog Artifactory. It only runs for changes under
`subscription-mediator/`:

| Event | What runs |
|---|---|
| Pull request | `go mod tidy` check, `go vet`, `go test -race`, `govulncheck`, then a multi-arch image build that is **not** pushed. No registry access. |
| Push to `main` | The same checks, then build and push to Artifactory |
| Manual run (*Run workflow*) | Same as a push to `main`, for the chosen branch |

The image is `<DEV_REGISTRY>/subscription-mediator`. Each push carries two tags:

- `sha-<short commit>`: never moves. Deploy by this tag or by the digest
  printed in the run summary.
- `dev`: moves to the newest build from `main`. Because it is overwritten, the
  CI user needs **Delete/Overwrite** on the repository.

Every image also carries an SBOM and build provenance. The workflow never pushes
`:latest`.

The publish job runs in the GitHub environment `dev` and signs in to
Artifactory with **OIDC**. GitHub hands the run a short-lived token that names
the repository and the environment the job runs in. `jfrog/setup-jfrog-cli`
exchanges it, through an OpenID Connect integration in Artifactory, for a
short-lived Artifactory token for one CI user, and `docker/login-action` uses
that token to sign in to the registry host. The CI user may only write to the
one repository. No password is stored anywhere, so there is nothing to leak or
rotate. Where OIDC can't be set up, a scoped access token works instead (see
[Access token instead of OIDC](#access-token-instead-of-oidc)).

The setup is done once, in Artifactory and then in GitHub.

### Before you start

- An Artifactory **admin** (or a project admin with rights to create users,
  permissions and OIDC integrations).
- **Admin** on the GitHub repository, to create the environment.
- The values below. `DEV_REGISTRY` is the image prefix, without a scheme:
  `<artifactory host>/<docker repo key>[/<path>]` with the repository-path
  method (e.g. `mycompany.jfrog.io/docker-dev/api-platform`), or
  `<repo key>.<host>/<path>` with the subdomain method.

```bash
JFROG_URL=https://mycompany.jfrog.io   # Artifactory base URL
REPO_KEY=docker-dev                    # local Docker repository
DEV_REGISTRY=mycompany.jfrog.io/docker-dev/api-platform
REPO=<owner>/<repo>                    # e.g. RakhithaRR/api-portal-platform-gateway-subscription-mediator
CI_USER=subscription-mediator-ci
PROVIDER=github-subscription-mediator  # name of the OIDC integration
```

`REPO` must be the repository the workflow **runs in**. A fork, and the
repository it was forked from, each need their own identity mapping.

### 1. Artifactory: the repository, the CI user and OIDC

1. **Repository.** Use an existing **local** Docker repository, or create one
   (*Administration → Repositories → Create a Repository → Local → Docker*)
   with the key `$REPO_KEY`.
2. **CI user.** Create `$CI_USER` and a permission that gives it **Read**,
   **Annotate**, **Deploy/Cache** and **Delete/Overwrite** on that repository
   only. Delete/Overwrite is what lets each run move the `dev` tag.
3. **OIDC integration.** *Administration → General Management → Manage
   Integrations → New Integration → OpenID Connect*: provider type **GitHub**,
   name `$PROVIDER`, provider URL `https://token.actions.githubusercontent.com`.
   The audience is optional; if you set one, `JFROG_OIDC_AUDIENCE` in step 2
   must be the same value.
4. **Identity mapping.** On that integration, add a mapping whose claims match
   this repository's publish job. The job runs in the `dev` environment, so
   use `{"repository": "<owner>/<repo>", "environment": "dev"}`. Set the token
   scope to the CI user, and a short expiry (e.g. 10 minutes).

The same steps through the REST API, with an admin token in `$ADMIN_TOKEN`:

```bash
curl -fsS -X POST "$JFROG_URL/access/api/v1/oidc" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d "{\"name\": \"$PROVIDER\", \"issuer_url\": \"https://token.actions.githubusercontent.com\",
       \"provider_type\": \"GitHub\", \"description\": \"subscription-mediator CI\"}"

curl -fsS -X POST "$JFROG_URL/access/api/v1/oidc/$PROVIDER/identity_mappings" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d "{\"name\": \"$PROVIDER-dev\", \"priority\": 1,
       \"claims\": {\"repository\": \"$REPO\", \"environment\": \"dev\"},
       \"token_spec\": {\"username\": \"$CI_USER\",
                        \"scope\": \"applied-permissions/user\",
                        \"expires_in\": 600}}"
```

Field names can differ slightly between Artifactory versions; check them
against what the UI shows.

The claims have to match exactly. They are case-sensitive, and they change if
the repository is renamed or transferred, or if the job's environment is
renamed.

#### Access token instead of OIDC

Create an access token for the CI user (*Administration → User Management →
Access Tokens*, or the REST API below), and set `JFROG_USERNAME` and the
`JFROG_ACCESS_TOKEN` secret in step 2 instead of `JFROG_OIDC_PROVIDER`. Leave
`JFROG_OIDC_PROVIDER` unset: when it is set, the workflow uses OIDC. Rotate the
token before it expires.

```bash
curl -fsS -X POST "$JFROG_URL/access/api/v1/tokens" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d username="$CI_USER" -d scope=applied-permissions/user -d expires_in=7776000
```

### 2. GitHub: the `dev` environment

In the repository, open **Settings → Environments → New environment** and name it
`dev`. That name must match the `environment` claim in the identity mapping and
`environment: dev` in the workflow.

- **Deployment branches and tags:** choose *Selected branches and tags* and add
  `main`. Any job can claim `environment: dev`, so this rule is what stops a
  manual run from another branch from pushing to the registry.
- **Environment variables:**

  | Name | Value |
  |---|---|
  | `DEV_REGISTRY` | `<artifactory host>/<docker repo key>[/<path>]`, no scheme |
  | `JFROG_URL` | the Artifactory base URL, e.g. `https://mycompany.jfrog.io` |
  | `JFROG_OIDC_PROVIDER` | the OIDC integration's name (OIDC sign-in) |
  | `JFROG_OIDC_AUDIENCE` | optional; only if the integration has an audience |
  | `JFROG_USERNAME` | access-token sign-in only: the CI user |

  These are variables because they are part of every image reference the run
  prints. A secret would mask the tags and the summary as `***`.
- **Environment secrets** (access-token sign-in only):

  | Name | Value |
  |---|---|
  | `JFROG_ACCESS_TOKEN` | the CI user's scoped access token |

  Add it to the **environment**, not the repository, so only the publish job
  can read it.
- **Optional:** *Required reviewers*, to have each push to the registry approved
  by a person.

The workflow checks these settings first and fails with a message that names
what is missing.

The same settings from the command line, if you prefer:

```bash
gh api -X PUT "repos/$REPO/environments/dev" \
  -F "deployment_branch_policy[protected_branches]=false" \
  -F "deployment_branch_policy[custom_branch_policies]=true"
gh api -X POST "repos/$REPO/environments/dev/deployment-branch-policies" -f name=main
gh variable set DEV_REGISTRY        --env dev --repo "$REPO" --body "$DEV_REGISTRY"
gh variable set JFROG_URL           --env dev --repo "$REPO" --body "$JFROG_URL"
gh variable set JFROG_OIDC_PROVIDER --env dev --repo "$REPO" --body "$PROVIDER"
# gh variable set JFROG_OIDC_AUDIENCE --env dev --repo "$REPO" --body "<audience>"

# Access token instead of OIDC:
# gh variable set JFROG_USERNAME --env dev --repo "$REPO" --body "$CI_USER"
# gh secret set JFROG_ACCESS_TOKEN --env dev --repo "$REPO" --body "<token>"
```

If the environment still has the settings from the earlier Azure Container
Registry setup, remove them:

```bash
for s in AZURE_CLIENT_ID AZURE_TENANT_ID AZURE_SUBSCRIPTION_ID; do
  gh secret delete "$s" --env dev --repo "$REPO"
done
gh variable delete ACR_NAME --env dev --repo "$REPO"
```

Then make sure Actions is enabled. On a fork it is off until you open the
**Actions** tab and enable workflows.

### 3. First run and checks

Start it from **Actions → subscription-mediator (dev) → Run workflow** on `main`,
or push a change under `subscription-mediator/`. When it finishes, the run
summary lists the digest and the tags. Then check the registry:

```bash
docker buildx imagetools inspect "$DEV_REGISTRY/subscription-mediator:dev"   # linux/amd64 and linux/arm64

# or list the tags (<path> is the part of DEV_REGISTRY after the repo key)
curl -H "Authorization: Bearer <token>" \
  "$JFROG_URL/artifactory/api/docker/$REPO_KEY/v2/<path>/subscription-mediator/tags/list"
```

### When it fails

| Symptom | Cause and fix |
|---|---|
| *Check registry settings* fails | A variable or secret is missing, or `DEV_REGISTRY` has a scheme or no repo key. The message names it. Check the settings are on the `dev` environment, not the repository. |
| The OIDC exchange fails: no identity mapping matched | The claims don't match. `repository` must be `<owner>/<repo>` of the repository the run belongs to, in exact case (check for a fork, a rename or a transfer), and `environment` must be `dev`: the job must run in the `dev` environment. |
| The OIDC exchange fails on the audience | `JFROG_OIDC_AUDIENCE` and the integration's audience differ. Set both to the same value, or neither. |
| `Unable to get ACTIONS_ID_TOKEN_REQUEST_URL` | The job has no `id-token: write` permission, or the run is from a fork's pull request, which never gets an OIDC token. |
| The publish job waits, or says the branch is not allowed to deploy | The `dev` environment's branch rule doesn't include the branch you ran from. |
| `unauthorized` on `docker login` | `DEV_REGISTRY`'s host is not the Artifactory registry host, or the access token has expired. |
| `denied` on push | The CI user lacks Deploy/Cache on the repository, or the repo key in `DEV_REGISTRY` is wrong. |
| The first push works, but the next fails on the `dev` tag | The CI user lacks Delete/Overwrite, so the moving `dev` tag can't be replaced. |
| The login or push times out | Artifactory isn't reachable from GitHub-hosted runners (private network or IP allow-list). Use a self-hosted runner inside that network. |
| *Check go.mod and go.sum are tidy* fails | Run `go mod tidy` in `subscription-mediator/` and commit the result. |
| *govulncheck* fails | A reachable vulnerability: often the Go standard library. Raise `go` in `go.mod` and the `golang:` tag in the Dockerfile to the fixed release. Both must move together, and setup-go follows `go.mod`. |

### Publishing from a workstation

The Makefile does the same build by hand, signed in as yourself (or as the CI
user with its token) rather than through OIDC:

```bash
docker login mycompany.jfrog.io      # the Artifactory host
make build-and-push-multiarch DOCKER_REGISTRY="$DEV_REGISTRY" VERSION=<x.y.z>
```

This pushes `:<x.y.z>` and `:latest`; add `PUSH_LATEST=false` to keep `:latest`
where it is. Unlike the workflow, the result has no SBOM or provenance
attached.

---

## Deploying to Kubernetes

[`deploy/helm/subscription-mediator`](deploy/helm/subscription-mediator/README.md)
is a Helm chart for Kubernetes 1.29+. It runs the mediator as a Deployment with
two replicas by default and no volume: the service keeps no state, so any pod
can take any delivery, and a rolling restart or node drain leaves one to answer.
The chart README covers:

- the install;
- credentials kept out of Helm;
- network policy;
- what an upgrade or a node drain costs.

```bash
helm upgrade --install subscription-mediator deploy/helm/subscription-mediator \
  -n subscription-mediator -f values-dev.yaml --set image.tag=sha-<commit> --atomic --wait
```

---

## Configuration

Everything lives in `configs/config.toml`, mounted read-only at
`/app/config.toml`. `configs/config-template.toml` is the annotated copy.

```toml
[webhook]
secret = ""                 # required

[gateway]
base_url = "..."            # defaults to http://host.docker.internal:9090/api/management/v1
ca_file = ""                # optional: CA bundle for the gateway's TLS certificate
username = ""               # required
password = ""               # required

[logging]
level = "info"              # optional: debug | info | warn | error

[api_handles]               # optional, see below
# "order api:v1.0" = "orangeapi-1.0.0"
```

Anything else — the listen address, the per-delivery time limit, the API cache
lifetime — is a constant in the source: `config.ListenAddress`,
`server.ApplyTimeout`, `mediator.APICacheTTL`, and so on. A demo has no reason
to change them, and leaving them out keeps the file to what matters.

Point the binary at a different file with `-config <path>`.

### Where a setting can come from

Three layers, each overriding the one before it:

```
built-in defaults  ->  configs/config.toml  ->  SM_* environment variables
```

| Setting | TOML key | Environment variable | |
|---|---|---|---|
| webhook secret | `[webhook] secret` | `SM_WEBHOOK_SECRET` | **required** |
| gateway address | `[gateway] base_url` | `SM_GATEWAY_BASE_URL` | defaults to `http://host.docker.internal:9090/api/management/v1` |
| gateway CA | `[gateway] ca_file` | `SM_GATEWAY_CA_FILE` | optional; a PEM bundle trusted in addition to the system roots |
| gateway user | `[gateway] username` | `SM_GATEWAY_USERNAME` | **required** |
| gateway password | `[gateway] password` | `SM_GATEWAY_PASSWORD` | **required** |
| log level | `[logging] level` | `SM_LOG_LEVEL` | defaults to `info` |
| API handle pins | `[api_handles]` | — | file only |

So the secrets need never be written into the mounted file: put them in a `.env`
beside `docker-compose.yaml`, and `configs/config.toml` can stay as shipped. An
empty variable counts as unset, so a blank `.env` line won't erase a file value,
and an unrecognized `SM_*` variable is logged and ignored rather than fatal.

The file itself is optional — with every required value in the environment the
mediator starts with nothing mounted at all.

### Failing fast

A missing or malformed required setting stops startup, naming the setting, why
it's needed, and both places to put it:

```
subscription-mediator: missing required configuration: [gateway] password is not
set — the management API authenticates with HTTP basic auth. Set it under
[gateway] in /app/config.toml, or set the SM_GATEWAY_PASSWORD environment variable
```

If the file wasn't found either, the message says so, which is usually the real
cause.

---

## Endpoints

All unauthenticated.

| | |
|---|---|
| `POST /devportal/events` | the webhook receiver; this is the `targetUrl` |
| `GET /health` | liveness, and the container healthcheck |
| `GET /status` | events applied and failed since this process started |

The webhook receiver answers once the event is applied:

| Status | Meaning |
|---|---|
| `200` `{"status":"processed"}` | the gateway reflects the event (or there was nothing to do) |
| `401` | the signature didn't verify |
| `400` | the body isn't a webhook envelope |
| `422` | the event can't be applied, and sending it again won't help; the body says why |
| `502` | the gateway failed or couldn't be reached |

Anything but `200` leaves the delivery `FAILED` in the portal's delivery history.

---

## Networking

The mediator is a standalone stack on **its own network**
(`subscription-mediator-network`), and joins neither the portal's nor the
gateway's. It can't: those two are separate deployments that may not be Docker at
all. Both directions go over the host instead, which works the same whichever
way the other two are deployed.

```
  API Portal ──────▶ host:8085 ──────▶ subscription-mediator
                                                 │
                                                 ▼
                                       host:9090 ──────▶ Gateway Controller
```

### Reaching the gateway (outbound)

One setting, `[gateway] base_url`, overridable end-to-end with
`SM_GATEWAY_BASE_URL` in a `.env` file beside `docker-compose.yaml` — no edit to
the mounted config. Its scheme and port pick the gateway listener:

| Listener | Address | `ca_file` |
|---|---|---|
| the controller's own (the default) | `http://<host>:9090/api/management/v1` | leave empty; setting it with an `http` address stops startup |
| a TLS-terminating proxy or ingress in front of it | `https://<proxy-host>[:port]/api/management/v1` | the proxy's CA, when self-signed or private |

The gateway 1.2.0 controller has no TLS listener of its own, so `https` is only
for a proxy in front of it. The table below gives `<host>` for each layout.

| Gateway runs | `SM_GATEWAY_BASE_URL` |
|---|---|
| its own compose stack, Docker Desktop | *(the default)* `http://host.docker.internal:9090/api/management/v1` |
| its own compose stack, Docker Engine on Linux | `http://172.17.0.1:9090/api/management/v1` |
| on the host machine | same as above — it's the same published port |
| the mediator itself run outside Docker | `http://localhost:9090/api/management/v1` |
| Kubernetes | `http://<gateway-controller-service>.<namespace>:9090/api/management/v1` |

`host.docker.internal` is a Docker Desktop name. On Docker Engine for Linux it
does not exist — use the bridge gateway address `172.17.0.1` rather than adding
an `extra_hosts: host.docker.internal:host-gateway` entry, which on Docker
Desktop *replaces* the working built-in name with the VM's bridge address and
makes the gateway unreachable.

The **API Platform Gateway 1.2.0 distribution's own`docker-compose.yaml` sets
that entry**, so on Rancher/Docker Desktop for macOS nothing in the gateway
stack can reach the host by any Docker name — every one resolves to `172.17.0.1`
and is refused. That stack needs the machine's LAN IP instead.
This one deliberately sets no `extra_hosts`, which is why the default above works.

#### TLS to the gateway

The gateway 1.2.0 controller serves its management API over plain HTTP only
(port `9090`); it has no TLS setting for it. For TLS, put a TLS-terminating
proxy or ingress in front of it and point `base_url` at that `https` address.
The mediator verifies the proxy's certificate like any HTTPS client, TLS 1.2 or
later:

- **The certificate must name the host in `base_url`.**
- **Its issuer must be trusted.** A certificate from a public CA needs nothing
  more. For a self-signed or private-CA certificate, set `[gateway] ca_file` /
  `SM_GATEWAY_CA_FILE` to a PEM bundle of that CA; it is trusted in addition to
  the system roots. With compose, put it in `certs/` and point at
  `/app/certs/<file>`.

A wrong setup shows at startup, in the gateway reachability check:

| Log says | Cause |
|---|---|
| `x509: certificate signed by unknown authority` | `ca_file` is not set, or holds a different CA |
| `x509: certificate is valid for ..., not ...` | the certificate doesn't name the `base_url` host |
| `connection refused` | nothing listens on that address and port: the proxy is down, or the port is not published |
| `gateway.ca_file is set but gateway.base_url ... is not https` | the address still points at plain HTTP |
| `failed to read the gateway CA file` / `holds no PEM certificate` | the path is wrong, or the file isn't PEM |

The last two stop startup; the others leave the mediator running, and every
delivery fails with `502` until the gateway is reachable.

### Being reached by the portal (inbound)

Port 8085 is published on all host interfaces, so register a `targetUrl` the
portal can resolve from wherever it runs:

| API Portal runs | `targetUrl` host |
|---|---|
| its own compose stack, Docker Desktop | `host.docker.internal:8085` |
| its own compose stack, Docker Engine on Linux | `172.17.0.1:8085` |
| on the host machine | `localhost:8085` |
| Kubernetes, or another machine | the mediator host's routable address or DNS name |

Change the published port with `SM_PUBLISHED_PORT`, and narrow the bind to
loopback with `SM_PUBLISHED_HOST=127.0.0.1` when only a portal on this same host
needs to reach it.

---

## What it does with each event

| API Portal event | Gateway call |
|---|---|
| `subscription.created` | `POST /subscriptions` with the portal's own token, and the portal's subscription ID as `externalRefId` (it becomes the subscription's `id`) |
| `subscription.updated` | `PUT /subscriptions/{id}` `{status}` |
| `subscription.plan_changed` | `PUT /subscriptions/{id}` `{subscriptionPlanId}` — no token needed |
| `subscription.token_regenerated` | `PUT /subscriptions/{id}` `{subscriptionToken}`; creates the subscription if the gateway has none |
| `subscription.deleted` | `DELETE /subscriptions/{id}` |
| `apikey.generated` | `POST /rest-apis/{api}/api-keys`, injecting the portal's key value, named `pk-<portal key ID>` and with the portal's key ID as `externalRefId` |
| `apikey.regenerated` | `PUT /rest-apis/{api}/api-keys/{name}`; creates the key if the gateway has none |
| `apikey.revoked` | `DELETE /rest-apis/{api}/api-keys/{name}` |
| `application.*`, `apikey.application_updated` | acknowledged, no gateway call — the gateway has no application resource, and the key-to-app link is attribution only |

Every subscription `{id}` is the portal's subscription ID itself: the gateway
stores the subscription under the `externalRefId` it was created with, so the
mediator reads it back with `GET /subscriptions/{portal subscription ID}`, where
a `404` means the gateway has none. Every key `{name}` comes from
`GET /rest-apis/{api}/api-keys`, matched on the name `pk-<portal key ID>` (or,
for a key created before that naming, on `externalRefId`). The name is what
lasts: gateway 1.2.0 clears a key's `externalRefId` when the key is updated, so
after one regeneration only the name still identifies it. Nothing is remembered
between events.

**The subscription calls need a gateway controller that includes
[wso2/api-platform#3804](https://github.com/wso2/api-platform/pull/3804)**
(issue [#3782](https://github.com/wso2/api-platform/issues/3782)). Stock 1.2.0
does not; `thivindu/gateway-controller:1.2.0.17-SNAPSHOT` does, and is the base
controller image of the custom gateway build. That change makes `POST
/subscriptions` accept an optional `externalRefId` (`^[A-Za-z0-9][A-Za-z0-9._-]*$`,
1–64 characters) that becomes the subscription's `id`, echoed back as
`externalRefId`, with a reused one answered `409`; and makes `PUT
/subscriptions/{id}` a partial update of `status`, `subscriptionPlanId` (`null`
or `""` clears it) and `subscriptionToken`. It also turns a plan renamed to a
taken name, or a plan deleted while in use, into a `409` instead of a `500`.

Plans are matched by name against `GET /subscription-plans` and created when
missing. A gateway API may also carry its own `spec.subscriptionPlans`
allowlist, and the gateway rejects a subscription whose plan is not on it — so
when the target plan is absent from the allowlist, the subscription is created
**without a plan**, or a plan change clears its plan, rather than the event
failing. That keeps access
working, at the cost of the plan's rate limit, so the decision is never taken on
a cached reading: the allowlist is cached for five minutes and an operator can
widen it on the gateway with no event to announce the change, so a plan that
looks disabled is confirmed against a fresh `GET /rest-apis` first. A widened
allowlist therefore takes effect on the next event, not five minutes later.

The portal side of each event is specified in the
[Webhook Event Catalog](https://wso2.com/api-platform/docs/api-portal/1.0.0/references/webhook-event-catalog/);
the gateway side is the
[Gateway Controller Management API](https://wso2.com/api-platform/docs/api-gateway/1.2.0/gateway-controller-management-api/overview/).

**Only `RestApi` artifacts work.** The gateway 1.2.0 subscription and API-key
endpoints reject every other kind, so an event for `Mcp`, `WebSubApi` etc. is
recorded as a permanent failure with that reason.

### Plan changes and token rotation

Both change the existing subscription in place, so its gateway ID stays the
same and it never briefly disappears. A plan change sends only the new plan:
the gateway keeps the token's hash, so the token the portal leaves out of
`subscription.plan_changed` isn't needed. A token rotation sends the new token
from the event.

### Lost deliveries

A status or plan change for a subscription the gateway doesn't have — its
`subscription.created` delivery was lost — fails with `422` and says so. The fix
is to regenerate the subscription token in the portal: that event carries
everything a create needs, so the mediator creates the subscription from it.
Likewise, an `apikey.regenerated` for a key the gateway doesn't have creates it.

### API key names

A gateway key is named after the portal key ID: `pk-` and the ID, slugified
(`pk-3f9a1c2b-…`, within the gateway's 63 characters). The portal's key handle
isn't used: two handles can slugify alike, and the name has to identify the key
on its own, since the gateway drops a key's `externalRefId` when the key is
updated. A portal key ID names one key for its whole life, so a `generated`
event for a key the gateway holds revoked (a late duplicate) is refused rather
than bringing the credential back.

### API keys don't survive a gateway reconnect to API Manager

With API Manager as its control plane, the gateway controller runs an API key
sync every time it connects (at startup, and on every reconnect): it fetches
API Manager's keys and revokes local keys that API Manager doesn't have. The
keys this mediator injects exist only on the gateway, so each connect revokes
all of them, and developers' API keys stop working until they regenerate them in
the portal. Subscriptions aren't affected. The mediator can't put the keys back:
the portal never stores a key's value. Seen on gateway 1.2.0.12 and
`thivindu/gateway-controller:1.2.0.17-SNAPSHOT` (log line
`Starting API key sync at gateway startup`, then `Revoking API key`); it needs a
gateway-side fix.

---

## How a portal API is matched to a gateway API

The portal sends `api.ref_id`, the listing's `spec.referenceId`. A publisher
that knows the gateway API sets it to that API's name (`metadata.name`); the API
catalog always does. Matching is tried in this order:

1. An `[api_handles]` entry in `configs/config.toml`.
2. A resolution from the last five minutes, kept in memory.
3. **The gateway API named exactly `api.ref_id`.** This is authoritative: an API
   that merely shares the display name and version can't take its place.
4. `GET /rest-apis?displayName=<name>&version=<version>`, the exact name match.
5. The gateway's own handle convention, `<slugified-name>-<version>`.
6. A scan of the full list, folding case, spacing and punctuation, so
   `Order API` matches `order-api`, `OrderAPI` or `Order-API`.

Steps 4–6 are the fallback for a listing without a `ref_id`, or whose `ref_id`
names nothing on the gateway (for example a Platform API UUID).

Versions aren't guessed at: portal `v1.0` against gateway `1.0.0` doesn't match,
because those are different versions. When nothing resolves, the delivery fails
with `422` and the log says what to add:

```bash
docker compose logs subscription-mediator | grep "cannot be applied"
#  -> no gateway REST API matches portal API "Order API" version "v1.0"; deploy it
#     to the gateway first, or pin it in configs/config.toml with
#     [api_handles] "order api:v1.0" = "<gateway-api-handle>"
```

Add it under `[api_handles]` in `configs/config.toml`, run
`docker compose restart`, then repeat the action in the portal so it sends the
event again.

---

## Delivery and failures

**The API Portal attempts each delivery exactly once — no retry, no dead-letter
queue.** A failed delivery is marked `FAILED` in the portal and never resent.
This service doesn't retry either. Instead:

- Each delivery is applied before it is answered, so `200` means the gateway
  already reflects it, and anything else means it doesn't. Nothing is accepted
  and then lost.
- A failure shows up in the portal's delivery history, which lists every
  delivery that didn't get a `2xx`. Fix the cause, then repeat the action in
  the portal so it sends a new event:

  ```bash
  curl -sk "$PORTAL/webhook-subscribers/<subscriber-id>/deliveries" \
    -H "Authorization: Bearer $TOKEN" \
    | jq -r '.list[] | select(.status != "DELIVERED")
             | "\(.occurredAt) \(.eventType) \(.status) \(.lastError)"'
  ```

- A gateway that's down or slow fails the delivery (`502`). The portal's
  `timeoutMs` bounds how long it waits, and the mediator spends at most 25
  seconds on one delivery.
- If the portal gives up waiting, the mediator still finishes the job. The
  portal then shows `FAILED` for an event that did apply; repeating it is safe.
- Repeats converge: a duplicate create gets a `409` and finds the subscription
  under the portal's subscription ID, the same `PUT` twice changes nothing, a delete of something
  already gone is success, and a regenerate whose generate was lost creates the
  key.
- Events for one subscription (or one API key) arriving together are applied one
  after the other within a pod. Unrelated ones run concurrently.

`GET /status` counts what this process applied and failed since it started. With
several replicas each pod counts its own.

---

## Compared with the Platform API

In a native setup the Platform API receives the same portal webhooks and does
this job itself. The comparison below is from the code: this service, and
[`wso2/api-platform` at `eb7dcaba9`](https://github.com/wso2/api-platform/tree/eb7dcaba9ab54924e66e3ebfbbbfc1cbf8ecce57)
(`main`, 2026-10-14). The gateway behaviour it relies on was also checked
against the `gateway/v1.2.0` tag. Paths starting with `platform-api/` or
`gateway/` are in that repository.

- **This service:** API Portal → webhook → mediator → gateway management REST
  API. The gateway's control plane is API Manager.
- **Native:** API Portal → webhook → Platform API, which writes to its own
  database and publishes to a SQL-backed event hub (`common/eventhub/`). The
  event reaches the gateway over its control-plane WebSocket. The Platform API
  *is* the gateway's control plane.

### Receiving the webhook

| | This service | Platform API |
|---|---|---|
| Signature check | HMAC over the raw body, with a time tolerance | Same (`platform-api/internal/webhook/receiver.go`, `ReceiveEvent`) |
| When it replies | **`200` once the gateway reflects the event**, in the same request | **`200` after the event is handled**, in the same request |
| On failure | `401` / `400` / `422` (can't be applied) / `502` (gateway failed) | An error status: `4xx` for a bad envelope or decryption, `5xx` for storage errors. The portal never retries either |
| Duplicates | No `event_id` check. Each handler converges on the gateway's state | No `event_id` check. Each handler is idempotent "by domain identity" |
| Retries | None. The failure goes back to the portal, which marks the delivery `FAILED` | None. Same |
| Ordering | Events for one subscription or key arriving together run one after the other within a pod | Nothing explicit. Each delivery is handled in its own request |

### Finding the API and the plan

| | This service | Platform API |
|---|---|---|
| API | The portal's `ref_id` is unknown to an API Manager-fed gateway, so: override, in-memory cache, then **search by name and version** | `ref_id` is the Platform API's own artifact handle, looked up by handle and kind |
| Plan | Gateway plan **by name**, else **create the plan**. If the gateway rejects the plan, the subscription goes without one | Looked up by handle. **A missing plan fails the event** (`SubscriptionPlanNotFound`). Plans are never created |

### Subscriptions

| | This service | Platform API |
|---|---|---|
| Identity | **The portal's `subscription_id`**, sent as `externalRefId` when the gateway subscription is created | **API + `subscriber_id`**, one subscription per pair (`FindByArtifactKindAndSubscriber`). The portal's `subscription_id` isn't used |
| ID on the gateway | **The portal's `subscription_id`**: the gateway uses the `externalRefId` as the ID, so `GET /subscriptions/{id}` finds it | **The Platform API's UUID.** The gateway looks subscriptions up by it |
| Token at rest | **Not kept.** Only the gateway's hash exists | **AES-256-GCM encrypted**, with a hash for lookups (`platform-api/internal/repository/subscription_repository.go`) |
| `created` | `POST /subscriptions` with the portal's token | Saved with the portal's token, then `subscription.created` is sent to every gateway the API is deployed on |
| `updated` (status) | `PUT /subscriptions/{id}` `{status}` | Database update, then `subscription.updated` with the full record |
| `plan_changed` | `PUT /subscriptions/{id}` `{subscriptionPlanId}`, **in place, no token needed** | Database update, then `subscription.updated` carrying the token and new plan. **The gateway updates in place** (`handleSubscriptionUpdatedEvent` in `gateway/gateway-controller/pkg/controlplane/client.go`) |
| `token_regenerated` | `PUT /subscriptions/{id}` `{subscriptionToken}`, in place; creates a missing subscription | Database update, then `subscription.updated` |
| `deleted` | `DELETE`; nothing found counts as success | Database delete, then broadcast. Already gone counts as success |

### API keys and applications

| | This service | Platform API |
|---|---|---|
| Gateway key name | `pk-<portal key ID>`. Found later by that name | The handle as-is; keys are unique by artifact + name |
| Duplicate generate | Finds the key by name and sets the same value again | A unique-constraint violation counts as success |
| `apikey.application_updated` | **Ignored** (logged and acknowledged) | Handled |
| `application.*` | Ignored. The gateway has no application resource | Handled |

### Reaching the gateway and recovering missed changes

| | This service | Platform API |
|---|---|---|
| Call to the gateway | Synchronous REST with basic auth | Publish to the event hub, delivered over the gateway's WebSocket |
| Gateway down | `502`; the portal marks the delivery `FAILED` | The publish error is **logged only**; the webhook still returns `200` |
| Gateway catching up | **Not possible.** The gateway skips its subscription resync for an on-prem control plane (`syncSubscriptionsForExistingAPIs`) | On every connect and reconnect the gateway pulls all plans and subscriptions from the Platform API |
| Portal delivery lost | Lost; redo the action in the portal | **Also lost**; same one-attempt webhook |

### State and deployment

| | This service | Platform API |
|---|---|---|
| Storage | **None.** Every lookup reads the gateway | A SQL database; the event hub is SQL-backed too |
| Contents | — | Its full domain model (APIs, plans, subscriptions, keys, applications), tokens encrypted |
| Replicas | Any number; the chart defaults to two | Not tied to a local file. Its multi-replica deployment wasn't checked here |

### What the differences come down to

1. **Who owns the gateway's state.** The Platform API is the gateway's control
   plane: it picks the IDs, pushes full records, and the gateway resyncs from
   it. This service is an outside client of a gateway API Manager owns, hence
   the caller-chosen subscription IDs and the name-based API matching.
2. **Where failures are absorbed.** Both handle the event in the request. The
   Platform API's own write always succeeds and the gateway resyncs from it
   later; this service reports a gateway failure straight back to the portal,
   where the action has to be repeated. Neither can recover a portal delivery
   that never arrived.
3. **What is stored.** The Platform API keeps a decryptable copy of every token.
   This service keeps nothing, which the in-place plan update makes possible:
   a plan change no longer needs the token.

---

## Layout

```
Dockerfile                   build context is THIS directory
.env.example                 template for the git-ignored .env (SM_* overrides)
.dockerignore                keeps .env and configs/ out of it
docker-compose.yaml          how it runs
configs/config.toml          all configuration; mounted read-only
configs/config-template.toml the annotated version to copy from
certs/                       the gateway's CA certificate, if any; mounted read-only
Makefile                     make build, make test, make build-and-push-multiarch
deploy/helm/                 the Helm chart for Kubernetes
cmd/subscription-mediator/   startup and wiring
internal/config/             file + env loading, validation, fixed constants
internal/webhook/            envelope, HMAC verification, AES-256-GCM decryption
internal/gateway/            management REST API client
internal/mediator/           event translation, lookups by portal ID
internal/server/             HTTP endpoints
```

The component is standalone: it depends on nothing else in this repository, and
builds from this directory alone. Its only external dependency is koanf (with
its TOML parser and env provider) for config loading — everything else is the
standard library.

---

## References

- [Webhook Event Catalog](https://wso2.com/api-platform/docs/api-portal/1.0.0/references/webhook-event-catalog/)
  — the delivery envelope, signature header, field decryption and delivery
  semantics this service implements, and the full event list it handles
- [Configure webhooks](https://wso2.com/api-platform/docs/api-portal/1.0.0/admin-settings/webhook-integration/)
  — registering a subscriber, the shared secret, and delivery behaviour
- [Gateway Controller Management API](https://wso2.com/api-platform/docs/api-gateway/1.2.0/gateway-controller-management-api/overview/)
  — every call this service makes; see
  [REST API Management](https://wso2.com/api-platform/docs/api-gateway/1.2.0/gateway-controller-management-api/rest-api-management/)
  for the REST API, subscription, plan and API-key resources
- [Policies Overview](https://wso2.com/api-platform/docs/api-gateway/1.2.0/policies/overview/)
  — `api-key-auth` and `subscription-validation`, the two policies that make a
  mediated subscription actually enforce anything
- [Manage Subscriptions](https://wso2.com/api-platform/docs/api-portal/1.0.0/consume-an-api/manage-subscriptions/)
  and [Manage API Keys](https://wso2.com/api-platform/docs/api-portal/1.0.0/consume-an-api/manage-api-keys/)
  — the portal actions that produce the events
- [Getting Started with Platform Gateway](https://apim.docs.wso2.com/en/latest/api-gateway/platform-gateway/getting-started/)
  — attaching the gateway to WSO2 API Manager 4.7.0, the setup this exists for
- Distributions: [`gateway/v1.2.0`](https://github.com/wso2/api-platform/releases/tag/gateway%2Fv1.2.0) ·
  [`api-portal/v1.0.0`](https://github.com/wso2/api-platform/releases/tag/api-portal%2Fv1.0.0) ·
  [all releases](https://github.com/wso2/api-platform/releases)
