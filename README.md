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

| Component | Version |
|---|---|
| WSO2 API Manager | **4.7.0** |
| WSO2 API Platform Gateway | **1.2.0** |
| WSO2 API Portal | **1.0.0** |

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
     │       verify → decrypt → queue → apply    │
     └───────────────────────────────────────────┘
```

## What's in here

```
subscription-mediator/      the bridge itself — Go, runs in Docker
                            (see its own README for internals and operations)
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
- **WSO2 API Platform Gateway 1.2.0** — follow
  [Getting started](https://apim.docs.wso2.com/en/latest/api-gateway/platform-gateway/getting-started/)
- **WSO2 API Portal 1.0.0** — follow
  [Secured API end to end](https://wso2.com/api-platform/docs/api-portal/1.0.0/tutorials/secured-api-end-to-end/)

Plus Docker with Compose v2, `curl`, `jq` and `openssl`.

Set these once; every command below uses them.

```bash
# WSO2 API Manager
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
```

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

Start API Manager, then add a gateway under **Admin → Gateways**. The version
must be the one you are actually running — `1.2.0` throughout this walkthrough:

| Field | Value |
|---|---|
| Name | `Dev Platform Gateway` |
| URL | `https://localhost:8443` |
| Version | `1.2.0` |

Note the **label** API Manager assigns it (`dev-platform-gateway` here) — the
gateway has to be told its own name.

## 2. Register a DCR application

The gateway needs an OAuth2 client to push artifacts back up to API Manager
(the DP→CP direction).

```bash
curl -k -X POST https://localhost:9444/client-registration/v0.17/register \
  -u "$APIM_USER:$APIM_PASS" \
  -H "Content-Type: application/json" \
  -d '{
    "callbackUrl": "https://localhost/callback",
    "clientName": "publisher_app",
    "owner": "'"$APIM_USER"'",
    "grantType": "password refresh_token client_credentials",
    "saasApp": true
  }'
```

The response carries `clientId` and `clientSecret`. Keep them out of version
control.

## 3. Configure and start the gateway

Set these on the gateway, taking the host and token from **Step 2: Configure the
Gateway** in the API Manager UI:

| Variable | Value |
|---|---|
| `APIP_GW_CONTROLLER_CONTROLPLANE_HOST` | from the UI |
| `APIP_GW_CONTROLLER_CONTROLPLANE_TOKEN` | from the UI |
| `APIP_GW_CONTROLLER_CONTROLPLANE_GATEWAY_NAME` | the gateway's label, e.g. `dev-platform-gateway` |
| `GATEWAY_CONTROLPLANE_ON_PREM` | `true` |
| `APIP_GW_CONTROLLER_CONTROLPLANE_APIM_OAUTH2_CLIENT_ID` | `clientId` from step 2 |
| `APIP_GW_CONTROLLER_CONTROLPLANE_APIM_OAUTH2_CLIENT_SECRET` | `clientSecret` from step 2 |

Start the gateway, and confirm the management API answers:

```bash
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis" | jq '.apis | length'
```

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

For the other direction, create an API in the API Manager publisher — Pizza Shack
is the stock sample — with **API key authentication** and **subscription
validation** enabled, and deploy it to `Dev Platform Gateway`.

Then find the handle the gateway gave it:

```bash
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis" | jq '.apis[].metadata.name'
# ... "pizzashack-api-1-0-0"
```

That handle is what goes in `spec.referenceId` of the portal's `api.yaml` —
`resources/pizza-shack-api/api.yaml` already has it. **This is the one field that
has to match by hand**; see [How an API is matched](#how-an-api-is-matched)
below.

```bash
curl -k "$GW_URL/pizzashack/1.0.0/menu"
# {"error":"Unauthorized","message":"Valid API key required"}
```

## 7. Start the subscription mediator

Generate a webhook secret and keep it — the portal and the mediator must hold the
same one.

```bash
export WEBHOOK_SECRET=$(openssl rand -base64 48)
```

Put it, and the gateway credentials, in a `.env` beside the compose file. It's
git-ignored, so nothing secret lands in the repo:

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
msg="subscription-mediator is ready" workers=4 max_pending_events=5000
msg="listening for API Portal webhooks" address=0.0.0.0:8085 path=/devportal/events
```

On Docker Engine for Linux use `http://172.17.0.1:9090/...` instead —
`host.docker.internal` is a Docker Desktop name. The mediator's
[README](subscription-mediator/README.md) has the full matrix, including running
the portal or gateway outside Docker or in Kubernetes.

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
       \"enabled\":true}"
```

`targetUrl` has to be resolvable **from wherever the portal runs** — use
`localhost:8085` if the portal is on the host, or the machine's routable address
if it's elsewhere.

> The secret does double duty: it signs each delivery *and* derives the key that
> encrypts the subscription token and API key inside it. If it doesn't match, the
> mediator rejects the delivery — and the portal never retries.

### Mirror the subscription plans

The portal has its own plans. Give them the same names as the gateway's, since
that's what the mediator matches on.

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

In the portal UI, subscribe an application to an API and generate both
credentials — see
[Get both credentials in the portal](https://wso2.com/api-platform/docs/api-portal/1.0.0/tutorials/secured-api-end-to-end/#step-6-get-both-credentials-in-the-portal).

Each action fires a webhook the mediator applies within a second or two. Watch it
land:

```bash
curl -s localhost:8085/status | jq
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscriptions" | jq
```

Then call the APIs with both headers:

```bash
curl -k "$GW_URL/reading-list/books" \
  -H "API-Key: <api-key-from-the-portal>" \
  -H "Subscription-Key: <subscription-key-from-the-portal>"

curl -k "$GW_URL/echo/foo" \
  -H "API-Key: <api-key-from-the-portal>" \
  -H "Subscription-Key: <subscription-key-from-the-portal>"

curl -k "$GW_URL/pizzashack/1.0.0/menu" \
  -H "API-Key: <api-key-from-the-portal>" \
  -H "Subscription-Key: <subscription-key-from-the-portal>"
```

## 10. Make the rate limit actually apply

Subscribe something to **Echo API**, which uses the `iron` plan that only ever
existed in the portal. The call succeeds — but it is never throttled.

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

**Creating each plan on the gateway with its limits before anyone subscribes**
(as `gold` was in step 4) avoids this entirely — the mediator binds to an
existing plan by name rather than creating a limitless one.

---

## How an API is matched

Three identifiers, translated twice:

1. **Portal → gateway handle.** The portal's webhook carries `api.ref_id` (a
   control-plane UUID), `name` and `version`. The mediator tries, in order: an
   `[api_handles]` pin in its config, a mapping it resolved earlier, an exact
   `displayName`+`version` query, then a full-list scan folding case, spacing and
   punctuation. More than one match is a permanent failure, never a guess.
2. **Handle → gateway id.** The mediator sends the handle; the gateway resolves
   it to its own internal UUID.
3. **Gateway id → data plane.** The subscription state is pushed to the router,
   which matches an incoming request's hashed subscription token against it.

Versions are not guessed at: portal `v1.0` against gateway `1.0.0` will not
match, because those are different versions. Use `spec.referenceId` in the
portal's `api.yaml` — or an `[api_handles]` pin — to be explicit. When nothing
resolves, `GET /failed` on the mediator names the exact line to add.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `rejected an unverified webhook delivery` in the mediator log | the secret in the portal's subscriber doesn't match the mediator's. Re-register the subscriber with the right secret, then `POST localhost:8085/failed/requeue` |
| `401 Valid API key required` with a fresh key | the event never reached the gateway — check `GET /status` and `GET /failed` on the mediator |
| Calls succeed but are never throttled | the gateway plan has no `throttleLimitCount`/`throttleLimitUnit` — see step 10 |
| `no gateway REST API matches portal API ...` | name/version mismatch. Pin it under `[api_handles]`, restart, requeue |
| Mediator container `Exited` right after start | a required setting is missing; the reason is its last log line |

The API Portal **attempts each delivery exactly once** — there is no retry and no
dead-letter queue on its side. Anything the mediator rejects is marked `FAILED`
in the portal and never resent, which is why the mediator persists every accepted
event before acknowledging it, and why `/failed/requeue` exists.

## Notes

- This is a demo project, not a WSO2 product — no affiliation, no endorsement, no support, no warranty.
- **Version-locked.** API Manager 4.7.0, API Platform Gateway 1.2.0, API Portal
  1.0.0. Other combinations are untested; see [Versions](#versions).
- Everything here runs over plaintext HTTP and self-signed TLS (`curl -k`), and
  the mediator's own endpoints are unauthenticated. It's a demo — run it where
  only you and the portal can reach it.
- The mediator only handles `RestApi` artifacts. The gateway's subscription and
  API-key endpoints reject every other kind.
- Full operational detail — configuration precedence, networking matrix,
  per-event behaviour, retries, state — is in
  [`subscription-mediator/README.md`](subscription-mediator/README.md).
