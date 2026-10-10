# Uptime, incidents and status pages

What this documents: luncur's built-in uptime checks, the incidents they open,
and the public status page you can share with users.

## Uptime checks

Every live **web** app is checked once a minute: public apps by default,
internal apps when you opt in. By default the check probes the app's
in-cluster Service on its health path (or `/`). Any response below 500 counts
as up.

```sh
luncur uptime status web --project shop
luncur uptime enable web --project shop --external          # probe the public URL (TLS + DNS too)
luncur uptime enable web --project shop --path /api/health  # probe a different path
luncur uptime disable web --project shop
```

- **3 consecutive failures** open an incident and send `app_down`.
- **2 consecutive passes** resolve it and send `app_recovered`.

`app_down` is in the default `notify_events`. Raw results are kept for 48
hours, and daily roll-ups (with p95 latency) for 400 days. The app's
**Observe** tab shows the current state, 24h/30d/90d uptime and 30 daily bars.

## Incidents

Uptime checks open and resolve incidents automatically. Open your own for
planned maintenance:

```sh
luncur incident open "Database upgrade" --project shop --message "Starting 02:00 UTC, ~10 minutes"
luncur incident note 4 "Upgrade done, verifying" --project shop
luncur incident resolve 4 --project shop
luncur incident list --project shop
```

The project page has an **Incidents** card with the same actions.

## Public status page

```sh
luncur status-page enable --project shop --slug acme --title "Acme" --apps web,api
luncur status-page show --project shop
luncur status-page disable --project shop
```

The page is served at `https://<panel>/status/acme`. It shows only what you
publish:

- the page title;
- the selected apps' names and current state (operational, degraded, down);
- 90 daily uptime bars per app;
- open incidents, plus incidents resolved in the last 14 days.

It never shows namespaces, images or internal URLs. Apps come from the
project's default environment.

The page is public: no login, and no JavaScript. Each client IP is limited to
120 requests a minute, and the rendered page is cached for 30 seconds. Also
served:

- `/status/acme.json`: the same data as JSON, CORS-enabled, for embeds.
- `/status/acme/badge.svg`: a README badge:
  `![status](https://panel.example.com/status/acme/badge.svg)`

**Related:** [Deploying](deploying.md) · [Settings](../reference/settings.md)
