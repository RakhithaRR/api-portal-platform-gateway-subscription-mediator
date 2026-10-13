# subscription-mediator runbook

Operating procedures for the subscription-mediator, from a code change to a
running pod in the **dev** environment, and what to do when something goes
wrong. The *why* behind each piece lives in the READMEs linked throughout; this
document is the *what to do*.

| | |
|---|---|
| Service | `subscription-mediator`: bridges API Portal 1.0.0 webhooks to the API Platform Gateway 1.2.0 management API |
| Environment | dev (AKS) |
| Image | `<registry>.azurecr.io/subscription-mediator`, tags `sha-<commit>` and `dev` |
| Kubernetes | namespace `subscription-mediator`, Helm release `subscription-mediator`, StatefulSet `subscription-mediator` (pod `subscription-mediator-0`), PVC `data-subscription-mediator-0` |
| Owner / on-call | `<team>` / `<on-call rota or channel>` |
| Escalation | API Portal: `<contact>` · API Platform Gateway: `<contact>` · AKS / Azure: `<contact>` |

The commands below assume:

```bash
NS=subscription-mediator
RELEASE=subscription-mediator
CHART=subscription-mediator/deploy/helm/subscription-mediator
ACR=<registry-name>                      # without .azurecr.io
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
 │  API Portal ── POST /devportal/events ──▶ subscription-mediator-0 ──▶ Gateway Controller
 │  (webhook subscriber)   (ClusterIP svc)        │  StatefulSet, 1 replica      management API :9090
 │                                                ▼
 │                                     PVC data-subscription-mediator-0
 │                                     (queue, mappings, tokens)
 └──────────────────────────────────────────────────────────────────────────────┘
```

Three facts drive almost every procedure in this runbook:

