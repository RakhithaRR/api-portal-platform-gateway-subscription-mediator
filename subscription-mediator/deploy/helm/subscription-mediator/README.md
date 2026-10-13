# subscription-mediator Helm chart

Deploys [subscription-mediator](../../../README.md), the bridge from WSO2 API
Portal 1.0.0 webhook events to the WSO2 API Platform Gateway 1.2.0 management
REST API, on Kubernetes 1.29 or later.

## What it deploys, and why

| Resource | Notes |
|---|---|
| StatefulSet, **always one replica** | All state is in one file on a persistent volume: the API mappings, the subscription tokens and the pending event queue. The service has no leader election or locking, so only one pod may ever run. Unlike a Deployment, a StatefulSet never runs two pods for the same identity, not during a rollout and not when a node is lost. |
| PersistentVolumeClaim (`data-<fullname>-0`) | `ReadWriteOncePod` by default, so only one pod can mount it. Kept (`Retain`) when the release is uninstalled, because losing it loses queued events: the API Portal never redelivers a webhook. |
| ConfigMap | `config.toml`: the gateway URL, the log level and `[api_handles]`. A change rolls the pod, because the service reads its config only at startup. |
| Secret (optional) | Only with `secret.create=true`. In production, point `secret.existingSecret` at a Secret managed outside Helm. |
| Service + headless Service | ClusterIP only, on port 8085. The headless one is the StatefulSet's governing Service. |
| PodDisruptionBudget (optional) | Blocks voluntary evictions of the single pod; see [Operating it](#operating-it). |
| NetworkPolicy (optional) | Limits inbound traffic to the API Portal, and outbound traffic to DNS and the gateway. |
| ServiceAccount | No API token is mounted; the service never calls the Kubernetes API. |
| `helm test` pod | Calls `/health` through the Service. |

The pod runs as UID/GID 10001 with a read-only root filesystem, no
capabilities, no privilege escalation and the `RuntimeDefault` seccomp profile.
It satisfies the Kubernetes **restricted** Pod Security Standard.

**The endpoints are unauthenticated.** `/status`, `/failed` and
`POST /failed/requeue` must stay inside the cluster. That is why the chart
offers no Ingress, LoadBalancer or NodePort: the API Portal runs in the same
cluster and calls the ClusterIP Service directly. In a shared cluster, also
enable the NetworkPolicy, so that only the portal can reach the mediator.

## Install

### 1. Credentials

Create the Secret outside Helm. This keeps the values out of Helm's release
history, which is stored in the cluster. Any of these work:

- **External Secrets Operator** or the **Azure Key Vault CSI driver**,
  syncing from Key Vault. This is the recommended option on AKS.
- Sealed Secrets or SOPS.
- Plain `kubectl`, for a first install:

```bash
kubectl create namespace subscription-mediator
kubectl -n subscription-mediator create secret generic subscription-mediator-credentials \
  --from-literal=webhook-secret="$(openssl rand -base64 48)" \
  --from-literal=gateway-username='<gateway admin user>' \
  --from-literal=gateway-password='<gateway admin password>'
```

The webhook secret must equal the one registered with the API Portal's webhook
subscriber. It verifies each delivery's signature, and the service derives from
it the key that decrypts subscription tokens and API key values.

The default key names are `webhook-secret`, `gateway-username` and
`gateway-password`. Change them with `secret.keys.*` if your Secret uses others.

### 2. Image pull

On AKS, let the cluster pull from the registry directly:

```bash
az aks update -g <rg> -n <cluster> --attach-acr <registry>
```

Elsewhere, create a docker-registry Secret and list it in `imagePullSecrets`.

### 3. Install

Start from [`examples/values-dev.yaml`](examples/values-dev.yaml):

```bash
helm upgrade --install subscription-mediator subscription-mediator/deploy/helm/subscription-mediator \
  --namespace subscription-mediator \
  -f values-dev.yaml \
  --set image.tag=sha-<commit> \
  --atomic --wait --timeout 5m
```

Three values are required, and the chart refuses to render without them:

- `image.repository`
- `image.tag` or `image.digest`: the immutable `sha-<commit>` tag CI pushes,
  never `dev`.
- `config.gateway.baseUrl`

You must also set `secret.existingSecret`, or `secret.create=true` with the
credential values.

### 4. Verify and connect the portal

```bash
kubectl -n subscription-mediator rollout status statefulset/subscription-mediator
kubectl -n subscription-mediator logs statefulset/subscription-mediator
helm test subscription-mediator -n subscription-mediator
```

A healthy start logs `the gateway management API is reachable and the
credentials were accepted`. Then register the `targetUrl` that the install
notes print with the API Portal's webhook subscriber. With the commands above,
that is:

```
http://subscription-mediator.subscription-mediator.svc:8085/devportal/events
```

## Operating it

- **Reading the operational endpoints:**
  ```bash
  kubectl -n subscription-mediator port-forward svc/subscription-mediator 8085:8085
  curl -s localhost:8085/status
  curl -s localhost:8085/failed
  curl -s -X POST localhost:8085/failed/requeue
  ```
- **Mapping a portal API to a gateway handle:** add it under
  `config.apiHandles` and run `helm upgrade`. The pod restarts with the new
  config. Then requeue the failed events.
- **Rotating credentials:** update the Secret, then run
  `kubectl rollout restart statefulset/<fullname>`. Environment variables are
  read only at startup. With `secret.create=true`, `helm upgrade` restarts the
  pod for you.
- **Every restart is a gap in which webhooks are lost.** The API Portal
  attempts each delivery exactly once and never retries. While the single pod
  is down, deliveries are marked `FAILED` in the portal and never resent.
  Events the mediator had already accepted are safe: each one is on the
  volume before the portal gets its `202`. The gap includes:
  - the old pod stopping, which stops accepting at once on SIGTERM;
  - the volume moving, if the new pod lands on another node, which can take
    a minute or more on Azure Disk;
  - the new pod starting.

  So:
  - Upgrade, rotate credentials and change config in a quiet window.
  - Afterwards, check the portal's webhook delivery history for `FAILED`
    deliveries in that window, and redo the action in the portal so it sends
    a new event. For a missed subscription event, regenerate the subscription
    token: the new event carries everything the mediator needs.
- **Node maintenance is the same kind of gap.** The chart has an optional
  `podDisruptionBudget` (`maxUnavailable: 0`). It is off by default because,
  with one replica, it blocks every node drain and managed cluster upgrade
  until someone deletes the pod by hand. Turn it on if you'd rather every
  eviction were a deliberate, scheduled act. Either way, give the pod a
  `priorityClassName` so it isn't preempted under resource pressure.
- **Backups:** the volume holds the subscription tokens the service needs to
  re-create subscriptions on a plan change. Include the PVC in your volume
  snapshot or backup policy (Velero, Azure Backup for AKS).
- **Uninstalling** keeps the PVC (`persistence.retentionPolicy.whenDeleted:
  Retain`). Delete it explicitly once you are sure it is no longer needed:
  `kubectl -n <ns> delete pvc data-<fullname>-0`.

### Settings that cannot change after install

A StatefulSet's volume claim template, selector and governing Service are
immutable. If you change `persistence.size`, `storageClass`, `accessModes` or
`annotations`, or `nameOverride` / `fullnameOverride`, `helm upgrade` fails.
To change them:

1. Delete the StatefulSet, keeping the pod and PVC:
   `kubectl delete statefulset <fullname> --cascade=orphan`.
2. Run the upgrade.
3. To resize the volume itself, edit the PVC; the StorageClass must allow
   expansion.

## Values

Every value is documented in [`values.yaml`](values.yaml).
[`values.schema.json`](values.schema.json) rejects unknown keys and invalid
values at install time. The ones most installs set:

| Value | Default | Purpose |
|---|---|---|
| `image.repository` | `""` | e.g. `myregistry.azurecr.io/subscription-mediator` |
| `image.tag` / `image.digest` | `""` | the build to run. A digest wins over a tag. |
| `config.gateway.baseUrl` | `""` | Gateway Controller management API URL |
| `config.logging.level` | `info` | `debug`, `info`, `warn` or `error` |
| `config.apiHandles` | `{}` | `"<name>:<version>": <gateway-handle>` pins |
| `secret.existingSecret` | `""` | Secret holding the credentials |
| `persistence.storageClass` | cluster default | e.g. `managed-csi` on AKS |
| `persistence.accessModes` | `[ReadWriteOncePod]` | use `ReadWriteOnce` if the CSI driver lacks RWOP |
| `networkPolicy.enabled` | `false` | enable in production, with `ingress.from` and `egress.rules` |
| `podDisruptionBudget.enabled` | `false` | make node drains wait for a planned window |
| `priorityClassName` | `""` | protect the pod from preemption |
| `resources` | 50m / 64Mi requested, 256Mi limit | |

## Checking a change to the chart

```bash
helm lint subscription-mediator/deploy/helm/subscription-mediator \
  --set image.repository=r/sm,image.tag=t,config.gateway.baseUrl=http://gw:9090,secret.existingSecret=s
helm template t subscription-mediator/deploy/helm/subscription-mediator -f examples/values-dev.yaml \
  --set image.tag=t | kubeconform -strict -summary
```
