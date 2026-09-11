# Sample APIs

The three APIs used by the [walkthrough](../README.md). Written for **WSO2 API
Manager 4.7.0**, **WSO2 API Platform Gateway 1.2.0** and **WSO2 API Portal
1.0.0** — the `apiVersion` at the top of each file is that release's schema, so
these won't load unchanged into another version.

| API | Path | Plan | Reaches the gateway by |
|---|---|---|---|
| Echo API | `/echo` | `iron` | created on the gateway, syncs up to API Manager |
| Reading List API | `/reading-list` | `gold` | created on the gateway, syncs up to API Manager |
| Pizza Shack API | `/pizzashack/1.0.0` | `gold` | created in API Manager, deployed down to the gateway |

## Files

| File | Goes to | `apiVersion` |
|---|---|---|
| `*-subscription-api.yaml` | the gateway's management REST API | `gateway.api-platform.wso2.com/v1` |
| `api.yaml` | the API Portal, as `metadata` | `api-portal.api-platform.wso2.com/v1` |
| `definition.yaml` | the API Portal, as `definition` | OpenAPI 3.0 |

Pizza Shack has no `*-subscription-api.yaml`: it is created in the API Manager
publisher instead, which is the whole point of including it.

## The field that has to match

`spec.referenceId` in each `api.yaml` is the API's **handle on the gateway** —
what `GET /rest-apis` returns as `metadata.name`:

```
echo-api-v1  ·  reading-list-api-v1  ·  pizzashack-api-1-0-0
```

The portal's own API UUID means nothing to a gateway fed by API Manager, so this
is how a portal subscription finds the right gateway API. Get it wrong and the
mediator reports `no gateway REST API matches portal API ...` on `GET /failed`.

`spec.subscriptionPlans` must likewise name a plan that exists on the gateway
**with its throttle limits set** — see step 10 of the walkthrough for why that
last part matters.

## Endpoints

`api.yaml` points both `productionUrl` and `sandboxUrl` at
`https://localhost:8443/...`. Change the host if your gateway isn't local.
