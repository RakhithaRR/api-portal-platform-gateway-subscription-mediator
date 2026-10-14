# subscription-mediator runbook

Operating procedures for the subscription-mediator, from a code change to a
running pod in the **dev** environment, and what to do when something goes
wrong. The *why* behind each piece lives in the READMEs linked throughout; this
document is the *what to do*.

| | |
|---|---|
| Service | `subscription-mediator`: bridges API Portal 1.0.0 webhooks to the API Platform Gateway management API. Needs the gateway's subscription `referenceId` and partial `PUT` support, which 1.2.0 lacks ([wso2/api-platform#3782](https://github.com/wso2/api-platform/issues/3782)) |
| Environment | dev (AKS) |
| Image | `<registry>.azurecr.io/subscription-mediator`, tags `sha-<commit>` and `dev` |
| Kubernetes | namespace `subscription-mediator`, Helm release `subscription-mediator`, Deployment `subscription-mediator` (2 replicas), no volumes |
| Owner / on-call | `<team>` / `<on-call rota or channel>` |
| Escalation | API Portal: `<contact>` · API Platform Gateway: `<contact>` · AKS / Azure: `<contact>` |

The commands below assume:

```bash
NS=subscription-mediator
RELEASE=subscription-mediator
CHART=subscription-mediator/deploy/helm/subscription-mediator
ACR=<registry-name>                      # without .azurecr.io
PODS="app.kubernetes.io/name=subscription-mediator,app.kubernetes.io/instance=$RELEASE"
```

---

## 1. How it fits together

```
 developer ── PR ──▶ GitHub Actions ── push to main ──▶ ACR  <registry>.azurecr.io/subscription-mediator:sha-<commit>
                     (vet, test, govulncheck,                     │
                      multi-arch build)                           │ helm upgrade --set image.tag=sha-<commit>
                                                                  ▼
 ┌──────────────────────────────── AKS cluster ─────────────────────────────────┐
 │                                                                              │
 │  API Portal ── POST /devportal/events ──▶ subscription-mediator ──▶ Gateway Controller
 │  (webhook subscriber)   (ClusterIP svc)        Deployment, 2 replicas    management API :9090
 │                                                (no state)
 └──────────────────────────────────────────────────────────────────────────────┘
```

Three facts drive almost every procedure in this runbook:

1. **The API Portal delivers each webhook exactly once.** It does not retry.
   The mediator applies each event to the gateway before it answers: `200`
   once the gateway reflects it, `422` when the event cannot be applied, `502`
   when the gateway failed or could not be reached. Any non-`200` answer, and
   any delivery that finds no pod, is marked `FAILED` in the portal and never
   resent. Recovery is to fix the cause and repeat the action in the portal.
   See [§6.4](#64-recovering-failed-deliveries).
2. **The mediator keeps no state.** No volume, no queue, no stored tokens.
   Subscriptions are found on the gateway by their `referenceId` (the portal
   subscription ID), and API keys by their `externalRefId` (the portal key ID).
   Any replica can take any delivery.
3. **Keep two or more replicas.** With two, a rolling restart, upgrade or node
   drain always leaves a pod to deliver to. With one, every restart is a gap
   in which deliveries fail.

---

## 2. One-time setup

Do these once per environment, in order. Each step links to the detailed
instructions.

| # | Step | Detail | Done when |
|---|---|---|---|
| 1 | Create the managed identity, give it AcrPush and Reader on the registry, and add the GitHub federated credential | [mediator README → Publishing the image, step 1](subscription-mediator/README.md#1-azure-create-the-identity-and-let-it-push) | `az identity federated-credential list` shows the `repo:<owner>/<repo>:environment:dev` subject |
| 2 | Create the GitHub `dev` environment: limit it to the `main` branch, and add three secrets and one variable | [step 2](subscription-mediator/README.md#2-github-the-dev-environment) | A manual run of the workflow pushes an image ([§3.2](#32-confirm-the-image-was-published)) |
| 3 | Let AKS pull from ACR | `az aks update -g <rg> -n <cluster> --attach-acr $ACR` | `az aks check-acr -g <rg> -n <cluster> --acr $ACR.azurecr.io` passes |
| 4 | Create a gateway user with the **admin** role for the mediator | The gateway's user configuration | `curl -u user:pass <gateway>/api/management/v1/subscription-plans` returns 200 |
| 5 | Generate the webhook secret | `openssl rand -base64 48` | Stored in Key Vault (or your secret store) |
| 6 | Create the namespace and credentials Secret | [chart README → Credentials](subscription-mediator/deploy/helm/subscription-mediator/README.md#1-credentials) | `kubectl -n $NS get secret subscription-mediator-credentials` |
| 7 | Write the environment's values file from `examples/values-dev.yaml` | Fill in the gateway URL and the NetworkPolicy namespaces | `helm template` renders without errors |
| 8 | First install | [§3.3](#33-deploy-to-the-cluster) | Both pods are `Running`, and `helm test` passes |
| 9 | Register the webhook subscriber in the API Portal | [Register the webhook subscriber](#register-the-webhook-subscriber) below | The subscriber is listed with `enabled: true`, and a test subscription appears on the gateway ([§3.4](#34-verify-end-to-end)) |
| 10 | Mirror the subscription plans in the portal | [README §8](README.md#8-set-up-the-api-portal) | Plan names match the gateway's |

Keep the values file for each environment in version control, without secrets.
The credentials live only in the Secret, or in the store that syncs it.

### Register the webhook subscriber

Done in the API Portal's settings, signed in through the identity provider as a
user with the portal **admin** role
([Configure webhooks](https://wso2.com/api-platform/docs/api-portal/1.0.0/admin-settings/webhook-integration/)).

1. Read the webhook secret from the mediator's Secret, so the two cannot differ:

   ```bash
   kubectl -n $NS get secret subscription-mediator-credentials \
     -o jsonpath='{.data.webhook-secret}' | base64 -d; echo
   ```

2. Open `https://<portal-host>/api-portal/<org-handle>/settings`, choose
   **Webhooks** under **Integrations**, then **Add webhook**.

3. Fill in the form:

   | Field | Value |
   |---|---|
   | Name | `Subscription Mediator` |
   | Target URL | `http://subscription-mediator.subscription-mediator.svc:8085/devportal/events` |
   | Secret | the value from step 1 |
   | Timeout (ms) | `15000` |
   | Events | **Select all** under **Subscriptions** and under **API keys** |
   | Enabled | checked |

   Then **Add webhook**.

   - The Target URL is the mediator's in-cluster Service. It works only because
     the portal runs in the same cluster; NetworkPolicy must let the portal's
     namespace in (`networkPolicy.ingress.from` in the values file).
   - The timeout must cover a whole apply: the mediator answers only once the
     gateway reflects the event, and the default of `5000` is tight.

4. Check it: the webhook is listed with those events and **Enabled** on. Then
   subscribe to an API in the portal and confirm it on the gateway
   ([§3.4](#34-verify-end-to-end)).

The webhook secret is set here and in the Secret; changing it later means
changing both ([§5.3](#53-rotate-the-webhook-secret)).

Subscribers belong to one organization, and the portal delivers an event only to
its own organization's subscribers. Repeat these steps in every organization
whose subscriptions should reach the gateway, each with the same secret: the
mediator verifies every delivery against its one webhook secret.

---

## 3. Releasing a change

### 3.1 Build and publish (automatic)

1. Open a PR that changes `subscription-mediator/**`. CI runs `go mod tidy`,
   `go vet`, `go test -race`, `govulncheck`, and a multi-arch build that it
   does not push. **All must pass before merge.**
2. Merge to `main`. The workflow **subscription-mediator (dev)** checks
   everything again, then pushes `sha-<short-commit>` and moves `dev`.
3. Note the tag from the run's **Summary**. For example `sha-1a2b3c4`, along
   with its digest.

If CI fails, use the table in
[mediator README → When it fails](subscription-mediator/README.md#when-it-fails).

### 3.2 Confirm the image was published

```bash
TAG=sha-<commit>
az acr repository show-tags -n $ACR --repository subscription-mediator --orderby time_desc --top 5 -o table
docker buildx imagetools inspect $ACR.azurecr.io/subscription-mediator:$TAG   # must list linux/amd64 and linux/arm64
```

### 3.3 Deploy to the cluster

The Deployment rolls one pod at a time and brings the new pod up before
taking an old one down, so with two replicas a deploy causes no webhook gap.
A pod that is stopping finishes the deliveries it is applying first.

Pre-checks:

```bash
helm -n $NS history $RELEASE --max 3   # note the current revision, for rollback
```

Deploy:

```bash
helm upgrade --install $RELEASE $CHART -n $NS \
  -f values-dev.yaml \
  --set image.tag=$TAG \
  --atomic --wait --timeout 5m
```

`--atomic` rolls back automatically if the pods don't become ready in time.

### 3.4 Verify end to end

```bash
kubectl -n $NS rollout status deployment/subscription-mediator
kubectl -n $NS logs deployment/subscription-mediator | head -20
helm test $RELEASE -n $NS
```

Look for these lines:

```
msg="the gateway management API is reachable and the credentials were accepted"
msg="subscription-mediator is ready"
msg="listening for API Portal webhooks" address=0.0.0.0:8085 path=/devportal/events
```

Then do a functional check: subscribe to an API in the portal, and confirm the
subscription appears on the gateway (`GET /subscriptions`) with the portal
subscription ID as its `referenceId`, and that the portal's delivery history
shows the delivery `DELIVERED`.

### 3.5 Roll back

```bash
helm -n $NS history $RELEASE
helm -n $NS rollback $RELEASE <revision> --wait
```

A rollback rolls the pods the same way a deploy does. There is no state to
restore.

---

## 4. Monitoring

There is no metrics endpoint. Health comes from Kubernetes, the logs, the
portal's webhook delivery history and `/status`.

| Check | How | Healthy | Act when |
|---|---|---|---|
| Pods up | `kubectl -n $NS get pods -l $PODS` | `1/1 Running` on every replica, restarts stable | Not ready, `CrashLoopBackOff`, `Pending` → [§6.1](#61-the-pod-is-not-running) |
| Failed deliveries | portal: webhook subscriber delivery history; `/status` → `events.failed` | none | Any → [§6.4](#64-recovering-failed-deliveries) |
| Gateway failures | logs: `event apply failed at the gateway` (HTTP `502`) | none | Repeated → [§6.2](#62-events-fail-at-the-gateway) |
| Events that cannot apply | logs: `event cannot be applied` (HTTP `422`) | none | Any → [§6.3](#63-events-cannot-be-applied) |
| Rejected deliveries | logs: `rejected an unverified webhook delivery` | none | Any → [§6.5](#65-deliveries-are-rejected-401) |

Suggested alerts in your cluster monitoring:
- fewer ready pods than `replicaCount` for more than 2 minutes;
- more than 3 restarts in an hour;
- any log line at level `ERROR`;
- `rejected an unverified webhook delivery`.

Reading `/status`:

```bash
kubectl -n $NS port-forward svc/subscription-mediator 8085:8085 &
curl -s localhost:8085/status | jq
```

```json
{
  "events": { "applied": 128, "failed": 0 }
}
```

The counts are since that pod started, and per pod: a port-forward through
the Service reaches one replica, so read the logs of every pod, or the portal's
delivery history, for the full picture.

---

## 5. Routine operations

### 5.1 Pin a portal API to a gateway API

You need this when a delivery fails with `no gateway REST API matches portal
API ...`. The error is in the `422` response and the pod's log.

1. Copy the exact key from the error, e.g. `"order api:v1.0"`.
2. Add it to `values-dev.yaml`:
   ```yaml
   config:
     apiHandles:
       "order api:v1.0": orderapi-1.0.0
   ```
3. Deploy ([§3.3](#33-deploy-to-the-cluster)), keeping the current tag. The
   config change rolls the pods.
4. Repeat the failed actions in the portal ([§6.4](#64-recovering-failed-deliveries)).

### 5.2 Rotate the gateway credentials

1. Create the new password, or user, on the gateway. Keep the old one working.
2. Update the Secret, either in Key Vault or with
   `kubectl -n $NS create secret generic subscription-mediator-credentials ... --dry-run=client -o yaml | kubectl apply -f -`.
3. `kubectl -n $NS rollout restart deployment/subscription-mediator`.
4. Confirm the log shows `credentials were accepted`, then retire the old
   password.

### 5.3 Rotate the webhook secret

**Order matters.** The secret verifies each delivery, **and** it decrypts the
tokens and key values the delivery carries. A delivery signed with the other
secret is rejected with `401`.

1. Pick a quiet window and announce a pause on subscription and API-key changes.
2. Generate a new secret: `openssl rand -base64 48`.
3. Make these two changes **back to back**:
   1. Update `webhook-secret` in the credentials Secret, then
      `kubectl -n $NS rollout restart deployment/subscription-mediator`.
   2. Update the webhook subscriber's secret in the API Portal.

   Between them, and while the pods roll, deliveries are rejected with `401`.
4. Check the logs for `rejected an unverified webhook delivery`. There should
   be none after both changes are made and the rollout finished.
5. Recover any deliveries that failed in the window
   ([§6.4](#64-recovering-failed-deliveries)).

### 5.4 Change the log level

Set `config.logging.level: debug` in the values file and deploy. Revert it when
you're done, because debug logs carry event detail.

### 5.5 Node maintenance and cluster upgrades

The PodDisruptionBudget (on by default, `maxUnavailable: 1`) lets a drain evict
one replica at a time, so the other keeps taking deliveries. Drains and AKS
upgrades need no special window. Spread the replicas across nodes, with
`topologySpreadConstraints` or `affinity`, so one node going down doesn't take
both.

### 5.6 Uninstall

```bash
helm -n $NS uninstall $RELEASE
```

Deregister, or disable, the webhook subscriber in the portal **first**.
Otherwise every portal event fails from that point on.

---

## 6. Incident playbooks

Start every investigation with:

```bash
kubectl -n $NS get pods -l $PODS -o wide
kubectl -n $NS describe pods -l $PODS | tail -20
kubectl -n $NS logs -l $PODS --tail=100 --prefix          # add --previous after a crash
```

### 6.1 The pod is not running

| What you see | Cause | Fix |
|---|---|---|
| `CrashLoopBackOff`, last log names a setting (e.g. `[webhook] secret is required`) | Missing or invalid config or credentials | Fix the Secret or the values, then restart or `helm upgrade` |
| `CreateContainerConfigError` | The Secret, or a key in it, doesn't exist | `kubectl -n $NS get secret subscription-mediator-credentials -o jsonpath='{.data}' \| jq keys`. The keys must match `secret.keys.*` |
| `ImagePullBackOff` | Wrong tag, or AKS can't pull from ACR | Check the tag exists ([§3.2](#32-confirm-the-image-was-published)). Run `az aks check-acr` |
| `Pending`, *Insufficient cpu/memory* | No node has room for the pod | Add capacity, or lower `resources.requests` |
| Startup probe failures, killed after ~60 s | The process is stuck before it listens | Check the logs. Raise `startupProbe.failureThreshold` only after you understand why |

### 6.2 Events fail at the gateway

Logs show `event apply failed at the gateway`, the portal got a `502`, and its
delivery history shows the deliveries `FAILED`.

1. Is the gateway up? Check its pods and its own health.
2. Can the mediator reach it? From inside a pod:
   `kubectl -n $NS exec deploy/subscription-mediator -- wget -qO- -T5 <baseUrl>/subscription-plans`.
   This call sends no credentials, so any HTTP reply, including `401`, proves
   the network path works. A timeout or *connection refused* means it doesn't.
   If the path works but the mediator still fails, check its credentials
   ([§5.2](#52-rotate-the-gateway-credentials)).
3. Is it blocked? A timeout with the NetworkPolicy on usually means a wrong
   `networkPolicy.egress.rules` namespace or port.
4. Is the URL right? Check `config.gateway.baseUrl` against the gateway
   controller Service: `kubectl -n <gateway-ns> get svc`.

Nothing is queued or retried. Once the gateway answers again, repeat the
failed actions in the portal ([§6.4](#64-recovering-failed-deliveries)).

### 6.3 Events cannot be applied

Logs show `event cannot be applied` and the portal got a `422`. The error says
why:

| Error contains | Fix |
|---|---|
| `no gateway REST API matches portal API` | Deploy the API to the gateway, or pin it ([§5.1](#51-pin-a-portal-api-to-a-gateway-api)) |
| `failed to decrypt` / `carries no subscription token` | The webhook secret differs between the portal and the mediator ([§5.3](#53-rotate-the-webhook-secret)) |
| `the gateway has no subscription with reference ID` | A status or plan change for a subscription the gateway doesn't have; its `subscription.created` delivery was missed. Regenerate the subscription token in the portal: that event creates it |
| Unsupported artifact kind (`Mcp`, `WebSubApi`, …) | The gateway supports subscriptions only on `RestApi`. Nothing to fix |
| A gateway `4xx` rejection | Read the message. Usually the plan or API state on the gateway; fix it there |

After fixing the cause, repeat the action in the portal.

### 6.4 Recovering failed deliveries

Do this after any incident, rotation or outage, and whenever the portal shows
failed deliveries.

1. In the API Portal, check the webhook subscriber's delivery history for
   deliveries marked `FAILED`.
2. Fix the cause first ([§6.2](#62-events-fail-at-the-gateway),
   [§6.3](#63-events-cannot-be-applied)).
3. For each, redo the action in the portal so it sends a new event:
   - **Subscription created or token regenerated:** regenerate the
     subscription token. The new event creates the subscription if the
     gateway doesn't have it.
   - **Plan changed:** change the plan again. If the subscription itself is
     missing, regenerate the token first.
   - **Subscription suspended, resumed or deleted:** repeat the action, or apply
     it on the gateway directly.
   - **API key generated, regenerated or revoked:** repeat the action. The
     mediator creates a missing key on regenerate.
4. Confirm on the gateway, and that the new deliveries show `DELIVERED`.

### 6.5 Deliveries are rejected (401)

Logs show `rejected an unverified webhook delivery`.

- The secret in the portal's webhook subscriber doesn't match `webhook-secret`
  in the Secret. Make them equal ([§5.3](#53-rotate-the-webhook-secret)), then
  recover the failed deliveries ([§6.4](#64-recovering-failed-deliveries)).
- A large clock skew between the portal and the cluster can also fail
  signature checks. Check that NTP is working on both sides.

A `400` (`rejected a malformed webhook envelope`) means the payload isn't a
portal event envelope. Check what is calling the endpoint.

### 6.6 `helm upgrade` fails

| Error | Fix |
|---|---|
| `values don't meet the specifications of the schema` | A misspelt or removed key (e.g. `persistence`, `ingress`, `service.type`), or an invalid value. The message names it |
| `... is required` / `credentials are required` | Set the named value |
| `Deployment ... field is immutable` (selector) | You changed `nameOverride` or `fullnameOverride`. Uninstall and install again |
| `another operation (install/upgrade/rollback) is in progress` | A previous run was interrupted. Run `helm -n $NS history $RELEASE`, then `helm -n $NS rollback $RELEASE <last-deployed-revision>` |
| Timed out, rolled back by `--atomic` | A new pod didn't become ready. Check its logs ([§6.1](#61-the-pod-is-not-running)) |

---

## 7. Quick reference

```bash
# status (needs a port-forward; counts are per pod)
kubectl -n $NS port-forward svc/subscription-mediator 8085:8085
curl -s localhost:8085/status | jq

# deploy / roll back / test
helm upgrade --install $RELEASE $CHART -n $NS -f values-dev.yaml --set image.tag=sha-<commit> --atomic --wait
helm -n $NS rollback $RELEASE <revision> --wait
helm test $RELEASE -n $NS

# restart (rolls one pod at a time)
kubectl -n $NS rollout restart deployment/subscription-mediator

# what is running
kubectl -n $NS get deployment subscription-mediator -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
helm -n $NS list
```

| Need | Where |
|---|---|
| How the mediator works, its config, endpoints and delivery semantics | [subscription-mediator/README.md](subscription-mediator/README.md) |
| CI and ACR setup, and CI failures | [README → Publishing the image](subscription-mediator/README.md#publishing-the-image) |
| Chart values and design | [deploy/helm/subscription-mediator/README.md](subscription-mediator/deploy/helm/subscription-mediator/README.md) |
| The full demo flow (gateway, APIM, portal) | [README.md](README.md) |