1. **The API Portal delivers each webhook exactly once.** It does not retry.
   Any moment the pod is not serving is a moment in which portal events are
   lost, and they are marked `FAILED` in the portal. Upgrades, config changes,
   credential rotation and node drains all cause such a gap. Do them
   deliberately. See [§6.4](#64-recovering-webhooks-lost-during-a-gap).
2. **The PVC is the only copy of the subscription tokens.** The gateway stores
   only a hash, and plan changes need the token. The file holds tokens **in
   plaintext**, so treat the volume and its backups as secrets.
3. **Exactly one replica, always.** The state file has a single writer and no
   locking. Never scale the StatefulSet, and never run a second copy against the
   same data.

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
| 7 | Write the environment's values file from `examples/values-dev.yaml` | Fill in the gateway URL, StorageClass and the NetworkPolicy namespaces | `helm template` renders without errors |
| 8 | First install | [§3.3](#33-deploy-to-the-cluster) | The pod is `Running`, and `helm test` passes |
| 9 | Register the webhook subscriber in the API Portal | `targetUrl` `http://subscription-mediator.subscription-mediator.svc:8085/devportal/events`, and the **same** secret as step 5 | A test subscription appears on the gateway ([§3.4](#34-verify-end-to-end)) |
| 10 | Mirror the subscription plans in the portal | [README §8](README.md#8-set-up-the-api-portal) | Plan names match the gateway's |

Keep the values file for each environment in version control, without secrets.
The credentials live only in the Secret, or in the store that syncs it.

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

> **This causes a webhook gap.** It is usually under a minute, and several
> minutes if the pod moves to another node and its disk has to follow. Deploy
> in a quiet window, and announce it if portal users are active. See fact 1 in [§1](#1-how-it-fits-together).

Pre-checks:

```bash
kubectl -n $NS port-forward svc/subscription-mediator 8085:8085 &
curl -s localhost:8085/status | jq     # note queue.ready; ideally 0
curl -s localhost:8085/failed | jq .count
kill %1
helm -n $NS history $RELEASE --max 3   # note the current revision, for rollback
```

Deploy:

```bash
helm upgrade --install $RELEASE $CHART -n $NS \
  -f values-dev.yaml \
  --set image.tag=$TAG \
  --atomic --wait --timeout 5m
```

`--atomic` rolls back automatically if the pod doesn't become ready in time.
Note the time of the deploy, because you will need it for
[§6.4](#64-recovering-webhooks-lost-during-a-gap).

### 3.4 Verify end to end

```bash
kubectl -n $NS rollout status statefulset/subscription-mediator
kubectl -n $NS logs subscription-mediator-0 | head -20
helm test $RELEASE -n $NS
```

Look for these lines:

```
msg="loaded state" ...
msg="the gateway management API is reachable and the credentials were accepted"
msg="subscription-mediator is ready" workers=4 max_pending_events=5000
msg="listening for API Portal webhooks" address=0.0.0.0:8085 path=/devportal/events
```

Then do a functional check: subscribe to an API in the portal, and confirm the
subscription appears on the gateway (`GET /subscriptions`) and in
`/status` → `mappings.subscriptions`. Finally, check for webhooks lost during
the deploy ([§6.4](#64-recovering-webhooks-lost-during-a-gap)).

### 3.5 Roll back

```bash
helm -n $NS history $RELEASE
helm -n $NS rollback $RELEASE <revision> --wait
```

A rollback is another restart, so it is another gap. The state file format is
shared across versions. If a release ever changes that, its PR must say so,
and rolling back past it needs the volume restored from a backup
([§5.6](#56-back-up-and-restore-the-state-volume)).

---

## 4. Monitoring

There is no metrics endpoint. Health comes from Kubernetes, the logs and
`/status`.

| Check | How | Healthy | Act when |
|---|---|---|---|
| Pod up | `kubectl -n $NS get pod subscription-mediator-0` | `1/1 Running`, restarts stable | `0/1`, `CrashLoopBackOff`, `Pending` → [§6.1](#61-the-pod-is-not-running) |
| Queue | `/status` → `queue.ready` | 0, or briefly small | Climbing steadily → gateway unreachable or slow ([§6.2](#62-events-queue-up-but-are-not-applied)) |
| Retrying | `/status` → `queue.failed` | 0 | Non-zero for more than a few minutes |
| Permanent failures | `/status` → `queue.failedFinal`, `/failed` → `count` | 0 | Any → [§6.3](#63-events-failed-permanently) |
| Rejected deliveries | logs: `rejected an unverified webhook delivery` | none | Any → [§6.5](#65-deliveries-are-rejected-401) |
| Queue full | logs or portal: HTTP `503` "the event queue is full" | never | → [§6.2](#62-events-queue-up-but-are-not-applied) |
| Volume | `kubectl -n $NS exec subscription-mediator-0 -- df -h /app/data` | < 70 % | > 80 % → [§5.7](#57-expand-the-volume) |

Suggested alerts in your cluster monitoring:
- the pod not ready for more than 2 minutes;
- more than 3 restarts in an hour;
- any log line at level `ERROR`;
- `rejected an unverified webhook delivery`;
- `state holds events that exhausted every attempt`.

Reading `/status`:

```bash
kubectl -n $NS port-forward svc/subscription-mediator 8085:8085 &
curl -s localhost:8085/status | jq
```

```json
{
  "queue":    { "ready": 0, "failed": 0, "applied": 128, "failedFinal": 0 },
  "mappings": { "subscriptions": 42, "apiKeys": 17, "processed": 133 }
}
```

- `ready`: queued and waiting for a worker.
- `failed`: being retried, with backoff, up to 6 attempts.
- `applied` and `failedFinal`: counts since the pod started.
- `mappings`: what the volume holds. `processed` is the record of event IDs
  already applied, which stops an event being applied twice.

---

## 5. Routine operations

### 5.1 Pin a portal API to a gateway API

You need this when `/failed` says `no gateway REST API matches portal API ...`.

1. Copy the exact key from the `/failed` error, e.g. `"order api:v1.0"`.
2. Add it to `values-dev.yaml`:
   ```yaml
   config:
     apiHandles:
       "order api:v1.0": orderapi-1.0.0
   ```
3. Deploy ([§3.3](#33-deploy-to-the-cluster)), keeping the current tag. The
   config change restarts the pod, which causes a gap.
4. Requeue the failed events: `curl -s -X POST localhost:8085/failed/requeue`
   (through a port-forward).

### 5.2 Rotate the gateway credentials

1. Create the new password, or user, on the gateway. Keep the old one working.
2. Update the Secret, either in Key Vault or with
   `kubectl -n $NS create secret generic subscription-mediator-credentials ... --dry-run=client -o yaml | kubectl apply -f -`.
3. `kubectl -n $NS rollout restart statefulset/subscription-mediator`. This
   causes a gap.
4. Confirm the log shows `credentials were accepted`, then retire the old
   password.

### 5.3 Rotate the webhook secret

**Order matters.** The secret verifies each delivery, **and** it decrypts tokens
and key values when each queued event is *applied*. Events still queued when
the secret changes fail permanently, with *decryption failed*.

1. Pick a quiet window and announce a pause on subscription and API-key changes.
2. Wait until the queue is empty: `/status` → `queue.ready` and `queue.failed`
   are both `0`.
3. Generate a new secret: `openssl rand -base64 48`.
4. Make these two changes **back to back**:
   1. Update `webhook-secret` in the credentials Secret, then
      `kubectl -n $NS rollout restart statefulset/subscription-mediator`.
   2. Update the webhook subscriber's secret in the API Portal.

   Between them, deliveries are rejected with `401`.
5. Check the logs for `rejected an unverified webhook delivery`. There should
   be none after both changes are made.
6. Recover any deliveries lost in the window
   ([§6.4](#64-recovering-webhooks-lost-during-a-gap)).

### 5.4 Change the log level

Set `config.logging.level: debug` in the values file and deploy, which causes a
gap. Revert it when you're done, because debug logs carry event detail.

### 5.5 Node maintenance and cluster upgrades

The pod moves when its node drains, which causes a gap, plus a disk detach and
attach of up to a few minutes on Azure Disk.

- **Default (no PodDisruptionBudget):** drains proceed. Schedule AKS upgrades
  and node-image updates in a quiet window, and follow
  [§6.4](#64-recovering-webhooks-lost-during-a-gap) afterwards.
- **With `podDisruptionBudget.enabled=true`:** a drain blocks on this pod.
  When you're ready, delete the pod (`kubectl -n $NS delete pod
  subscription-mediator-0`) to let the drain continue.
- **Azure Disk is zonal.** The pod can only come back on a node in the disk's
  zone. Make sure the node pool has capacity in that zone, or the pod stays
  `Pending` ([§6.1](#61-the-pod-is-not-running)).

### 5.6 Back up and restore the state volume

The volume holds tokens in plaintext, so backups must be encrypted and access
to them restricted.

- **Back up** with Azure Backup for AKS or Velero (CSI snapshots), on at least
  a daily schedule. For an ad-hoc snapshot before risky work, take a
  `VolumeSnapshot` of `data-subscription-mediator-0`.
- **Restore:**
  1. `kubectl -n $NS scale statefulset/subscription-mediator --replicas=0`.
  2. Restore the snapshot into a PVC named `data-subscription-mediator-0`
     (delete or rename the old one first).
  3. `helm upgrade` (or scale back to 1).
  4. Everything that happened after the snapshot is missing from it. Use
     [§6.6](#66-the-state-volume-is-lost-or-corrupt) to reconcile.

### 5.7 Expand the volume

The StatefulSet's volume template can't be changed in place.

1. Check the StorageClass allows expansion: `kubectl get sc <class> -o
   jsonpath='{.allowVolumeExpansion}'`.
2. Expand the PVC itself:
   `kubectl -n $NS patch pvc data-subscription-mediator-0 -p '{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}'`.
3. To keep Helm in step, run `kubectl -n $NS delete statefulset
   subscription-mediator --cascade=orphan`, then `helm upgrade --set
   persistence.size=2Gi ...`. The pod and PVC are kept.

### 5.8 Uninstall

```bash
helm -n $NS uninstall $RELEASE
```

The PVC is **kept** (`Retain`). Before you delete it, make sure nothing will
need the tokens: `kubectl -n $NS delete pvc data-subscription-mediator-0`.
Deregister, or disable, the webhook subscriber in the portal **first**.
Otherwise every portal event fails from that point on.

---

## 6. Incident playbooks

Start every investigation with:

```bash
kubectl -n $NS get pod subscription-mediator-0 -o wide
kubectl -n $NS describe pod subscription-mediator-0 | tail -20
kubectl -n $NS logs subscription-mediator-0 --tail=100          # add --previous after a crash
```

### 6.1 The pod is not running

| What you see | Cause | Fix |
|---|---|---|
| `CrashLoopBackOff`, last log names a setting (e.g. `[webhook] secret is required`) | Missing or invalid config or credentials | Fix the Secret or the values, then restart or `helm upgrade` |
| `CreateContainerConfigError` | The Secret, or a key in it, doesn't exist | `kubectl -n $NS get secret subscription-mediator-credentials -o jsonpath='{.data}' \| jq keys`. The keys must match `secret.keys.*` |
| `ImagePullBackOff` | Wrong tag, or AKS can't pull from ACR | Check the tag exists ([§3.2](#32-confirm-the-image-was-published)). Run `az aks check-acr` |
| `Pending`, *volume node affinity conflict* | No schedulable node in the disk's zone | Add capacity in that zone, or uncordon a node there |
| `Pending`, *unbound PersistentVolumeClaim* | StorageClass missing, or it doesn't support `ReadWriteOncePod` | Check `persistence.storageClass`. If needed, use `persistence.accessModes=[ReadWriteOnce]` (that means a reinstall, [§5.7](#57-expand-the-volume) step 3) |
| Startup probe failures, killed after ~60 s | The process is stuck before it listens | Check the logs. Raise `startupProbe.failureThreshold` only after you understand why |

### 6.2 Events queue up but are not applied

`queue.ready` or `queue.failed` climbs, and logs show `could not reach the
gateway management API` or retry errors.

1. Is the gateway up? Check its pods and its own health.
2. Can the mediator reach it? From inside the pod:
   `kubectl -n $NS exec subscription-mediator-0 -- wget -qO- -T5 <baseUrl>/subscription-plans`.
   This call sends no credentials, so any HTTP reply, including `401`, proves
   the network path works. A timeout or *connection refused* means it doesn't.
   If the path works but the mediator still fails, check its credentials
   ([§5.2](#52-rotate-the-gateway-credentials)).
3. Is it blocked? A timeout with the NetworkPolicy on usually means a wrong
   `networkPolicy.egress.rules` namespace or port.
4. Is the URL right? Check `config.gateway.baseUrl` against the gateway
   controller Service: `kubectl -n <gateway-ns> get svc`.

Queued events are safe on the volume. They are applied once the gateway
answers, as long as it's within the retry budget. Events that exhaust it move
to `/failed`; requeue them after the fix ([§6.3](#63-events-failed-permanently)).
At **5000** pending events the mediator returns `503` and the portal marks
those deliveries `FAILED`, so treat a fast-growing queue as urgent.

### 6.3 Events failed permanently

```bash
curl -s localhost:8085/failed | jq '.failed[] | {eventType, attempts, lastError}'
```

| `lastError` contains | Fix |
|---|---|
| `no gateway REST API matches portal API` | Deploy the API to the gateway, or pin it ([§5.1](#51-pin-a-portal-api-to-a-gateway-api)) |
| `failed to decrypt` | The webhook secret differs between the portal and the mediator, or it was rotated with events queued ([§5.3](#53-rotate-the-webhook-secret)). Those events can't be recovered: redo the actions in the portal |
| Unsupported artifact kind (`Mcp`, `WebSubApi`, …) | The gateway 1.2.0 supports subscriptions only on `RestApi`. Nothing to fix |
| A gateway `4xx` rejection | Read the message. Usually the plan or API state on the gateway; fix it there |

After fixing the cause: `curl -s -X POST localhost:8085/failed/requeue`.

### 6.4 Recovering webhooks lost during a gap

Do this after any deploy, restart, rotation, node drain or outage.

1. Note the window: from when the old pod stopped until the new one logged
   `ready`.
2. In the API Portal, check the webhook subscriber's delivery history for
   deliveries marked `FAILED` in that window.
3. For each, redo the action in the portal so it sends a new event:
   - **Subscription created, plan changed or token regenerated:** regenerate the
     subscription token. The new event carries everything the mediator needs.
   - **Subscription suspended, resumed or deleted:** repeat the action, or apply
     it on the gateway directly.
   - **API key generated, regenerated or revoked:** repeat the action. The
     mediator creates a missing key on regenerate.
4. Confirm on the gateway, and in `/status` → `mappings`.

### 6.5 Deliveries are rejected (401)

Logs show `rejected an unverified webhook delivery`.

- The secret in the portal's webhook subscriber doesn't match `webhook-secret`
  in the Secret. Make them equal ([§5.3](#53-rotate-the-webhook-secret)), then
  recover the lost deliveries ([§6.4](#64-recovering-webhooks-lost-during-a-gap)).
- A large clock skew between the portal and the cluster can also fail
  signature checks. Check that NTP is working on both sides.

A `400` (`rejected a malformed webhook envelope`) means the payload isn't a
portal event envelope. Check what is calling the endpoint.

### 6.6 The state volume is lost or corrupt

Signs: the logs fail at `loaded state`, or the PVC is gone, or `/status`
mappings drop to zero after a restart.

1. Restore the latest backup if there is one ([§5.6](#56-back-up-and-restore-the-state-volume)).
2. Then reconcile, or do this alone if there is no backup:
   - The mediator re-resolves API mappings by itself, and adopts existing
     gateway subscriptions when the next event for them arrives.
   - **Tokens are not recoverable** from anywhere. Until a subscription's token
     is regenerated in the portal, its **plan changes will fail**. Ask the
     owners of active subscriptions to regenerate tokens, or do it as an
     administrator, starting with the most important applications.
   - Events queued at the time of the loss are gone. Treat the time since the
     last backup as a gap ([§6.4](#64-recovering-webhooks-lost-during-a-gap)).

### 6.7 `helm upgrade` fails

| Error | Fix |
|---|---|
| `values don't meet the specifications of the schema` | A misspelt or removed key (e.g. `ingress`, `service.type`), or an invalid value. The message names it |
| `... is required` / `credentials are required` | Set the named value |
| `StatefulSet ... spec: Forbidden: updates to statefulset spec for fields other than ...` | You changed a `persistence.*` setting or the name. See [§5.7](#57-expand-the-volume) step 3 |
| `another operation (install/upgrade/rollback) is in progress` | A previous run was interrupted. Run `helm -n $NS history $RELEASE`, then `helm -n $NS rollback $RELEASE <last-deployed-revision>` |
| Timed out, rolled back by `--atomic` | The new pod didn't become ready. Check its logs ([§6.1](#61-the-pod-is-not-running)) |

---

## 7. Quick reference

```bash
# status, failures, requeue (needs a port-forward)
kubectl -n $NS port-forward svc/subscription-mediator 8085:8085
curl -s localhost:8085/status | jq
curl -s localhost:8085/failed | jq
curl -s -X POST localhost:8085/failed/requeue

# deploy / roll back / test
helm upgrade --install $RELEASE $CHART -n $NS -f values-dev.yaml --set image.tag=sha-<commit> --atomic --wait
helm -n $NS rollback $RELEASE <revision> --wait
helm test $RELEASE -n $NS

# restart (causes a webhook gap; see §6.4)
kubectl -n $NS rollout restart statefulset/subscription-mediator

# what is running
kubectl -n $NS get statefulset subscription-mediator -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
helm -n $NS list
```

| Need | Where |
|---|---|
| How the mediator works, its config, endpoints and delivery semantics | [subscription-mediator/README.md](subscription-mediator/README.md) |
| CI and ACR setup, and CI failures | [README → Publishing the image](subscription-mediator/README.md#publishing-the-image) |
| Chart values and design | [deploy/helm/subscription-mediator/README.md](subscription-mediator/deploy/helm/subscription-mediator/README.md) |
| The full demo flow (gateway, APIM, portal) | [README.md](README.md) |
