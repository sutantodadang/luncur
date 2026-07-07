# Nebius smoke test — VERIFY gate checklist

## Why this exists

`internal/gpucloud/nebius.go` and `internal/gpucloud/nebiusauth.go` implement
the Nebius AI Cloud IAM token-exchange and Compute API against **shapes
derived from public docs** (docs.nebius.com, as read 2026-07-07) — not against
a live account. Every request/response shape and REST path that couldn't be
confirmed against a real Nebius tenant is marked in the source with a
`// VERIFY(nebius-smoke): <what it asserts>` comment.

This checklist walks a real account through the same calls luncur's Nebius
provider makes, so each marker can be confirmed (or fixed) before Nebius is
announced as production-ready. Until every gate below is checked off, treat
Nebius support as **docs-derived, not field-verified**.

### Current gates (from `internal/gpucloud/`)

Reproduce with:

```sh
grep -rn "VERIFY(nebius-smoke)" internal/gpucloud/
```

```
internal/gpucloud/nebius.go:199:              // VERIFY(nebius-smoke): CreateInstance JSON body shape
internal/gpucloud/nebius.go:204:      // VERIFY(nebius-smoke): CreateInstance REST path
internal/gpucloud/nebius.go:218:              // VERIFY(nebius-smoke): operation poll path
internal/gpucloud/nebius.go:219:              // VERIFY(nebius-smoke): operation response shape
internal/gpucloud/nebius.go:248:      // VERIFY(nebius-smoke): List instances path
internal/gpucloud/nebius.go:266:      // VERIFY(nebius-smoke): Destroy instance path
internal/gpucloud/nebiusauth.go:173:  // VERIFY(nebius-smoke): token-exchange endpoint path
```

| # | File:line | Asserts |
|---|---|---|
| 1 | `nebiusauth.go:173` | Token-exchange endpoint is `POST {endpoint}/iam/v1/tokens:exchange` |
| 2 | `nebius.go:199` | `CreateInstance` JSON body shape (`parent_id`, `name`, `resources.platform`/`preset`, `boot_disk`, `network_interfaces`, `cloud_init_user_data`) |
| 3 | `nebius.go:204` | `CreateInstance` REST path is `POST {endpoint}/compute/v1/instances` |
| 4 | `nebius.go:218` | Operation poll path is `GET {endpoint}/compute/v1/operations/{id}` |
| 5 | `nebius.go:219` | Operation response shape (`id`, `resource_id`, `done`) |
| 6 | `nebius.go:248` | List path is `GET {endpoint}/compute/v1/instances?parent_id=...`, response `items[].{id,name,status}` |
| 7 | `nebius.go:266` | Destroy path is `DELETE {endpoint}/compute/v1/instances/{id}` |

Run the grep yourself before starting — if the line numbers or marker text
have drifted from the table above, trust the live grep output, not this doc.

## Prerequisites

- A Nebius account with an active tenant + project (folder), and permission
  to create service accounts, authorized keys, and compute instances.
- `openssl`, `curl`, `jq` on your machine.
- Placeholders used below — replace with your real values:
  `$TENANT_ID`, `$PROJECT_ID` (this is `parent_id`), `$SUBNET_ID`, `$SA_ID`,
  `$PUBKEY_ID`.

## Steps

### a. Create a service account

In the Nebius console, create a service account scoped to the project you'll
rent instances in. Note its id as `$SA_ID`.

### b. Generate an authorized key

```sh
openssl genrsa -out nebius-sa.pem 4096
openssl rsa -in nebius-sa.pem -pubout -out nebius-sa.pub
```

Upload `nebius-sa.pub` to the service account in the console as an
"authorized key" / "access key". Note the key id it's assigned as
`$PUBKEY_ID`.

### c. Build the JWT

luncur builds this internally (`internal/gpucloud/nebiusauth.go`, `nebiusJWT`)
as an RS256-signed JWT:

