# subscription-mediator Helm chart

Deploys [subscription-mediator](../../../README.md), the bridge from WSO2 API
Portal 1.0.0 webhook events to the WSO2 API Platform Gateway 1.2.0 management
REST API, on Kubernetes 1.29 or later.

## What it deploys, and why

| Resource | Notes |
|---|---|
| Deployment, **two replicas** by default | The service keeps no state: every lookup reads the gateway, and each webhook is applied before the portal is answered, so any replica can take any delivery. A rolling update brings the new pod up before taking an old one down (`maxSurge: 1`, `maxUnavailable: 0`). |
| ConfigMap | `config.toml`: the gateway URL, the log level and `[api_handles]`. A change rolls the pods, because the service reads its config only at startup. |
| Secret (optional) | Only with `secret.create=true`. In production, point `secret.existingSecret` at a Secret managed outside Helm. |
| Service | ClusterIP only, on port 8085. |
| PodDisruptionBudget | On by default with `maxUnavailable: 1`, so a node drain evicts one replica at a time; see [Operating it](#operating-it). |
| NetworkPolicy (optional) | Limits inbound traffic to the API Portal, and outbound traffic to DNS and the gateway. |
| ServiceAccount | No API token is mounted; the service never calls the Kubernetes API. |
| `helm test` pod | Calls `/health` through the Service. |

The pod runs as UID/GID 10001 with a read-only root filesystem, no
capabilities, no privilege escalation and the `RuntimeDefault` seccomp profile.
It satisfies the Kubernetes **restricted** Pod Security Standard.

**The endpoints are unauthenticated.** The webhook endpoint and `/status`
must stay inside the cluster. That is why the chart
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

The image lives in a Docker repository in JFrog Artifactory, which requires
sign-in. Create a pull-only Artifactory user or access token (Read on the
repository), store it in a docker-registry Secret, and list that Secret in
`imagePullSecrets`:

```bash
kubectl -n subscription-mediator create secret docker-registry artifactory-pull \
  --docker-server=<artifactory host> \
  --docker-username=<user> --docker-password=<token>
```

```yaml
imagePullSecrets:
  - name: artifactory-pull
```

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
- `config.gateway.baseUrl`: the management API,
  `http://<release>-gateway-controller.<namespace>.svc:9090/api/management/v1`.
  The gateway 1.2.0 controller serves it over plain HTTP only; it has no TLS
  listener. For TLS, put a TLS-terminating proxy or ingress in front of it and
  use that `https` address, whose certificate must name that host. Allow the
  same port (`9090`, or the proxy's) in `networkPolicy.egress.rules`.

You must also set `secret.existingSecret`, or `secret.create=true` with the
credential values.

When that proxy's certificate is self-signed or from a private CA, give the
mediator that CA with `config.gateway.ca`: the name of a Secret (for example
cert-manager's, which carries `ca.crt`) or a ConfigMap (for example a
trust-manager bundle) in the mediator's namespace, and the key holding the PEM.
It is mounted read-only and trusted in addition to the system roots. It is read
at startup, so after the CA changes run
`kubectl rollout restart deployment/<fullname>`.

### 4. Verify and connect the portal

```bash
kubectl -n subscription-mediator rollout status deployment/subscription-mediator
kubectl -n subscription-mediator logs deployment/subscription-mediator
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

- **Reading the operational endpoint:**
  ```bash
  kubectl -n subscription-mediator port-forward svc/subscription-mediator 8085:8085
  curl -s localhost:8085/status
  ```
  `/status` reports `{"events":{"applied":N,"failed":N}}`. The counters are
  in memory and per pod, so they reset on restart and a port-forward shows
  one replica only.
- **Finding failed events:** each webhook is applied before the portal gets
  its answer: `200` once the gateway reflects the event, `422` when the event
  cannot be applied (the response and the pod log say why), `502` when the
  gateway failed or could not be reached. The API Portal attempts each
  delivery exactly once and never retries, so a failure shows up as a
  `FAILED` delivery in the portal's webhook delivery history. Fix the cause,
  then repeat the action in the portal so it sends a new event. For a missed
  `subscription.created`, regenerate the subscription token: that event
  carries everything the mediator needs to create the subscription.
- **Mapping a portal API to a gateway handle:** add it under
  `config.apiHandles` and run `helm upgrade`. The pods restart with the new
  config. Then repeat the failed actions in the portal.
- **Rotating credentials:** update the Secret, then run
  `kubectl rollout restart deployment/<fullname>`. Environment variables are
  read only at startup. With `secret.create=true`, `helm upgrade` restarts the
  pods for you.
- **Restarts and node maintenance:** on SIGTERM a pod stops accepting
  webhooks and finishes the ones it is applying (each within 25s;
  `terminationGracePeriodSeconds` is 45). With two or more replicas, a rolling
  restart, an upgrade or a node drain always leaves a pod to deliver to, and
  the PodDisruptionBudget keeps a drain from evicting both at once. With
  `replicaCount: 1`, every restart is a gap in which deliveries fail. Either
  way, give the pods a `priorityClassName` so they aren't preempted under
  resource pressure.

### Settings that cannot change after install

A Deployment's selector is immutable. If you change `nameOverride` or
`fullnameOverride`, `helm upgrade` fails; uninstall and install again instead.

## Values

Every value is documented in [`values.yaml`](values.yaml).
[`values.schema.json`](values.schema.json) rejects unknown keys and invalid
values at install time. The ones most installs set:

| Value | Default | Purpose |
|---|---|---|
| `image.repository` | `""` | e.g. `mycompany.jfrog.io/docker-dev/api-platform/subscription-mediator` |
| `imagePullSecrets` | `[]` | the docker-registry Secret for Artifactory, e.g. `[{name: artifactory-pull}]` |
| `image.tag` / `image.digest` | `""` | the build to run. A digest wins over a tag. |
| `config.gateway.baseUrl` | `""` | Gateway Controller management API URL: `http://…:9090/api/management/v1`, or `https://…` for a TLS-terminating proxy in front of it |
| `config.gateway.ca.secretName` / `.configMapName` | `""` | the TLS proxy's CA, when self-signed or private; one of the two. `https` only |
| `config.gateway.ca.key` | `ca.crt` | the key holding the PEM in that Secret or ConfigMap |
| `config.logging.level` | `info` | `debug`, `info`, `warn` or `error` |
| `config.apiHandles` | `{}` | `"<name>:<version>": <gateway-handle>` pins |
| `secret.existingSecret` | `""` | Secret holding the credentials |
| `replicaCount` | `2` | keep at 2 or more so a restart doesn't drop deliveries |
| `networkPolicy.enabled` | `false` | enable in production, with `ingress.from` and `egress.rules` |
| `podDisruptionBudget.enabled` | `true` | evict at most `podDisruptionBudget.maxUnavailable` (1) replica at a time |
| `priorityClassName` | `""` | protect the pods from preemption |
| `resources` | 50m / 64Mi requested, 256Mi limit | |

## Checking a change to the chart

```bash
helm lint subscription-mediator/deploy/helm/subscription-mediator \
  --set image.repository=r/sm,image.tag=t,config.gateway.baseUrl=http://gw:9090/api/management/v1,secret.existingSecret=s
helm template t subscription-mediator/deploy/helm/subscription-mediator -f examples/values-dev.yaml \
  --set image.tag=t | kubeconform -strict -summary
```
