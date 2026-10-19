# API Portal → API Platform Gateway subscription bridge

> **This is built for a demo.** It is not a WSO2 product and carries no warranty of any kind. Don't run it in production.

Run the **WSO2 API Portal 1.0.0** as the developer-facing portal for a **WSO2 API
Platform Gateway 1.2.0** whose APIs come from **WSO2 API Manager 4.7.0** — and
have subscriptions, subscription plans and API keys created in the portal
actually take effect on that gateway.

This repository holds the service that makes that work, plus the sample APIs and
the end-to-end walkthrough to set it up from scratch.

## Versions

Everything here was built and tested against exactly these versions, and nothing
else:

| Component | Version | Documentation | Distribution |
|---|---|---|---|
| WSO2 API Manager | **4.7.0** | [apim.docs.wso2.com](https://apim.docs.wso2.com/en/latest/) | — |
| WSO2 API Platform Gateway | **1.2.0** | [API Gateway 1.2.0](https://wso2.com/api-platform/docs/api-gateway/1.2.0/overview/) | [`gateway/v1.2.0`](https://github.com/wso2/api-platform/releases/tag/gateway%2Fv1.2.0) |
| WSO2 API Portal | **1.0.0** | [API Portal & MCP Hub 1.0.0](https://wso2.com/api-platform/docs/api-portal/1.0.0/overview/) | [`api-portal/v1.0.0`](https://github.com/wso2/api-platform/releases/tag/api-portal%2Fv1.0.0) |

These products move quickly and their REST APIs are versioned independently of
the products themselves, so treat another combination as untested — the
management API paths, the webhook envelope and the subscription-plan fields used
below all vary between releases.

## The problem

A gateway attached to the Platform API holds a WebSocket to it and receives
subscription, plan and API-key events over that channel. A gateway attached to
**API Manager** does not — it has no connection to the API Portal at all. So a
developer can subscribe to an API in the portal and generate credentials, and the
gateway will still reject every call: it was never told.

The API Portal doesn't talk to a gateway either. It delivers a signed HTTP POST
to whatever URL you register as a webhook subscriber, and leaves it to the
receiver to do something useful. **`subscription-mediator` is that receiver** —
it verifies each delivery, decrypts the subscription token and API key, and
replays them onto the gateway's management REST API.

```
       ┌───────────────────────────────┐
       │     WSO2 API Manager 4.7.0    │  control plane — API lifecycle
       └───────────────┬───────────────┘
                       │  API definitions (both directions)
                       ▼
  ┌──────────────────┐         ┌──────────────────────────────┐
  │ WSO2 API Portal  │         │  API Platform Gateway 1.2.0  │
  │      1.0.0       │         │                              │
  │ subscriptions,   │         │  enforces api-key-auth +     │
  │ plans, API keys  │         │  subscription-validation     │
  └────────┬─────────┘         └──────────────▲───────────────┘
           │                                  │
           │ signed webhooks                  │ management REST API
           │ (HMAC-SHA256, fields             │ (HTTP basic auth)
           │  AES-256-GCM encrypted)          │
           ▼                                  │
     ┌────────────────────────────────────────┴──┐
     │            subscription-mediator          │
     │       verify → decrypt → apply → answer   │
     └───────────────────────────────────────────┘
```

## What's in here

```
RUNBOOK.md                  operating it in dev: release, deploy, rotate,
                            monitor, and the incident playbooks
agent-verification.md       an end-to-end test script for a coding agent —
                            every subscription and API-key operation, asserted
subscription-mediator/      the bridge itself — Go, runs in Docker
                            (see its own README for internals and operations)
.github/workflows/          CI: tests the mediator and publishes its image to
                            the dev Docker repository in JFrog Artifactory (setup: the
                            mediator README, "Publishing the image")
resources/                  the sample APIs (see resources/README.md)
  echo-api/                 a trivial httpbin-backed API
  reading-list-api/         a small CRUD API
  pizza-shack-api/          the APIM sample, used for the control-plane path
```

Each API folder holds up to three files:

| File | Used for |
|---|---|
| `*-subscription-api.yaml` | creating the API **on the gateway** (data plane) |
| `api.yaml` | the API's **portal** metadata |
| `definition.yaml` | its OpenAPI definition, uploaded with `api.yaml` |

Echo and Reading List are created on the gateway and sync *up* to API Manager
(DP→CP). Pizza Shack goes the other way: created in API Manager and deployed
*down* to the gateway. Both end up in the portal the same way.

---

## Prerequisites

The three products at the versions in the table above:

- **WSO2 API Manager 4.7.0**
- **WSO2 API Platform Gateway 1.2.0** —
  [`wso2apip-api-gateway-1.2.0.zip`](https://github.com/wso2/api-platform/releases/tag/gateway%2Fv1.2.0);
  setup steps in
  [Getting Started with Platform Gateway](https://apim.docs.wso2.com/en/latest/api-gateway/platform-gateway/getting-started/).
  Its gateway controller must include
  [wso2/api-platform#3804](https://github.com/wso2/api-platform/pull/3804)
  (issue [#3782](https://github.com/wso2/api-platform/issues/3782)): `POST
  /subscriptions` accepting an `externalRefId` that becomes the subscription's
  `id`, and a partial `PUT /subscriptions/{id}`. Stock 1.2.0 does not;
  `thivindu/gateway-controller:1.2.0.17-SNAPSHOT` does, and is the base
  controller image of the custom gateway build.
- **WSO2 API Portal 1.0.0** —
  [`wso2apip-api-portal-1.0.0.zip`](https://github.com/wso2/api-platform/releases/tag/api-portal%2Fv1.0.0);
  setup steps in
  [Secured API End to End](https://wso2.com/api-platform/docs/api-portal/1.0.0/tutorials/secured-api-end-to-end/)

Both distributions unzip with their own `README.md` and a one-time
`scripts/setup.sh`; run that before the first `docker compose up`.

Plus Docker with Compose v2, `curl`, `jq` and `openssl`.

**API Manager 4.7.0 needs JDK 21 or later** — its start script passes
`--enable-native-access=ALL-UNNAMED`, which JDK 11 rejects outright with
`Could not create the Java Virtual Machine`. Set `JAVA_HOME` to a 21+ JDK before
`./bin/api-manager.sh`.

This walkthrough runs API Manager with **port offset 1** (so `9444`, leaving
`9443` free). Set it once in `repository/conf/deployment.toml` before the first
start:

```toml
[server]
offset = 1
```

With the default offset 0, use `9443` everywhere `9444` appears below.

Set these once; every command below uses them.

```bash
# WSO2 API Manager
export APIM=<apim-url>
export APIM_USER=<apim-admin-username>
export APIM_PASS=<apim-admin-password>

# API Platform Gateway — management REST API (admin role required)
export GW_USER=<gateway-admin-username>
export GW_PASS=<gateway-admin-password>
export GW_MGMT=http://localhost:9090/api/management/v1
export GW_URL=https://localhost:8443

# WSO2 API Portal
export PORTAL_USER=<portal-admin-username>
export PORTAL_PASS=<portal-admin-password>
export PORTAL=https://localhost:9543/api-portal/api/v0.9

# The address containers use to reach services on this machine (see below)
export HOST_ADDR=$(ipconfig getifaddr en0)      # macOS
# export HOST_ADDR=172.17.0.1                   # Docker Engine on Linux
```

> **`host.docker.internal` does not work from the gateway container.** The
> gateway 1.2.0 `docker-compose.yaml` sets
> `extra_hosts: ["host.docker.internal:host-gateway"]`, which on **Docker Desktop
> for macOS** *replaces* the working built-in name with the Linux bridge address
> `172.17.0.1` — unreachable there, so every connection out to the host is
> refused. Use the machine's own LAN IP (`$HOST_ADDR`) for anything the
> **gateway** must reach on the host. The portal and the mediator don't set
> `extra_hosts`, so `host.docker.internal` works normally for them.

The gateway's management API is HTTP basic auth and its subscription and
subscription-plan operations are **admin-scoped**, so `$GW_USER` needs the admin
role.

---

## 1. Point API Manager at the platform gateway

Add the supported gateway versions to API Manager 4.7.0's
`repository/conf/deployment.toml`:

```toml
[apim.platform_gateway]
versions = ["1.0.0", "1.1.0", "1.2.0"]
```

Start API Manager and open the **Admin Portal** at
<https://localhost:9444/admin>. Go to **Gateways → Add Gateway**, choose the
**API Platform Gateway** type, and fill in
(see [Getting Started with Platform Gateway](https://apim.docs.wso2.com/en/latest/api-gateway/platform-gateway/getting-started/)):

| Field | Value |
|---|---|
| Name | `dev-platform-gateway` |
| Display Name | `Dev Platform Gateway` |
| URL | `https://localhost:8443` |
| Version | `1.2.0` |
| Visibility | Public |

**Name** must be lowercase letters, digits and hyphens (3–64 characters). It is
the gateway's identity, and you give it back to the gateway as
`APIP_GW_CONTROLLER_CONTROLPLANE_GATEWAY_NAME` in step 3.

On save, the Admin Portal shows a **registration token once**. Copy it now — it
is stored only as a hash and can never be displayed again, only regenerated from
the gateway's row.

> If you don't see an API Platform Gateway option, the `[apim.platform_gateway]`
> block above wasn't picked up — check it and restart API Manager.

## 2. Register a DCR application

The gateway needs an OAuth2 client to push artifacts back up to API Manager
(the DP→CP direction).

```bash
curl -sk -X POST "$APIM/client-registration/v0.17/register" \
  -u "$APIM_USER:$APIM_PASS" \
  -H "Content-Type: application/json" \
  -d '{
    "callbackUrl": "https://localhost/callback",
    "clientName": "publisher_app",
    "owner": "'"$APIM_USER"'",
    "grantType": "password refresh_token client_credentials",
    "saasApp": true
  }' | jq
```

The response carries `clientId` and `clientSecret`. Keep them out of version
control, then exchange them for the admin token the steps above and below use:

```bash
export CID=<clientId>
export CSEC=<clientSecret>
export APIM_TOKEN=$(curl -sk -X POST "$APIM/oauth2/token" -u "$CID:$CSEC" \
  -d "grant_type=password&username=$APIM_USER&password=$APIM_PASS&scope=apim:admin%20apim:api_view%20apim:api_create%20apim:api_publish%20apim:api_manage" \
  | jq -r .access_token)
```

`$APIM_TOKEN` isn't needed for the walkthrough itself — the Publisher and Admin
portals handle their own sessions — but it's what the verification commands and
[`agent-verification.md`](agent-verification.md) use.

## 3. Configure and start the gateway

Set these on the gateway, taking the host and token from **Step 2: Configure the
Gateway** in the API Manager UI:

| Variable | Value |
|---|---|
| `APIP_GW_CONTROLLER_CONTROLPLANE_HOST` | `$HOST_ADDR:9444` — **not** `host.docker.internal` |
| `APIP_GW_CONTROLLER_CONTROLPLANE_TOKEN` | the registration token from step 1 |
| `APIP_GW_CONTROLLER_CONTROLPLANE_GATEWAY_NAME` | the gateway's `name`, e.g. `dev-platform-gateway` |
| `APIP_GW_CONTROLLER_CONTROLPLANE_INSECURE_SKIP_VERIFY` | `true` (API Manager's TLS cert is self-signed) |
| `GATEWAY_CONTROLPLANE_ON_PREM` | `true` |
| `APIP_GW_CONTROLLER_CONTROLPLANE_APIM_OAUTH2_CLIENT_ID` | `clientId` from step 2 |
| `APIP_GW_CONTROLLER_CONTROLPLANE_APIM_OAUTH2_CLIENT_SECRET` | `clientSecret` from step 2 |

Reference:
[Control Plane Connection](https://wso2.com/api-platform/docs/api-gateway/1.2.0/deployment/production-deployment/control-plane-connection/)
and the distribution's own `README.md` ("Connecting to WSO2 API Platform").

Append them to `api-platform.env` (created by `./scripts/setup.sh`).

Then `docker compose up -d`. A connected gateway logs:

```
msg="Control plane connection established" gateway_id=... connection_id=...
```

and the Admin Portal's Gateways page flips the gateway to **Active**.

Confirm the management API answers:

```bash
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis" | jq '.apis | length'
```

The controller serves the management API over plain HTTP on `9090` only; it has
no TLS listener of its own. The mediator uses that same port.

## 4. Create the subscription plans on the gateway

A plan is what carries the rate limit. Create `gold` up front:

```bash
curl -s -X POST "$GW_MGMT/subscription-plans" \
  -u "$GW_USER:$GW_PASS" \
  -H 'Content-Type: application/json' \
  -d '{
    "planName": "gold",
    "billingPlan": "COMMERCIAL",
    "throttleLimitCount": 10000,
    "throttleLimitUnit": "Hour",
    "stopOnQuotaReach": true,
    "status": "ACTIVE"
  }' | jq
```

```bash
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscription-plans" | jq
```

> **`throttleLimitCount` and `throttleLimitUnit` must both be set**, or the data
> plane skips rate limiting for that plan entirely. `throttleLimitUnit` is one of
> `Min`, `Hour`, `Day`, `Month` — exactly those, case-sensitive.

The full schema is in the gateway's own
`gateway-controller/api/management-openapi.yaml`, documented at
[Gateway Controller Management API](https://wso2.com/api-platform/docs/api-gateway/1.2.0/gateway-controller-management-api/overview/).

We deliberately *don't* create `iron` here. Step 10 shows what happens when a
plan reaches the gateway only through a subscription event.

## 5. Create the sample APIs on the gateway (DP→CP)

```bash
curl -s -X POST "$GW_MGMT/rest-apis" \
  -u "$GW_USER:$GW_PASS" \
  -H 'Content-Type: text/yaml' \
  --data-binary '@resources/echo-api/echo-subscription-api.yaml'

curl -s -X POST "$GW_MGMT/rest-apis" \
  -u "$GW_USER:$GW_PASS" \
  -H 'Content-Type: text/yaml' \
  --data-binary '@resources/reading-list-api/reading-list-subscription-api.yaml'
```

Both carry the two policies that make this demo meaningful:

```yaml
policies:
  - name: api-key-auth
    params: { key: API-Key, in: header }
  - name: subscription-validation
    params: { subscriptionKeyHeader: Subscription-Key }
```

See [Policies Overview](https://wso2.com/api-platform/docs/api-gateway/1.2.0/policies/overview/)
for how policies are declared and ordered, and
[REST API Management](https://wso2.com/api-platform/docs/api-gateway/1.2.0/gateway-controller-management-api/rest-api-management/)
for this endpoint.

List what's deployed, and delete one by its handle if you need to start over:

```bash
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis" | jq '.apis[].status.id'

curl -s -X DELETE -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis/echo-api-v1"
```

They should now appear in API Manager too. Calling them fails, as it should:

```bash
curl -k "$GW_URL/reading-list/books"
curl -k "$GW_URL/echo/foo"
# {"error":"Unauthorized","message":"Valid API key required"}
```

## 6. Create an API in API Manager and deploy it (CP→DP)

Everything in this step happens in the **Publisher Portal** at
<https://localhost:9444/publisher>.

### Create the API

**Create API → REST API → Import Open API**, upload
`resources/pizza-shack-api/definition.yaml`, and set:

| Field | Value |
|---|---|
| Name | `PizzaShack API` |
| Context | `/pizzashack` |
| Version | `1.0.0` |
| Endpoint | `https://<your-host>:9444/am/sample/pizzashack/v1/api` |
| Gateway | **Dev Platform Gateway** |

The endpoint has to be reachable **from the router container**, so use the host
address rather than `localhost`.

### Secure it from the Policies page

A platform-gateway API is **not** secured through **Runtime Configuration → API
Security**. That page drives `securityScheme`, a Synapse-gateway concept: API
Manager accepts it and leaves it unchanged for a platform-gateway API, and
nothing reaches the gateway from it.

Security comes from **policies** instead. Open the API's **Policies** tab and
drag both onto the **Request** flow, at API level:

| Policy | Parameters |
|---|---|
| **API Key Auth** | `key` = `API-Key`, `in` = `header` |
| **Subscription Validation** | `subscriptionKeyHeader` = `Subscription-Key` |

These are the same two policies the step 5 YAML declares. Save.

> The Policies page for a platform-gateway API reads the **Policy Hub**, a remote
> catalogue (`app.policyHub.endpoint` in the Publisher's `settings.json`). It
> needs outbound internet access from your browser — with none, the palette is
> empty and there is nothing to drag.

### Deploy and publish

**Deploy → Deploy New Revision**, pick `Dev Platform Gateway`, deploy. Then
**Lifecycle → Publish**.

Policies only reach the gateway with a deployment, so **any later policy change
needs a new revision deployed** — saving alone changes nothing on the gateway.

### Confirm what landed

```bash
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis" | jq '.apis[].metadata.name'
# ... "pizzashack-api-1-0-0"

curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis/pizzashack-api-1-0-0" \
  | jq '.spec.policies[].name'
# "api-key-auth"
# "subscription-validation"
```

That handle — `pizzashack-api-1-0-0` — is what goes in `spec.referenceId` of the
portal's `api.yaml`, and `resources/pizza-shack-api/api.yaml` already has it.
**This is the one field you must match by hand**; see
[How an API is matched](#how-an-api-is-matched).

```bash
curl -k "$GW_URL/pizzashack/1.0.0/menu"
# {"error":"Unauthorized","message":"Valid API key required"}
```

If instead the call is proxied straight through, the policies didn't reach the
gateway — re-check the `jq '.spec.policies'` output, and that you deployed a new
revision after saving them.

> A backend on a self-signed certificate (API Manager's own sample backend is)
> gives `503` with `CERTIFICATE_VERIFY_FAILED` in the router log *after* auth
> passes — the policies are working; the gateway just doesn't trust the backend.
> Register its CA through the controller's `POST /certificates` +
> `POST /certificates/reload`. Dropping a PEM into `resources/certificates/` is
> not enough: that directory is mounted into the controller, not the router.

## 7. Start the subscription mediator

Generate a webhook secret and keep it — the portal and the mediator must hold the
same one.

```bash
export WEBHOOK_SECRET=$(openssl rand -base64 48)
```

Put the secret and the gateway credentials in a `.env` beside the compose
file. It's git-ignored, so nothing secret lands in the repo. `.env.example` is
the tracked template with the same keys — `cp .env.example .env` and fill it in
by hand if you prefer — or write it from the variables above:

```bash
cd subscription-mediator
cat > .env <<EOF
SM_WEBHOOK_SECRET=$WEBHOOK_SECRET
SM_GATEWAY_USERNAME=$GW_USER
SM_GATEWAY_PASSWORD=$GW_PASS
SM_GATEWAY_BASE_URL=http://host.docker.internal:9090/api/management/v1
EOF

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

`SM_GATEWAY_BASE_URL` above is also the default. On Docker Engine for Linux use
`http://172.17.0.1:9090/...` instead — `host.docker.internal` is a Docker
Desktop name. For TLS, put a TLS-terminating proxy or ingress in front of the
controller and use its `https` address (plus `SM_GATEWAY_CA_FILE` for a private
CA). The mediator's [README](subscription-mediator/README.md) has the full
matrix, including running the portal or gateway outside Docker or in
Kubernetes.

## 8. Set up the API Portal

Follow API Portal 1.0.0's [Secured API end to
end](https://wso2.com/api-platform/docs/api-portal/1.0.0/tutorials/secured-api-end-to-end/)
from **Step 3** onwards, then get a token. In local auth mode (`auth.mode =
"local"`, the development default):

```bash
export TOKEN=$(curl -sk -X POST https://localhost:9243/api/portal/v0.9/auth/login \
  -d "username=$PORTAL_USER&password=$PORTAL_PASS" | jq -r .token)
```

### Register the mediator as a webhook subscriber

```bash
curl -sk -X POST "$PORTAL/webhook-subscribers" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"id\":\"subscription-mediator\",
       \"displayName\":\"Subscription Mediator\",
       \"targetUrl\":\"http://host.docker.internal:8085/devportal/events\",
       \"secret\":\"$WEBHOOK_SECRET\",
       \"events\":[\"apikey.*\",\"subscription.*\"],
       \"enabled\":true,
       \"timeoutMs\":15000}"
```

`targetUrl` has to be resolvable **from wherever the portal runs** — use
`localhost:8085` if the portal is on the host, or the machine's routable address
if it's elsewhere.

`timeoutMs` defaults to `5000`, which is tighter than it looks. The mediator
answers a delivery only once the gateway reflects it, so the whole apply — an
API lookup, maybe a plan lookup or create, then the subscription or key call —
happens inside that window. The portal gives up after one attempt, and the
mediator's first connection after some idle minutes can take longer than 5s
through Docker Desktop's `host.docker.internal`. Raising it costs nothing and
prevents a dropped event.

> The secret does double duty: it signs each delivery *and* derives the key that
> encrypts the subscription token and API key inside it. If it doesn't match, the
> mediator rejects the delivery — and the portal never retries.

References:
[Configure webhooks](https://wso2.com/api-platform/docs/api-portal/1.0.0/admin-settings/webhook-integration/)
for subscriber registration and delivery behaviour, and the
[Webhook Event Catalog](https://wso2.com/api-platform/docs/api-portal/1.0.0/references/webhook-event-catalog/)
for the envelope, headers, signature verification, field decryption and the full
`subscription.*` / `apikey.*` / `application.*` event list.

### Mirror the subscription plans

The portal has its own plans (see
[Subscription Plans](https://wso2.com/api-platform/docs/api-portal/1.0.0/admin-settings/subscription-plans/)).
Give them the same names as the gateway's, since that's what the mediator matches
on.

```bash
# gold — mirrors the plan created in step 4
curl -sk -X PUT "$PORTAL/subscription-plans" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"id":"gold","displayName":"Gold","refId":"gold",
       "limits":[{"limitType":"REQUEST_COUNT","limitCount":10000,
                  "timeUnit":"HOUR","timeAmount":1}]}'

# iron — exists only in the portal for now
curl -sk -X POST "$PORTAL/subscription-plans" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"id":"iron","displayName":"Iron","refId":"iron",
       "description":"10 requests per minute",
       "limits":[{"limitType":"REQUEST_COUNT","limitCount":10,
                  "timeUnit":"MINUTE","timeAmount":1}]}'

curl -sk "$PORTAL/subscription-plans" -H "Authorization: Bearer $TOKEN" | jq
```

### Publish the APIs to the portal

These three uploads are the portal's content-import API — there is no UI
equivalent, so they stay as `curl`. Everything a *consumer* does (subscribing,
keys) is the portal UI, in step 9.

```bash
for api in echo-api reading-list-api pizza-shack-api; do
  curl -sk -X POST "$PORTAL/apis" \
    -H "Authorization: Bearer $TOKEN" \
    -F "metadata=@resources/$api/api.yaml;type=application/yaml" \
    -F "definition=@resources/$api/definition.yaml;type=application/yaml"
done

curl -sk "$PORTAL/apis" -H "Authorization: Bearer $TOKEN" | jq
```

To remove one:

```bash
curl -sk -X DELETE "$PORTAL/apis/pizza-shack-api-v1" -H "Authorization: Bearer $TOKEN"
```

## 9. Subscribe and invoke

Everything in this step happens in the **API Portal** at
<https://localhost:9543/api-portal/default/views/default>, signed in with the
admin credentials `setup.sh` printed.

1. Open **Reading List API** and click **Subscribe**.
2. Pick the **Gold** plan and confirm. The portal shows the **subscription key**
   — copy it.
3. On the same API, go to **API Keys → Generate Key**, give it a name, and copy
   the **API key**. Both values are shown once and never stored in plaintext.

Full UI reference:
[Manage Subscriptions](https://wso2.com/api-platform/docs/api-portal/1.0.0/consume-an-api/manage-subscriptions/)
(subscribe, switch plan, cancel) and
[Manage API Keys](https://wso2.com/api-platform/docs/api-portal/1.0.0/consume-an-api/manage-api-keys/)
(generate, rotate, revoke). This walkthrough mirrors
[Secured API End to End](https://wso2.com/api-platform/docs/api-portal/1.0.0/tutorials/secured-api-end-to-end/)
step 6, with the mediator standing in for the Platform API connection.

Each action fires a webhook the mediator applies before it answers the portal.
Watch it land:

```bash
curl -s localhost:8085/status | jq
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscriptions" | jq
```

`events.applied` should climb by one per action and `events.failed` stay at
`0`. The counters are in memory, per mediator process, since it started. The
mediator log names what it did:

```
msg="resolved portal API to gateway API" portal_api="Reading List API v1" gateway_api_handle=reading-list-api-v1
msg="created a gateway subscription" ... gateway_plan_id=... status=ACTIVE
msg="injected the portal API key into the gateway" ... gateway_key_name=...
```

Then call the API with both headers:

```bash
curl -k "$GW_URL/reading-list/books" \
  -H "API-Key: <api-key-from-the-portal>" \
  -H "Subscription-Key: <subscription-key-from-the-portal>"
```

Repeat the subscribe-and-generate for **Echo API** (plan **Iron**) and **Pizza
Shack API** (plan **Gold**) and call those too:

```bash
curl -k "$GW_URL/echo/foo"             -H "API-Key: ..." -H "Subscription-Key: ..."
curl -k "$GW_URL/pizzashack/1.0.0/menu" -H "API-Key: ..." -H "Subscription-Key: ..."
```

Dropping either header shows which policy is doing what — no API key is `401`
from `api-key-auth`, a valid key with no subscription key is `403` from
`subscription-validation`.

The portal is also where you **regenerate** or **revoke** an API key, **suspend**
or **resume** a subscription, **change its plan**, and **regenerate** the
subscription token. Each is one more webhook, and the mediator applies it to the
gateway the same way. `agent-verification.md` walks every one of them with
expected results.

## 10. Make the rate limit actually apply

In the portal, subscribe to **Echo API** on the **Iron** plan — a plan that only
ever existed in the portal. The call succeeds, but it is never throttled.

The reason: the portal's webhook payload carries a plan's `ref_id` and `name`,
**not its limits**. So when the mediator meets a plan the gateway doesn't have,
it creates it by name with no `throttleLimitCount` — and the data plane skips
rate limiting for any plan whose count is 0.

Find the plan the mediator created and give it the limits:

```bash
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscription-plans" | jq

curl -s -X PUT "$GW_MGMT/subscription-plans/<plan-id>" \
  -u "$GW_USER:$GW_PASS" \
  -H 'Content-Type: application/json' \
  -d '{
    "planName": "iron",
    "billingPlan": "FREE",
    "throttleLimitCount": 10,
    "throttleLimitUnit": "Min",
    "stopOnQuotaReach": true,
    "status": "ACTIVE"
  }' | jq
```

`PUT` is a full replace — resend `planName` and `status` along with the limits.
Existing subscriptions pick the change up on the next config push; no
resubscribe needed.

Now the 11th call in a minute returns `429`:

```bash
for i in $(seq 1 12); do
  curl -sk -o /dev/null -w "%{http_code} " "$GW_URL/echo/foo" \
    -H "API-Key: <api-key>" -H "Subscription-Key: <subscription-key>"
done
# 200 200 200 200 200 200 200 200 200 200 429 429
```

The counter is per policy-engine process and the window is fixed, not sliding —
so N replicas give roughly N× the limit, and a burst can straddle a boundary.

**Creating each plan on the gateway with its limits before anyone subscribes**
(as `gold` was in step 4) avoids this entirely — the mediator binds to an
existing plan by name rather than creating a limitless one.

---

## Restarting the gateway revokes every portal-issued API key

When the gateway is attached to API Manager as its control plane, it runs an API
key sync on every startup and **revokes each key API Manager doesn't know about**
— which is all of them, since the portal issued them and the mediator injected
them straight into the gateway:

```
msg="Starting API key sync at gateway startup"
msg="Processing API key revoke event" api_id=...
msg="Revoking API key" api_id=... api_key=persist-test-key
```

Verified: a key that returned `200` immediately before a
`docker compose restart gateway-controller` returns `401` after it, and
`GET /rest-apis/{api}/api-keys` drops to `0`.

**Subscriptions are not affected** — they survive the restart intact. Only API
keys are lost.

There is nothing for the mediator to redo: those events succeeded, and the
revocation happened afterwards on the gateway side, so the portal shows them as
delivered and the mediator logged no failure. Recover by generating a fresh key in the portal,
which fires a new `apikey.generated` the mediator applies. Plan around it: avoid
restarting the gateway mid-demo, and re-issue keys after any restart.

## How an API is matched

Three identifiers, translated twice:

1. **Portal → gateway handle.** The portal's webhook carries `api.ref_id` (a
   control-plane UUID), `name` and `version`. The mediator tries, in order: an
   `[api_handles]` pin in its config, a resolution cached in memory for five
   minutes, an exact
   `displayName`+`version` query, then a full-list scan folding case, spacing and
   punctuation. More than one match is a permanent failure, never a guess.
2. **Handle → gateway id.** The mediator sends the handle; the gateway resolves
   it to its own internal UUID.
3. **Gateway id → data plane.** The subscription state is pushed to the router,
   which matches an incoming request's hashed subscription token against it.

Versions are not guessed at: portal `v1.0` against gateway `1.0.0` will not
match, because those are different versions. Use `spec.referenceId` in the
portal's `api.yaml` — or an `[api_handles]` pin — to be explicit. When nothing
resolves, the error in the mediator log — and in its `422` answer, which the
portal records against the delivery — names the exact line to add.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `rejected an unverified webhook delivery` in the mediator log | the secret in the portal's subscriber doesn't match the mediator's. Re-register the subscriber with the right secret, then repeat the portal actions that failed |
| `401 Valid API key required` with a fresh key | the event never reached the gateway — check the portal's delivery history (below) and `events.failed` in `GET /status` on the mediator |
| A delivery `FAILED` with HTTP `502` | the mediator could not reach the gateway, or the gateway failed. Fix that, then repeat the portal action |
| A delivery `FAILED` with HTTP `422` | the event cannot be applied as it stands; the mediator log says why and what to change |
| `the gateway has no subscription with ID ...` | the subscription's `created` delivery was lost. Have the developer regenerate the subscription token, which creates it |
| Calls succeed but are never throttled | the gateway plan has no `throttleLimitCount`/`throttleLimitUnit` — see step 10 |
| `no gateway REST API matches portal API ...` | name/version mismatch. Pin it under `[api_handles]`, restart the mediator, repeat the portal action |
| Mediator container `Exited` right after start | a required setting is missing or invalid; the reason is its last log line. For the gateway CA: `gateway.ca_file is set but gateway.base_url ... is not https`, `failed to read the gateway CA file`, or `... holds no PEM certificate` |
| Mediator logs `connection refused` | nothing answers at `SM_GATEWAY_BASE_URL` — the gateway isn't up, `9090` isn't published, or the host is wrong for this Docker (step 7) |
| Mediator logs `x509: ...` | only with an `https` address through a TLS proxy: `SM_GATEWAY_CA_FILE` is unset or holds a different CA, or the certificate doesn't name the host — see the mediator README, "TLS to the gateway" |
| Subscriptions are created, but every status or plan change fails with `the gateway has no subscription with ID ...` | the gateway controller predates [#3804](https://github.com/wso2/api-platform/pull/3804), so it ignored `externalRefId` and generated its own IDs — use a controller that includes it (Prerequisites) |
| Gateway logs `WebSocket connection failed ... dial tcp 172.17.0.1:9444: connect: connection refused` | the `extra_hosts` trap — point `APIP_GW_CONTROLLER_CONTROLPLANE_HOST` at `$HOST_ADDR`, not `host.docker.internal` |
| A CP-created API answers without credentials | its policies aren't attached, or weren't redeployed — see step 6; **API Security / `securityScheme` does nothing** for a platform-gateway API |
| The publisher's Policies page is empty for a platform-gateway API | it reads the remote Policy Hub — that host must be reachable from the browser |
| Every API key returns `401` after a gateway restart | the startup API key sync revoked them; re-issue from the portal (see above) |
| `503` with `CERTIFICATE_VERIFY_FAILED` once auth passes | the backend's CA isn't in the gateway's `resources/certificates/` |
| APIM won't start: `Unrecognized option: --enable-native-access` | JDK 11 on `JAVA_HOME`; 4.7.0 needs 17+ |

The API Portal **attempts each delivery exactly once** — there is no retry and no
dead-letter queue on its side. The mediator keeps no queue either: it applies
each event before answering, `200` once the gateway reflects it, `422` when the
event cannot be applied, `502` when the gateway failed. Anything but `200` is
marked `FAILED` in the portal and never resent, so the portal's delivery history
is the record of what didn't land.

A delivery can also be marked `FAILED` **without the mediator ever seeing it** —
a connection timeout, most likely. The mediator's counters then stay clean —
`/status` reports `events.failed: 0` — while a portal action has in fact been
lost, leaving the gateway out of sync with nothing to reconcile it. Only the
portal knows:

```bash
curl -sk "$PORTAL/webhook-subscribers/subscription-mediator/deliveries" \
  -H "Authorization: Bearer $TOKEN" \
  | jq -r '.list[] | select(.status != "DELIVERED")
           | "\(.occurredAt) \(.eventType) \(.status) \(.lastError)"'
```

Anything listed has to be recovered by **redoing the portal action** — there is
no redelivery endpoint. Worth checking whenever the gateway and the portal
disagree and the mediator reports nothing wrong.

## References

Official documentation for the exact versions this was built against.

**WSO2 API Manager 4.7.0**

- [Documentation home](https://apim.docs.wso2.com/en/latest/)
- [Getting Started with Platform Gateway](https://apim.docs.wso2.com/en/latest/api-gateway/platform-gateway/getting-started/)
  — registering the gateway in the Admin Portal, then configuring and starting it

**WSO2 API Platform Gateway 1.2.0** — [`gateway/v1.2.0`](https://github.com/wso2/api-platform/releases/tag/gateway%2Fv1.2.0)

- [Overview](https://wso2.com/api-platform/docs/api-gateway/1.2.0/overview/)
- [Control Plane Connection](https://wso2.com/api-platform/docs/api-gateway/1.2.0/deployment/production-deployment/control-plane-connection/)
- [Gateway Controller Management API](https://wso2.com/api-platform/docs/api-gateway/1.2.0/gateway-controller-management-api/overview/)
  and [REST API Management](https://wso2.com/api-platform/docs/api-gateway/1.2.0/gateway-controller-management-api/rest-api-management/)
- [Policies Overview](https://wso2.com/api-platform/docs/api-gateway/1.2.0/policies/overview/)

**WSO2 API Portal & MCP Hub 1.0.0** — [`api-portal/v1.0.0`](https://github.com/wso2/api-platform/releases/tag/api-portal%2Fv1.0.0)

- [Overview](https://wso2.com/api-platform/docs/api-portal/1.0.0/overview/)
- [Secured API End to End](https://wso2.com/api-platform/docs/api-portal/1.0.0/tutorials/secured-api-end-to-end/)
  — the tutorial this walkthrough parallels
- [Configure webhooks](https://wso2.com/api-platform/docs/api-portal/1.0.0/admin-settings/webhook-integration/)
  and the [Webhook Event Catalog](https://wso2.com/api-platform/docs/api-portal/1.0.0/references/webhook-event-catalog/)
  — the contract this whole project consumes
- [Subscription Plans](https://wso2.com/api-platform/docs/api-portal/1.0.0/admin-settings/subscription-plans/)
- [Manage Subscriptions](https://wso2.com/api-platform/docs/api-portal/1.0.0/consume-an-api/manage-subscriptions/)
  and [Manage API Keys](https://wso2.com/api-platform/docs/api-portal/1.0.0/consume-an-api/manage-api-keys/)

**Distributions** — [all releases](https://github.com/wso2/api-platform/releases).
Each `wso2apip-*.zip` carries its own `README.md`, which is the authority on that
build's ports, `scripts/setup.sh` flags and `configs/config-template.toml`.

## Notes

- This is a demo project, not a WSO2 product — no affiliation, no endorsement, no support, no warranty.
- **Version-locked.** API Manager 4.7.0, API Platform Gateway 1.2.0,
  API Portal 1.0.0. Other combinations are untested; see [Versions](#versions).
- Everything here runs over plaintext HTTP and self-signed TLS (`curl -k`), and
  the mediator's own endpoints are unauthenticated. It's a demo — run it where
  only you and the portal can reach it.
- The mediator only handles `RestApi` artifacts. The gateway's subscription and
  API-key endpoints reject every other kind.
- Full operational detail — configuration precedence, networking matrix,
  per-event behaviour, failure handling — is in
  [`subscription-mediator/README.md`](subscription-mediator/README.md).
- [`agent-verification.md`](agent-verification.md) drives the same flow entirely
  over REST, with expected results for every subscription and API-key operation
  — key generation, regeneration and revocation, subscription suspension,
  resumption, plan change, token regeneration and deletion, plus a cross-API
  isolation matrix. It's meant for a coding agent to execute and report on.