- Header: `{"alg":"RS256","kid":"$PUBKEY_ID","typ":"JWT"}`
- Claims: `{"iss":"$SA_ID","sub":"$SA_ID","iat":<now>,"exp":<now+3600>}`

For a one-off manual test, use any small script or JWT CLI that can sign
those exact header/claims with `nebius-sa.pem` (RS256, unencoded base64url
segments joined by `.`, final `.` + base64url signature). The point of this
step is just to produce `$JWT` for step (d) — don't over-engineer it.

### d. Token exchange (confirms gate #1)

```sh
curl -sS -X POST "https://api.nebius.cloud/iam/v1/tokens:exchange" \
  -H "Content-Type: application/json" \
  -d '{
    "grant_type": "urn:ietf:params:oauth:grant-type:token-exchange",
    "requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
    "subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
    "subject_token": "'"$JWT"'"
  }' | tee token-response.json | jq .

export IAM_TOKEN=$(jq -r .access_token token-response.json)
```

Confirm the response has `access_token` (string) and `expires_in` (seconds,
number) — that's the exact shape `nebiusTokenSource.Token` decodes.

### e. Create the smallest VM (confirms gates #2, #3)

Use the cheapest CPU-only platform/preset your account has access to — this
is just testing the plumbing, not renting a GPU.

```sh
curl -sS -X POST "https://api.nebius.cloud/compute/v1/instances" \
  -H "Authorization: Bearer $IAM_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "parent_id": "'"$PROJECT_ID"'",
    "name": "luncur-smoke-test",
    "resources": {
      "platform": "cpu-e2",
      "preset": "2vcpu-8gb"
    },
    "boot_disk": {
      "size_gibibytes": 20,
      "source_image_family": {
        "image_family": "ubuntu22.04-driverless"
      }
    },
    "network_interfaces": [
      { "subnet_id": "'"$SUBNET_ID"'" }
    ],
    "cloud_init_user_data": "#cloud-config\nruncmd:\n  - [sh, -c, \"echo smoke > /tmp/ok\"]"
  }' | tee create-response.json | jq .

export OP_ID=$(jq -r .id create-response.json)
export INSTANCE_ID=$(jq -r .resource_id create-response.json)
```

Confirm: HTTP 2xx, and the response is an operation envelope (`id`,
`resource_id`, `done`) — matching `nebiusOperation` in `nebius.go`. Adjust
`platform`/`preset` to whatever the cheapest CPU shape is in your account;
the field names (`platform`, `preset`) are what's under VERIFY, not these
specific values.

### f. Poll the operation (confirms gates #4, #5)

```sh
curl -sS "https://api.nebius.cloud/compute/v1/operations/$OP_ID" \
  -H "Authorization: Bearer $IAM_TOKEN" | jq .
```

Repeat until `.done == true`. Confirm the polled response has the same
`id`/`resource_id`/`done` shape as the create response.

### g. List instances (confirms gate #6)

```sh
curl -sS "https://api.nebius.cloud/compute/v1/instances?parent_id=$PROJECT_ID" \
  -H "Authorization: Bearer $IAM_TOKEN" | jq .
```

Confirm the response has `items: [...]`, each item with `id`, `name`,
`status` fields.

### h. Delete the instance (confirms gate #7)

```sh
curl -sS -X DELETE "https://api.nebius.cloud/compute/v1/instances/$INSTANCE_ID" \
  -H "Authorization: Bearer $IAM_TOKEN"
```

Confirm 2xx and that the console shows the instance gone/deleting.

## Closeout

All gates confirmed → open a follow-up commit that removes every
`// VERIFY(nebius-smoke):` marker in `internal/gpucloud/nebius.go` and
`nebiusauth.go` (they're now verified truth, not assumptions) and updates
this doc's status line.

Any mismatch → **fix the shape in `nebius.go`/`nebiusauth.go` first**, re-run
the affected step, then remove that marker. Do not remove a marker without a
passing run against a real account.

Until this checklist is fully run and closed out, Nebius support in luncur is
**docs-derived** — functional against the documented API shape, unconfirmed
against a live account.
