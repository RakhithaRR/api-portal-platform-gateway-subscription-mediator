# Agent verification run

A **standalone** end-to-end check of the subscription bridge, written to be
executed by a coding agent (Claude Code or similar) rather than read by a person.

It starts from nothing but the distributions and brings the whole stack up
itself: **Part 1** provisions and starts API Manager, the platform gateway, the
API Portal and the mediator; **Part 2** verifies the flow. Every step is a shell
command with a **stated expected result**. Anything that doesn't match is a
finding — report it, don't work around it silently.

[`README.md`](README.md) covers the same ground for a person, and sends you to
the Publisher and API Portal UIs. Those UI steps have no place in an unattended
run, so Part 1 below does the same work over REST. Where the REST shape is not
obvious from the UI, it is spelled out.

**Versions under test:** WSO2 API Manager 4.7.0
([docs](https://apim.docs.wso2.com/en/latest/)), API Platform Gateway 1.2.0
([docs](https://wso2.com/api-platform/docs/api-gateway/1.2.0/overview/) ·
[`gateway/v1.2.0`](https://github.com/wso2/api-platform/releases/tag/gateway%2Fv1.2.0)),
API Portal 1.0.0
([docs](https://wso2.com/api-platform/docs/api-portal/1.0.0/overview/) ·
[`api-portal/v1.0.0`](https://github.com/wso2/api-platform/releases/tag/api-portal%2Fv1.0.0)).

> **The gateway controller must include
> [wso2/api-platform#3804](https://github.com/wso2/api-platform/pull/3804)**
> (issue [#3782](https://github.com/wso2/api-platform/issues/3782)), which stock
> 1.2.0 does not; `thivindu/gateway-controller:1.2.0.17-SNAPSHOT` does, and is
> the base controller image of the custom gateway build. With it, `POST
> /subscriptions` accepts an optional `externalRefId`
> (`^[A-Za-z0-9][A-Za-z0-9._-]*$`, 1–64 characters) that becomes the
> subscription's `id` (responses carry `externalRefId` equal to `id`; a reused
> one is `409`), and `PUT /subscriptions/{id}` is a partial update of `status`,
> `subscriptionPlanId` (`null` or `""` clears it) and `subscriptionToken`. It
> also makes renaming a plan to a taken name, or deleting a plan in use, `409`
> instead of `500`. The mediator creates each gateway subscription with the
> portal's subscription id as `externalRefId`, finds it with
> `GET /subscriptions/{portal subscription id}` (`404` = none), and changes
> plans and tokens in place. Against a stock 1.2.0 gateway, which ignores
> `externalRefId` and generates its own ids, Phases 6–9 do not behave as
> written — status and plan changes answer `422` because no subscription has
> the portal's id, and a token regeneration creates a second subscription. That
> is a missing prerequisite, not a finding.

The portal endpoints used below are specified in the API Portal
[Management REST API reference](https://wso2.com/api-platform/docs/api-portal/1.0.0/overview/)
(Subscriptions, API Keys, Subscription Plans, Webhook Subscribers); the gateway
endpoints are in the
[Gateway Controller Management API](https://wso2.com/api-platform/docs/api-gateway/1.2.0/gateway-controller-management-api/overview/).
Each portal action's webhook is described in the
[Webhook Event Catalog](https://wso2.com/api-platform/docs/api-portal/1.0.0/references/webhook-event-catalog/).

---

## What you need before starting

| Input | Notes |
|---|---|
| `wso2am-4.7.0` pack, unpacked | not started, default config is fine |
| `wso2apip-api-gateway-1.2.0.zip` | [`gateway/v1.2.0`](https://github.com/wso2/api-platform/releases/tag/gateway%2Fv1.2.0), **with a gateway controller that includes** [#3804](https://github.com/wso2/api-platform/pull/3804) (e.g. `thivindu/gateway-controller:1.2.0.17-SNAPSHOT`) — see above |
| `wso2apip-api-portal-1.0.0.zip` | [`api-portal/v1.0.0`](https://github.com/wso2/api-platform/releases/tag/api-portal%2Fv1.0.0) |
| This repository | the `resources/` and `subscription-mediator/` directories |
| JDK 21+ | API Manager 4.7.0 passes `--enable-native-access=ALL-UNNAMED`; **JDK 11 refuses to start** with `Could not create the Java Virtual Machine` |
| Docker with Compose v2 | Desktop or Engine |
| `curl`, `jq`, `openssl`, `unzip` | |

Ports used: `9444` (API Manager, offset 1), `9090`/`8443` (gateway
management / ingress), `9543`/`9243` (portal / platform API), `8085` (mediator). All must be
free.

## How to run this

Work top to bottom; later steps depend on earlier ones. Keep a scratch file of
the ids and secrets each step emits — sourcing one env file between commands is
the simplest way to survive a fresh shell per step.

After each portal action, **wait for the mediator to apply it** before asserting
on the gateway. The portal delivers asynchronously — its delivery worker polls
every couple of seconds — and the mediator applies each event before it answers,
so deliveries typically land within 5–20 seconds:

```bash
wait_applied() {          # wait_applied <expected-applied-count>
  for _ in $(seq 1 20); do
    n=$(curl -s localhost:8085/status | jq -r .events.applied)
    if [ "$n" -ge "$1" ]; then
      sleep 5            # settle — see below; do not remove
      return 0
    fi
    sleep 2
  done
  echo "TIMED OUT waiting for applied >= $1 (now $n)"; return 1
}
```

Polling `/status` beats a fixed `sleep`: `events.applied` must rise by one per
portal action, and `events.failed` must stay `0` throughout (Phase 11 makes one
failure on purpose). A non-zero `events.failed` is a finding: the mediator log
line for that event gives the reason, and the portal's delivery history (end of
Phase 11) shows it as `FAILED` with the HTTP status the mediator answered —
`422` for an event that cannot be applied as it stands, `502` for a gateway that
failed or could not be reached. There is no requeue; recovery is to fix the
cause and repeat the portal action.

> **Why the `sleep 5` inside the helper.** `events.applied` rises when the
> mediator's call to the gateway **controller** succeeded. The **router** learns
> about the change over xDS a moment later, so an invocation fired the instant
> the counter moves can still hit the old state. Asserting with no settle
> produced an exactly-inverted Phase 4 result (old key `200`, new key `401`)
> that corrected itself 5 seconds later and then held for 30 seconds. Every
> phase below relies on the settle being inside `wait_applied`; if you replace
> the helper, keep it.

The counters below (`wait_applied 2`, `wait_applied 4`, …) are **absolute**, and
assume Part 1 left the mediator at `applied: 0` — which step S8 guarantees. The
counters are in memory and reset whenever the mediator restarts. If you re-run
Part 2 against a mediator that already has history, read the starting
value `N` from `/status` first and treat every `wait_applied K` as
`wait_applied <N+K>`.

---

# Part 1 — Bring the stack up

## S1 — Paths and credentials

Set these once. Everything below reads them; nothing else is a placeholder.

```bash
# --- inputs: adjust these four to your machine ---------------------------
export APIM_PACK=/path/to/wso2am-4.7.0
export ZIPS=/path/to/distribution/zips        # holds the two .zip files
export WORK=/path/to/unpack/into              # the two distributions land here
export DEMO=/path/to/this/repository
export JAVA_HOME=/path/to/jdk-21              # e.g. ~/.sdkman/candidates/java/21.0.5-tem

# --- credentials we choose, so no step needs a human to read a prompt ----
export APIM_USER=admin
export APIM_PASS=admin                        # the pack's default
export GW_USER=admin
export GW_PASS="$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-20)"
export PORTAL_USER=admin
export PORTAL_PASS="$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-20)"

# --- endpoints -----------------------------------------------------------
export APIM=https://localhost:9444
export GW_DIR="$WORK/wso2apip-api-gateway-1.2.0"
export GW_MGMT=http://localhost:9090/api/management/v1
export GW_URL=https://localhost:8443
export PORTAL_DIR="$WORK/wso2apip-api-portal-1.0.0"
export PORTAL=https://localhost:9543/api-portal/api/v0.9
export PLATFORM_API=https://localhost:9243

# --- the address containers use to reach this machine --------------------
export HOST_ADDR=$(ipconfig getifaddr en0)    # macOS
# export HOST_ADDR=172.17.0.1                 # Docker Engine on Linux
```

> **Use `$HOST_ADDR`, not `host.docker.internal`, for anything the gateway must
> reach on the host.** The gateway 1.2.0 `docker-compose.yaml` sets
> `extra_hosts: ["host.docker.internal:host-gateway"]` on `gateway-controller`,
> which on **Docker Desktop for macOS** replaces the working built-in name with
> the Linux bridge address `172.17.0.1` — unreachable there, so every connection
> out to the host is refused. The portal and the mediator set no `extra_hosts`,
> so `host.docker.internal` works normally for them.

## S2 — Start API Manager

Two edits to `repository/conf/deployment.toml`: port offset 1 (so API Manager
serves on `9444` and leaves `9443` free), and the platform-gateway versions,
without which API Manager has no API Platform Gateway type to register.

```bash
cd "$APIM_PACK"
sed -i '' 's/^#offset=0/offset=1/' repository/conf/deployment.toml   # macOS
# GNU sed:  sed -i 's/^#offset=0/offset=1/' repository/conf/deployment.toml

grep -q '\[apim.platform_gateway\]' repository/conf/deployment.toml || \
cat >> repository/conf/deployment.toml <<'EOF'

[apim.platform_gateway]
versions = ["1.0.0", "1.1.0", "1.2.0"]
EOF

nohup ./bin/api-manager.sh > /tmp/apim.log 2>&1 &
```

Wait for startup — roughly 30–60 seconds:

```bash
for i in $(seq 1 60); do
  grep -q "WSO2 Carbon started in" /tmp/apim.log && { echo "APIM up"; break; }
  sleep 5
done
curl -sk -o /dev/null -w "apim %{http_code}\n" "$APIM/services/Version"
# apim 200
```

**Expected:** `WSO2 Carbon started in <n> sec` in the log and `200` from the
version endpoint. `Unrecognized option: --enable-native-access=ALL-UNNAMED`
means `$JAVA_HOME` is still on a JDK older than 17 — fix that before going on.

## S3 — Register a DCR application and get a token

```bash
DCR=$(curl -sk -X POST "$APIM/client-registration/v0.17/register" \
  -u "$APIM_USER:$APIM_PASS" -H "Content-Type: application/json" \
  -d '{"callbackUrl":"https://localhost/callback","clientName":"publisher_app",
       "owner":"'"$APIM_USER"'","grantType":"password refresh_token client_credentials",
       "saasApp":true}')
export CID=$(echo "$DCR" | jq -r .clientId)
export CSEC=$(echo "$DCR" | jq -r .clientSecret)

export APIM_TOKEN=$(curl -sk -X POST "$APIM/oauth2/token" -u "$CID:$CSEC" \
  -d "grant_type=password&username=$APIM_USER&password=$APIM_PASS&scope=apim:admin apim:api_view apim:api_create apim:api_publish apim:api_manage" \
  | jq -r .access_token)
echo "token length: ${#APIM_TOKEN}"
```

**Expected:** a `clientId`/`clientSecret` pair, and a token (36 characters).
Keep `$CID`/`$CSEC` — the gateway needs them in S5 to push artifacts back up.

This token expires. Re-run the second command whenever an API Manager call
starts returning `401`.

## S4 — Register the platform gateway

In the walkthrough this is **Admin Portal → Gateways → Add Gateway**. Over REST
it is one call, and the request body is **not** the shape the UI's fields
suggest:

```bash
GWREG=$(curl -sk -X POST "$APIM/api/am/admin/v4/gateways" \
  -H "Authorization: Bearer $APIM_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"dev-platform-gateway",
       "displayName":"Dev Platform Gateway",
       "description":"Local platform gateway 1.2.0",
       "vhost":"https://localhost:8443",
       "properties":{"version":"1.2.0"},
       "permissions":{"permissionType":"PUBLIC"}}')
echo "$GWREG" | jq 'del(.registrationToken)'
export GW_REG_TOKEN=$(echo "$GWREG" | jq -r .registrationToken)
echo "registration token length: ${#GW_REG_TOKEN}"
```

**Expected:** `201`, `"isActive": false` (it flips once the gateway connects),
and a `registrationToken` of 80 characters.

> The body takes exactly `name`, `displayName`, `description`, `vhost`,
> `properties` and `permissions`. The UI's *Type*, *Version* and *Visibility*
> fields are **not** top-level properties here: sending `type`, `gatewayType`,
> `version` or `visibility` fails with
> `400 Unrecognized property '<name>'`. The version goes in `properties`, and
> visibility in `permissions.permissionType`.
>
> `name` must match `^[a-z0-9-]+$`, 3–64 characters. It is the gateway's
> identity and is given back to the gateway as
> `APIP_GW_CONTROLLER_CONTROLPLANE_GATEWAY_NAME` in S5.
>
> The registration token is returned **once** and stored only as a hash. If it
> is lost, regenerate it with
> `POST /api/am/admin/v4/gateways/{id}/regenerate-token`.

## S5 — Unpack, configure and start the gateway

`scripts/setup.sh` normally prompts for admin credentials and prints a generated
password once. Setting `ADMIN_USERNAME`/`ADMIN_PASSWORD` skips both prompts and
pins the credentials, which is what makes this runnable unattended.

```bash
mkdir -p "$WORK" && unzip -q "$ZIPS/wso2apip-api-gateway-1.2.0.zip" -d "$WORK"
cd "$GW_DIR"
ADMIN_USERNAME="$GW_USER" ADMIN_PASSWORD="$GW_PASS" ./scripts/setup.sh < /dev/null
```

**Expected:** the script provisions the listener certificate, the AES-256 key,
a pinned compose project name and `api-platform.env`, then prints the admin
credentials — the ones just supplied.

Append the control-plane settings
([Control Plane Connection](https://wso2.com/api-platform/docs/api-gateway/1.2.0/deployment/production-deployment/control-plane-connection/)),
then start:

```bash
cat >> api-platform.env <<EOF

APIP_GW_CONTROLLER_CONTROLPLANE_HOST=$HOST_ADDR:9444
APIP_GW_CONTROLLER_CONTROLPLANE_TOKEN=$GW_REG_TOKEN
APIP_GW_CONTROLLER_CONTROLPLANE_GATEWAY_NAME=dev-platform-gateway
APIP_GW_CONTROLLER_CONTROLPLANE_INSECURE_SKIP_VERIFY=true
GATEWAY_CONTROLPLANE_ON_PREM=true
APIP_GW_CONTROLLER_CONTROLPLANE_APIM_OAUTH2_CLIENT_ID=$CID
APIP_GW_CONTROLLER_CONTROLPLANE_APIM_OAUTH2_CLIENT_SECRET=$CSEC
EOF
```

`api-platform.env` now holds a registration token and an OAuth2 client secret.
It is specific to this machine — never commit it.

The controller serves the management API over plain HTTP on `9090` only — it
has no TLS listener, and a `[controller.server.tls]` block is ignored — so the
mediator uses `9090` too. Start:

```bash
docker compose up -d
```

```bash
sleep 20
docker compose logs gateway-controller | grep -i "control plane connection"
# msg="Control plane connection established" gateway_id=... connection_id=...

curl -s -o /dev/null -w "gw mgmt %{http_code}\n" -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis"
# gw mgmt 200
```

**Expected:** `Control plane connection established`, and `200` from the
management API. `dial tcp 172.17.0.1:9444: connection refused` means
`APIP_GW_CONTROLLER_CONTROLPLANE_HOST` resolved through
`host.docker.internal` — go back to S1 and use the LAN address.

## S6 — Subscription plan and sample APIs on the gateway (DP→CP)

The `gold` plan carries the rate limit. `iron` is deliberately **not** created
here — Phase 8 depends on it reaching the gateway only through a subscription
event.

```bash
curl -s -X POST "$GW_MGMT/subscription-plans" -u "$GW_USER:$GW_PASS" \
  -H 'Content-Type: application/json' \
  -d '{"planName":"gold","billingPlan":"COMMERCIAL","throttleLimitCount":10000,
       "throttleLimitUnit":"Hour","stopOnQuotaReach":true,"status":"ACTIVE"}' | jq -c
```

**Expected:** `201` and an `id`. Both `throttleLimitCount` and
`throttleLimitUnit` must be set or the data plane skips rate limiting for that
plan entirely; the unit is one of `Min`, `Hour`, `Day`, `Month`, case-sensitive.

```bash
cd "$DEMO"
curl -s -X POST "$GW_MGMT/rest-apis" -u "$GW_USER:$GW_PASS" -H 'Content-Type: text/yaml' \
  --data-binary '@resources/echo-api/echo-subscription-api.yaml' | jq -c '.status.id'
curl -s -X POST "$GW_MGMT/rest-apis" -u "$GW_USER:$GW_PASS" -H 'Content-Type: text/yaml' \
  --data-binary '@resources/reading-list-api/reading-list-subscription-api.yaml' | jq -c '.status.id'
# "echo-api-v1"
# "reading-list-api-v1"
```

**Expected:** both handles returned. Phase 1 asserts on the policies they carry.

## S7 — Unpack and start the API Portal

```bash
unzip -q "$ZIPS/wso2apip-api-portal-1.0.0.zip" -d "$WORK"
cd "$PORTAL_DIR"
ADMIN_USERNAME="$PORTAL_USER" ADMIN_PASSWORD="$PORTAL_PASS" ./scripts/setup.sh < /dev/null
docker compose up -d
```

**Expected:** `COMPOSE_PROFILES=api-portal,platform-api` in the generated `.env`
— both services are needed. Compose waits for `platform-api` to report healthy
before starting `api-portal`.

```bash
curl -sk -o /dev/null -w "platform-api %{http_code}\n" "$PLATFORM_API/health"
# platform-api 200
```

## S8 — Build and start the mediator

The webhook secret does double duty: it signs each delivery *and* derives the
key that encrypts the subscription token and API key inside it. The portal and
the mediator must hold the same one.

The `.env` below has the same keys as the tracked `.env.example`; it is
written from the variables rather than copied, so the run stays unattended.

```bash
export WEBHOOK_SECRET=$(openssl rand -base64 48 | tr -d '\n')
cd "$DEMO/subscription-mediator"
cat > .env <<EOF
SM_WEBHOOK_SECRET=$WEBHOOK_SECRET
SM_GATEWAY_USERNAME=$GW_USER
SM_GATEWAY_PASSWORD=$GW_PASS
SM_GATEWAY_BASE_URL=http://host.docker.internal:9090/api/management/v1
EOF

make build
docker compose up -d --force-recreate      # a fresh process starts its counters at zero
sleep 10
docker compose logs | grep -E "reachable|is ready"
```

**Expected:**

```
msg="the gateway management API is reachable and the credentials were accepted"
msg="subscription-mediator is ready"
```

A `could not reach the gateway management API` warning instead of the first
line is a finding; its error says why — `connection refused` is nothing
answering at `SM_GATEWAY_BASE_URL` (the gateway down, `9090` unpublished, or
the wrong host for this Docker), `401` is the credentials. A container that
exits instead names the missing or invalid setting in its last log line.

The mediator keeps no state: there is no volume to clear between runs. Every
lookup reads the gateway, so a gateway left with subscriptions or keys from an
earlier run is what a clean run has to avoid, not the mediator.

On Docker Engine for Linux use `http://172.17.0.1:9090/...` for
`SM_GATEWAY_BASE_URL` — `host.docker.internal` is a Docker Desktop name.

## S9 — Portal: webhook subscriber, plans, APIs

```bash
export TOKEN=$(curl -sk -X POST "$PLATFORM_API/api/portal/v0.9/auth/login" \
  -d "username=$PORTAL_USER&password=$PORTAL_PASS" | jq -r .token)
echo "token length: ${#TOKEN}"
```

**Expected:** a JWT, ~1700 characters. It expires — re-run this whenever a
portal call starts returning `401`.

```bash
curl -sk -X POST "$PORTAL/webhook-subscribers" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"subscription-mediator\",
       \"displayName\":\"Subscription Mediator\",
       \"targetUrl\":\"http://host.docker.internal:8085/devportal/events\",
       \"secret\":\"$WEBHOOK_SECRET\",
       \"events\":[\"apikey.*\",\"subscription.*\"],
       \"enabled\":true,
       \"timeoutMs\":15000}" -o /dev/null -w '%{http_code}\n'
# 201
```

> **Set `timeoutMs` explicitly; the default of 5000 is too tight here.** The
> portal attempts each delivery **once** — there is no retry and no redelivery
> endpoint — so a single slow response loses the event permanently. The
> mediator answers only after it has applied the event to the gateway, so that
> whole apply — an API lookup, maybe a plan lookup or create, then the
> subscription or key call — runs inside the window. Its first connection after
> several idle minutes can also exceed 5 s through Docker Desktop's
> `host.docker.internal`. A run of this document lost one
> `subscription.plan_changed` that way, recorded `status: FAILED,
> lastError: "timeout"`, while the mediator's own counters stayed clean. See the
> delivery check at the end of Phase 11.

`targetUrl` has to be resolvable **from wherever the portal runs** — use
`localhost:8085` if the portal is on the host, or the machine's routable address
if it is elsewhere. See
[Configure webhooks](https://wso2.com/api-platform/docs/api-portal/1.0.0/admin-settings/webhook-integration/).

Mirror the plans. The mediator matches on **name**, so these must match the
gateway's. `gold` already exists in the portal's defaults, hence `PUT`; `iron`
is new, hence `POST`:

```bash
curl -sk -X PUT "$PORTAL/subscription-plans" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id":"gold","displayName":"Gold","refId":"gold",
       "limits":[{"limitType":"REQUEST_COUNT","limitCount":10000,"timeUnit":"HOUR","timeAmount":1}]}' \
  -o /dev/null -w 'gold %{http_code}\n'

curl -sk -X POST "$PORTAL/subscription-plans" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id":"iron","displayName":"Iron","refId":"iron","description":"10 requests per minute",
       "limits":[{"limitType":"REQUEST_COUNT","limitCount":10,"timeUnit":"MINUTE","timeAmount":1}]}' \
  -o /dev/null -w 'iron %{http_code}\n'
# gold 201
# iron 201
```

Publish the three sample APIs to the portal:

```bash
cd "$DEMO"
for api in echo-api reading-list-api pizza-shack-api; do
  printf "%-20s " "$api"
  curl -sk -X POST "$PORTAL/apis" -H "Authorization: Bearer $TOKEN" \
    -F "metadata=@resources/$api/api.yaml;type=application/yaml" \
    -F "definition=@resources/$api/definition.yaml;type=application/yaml" \
    -o /dev/null -w "%{http_code}\n"
done
# echo-api             201
# reading-list-api     201
# pizza-shack-api      201
```

`pizza-shack-api` has no gateway counterpart yet — Phase 10 creates it from API
Manager. Its `spec.referenceId` already names the handle that will appear.

---

# Part 2 — Verification

## Phase 0 — Environment

**Expected:** all four answer.

```bash
curl -sk -o /dev/null -w "apim          %{http_code}\n" "$APIM/services/Version"
curl -s  -o /dev/null -w "gw mgmt       %{http_code}\n" -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis"
curl -sk -o /dev/null -w "platform-api  %{http_code}\n" "$PLATFORM_API/health"
curl -s  -o /dev/null -w "mediator      %{http_code}\n" localhost:8085/health
# apim          200
# gw mgmt       200
# platform-api  200
# mediator      200
```

Record the starting counters. Part 1 leaves them at zero:

```bash
curl -s localhost:8085/status | jq -c
# {"events":{"applied":0,"failed":0}}
```

**Expected:** `applied: 0`. Anything else means the mediator was not restarted
in S8, and every counter below is off by that amount.

---

## Phase 1 — Baseline

Both sample APIs must be deployed on the gateway with the two policies, and must
reject unauthenticated calls.

```bash
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis" \
  | jq -r '.apis[] | "\(.metadata.name)  \(.spec.policies|map(.name)|join(","))"'
# reading-list-api-v1  api-key-auth,subscription-validation
# echo-api-v1          api-key-auth,subscription-validation

curl -sk -o /dev/null -w "reading-list %{http_code}\n" "$GW_URL/reading-list/books"
curl -sk -o /dev/null -w "echo         %{http_code}\n" "$GW_URL/echo/foo"
# reading-list 401
# echo         401
```

**Expected:** exactly those two APIs, and both `401`. A `200` or `503` means the
policies are missing — stop and report it. (Phase 10 adds a third API; if you
are re-running phases out of order, expect it in this listing.)

The gateway must hold the `gold` plan **with limits**:

```bash
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscription-plans" \
  | jq -r '.subscriptionPlans[] | "\(.planName) \(.throttleLimitCount) \(.throttleLimitUnit)"'
# gold 10000 Hour
```

---

## Phase 2 — Subscribe and generate credentials

```bash
curl -sk -X POST "$PORTAL/subscriptions" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"artifactId":"reading-list-api-v1","subscriptionPlanId":"gold"}' | jq
```

**Expected:** `201`, with `subscriptionToken` and `subscriptionPlanName: "Gold"`.
Record `subscriptionId` as `$SUB` and `subscriptionToken` as `$RL_SUB`.

> The body takes only `artifactId` and `subscriptionPlanId`. Adding
> `applicationId` returns
> `400 request/body must NOT have additional properties`.

```bash
curl -sk -X POST "$PORTAL/apis/reading-list-api-v1/api-keys/generate" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"id":"verify_rl","displayName":"Verify RL"}' | jq
```

**Expected:** `201`, with a plaintext `key` — returned once. Record as `$RL_KEY`.

Wait for both, then assert the gateway state:

```bash
wait_applied 2

curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscriptions" | jq '.count'
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis/reading-list-api-v1/api-keys" \
  | jq -r '.apiKeys[].name'
# 1
# verify-rl
```

**Expected:** one subscription, and the key appears as `verify-rl` — the
portal's `verify_rl` **slugified**, underscores to hyphens.

```bash
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscriptions/$SUB" | jq -c '{id, externalRefId, status}'
# {"id":"<$SUB>","externalRefId":"<$SUB>","status":"ACTIVE"}
```

**Expected:** the gateway subscription's `id` and `externalRefId` are both the
portal's subscription id. A `404`, or an `id` that differs, means the gateway
controller predates [#3804](https://github.com/wso2/api-platform/pull/3804) —
a missing prerequisite, not a finding.

```bash
curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" \
  -H "API-Key: $RL_KEY" -H "Subscription-Key: $RL_SUB"
# 200
```

### Negative checks — which policy rejects what

```bash
curl -sk -o /dev/null -w "no headers      %{http_code}\n" "$GW_URL/reading-list/books"
curl -sk -o /dev/null -w "key only        %{http_code}\n" "$GW_URL/reading-list/books" -H "API-Key: $RL_KEY"
curl -sk -o /dev/null -w "sub-token only  %{http_code}\n" "$GW_URL/reading-list/books" -H "Subscription-Key: $RL_SUB"
# no headers      401
# key only        403
# sub-token only  401
```

**Expected:** `api-key-auth` answers `401`, `subscription-validation` answers
`403`. A `403` where `401` is expected means the policy order is wrong — see
[Policies Overview](https://wso2.com/api-platform/docs/api-gateway/1.2.0/policies/overview/).

---

## Phase 3 — Cross-API isolation

The point of this phase: **credentials must not be interchangeable between
APIs.** Subscribe to Echo API and generate its own key, then cross them.

```bash
curl -sk -X POST "$PORTAL/subscriptions" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"artifactId":"echo-api-v1","subscriptionPlanId":"iron"}' | jq
# record subscriptionId as $ECHO_SUB_ID and subscriptionToken as $ECHO_SUB

curl -sk -X POST "$PORTAL/apis/echo-api-v1/api-keys/generate" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"id":"verify_echo"}' | jq
# record key as $ECHO_KEY

wait_applied 4
```

```bash
printf "rl key  + rl sub   on reading-list : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" -H "API-Key: $RL_KEY"   -H "Subscription-Key: $RL_SUB"
printf "echo key+ echo sub on echo         : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/echo/foo"          -H "API-Key: $ECHO_KEY" -H "Subscription-Key: $ECHO_SUB"
printf "rl key  + echo sub on echo         : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/echo/foo"          -H "API-Key: $RL_KEY"   -H "Subscription-Key: $ECHO_SUB"
printf "echo key+ rl sub   on reading-list : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" -H "API-Key: $ECHO_KEY" -H "Subscription-Key: $RL_SUB"
printf "rl key  + echo sub on reading-list : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" -H "API-Key: $RL_KEY"   -H "Subscription-Key: $ECHO_SUB"
printf "echo key+ rl sub   on echo         : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/echo/foo"          -H "API-Key: $ECHO_KEY" -H "Subscription-Key: $RL_SUB"
```

**Expected — this is the security assertion of the whole run:**

| Case | Expected | Meaning |
|---|---|---|
| rl key + rl sub on reading-list | `200` | matched pair works |
| echo key + echo sub on echo | `200` | matched pair works |
| rl key + echo sub on **echo** | `401` | a key is scoped to its own API |
| echo key + rl sub on **reading-list** | `401` | same, other direction |
| rl key + **echo** sub on reading-list | `403` | a subscription token is scoped too |
| echo key + **rl** sub on echo | `403` | same, other direction |

**Any `200` in the last four rows is a critical finding.** It means a credential
issued for one API is accepted by another.

---

## Phase 4 — API key regeneration

```bash
curl -sk -X POST "$PORTAL/apis/reading-list-api-v1/api-keys/regenerate" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"keyId":"verify_rl"}' | jq
```

**Expected:** `200` with a new plaintext `key`. Record as `$RL_KEY2`.

> The field is `keyId`, not `id` — `{"id": ...}` returns `400`.

```bash
wait_applied 5
printf "old key : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" -H "API-Key: $RL_KEY"  -H "Subscription-Key: $RL_SUB"
printf "new key : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" -H "API-Key: $RL_KEY2" -H "Subscription-Key: $RL_SUB"
# old key : 401
# new key : 200
```

**Expected:** the old key is dead, the new one works. If this comes out
**inverted** (old `200`, new `401`), re-read both a few seconds later before
reporting anything: that is the settle problem described under *How to run
this*, not a rotation failure. A genuinely stale old key stays `200`
indefinitely. Mediator log:

```
msg="rotated the gateway API key" event_type=apikey.regenerated gateway_key_name=verify-rl
```

---

## Phase 5 — API key revocation

Generate a throwaway key so the before/after is unambiguous.

```bash
curl -sk -X POST "$PORTAL/apis/reading-list-api-v1/api-keys/generate" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"id":"revoke_me"}' | jq        # record as $TMP_KEY
wait_applied 6
printf "before revoke : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" -H "API-Key: $TMP_KEY" -H "Subscription-Key: $RL_SUB"
# before revoke : 200

curl -sk -X POST "$PORTAL/apis/reading-list-api-v1/api-keys/revoke" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"keyId":"revoke_me"}' -w '\nrevoke %{http_code}\n'
# revoke 204

wait_applied 7
printf "after revoke  : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" -H "API-Key: $TMP_KEY" -H "Subscription-Key: $RL_SUB"
printf "other key     : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" -H "API-Key: $RL_KEY2" -H "Subscription-Key: $RL_SUB"
# after revoke  : 401
# other key     : 200
```

**Expected:** the revoked key is rejected and **the other key on the same API is
untouched**. The second assertion matters — a revoke that kills sibling keys is a
finding.

---

## Phase 6 — Subscription suspension and resumption

```bash
curl -sk -X PUT "$PORTAL/subscriptions/$SUB" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"status":"INACTIVE"}' | jq -r .status
# INACTIVE

wait_applied 8
curl -sk -o /dev/null -w "suspended : %{http_code}\n" "$GW_URL/reading-list/books" \
  -H "API-Key: $RL_KEY2" -H "Subscription-Key: $RL_SUB"
# suspended : 403

curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscriptions" | jq -r '.subscriptions[] | "\(.id) \(.status)"'
# ... one row INACTIVE
```

**Expected:** `403` while suspended, and the gateway row reads `INACTIVE`. A
`200` means the status change didn't propagate.

```bash
curl -sk -X PUT "$PORTAL/subscriptions/$SUB" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"status":"ACTIVE"}' | jq -r .status
wait_applied 9
curl -sk -o /dev/null -w "resumed   : %{http_code}\n" "$GW_URL/reading-list/books" \
  -H "API-Key: $RL_KEY2" -H "Subscription-Key: $RL_SUB"
# resumed   : 200
```

**Expected:** back to `200` with the **same** key and token — suspension must not
invalidate credentials, only gate them. Mediator log for both:

```
msg="updated the gateway subscription status" event_type=subscription.updated status=INACTIVE
msg="updated the gateway subscription status" event_type=subscription.updated status=ACTIVE
```

---

## Phase 7 — Subscription token regeneration

```bash
curl -sk -X POST "$PORTAL/subscriptions/$SUB/regenerate-token" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' | jq
```

**Expected:** `200` with a new `subscriptionToken`. Record as `$RL_SUB2`.

```bash
wait_applied 10
printf "old token : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" -H "API-Key: $RL_KEY2" -H "Subscription-Key: $RL_SUB"
printf "new token : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" -H "API-Key: $RL_KEY2" -H "Subscription-Key: $RL_SUB2"
# old token : 403
# new token : 200
```

**Expected:** old token rejected, new one accepted, **API key unchanged**. The
mediator replaces the token on the existing gateway subscription — whose id
is the portal's subscription id — so the gateway subscription id does not
change:

```
msg="replaced the gateway subscription's token" event_type=subscription.token_regenerated gateway_subscription_id=<same as before>
```

---

## Phase 8 — Plan change, and the two allowlists

A plan change only takes effect if the target plan is allowed **in both places**:
`spec.subscriptionPlans` on the portal API *and* on the gateway API. This phase
deliberately tests the mismatch first.

### 8a — target plan not allowed on the gateway API

`reading-list-api-v1` ships allowing `gold` only, so first widen the **portal**
side alone:

```bash
cd "$DEMO"
mkdir -p /tmp/rlmeta
sed 's/^    - gold$/    - gold\n    - iron/' resources/reading-list-api/api.yaml > /tmp/rlmeta/api.yaml
curl -sk -X PUT "$PORTAL/apis/reading-list-api-v1" -H "Authorization: Bearer $TOKEN" \
  -F "metadata=@/tmp/rlmeta/api.yaml;type=application/yaml" -w '\n%{http_code}\n'
# 200
```

> The upload's **filename** must be `api.yaml` (or `metadata.yaml`/`.yml`/`.json`)
> — any other name is rejected `400 Invalid metadata file name`, which is why the
> edited copy is written into its own directory rather than renamed.

```bash
curl -sk -X POST "$PORTAL/subscriptions/$SUB/change-plan" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"planId":"iron"}' | jq -r .subscriptionPlanName
# Iron

wait_applied 11
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscriptions" \
  | jq -r '.subscriptions[] | "\(.id) \(.subscriptionPlanId // "(none)")"'
```

**Expected:** the portal reports `Iron`, but the gateway subscription is left
**with no plan** — the same subscription, its plan cleared in place
(`subscriptionPlanId: null`) — and the mediator warns:

```
msg="the new plan is not enabled for this gateway API; clearing the subscription's plan" plan_name=Iron
```

The `plan_changed` event carries no subscription token, and none is needed: the
plan changes on the existing subscription, which keeps its token.

This is deliberate: the mediator prefers a working subscription over a failed
event. The consequence is **no rate limiting** — 12 rapid calls all return `200`.

```bash
for i in $(seq 1 12); do curl -sk -o /dev/null -w "%{http_code} " "$GW_URL/reading-list/books" \
  -H "API-Key: $RL_KEY2" -H "Subscription-Key: $RL_SUB2"; done; echo
# 200 200 200 200 200 200 200 200 200 200 200 200
```

### 8b — allowlists aligned

Widen the gateway side too, then change the plan again (away and back, so a fresh
`subscription.plan_changed` fires):

```bash
sed 's/^    - gold$/    - gold\n    - iron/' \
  resources/reading-list-api/reading-list-subscription-api.yaml > /tmp/rl-gw.yaml
curl -s -X PUT "$GW_MGMT/rest-apis/reading-list-api-v1" -u "$GW_USER:$GW_PASS" \
  -H 'Content-Type: text/yaml' --data-binary @/tmp/rl-gw.yaml -o /dev/null -w '%{http_code}\n'
# 200

curl -sk -X POST "$PORTAL/subscriptions/$SUB/change-plan" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"planId":"gold"}' -o /dev/null -w 'to gold %{http_code}\n'
wait_applied 12
curl -sk -X POST "$PORTAL/subscriptions/$SUB/change-plan" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"planId":"iron"}' -o /dev/null -w 'to iron %{http_code}\n'
wait_applied 13

curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscriptions" \
  | jq -r '.subscriptions[] | "\(.id) \(.subscriptionPlanId // "(none)")"'
```

**Expected:** the subscription now carries a plan id.

> **No wait is needed between the gateway `PUT` and the plan change**, even
> though the mediator caches each API's plan allowlist for `APICacheTTL`
> (5 minutes). Nothing on the portal side announces a gateway-side change, so
> before dropping a plan as not-allowed the mediator re-reads the allowlist from
> the gateway and decides on that; the cached copy alone never costs a
> subscription its rate limit. When the cached reading was stale it says so:
>
> ```
> msg="the plan is enabled for this gateway API on a fresh read; the cached allowlist was stale" plan_name=Iron
> ```
>
> A `(none)` here, with
> `msg="the new plan is not enabled for this gateway API; clearing the subscription's plan"`,
> means the `PUT` above did not land — check
> `GET $GW_MGMT/rest-apis/reading-list-api-v1 | jq .spec.subscriptionPlans`
> actually reads `["gold","iron"]`.

The `iron` plan the mediator auto-created has **no limits**, so give it some —
this is the gap the walkthrough's step 10 is about:

```bash
IRON=$(curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscription-plans" \
  | jq -r '.subscriptionPlans[] | select(.planName|ascii_downcase=="iron") | .id')
curl -s -X PUT "$GW_MGMT/subscription-plans/$IRON" -u "$GW_USER:$GW_PASS" \
  -H 'Content-Type: application/json' \
  -d '{"planName":"Iron","billingPlan":"FREE","throttleLimitCount":10,
       "throttleLimitUnit":"Min","stopOnQuotaReach":true,"status":"ACTIVE"}' | jq -c
sleep 8
```

```bash
for i in $(seq 1 12); do curl -sk -o /dev/null -w "%{http_code} " "$GW_URL/reading-list/books" \
  -H "API-Key: $RL_KEY2" -H "Subscription-Key: $RL_SUB2"; done; echo
# 200 200 200 200 200 200 200 200 200 200 429 429
```

**Expected:** ten `200` then `429`. If all twelve are `200`, check that the
subscription actually carries the iron plan id and that the plan has both
`throttleLimitCount` and `throttleLimitUnit` — with either unset the data plane
skips rate limiting entirely.

```bash
curl -sk -D- -o /dev/null "$GW_URL/reading-list/books" \
  -H "API-Key: $RL_KEY2" -H "Subscription-Key: $RL_SUB2" | grep -i ratelimit
# x-ratelimit-limit: 10
# x-ratelimit-remaining: 0
# x-ratelimit-reset: <epoch seconds>
# x-ratelimit-full-quota-reset: <epoch seconds>
# ratelimit-limit: 10
# ratelimit-full-quota-reset: <seconds>
# ratelimit-remaining: 0
# ratelimit-reset: <seconds>
# ratelimit-policy: 10;w=60
```

---

## Phase 9 — Subscription deletion

The 10/min window from Phase 8 is probably still exhausted, and a `429` there
would be easy to misread as the `403` this phase expects. Let it reset first:

```bash
sleep 60
printf "pre-delete : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" \
  -H "API-Key: $RL_KEY2" -H "Subscription-Key: $RL_SUB2"
# pre-delete : 200

curl -sk -X DELETE "$PORTAL/subscriptions/$SUB" -H "Authorization: Bearer $TOKEN" -w '\n%{http_code}\n'
# 200
wait_applied 14
printf "after delete : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/reading-list/books" \
  -H "API-Key: $RL_KEY2" -H "Subscription-Key: $RL_SUB2"
# after delete : 403
```

**Expected:** `403` — the API key is still valid, but there is no subscription
behind it any more.

---

## Phase 10 — An API created in API Manager (CP→DP)

Phases 1–9 exercised APIs created **on the gateway**, which the gateway pushes up
to API Manager. This phase goes the other way: an API created in API Manager and
deployed **down** to the gateway, which is how a platform-gateway API normally
reaches a data plane.

In the walkthrough this is the Publisher UI. Over REST it is four calls.

```bash
cd "$DEMO"
ADD='{"name":"PizzaShack API","context":"/pizzashack","version":"1.0.0",
      "gatewayType":"APIPlatform","gatewayVendor":"wso2","policies":["Unlimited"],
      "endpointConfig":{"endpoint_type":"http",
        "production_endpoints":{"url":"https://'"$HOST_ADDR"':9444/am/sample/pizzashack/v1/api"},
        "sandbox_endpoints":{"url":"https://'"$HOST_ADDR"':9444/am/sample/pizzashack/v1/api"}}}'
curl -sk -X POST "$APIM/api/am/publisher/v4/apis/import-openapi" \
  -H "Authorization: Bearer $APIM_TOKEN" \
  -F "file=@resources/pizza-shack-api/definition.yaml" \
  -F "additionalProperties=$ADD" -o /tmp/pz.json -w 'import %{http_code}\n'
export PZ_ID=$(jq -r .id /tmp/pz.json)
```

**Expected:** `201`, and the created API reports `"gatewayType": "APIPlatform"`.
`409 The API already exists` means a previous run created it — recover the id
with
`curl -sk "$APIM/api/am/publisher/v4/apis?query=PizzaShack" -H "Authorization: Bearer $APIM_TOKEN" | jq -r '.list[0].id'`.

The endpoint must be reachable **from the router container**, hence `$HOST_ADDR`
rather than `localhost`.

Now the security. A platform-gateway API is **not** secured through *Runtime
Configuration → API Security*: that page drives `securityScheme`, a Synapse
concept API Manager accepts and leaves unchanged here, and nothing reaches the
gateway from it. Security comes from **policies**, which over REST is the
`apiPolicies` object, with a `policyId` of `<name>::<version>`:

```bash
curl -sk "$APIM/api/am/publisher/v4/apis/$PZ_ID" -H "Authorization: Bearer $APIM_TOKEN" -o /tmp/pz-api.json
jq '.apiPolicies = {"request":[
  {"policyName":"api-key-auth","policyVersion":"v1.2","policyId":"api-key-auth::v1.2",
   "parameters":{"key":"API-Key","in":"header"}},
  {"policyName":"subscription-validation","policyVersion":"v1.0","policyId":"subscription-validation::v1.0",
   "parameters":{"subscriptionKeyHeader":"Subscription-Key"}}
],"response":[],"fault":[]}' /tmp/pz-api.json > /tmp/pz-upd.json

curl -sk -X PUT "$APIM/api/am/publisher/v4/apis/$PZ_ID" -H "Authorization: Bearer $APIM_TOKEN" \
  -H 'Content-Type: application/json' --data-binary @/tmp/pz-upd.json \
  -o /tmp/pz-put.json -w 'policies %{http_code}\n'
jq -r '.apiPolicies.request[].policyName' /tmp/pz-put.json
# policies 200
# api-key-auth
# subscription-validation
```

**Expected:** `200`, and the response echoes both policies with server-assigned
UUID `policyId`s. Omitting `policyId` from the request fails with
`500 External policy identifier cannot be empty`.

Policies only reach the gateway with a deployment, so deploy a revision and
publish:

```bash
RID=$(curl -sk -X POST "$APIM/api/am/publisher/v4/apis/$PZ_ID/revisions" \
  -H "Authorization: Bearer $APIM_TOKEN" -H 'Content-Type: application/json' \
  -d '{"description":"verification run"}' | jq -r .id)

curl -sk -X POST "$APIM/api/am/publisher/v4/apis/$PZ_ID/deploy-revision?revisionId=$RID" \
  -H "Authorization: Bearer $APIM_TOKEN" -H 'Content-Type: application/json' \
  -d '[{"name":"dev-platform-gateway","vhost":"localhost","displayOnDevportal":true}]' \
  -o /dev/null -w 'deploy %{http_code}\n'

curl -sk -X POST "$APIM/api/am/publisher/v4/apis/change-lifecycle?apiId=$PZ_ID&action=Publish" \
  -H "Authorization: Bearer $APIM_TOKEN" -o /dev/null -w 'publish %{http_code}\n'
# deploy 201
# publish 200
```

`name` in the deploy body is the gateway's `name` from S4, not its display name.

```bash
sleep 10
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis" | jq -r '.apis[].metadata.name'
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis/pizzashack-api-1-0-0" | jq -r '.spec.policies[].name'
curl -sk "$GW_URL/pizzashack/1.0.0/menu" -w '\n%{http_code}\n'
# pizzashack-api-1-0-0 (plus the two from S6)
# api-key-auth
# subscription-validation
# {"error":"Unauthorized","message":"Valid API key required"}
# 401
```

**Expected:** the handle `pizzashack-api-1-0-0` — which is what
`resources/pizza-shack-api/api.yaml` already carries in `spec.referenceId`, the
one field matched by hand — both policies present, and `401` unauthenticated. A
call proxied straight through means the policies didn't reach the gateway:
re-check `.spec.policies` and that a **new revision was deployed after** saving
them.

Now subscribe to it in the portal, exactly as in Phase 2:

```bash
curl -sk -X POST "$PORTAL/subscriptions" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"artifactId":"pizza-shack-api-v1","subscriptionPlanId":"gold"}' | jq
# record subscriptionToken as $PZ_SUB, subscriptionId as $PZ_SUB_ID

curl -sk -X POST "$PORTAL/apis/pizza-shack-api-v1/api-keys/generate" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"id":"verify_pizza"}' | jq
# record key as $PZ_KEY

wait_applied 16
printf "no creds : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/pizzashack/1.0.0/menu"
printf "key only : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/pizzashack/1.0.0/menu" -H "API-Key: $PZ_KEY"
printf "both     : "; curl -sk -o /dev/null -w "%{http_code}\n" "$GW_URL/pizzashack/1.0.0/menu" -H "API-Key: $PZ_KEY" -H "Subscription-Key: $PZ_SUB"
# no creds : 401
# key only : 403
# both     : 503
```

**Expected — `503` on the last line is a pass, not a failure.** Both policies
ran and admitted the request; the router then failed to reach the backend,
because API Manager's own sample backend serves a self-signed certificate:

```bash
docker compose -f "$GW_DIR/docker-compose.yaml" logs gateway-runtime | grep pizzashack | tail -1
# ... 503 UF upstream_reset_before_response_started{... CERTIFICATE_VERIFY_FAILED:
#     ... unable_to_get_local_issuer_certificate ...}
```

The `401 → 403 → 503` progression *is* the assertion here: it proves the policy
chain reached the gateway and is ordered correctly. Verifying a `200` body needs
a backend the router trusts, which this walkthrough's sample backend is not.

> **Optional, and it does not reach `200`.** Registering the backend's CA with
> the controller fixes the *trust* half:
>
> ```bash
> openssl s_client -connect $HOST_ADDR:9444 -showcerts </dev/null 2>/dev/null \
>   | openssl x509 -outform PEM > /tmp/apim-ca.pem
> curl -s -X POST "$GW_MGMT/certificates" -u "$GW_USER:$GW_PASS" \
>   -H 'Content-Type: application/json' \
>   --data-binary "$(jq -n --arg c "$(cat /tmp/apim-ca.pem)" '{name:"apim-ca", certificate:$c}')"
> curl -s -X POST "$GW_MGMT/certificates/reload" -u "$GW_USER:$GW_PASS"
> ```
>
> Both return success and the router error **changes** — from
> `unable_to_get_local_issuer_certificate` to a `SAN_matcher` failure. It stops
> there: API Manager's default certificate is issued for `localhost`, the router
> dials `$HOST_ADDR`, and `[router.upstream.tls] verify_host_name` defaults to
> `true`. Getting to `200` needs a backend certificate that matches the address
> the router actually dials. Note also that dropping a PEM into
> `resources/certificates/` is **not** equivalent: that directory is bind-mounted
> into the *controller* (`/app/certificates`), and the runtime service mounts no
> such volume.

---

## Phase 11 — Known behaviours to confirm, not "fix"

These are real characteristics of the setup. An agent that meets them should
**report them as confirmed**, not try to route around them.

### Restarting the gateway revokes every portal-issued API key

```bash
cd "$GW_DIR" && docker compose restart gateway-controller
for i in $(seq 1 30); do
  c=$(curl -s -o /dev/null -w "%{http_code}" -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis")
  [ "$c" = "200" ] && break; sleep 2
done
sleep 6

curl -sk -o /dev/null -w "echo : %{http_code}\n" "$GW_URL/echo/foo" \
  -H "API-Key: $ECHO_KEY" -H "Subscription-Key: $ECHO_SUB"
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis/echo-api-v1/api-keys" | jq .totalCount
curl -s -u "$GW_USER:$GW_PASS" "$GW_MGMT/subscriptions" | jq -c '{count, statuses:[.subscriptions[].status]}'
# echo : 401
# 0
# {"count":2,"statuses":["ACTIVE","ACTIVE"]}
```

When the gateway is attached to API Manager, its startup API key sync revokes
every key API Manager doesn't know about — which is all of them, since the portal
issued them, and across every API at once. The controller log shows it plainly:

```
msg="Starting API key sync at gateway startup"
msg="Revoking API key" api_key=<name>
```

**Subscriptions survive; only keys are lost.** The mediator has nothing to redo
— those events succeeded and the revocation happened afterwards — so its
`events.failed` stays `0` and the portal shows those deliveries as `DELIVERED`.
Recover by generating a fresh key in the portal, which works immediately:

```bash
curl -sk -X POST "$PORTAL/apis/echo-api-v1/api-keys/generate" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"id":"post_restart"}' | jq -r .key        # record as $NEW_KEY
wait_applied 17
curl -sk -o /dev/null -w "echo with fresh key : %{http_code}\n" "$GW_URL/echo/foo" \
  -H "API-Key: $NEW_KEY" -H "Subscription-Key: $ECHO_SUB"
# echo with fresh key : 200
```

### A gateway that is down fails the delivery, and the portal records it

The mediator keeps no queue and does not retry: it applies each event before it
answers, and a gateway it cannot reach fails the delivery with `502`. This
checks that, and the recovery. It runs **after** the restart check above,
because starting the controller again revokes portal-issued keys once more.

```bash
cd "$GW_DIR" && docker compose stop gateway-controller

curl -sk -X POST "$PORTAL/apis/echo-api-v1/api-keys/generate" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"id":"while_down"}' | jq -r .key        # record as $DOWN_KEY
sleep 15
curl -s localhost:8085/status | jq -c .events
# {"applied":17,"failed":1}
cd "$DEMO/subscription-mediator" && docker compose logs | grep "event apply failed at the gateway" | tail -1
```

**Expected:** `failed` rises to `1` and `applied` stays `17`; the mediator log
reads `msg="event apply failed at the gateway" event_type=apikey.generated`.

Bring the controller back, then recover the way an operator would — a portal
action that sends the key again. Regenerating it does: when the gateway has no
key for that portal key, the mediator creates it.

```bash
cd "$GW_DIR" && docker compose start gateway-controller
for i in $(seq 1 30); do
  c=$(curl -s -o /dev/null -w "%{http_code}" -u "$GW_USER:$GW_PASS" "$GW_MGMT/rest-apis")
  [ "$c" = "200" ] && break; sleep 2
done
sleep 6

curl -sk -X POST "$PORTAL/apis/echo-api-v1/api-keys/regenerate" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"keyId":"while_down"}' | jq -r .key     # record as $DOWN_KEY2
wait_applied 18
curl -sk -o /dev/null -w "recovered key : %{http_code}\n" "$GW_URL/echo/foo" \
  -H "API-Key: $DOWN_KEY2" -H "Subscription-Key: $ECHO_SUB"
# recovered key : 200
```

**Expected:** `200`. The failed generate shows in the portal's delivery history
below.

### The mediator's counters must match

At the end of the run:

```bash
curl -s localhost:8085/status | jq -c .events
# {"applied":18,"failed":1}
```

**Expected:** `failed: 1` — only the deliberate one above. Any other failure has
a mediator log line with the reason, and the exact `[api_handles]` line to add
if it was a resolution failure.

### The portal's delivery log must match too

Clean mediator counters are **not** sufficient. They only cover events the
mediator received; an event the portal failed to deliver leaves no trace in
them at all. Ask the portal:

```bash
curl -sk "$PORTAL/webhook-subscribers/subscription-mediator/deliveries" \
  -H "Authorization: Bearer $TOKEN" \
  | jq -r '[.list[] | select(.status != "DELIVERED")]
           | "not delivered: \(length)", (.[] | "  \(.occurredAt) \(.eventType) \(.status) \(.lastHttpStatus) \(.lastError)")'
# not delivered: 1
#   <time> apikey.generated FAILED 502 null
```

**Expected:** `not delivered: 1`, the deliberate `apikey.generated` with HTTP
`502`. Any other `FAILED` row is a **lost portal action** —
the gateway is out of sync with the portal and nothing will reconcile it on its
own. The portal delivers each event once, with no retry and no redelivery
endpoint (`/redeliver` and `/retry` both answer `404`), so recovery is to redo
the portal action that produced it. If the row's `lastError` is `timeout`, raise
the subscriber's `timeoutMs` (S9) before re-running.

Note this endpoint takes no query parameters — `?limit=100` is rejected with
`400 Unknown query parameter 'limit'`; it returns the most recent 20 with a
`pagination` object.

---

## Reporting

Report per phase: **pass**, or the command, the expected result and what actually
happened. Call out specifically:

- any `200` in the Phase 3 cross-API matrix (critical),
- any credential that outlives its revocation, regeneration or deletion,
- any `events.failed` beyond the deliberate one in Phase 11, and any portal
  delivery not `DELIVERED` beyond that one,
- any `503` **other than** the expected one in Phase 10 — that is the gateway
  reaching an unreachable or untrusted backend, not an auth problem, and the
  router log gives the reason.

Distinguish a genuine failure from a settle artefact before reporting it: re-run
the assertion once, a few seconds later. A real failure repeats.

## Teardown

```bash
cd "$DEMO/subscription-mediator" && docker compose down
cd "$GW_DIR"     && docker compose down
cd "$PORTAL_DIR" && docker compose down
cd "$APIM_PACK"  && ./bin/api-manager.sh --stop                # or kill the process
```

`docker compose down` without `-v` keeps the named volumes, so the gateway and
portal databases survive a restart. The mediator has no volume; restarting it
(as in S8) resets its counters to zero for a re-run of Part 2.

To reset only the verification artefacts and keep the stack up:

```bash
curl -sk -X POST "$PORTAL/apis/reading-list-api-v1/api-keys/revoke" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"keyId":"verify_rl"}'
curl -sk -X POST "$PORTAL/apis/echo-api-v1/api-keys/revoke" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"keyId":"verify_echo"}'
curl -sk -X POST "$PORTAL/apis/echo-api-v1/api-keys/revoke" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"keyId":"while_down"}'
curl -sk -X DELETE "$PORTAL/subscriptions/$ECHO_SUB_ID" -H "Authorization: Bearer $TOKEN"
curl -sk -X DELETE "$PORTAL/subscriptions/$PZ_SUB_ID"   -H "Authorization: Bearer $TOKEN"
```

Phase 8 edits copies under `/tmp`, so the repository's
`resources/reading-list-api/*.yaml` are never modified — nothing to restore.

---

## References

Endpoint contracts behind every assertion above.

| Area | Reference |
|---|---|
| Platform gateway registration in API Manager | [Getting Started with Platform Gateway](https://apim.docs.wso2.com/en/latest/api-gateway/platform-gateway/getting-started/) |
| Portal subscriptions, keys, plans, webhooks | [API Portal & MCP Hub 1.0.0 docs](https://wso2.com/api-platform/docs/api-portal/1.0.0/overview/) |
| Webhook envelope, signature, event list | [Webhook Event Catalog](https://wso2.com/api-platform/docs/api-portal/1.0.0/references/webhook-event-catalog/) |
| Webhook subscriber registration, delivery | [Configure webhooks](https://wso2.com/api-platform/docs/api-portal/1.0.0/admin-settings/webhook-integration/) |
| Subscription UI semantics | [Manage Subscriptions](https://wso2.com/api-platform/docs/api-portal/1.0.0/consume-an-api/manage-subscriptions/) |
| API key UI semantics | [Manage API Keys](https://wso2.com/api-platform/docs/api-portal/1.0.0/consume-an-api/manage-api-keys/) |
| Portal plan limits | [Subscription Plans](https://wso2.com/api-platform/docs/api-portal/1.0.0/admin-settings/subscription-plans/) |
| Gateway management endpoints | [Gateway Controller Management API](https://wso2.com/api-platform/docs/api-gateway/1.2.0/gateway-controller-management-api/overview/) · [REST API Management](https://wso2.com/api-platform/docs/api-gateway/1.2.0/gateway-controller-management-api/rest-api-management/) |
| `api-key-auth`, `subscription-validation` | [Policies Overview](https://wso2.com/api-platform/docs/api-gateway/1.2.0/policies/overview/) |
| Gateway ↔ control plane | [Control Plane Connection](https://wso2.com/api-platform/docs/api-gateway/1.2.0/deployment/production-deployment/control-plane-connection/) |
| Distributions | [all releases](https://github.com/wso2/api-platform/releases) |

The gateway's own OpenAPI document
(`gateway-controller/api/management-openapi.yaml` in the repository) and API
Manager's `admin-api.yaml` / `publisher-api.yaml` (under
`repository/deployment/server/webapps/` in the pack) are the authority on request
and response shapes when a doc page and the running build disagree.
