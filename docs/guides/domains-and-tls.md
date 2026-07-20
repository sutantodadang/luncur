# Domains & TLS

Point your own domain at an app and get it serving HTTPS — luncur handles
the DNS check, the certificate request, and renewal for you. Reach for this
once an app is deployed and you want it at `yourdomain.com` instead of the
default URL.

## Add a custom domain

```sh
luncur domain add myapp www.example.com --project myproj
luncur domain list myapp --project myproj
luncur domain remove myapp www.example.com --project myproj
luncur domain retry myapp www.example.com --project myproj   # builtin provider only
```

Point a DNS A record for the hostname at the server's advertised IP (the
`--ip` luncur was installed with, printed by `luncur up`). `domain add`
checks this immediately and returns a warning — shown once in the CLI
output and, in the web UI, above the Domains table — if the hostname
doesn't resolve there yet; the domain is still created, since DNS often
lands after the request.

## Choose a TLS certificate provider

TLS certs come from one of three providers, chosen with `luncur up
--cert-provider <name>` (fixed for the life of the install; re-run `luncur
up --cert-provider ...` to change it):

- `builtin` (default) — luncur runs its own ACME (Let's Encrypt) HTTP-01
  client: certs are requested automatically per domain, stored as
  Kubernetes Secrets, and renewed within 30 days of expiry. Status
  (`none → pending → issued`, or `failed` with an error) shows in `domain
  list` and the web UI; `luncur domain retry` re-kicks a stuck domain.
- `traefik` — `luncur up --cert-provider traefik` delegates TLS
  termination and ACME to K3s's bundled Traefik (K3s-only).
- `cert-manager` — `luncur up --cert-provider cert-manager` delegates via a
  `ClusterIssuer` to an already-installed cert-manager.

Both `traefik` and `cert-manager` issue certs themselves — luncur just
annotates the Ingress and marks the domain `external` rather than tracking
issuance state. For `cert-manager`, the daily cert sweep additionally reads
the issued cert's expiry back from the TLS Secret cert-manager maintains, so
`domain list` and the web UI show a real `cert_expires_at` once cert-manager
has issued it.

Set the ACME account email (used by the `builtin` provider and passed to
`traefik`/`cert-manager`'s issuer config) with:

```sh
luncur config set acme_email you@example.com
```

## Use wildcard domains (DNS-01)

`domain add` also accepts `*.example.com` (one leading `*.`). Wildcards
can't be validated over HTTP-01, so they need a DNS provider luncur can
publish TXT records through:

```sh
luncur config set dns_provider cloudflare       # cloudflare | route53 | rfc2136 | none (default)
luncur config set dns_cloudflare_token ...      # write-only: reads show "(set)"

# route53
luncur config set dns_route53_access_key AKIA...
luncur config set dns_route53_secret_key ...    # write-only
luncur config set dns_route53_region us-east-1  # optional

# rfc2136 (nsupdate + TSIG, e.g. BIND)
luncur config set dns_rfc2136_server ns1.example.com
luncur config set dns_rfc2136_tsig_name luncur-key
luncur config set dns_rfc2136_tsig_secret ...   # write-only
luncur config set dns_rfc2136_tsig_algo hmac-sha256  # optional, default
```

With a provider configured, the `builtin` cert manager validates every
domain via DNS-01 (a TXT record at `_acme-challenge.<domain>`, polling the
zone's authoritative nameservers with a ~2 minute timeout) instead of
HTTP-01 — wildcards require it, plain hostnames just use it. A wildcard
without a configured `dns_provider` is refused with a 400. `traefik` and
`cert-manager` providers are unchanged — they own their own solving.
Failures behave like HTTP-01: the domain shows `cert_status failed` with
the message and the app keeps serving.

!!! note
    Internal (`--internal`) apps can't take a custom domain — see
    [Apps & projects](apps-and-projects.md).

**Related:** [Deploying](deploying.md) · [Apps & projects](apps-and-projects.md)
