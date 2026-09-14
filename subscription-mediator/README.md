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
> unauthenticated and plaintext, and both `configs/config.toml` and the state file 
> hold secrets in the clear (unless you keep them in the environment — see Configuration).
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
  │  verify → decrypt → queue → apply                   │
  └─────────────────────────────────────────────────────┘
```

The gateway keeps getting its API definitions from API Manager. This service
only adds the subscription and credential state the portal owns.

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
these through, and `.env` is git-ignored — so nothing secret is ever staged:

```bash
SM_WEBHOOK_SECRET=<the generated secret>
SM_GATEWAY_USERNAME=<gateway-admin-username>
SM_GATEWAY_PASSWORD=<gateway-admin-password>
SM_GATEWAY_BASE_URL=http://host.docker.internal:9090/api/management/v1
```

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
msg="subscription-mediator is ready" workers=4 max_pending_events=5000
msg="listening for API Portal webhooks" address=0.0.0.0:8085 path=/devportal/events
```

If the gateway isn't up yet you get a warning instead of a failure — events are
queued and applied once it answers.

### Register it with the API Portal

Registering a subscriber has no UI, so it is a `curl` — see
[Configure webhooks](https://wso2.com/api-platform/docs/api-portal/1.0.0/admin-settings/webhook-integration/).
Use the **same** `[webhook] secret`. Without a matching secret the portal omits
the subscription token and API key value from its deliveries entirely, and the
mediator has nothing to give the gateway.

```bash
curl -k -X POST "https://localhost:9543/api-portal/api/v0.9/webhook-subscribers" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "displayName": "Platform gateway mediator",
    "targetUrl": "http://<host-reachable-from-the-portal>:8085/devportal/events",
    "secret": "<the same secret from configs/config.toml>",
    "events": ["subscription.*", "apikey.*"],
    "timeoutMs": 5000
  }'
```

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
make test      # run the tests
make build     # build the image (docker compose build)
```

Everything else is plain Compose:

```bash
docker compose up -d       # start; also picks up a freshly built image
docker compose logs -f
docker compose ps
docker compose down        # stop, keeping the state volume
docker compose down -v     # ...and delete the state volume
```

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

---

## Configuration

Everything lives in `configs/config.toml`, mounted read-only at
`/app/config.toml`. `configs/config-template.toml` is the annotated copy.

```toml
[webhook]
secret = ""                 # required

[gateway]
base_url = "..."            # required
username = ""               # required
password = ""               # required

[logging]
level = "info"              # optional: debug | info | warn | error

[api_handles]               # optional, see below
# "order api:v1.0" = "orangeapi-1.0.0"
```

Anything else — worker count, retry budget, queue ceiling, the listen address,
timeouts — is a constant in the source: `mediator.Workers`,
`mediator.MaxAttempts`, `config.ListenAddress`, and so on. A demo has no reason
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
| `GET /status` | queue depth, applied/failed counters, mapping counts |
| `GET /failed` | events that exhausted every attempt, with the reason |
| `POST /failed/requeue` | put them back on the queue after fixing the cause |

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
the mounted config:

| Gateway runs | `SM_GATEWAY_BASE_URL` |
|---|---|
| its own compose stack, Docker Desktop | *(the default)* `http://host.docker.internal:9090/api/management/v1` |
| its own compose stack, Docker Engine on Linux | `http://172.17.0.1:9090/api/management/v1` |
| on the host machine | same as above — it's the same published port |
| the mediator itself run outside Docker | `http://localhost:9090/api/management/v1` |
| Kubernetes, or any remote host | `https://gateway.example.com/api/management/v1` |

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
| `subscription.created` | `POST /subscriptions` with the portal's own token |
| `subscription.updated` | `PUT /subscriptions/{id}` (status only) |
| `subscription.plan_changed` | `DELETE` + `POST /subscriptions` — see below |
| `subscription.token_regenerated` | `DELETE` + `POST /subscriptions` with the new token |
| `subscription.deleted` | `DELETE /subscriptions/{id}` |
| `apikey.generated` | `POST /rest-apis/{api}/api-keys`, injecting the portal's key value |
| `apikey.regenerated` | `PUT /rest-apis/{api}/api-keys/{name}` |
| `apikey.revoked` | `DELETE /rest-apis/{api}/api-keys/{name}` |
| `application.*`, `apikey.application_updated` | acknowledged, no gateway call — the gateway has no application resource, and the key-to-app link is attribution only |

Plans are matched by name against `GET /subscription-plans` and created when
missing. A gateway API may also carry its own `spec.subscriptionPlans`
allowlist, and the gateway rejects a subscription whose plan is not on it — so
when the target plan is absent from the allowlist, the subscription is created
or recreated **without a plan** rather than not at all. That keeps access
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

The gateway's `PUT /subscriptions/{id}` accepts **only** a status change — there
is no way to change a plan or replace a token in place, and the gateway stores
only the token's hash. So both are applied as a delete followed by a create. A
token rotation carries its new token; a plan change doesn't, which is why the
state file keeps each subscription token.

---

## How a portal API is matched to a gateway API

The portal sends `api.ref_id` — a Platform API UUID your gateway has never seen,
because its APIs came from API Manager. So matching is on display name and
version, tried in this order:

1. An `[api_handles]` entry in `configs/config.toml`.
2. A mapping resolved earlier and persisted.
3. `GET /rest-apis?displayName=<name>&version=<version>` — the exact match.
4. A scan of the full list, folding case, spacing and punctuation, so
   `Order API` matches `order-api`, `OrderAPI` or `Order-API`.
5. The gateway's own handle convention, `<slugified-name>-<version>`.

Versions aren't guessed at: portal `v1.0` against gateway `1.0.0` doesn't match,
because those are different versions. When nothing resolves the event fails
permanently and says what to add:

```bash
curl -s localhost:8085/failed | jq
#  -> no gateway REST API matches portal API "Order API" version "v1.0"; deploy it
#     to the gateway first, or pin it in configs/config.toml with
#     [api_handles] "order api:v1.0" = "<gateway-api-handle>"
```

Add it under `[api_handles]` in `configs/config.toml`, then:

```bash
docker compose restart
curl -s -X POST localhost:8085/failed/requeue
```

---

## Delivery and retries

**The API Portal attempts each delivery exactly once — no retry, no dead-letter
queue.** A failed delivery is marked `FAILED` in the portal and never resent.
Hence:

- A verified event is written to the state file and fsync'd **before** the
  mediator returns `202`, so a restart between accepting and applying loses
  nothing. That's what the `mediator-data` volume is for.
- A gateway that's down or slow doesn't cost you the event: it stays queued and
  is retried with exponential backoff, up to 6 attempts.
- The queue is capped at 5000 pending events; past that the endpoint returns
  `503` rather than growing without limit.
- Events for one subscription (or one API key) are routed to the same worker
  lane and applied in order, so a create and the delete after it can't cross.
  Unrelated subscriptions run concurrently across 4 lanes.
- Each `event_id` is applied once, and that survives a restart. Beyond that the
  handlers converge: a duplicate create adopts the existing subscription, a
  delete of something already gone is success, and a rotate whose create was
  missed creates the key.
- A permanent failure (unresolvable API, unsupported kind, a request the gateway
  rejects) is retired immediately rather than burning the retry budget. See
  `GET /failed`.

If a `subscription.created` delivery is missed entirely, the mediator can adopt
the matching gateway subscription when a later event arrives — but it can't
invent a token it never received. The fix is to regenerate the subscription
token in the portal, which sends a fresh event the mediator can apply in full.

---

## Layout

```
Dockerfile                   build context is THIS directory
docker-compose.yaml          how it runs
configs/config.toml          all configuration; mounted read-only
configs/config-template.toml the annotated version to copy from
Makefile                     make build, make test
cmd/subscription-mediator/   startup and wiring
internal/config/             file + env loading, validation, fixed constants
internal/webhook/            envelope, HMAC verification, AES-256-GCM decryption
internal/gateway/            management REST API client
internal/mediator/           event translation, worker lanes, retry
internal/store/              durable mappings, idempotency ledger, pending queue
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
