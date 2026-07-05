# Design System — luncur

## Product Context
- **What this is:** Self-hosted PaaS in one Go binary — Heroku-simple deploys on your own K3s, with an escape hatch to raw K8s objects.
- **Who it's for:** Solo developers and small teams operating their own servers; comfortable with terminals but wanting Dokploy/Coolify-grade convenience.
- **Space/industry:** Self-hosted PaaS (peers: Dokploy, Coolify, CapRover).
- **Project type:** Operator dashboard (web app), server-rendered html/template + htmx + Tailwind, air-gapped (all assets vendored + go:embed).

## North Star
**"Semua bisa dari sini."** Every CLI capability has a UI control, and every UI
control teaches its CLI command back (see CLI-echo). A user should never be
*forced* to open a terminal — and never feel lost when they do.

## Aesthetic Direction
- **Direction:** Industrial/Utilitarian — launch-control room. "luncur" = launch; the UI is a control panel, not a marketing site.
- **Decoration level:** minimal-intentional. Only two textures allowed: a subtle 1px engineering-grid on select panels, and faint scanlines on log terminals. No illustrations, no gradients, no blobs.
- **Mood:** Serious, dense, fast. Machine truth over friendliness. Engineered, not bubbly.
- **Signature pattern — CLI-echo:** Under (or beside) every action control, a muted monospace line shows the equivalent CLI command, e.g. `$ luncur scale api 3`. This is the product differentiator made visible: transparency and escape-hatch, everywhere. Style: Plex Mono 11px, color `#6E6E78`, command text `#8A8A94`.

## Typography
- **UI/Body:** IBM Plex Sans (400/500/600/700) — industrial heritage, excellent at small sizes, not Inter/system-ui.
- **Display/Headings:** IBM Plex Sans 600–700. Page titles 20px, section headers via section-labels (below).
- **Section labels:** IBM Plex Mono 600, 11px, uppercase, letter-spacing 0.14em, color muted — the control-panel voice. Used for every card/section heading.
- **Data/Tables/Paths/IDs:** IBM Plex Mono with `font-variant-numeric: tabular-nums`.
- **Code/Logs:** IBM Plex Mono.
- **Loading:** self-hosted WOFF2, vendored in `internal/server/static/fonts/` and served via the existing go:embed static handler. NO CDN (air-gapped rule). Weights: Sans 400/500/600/700, Mono 400/500/600 (~6 files).
- **Scale:** 11px (labels/echo) · 12px (dense data) · 13px (table/body-sm) · 14px (body, base) · 16px (card titles) · 20px (page titles) · 28px (login/hero only).

## Color
- **Approach:** restrained — one accent, semantic status colors, everything else neutral.
- **Page background:** `#0A0A0B` · **Panel:** `#131316` · **Panel-raised:** `#18181C` · **Border:** `#26262B` · **Row divider:** `#1C1C20`
- **Text:** `#C9C9D1` · **Headings/white:** `#F4F4F6` · **Muted:** `#6E6E78`
- **Accent (signal orange):** `#FF6A00`, hover `#FF7E22`. **Discipline rule:** orange is ONLY for primary actions (deploy, create, save), active-nav indicator, and focus rings. Never for status, never for decoration. If orange appears more than ~2 times per viewport, something is wrong.
- **Semantic:** live/ok `#3DDC84` (phosphor green) · building/warn `#FFB224` (pulses while building) · failed/error `#FF4D4F` · idle/muted slate `#6E6E78`. Chips: 10% tinted bg + 25% tinted border + colored mono text.
- **Terminal pane:** bg `#060607`, log text `#9BE9BC`, timestamps `#4A4A52`.
- **Dark mode:** dark-ONLY by design (operator tool). No light theme, no toggle. Do not add one without explicit owner approval.
- **Forbidden:** purple/indigo accents, gradients of any kind, colored icon-circles.

## Spacing
- **Base unit:** 4px
- **Density:** compact — information beats whitespace. Table rows 32px. Card padding 16–20px.
- **Scale:** 2xs(2) xs(4) sm(8) md(12) lg(16) xl(24) 2xl(32) 3xl(48)

## Layout
- **Approach:** grid-disciplined. Fixed left sidebar (180–224px, bg `#0D0D0F`): brand `luncur_` (orange underscore), nav groups with mono uppercase group labels, active item = 2px orange left-border. Main column: mono breadcrumb (`project / app`), page title row with actions right-aligned, then stacked section cards.
- **Max content width:** 1080px (max-w-6xl).
- **Inline actions:** every resource row (deploy, domain, volume, addon, key, token) carries its actions in the rightmost cell — never navigate away for a single action. Destructive = `btn-danger` (outlined red) + `hx-confirm`.
- **Border radius:** 4px (chips/inputs) · 5px (buttons) · 6px (cards/panels) · 8px (page-level mockups). Nothing rounder — engineered, not friendly.
- **Empty states:** one muted sentence + the CLI-echo of the command that creates the first item.

## Motion
- **Approach:** minimal-functional.
- **Allowed:** htmx swap fade 120ms ease-out; `building` chip pulse (1.6s ease-in-out); log-cursor blink. Nothing else.
- **Easing:** enter(ease-out) exit(ease-in). **Duration:** micro(120ms) only.

## Parity Contract (functional design rule)
The UI is incomplete while any CLI verb lacks a UI control. Known gaps to close
(tracked for the v3 redesign): `project create`, `project add-member`,
`app destroy`, `eject`, `domain retry`, `addon upgrade/info`. `restore` stays
CLI-only deliberately (destructive; the Backups page must say so with its
CLI-echo). Any NEW CLI verb ships with its UI control in the same PR, or the PR
description must say why not.

## Decisions Log
| Date | Decision | Rationale |
|------|----------|-----------|
| 2026-07-05 | Initial design system created | /design-consultation; owner chose full redesign with originality, memorable thing = "semua bisa dari sini" |
| 2026-07-05 | Signal orange accent, drop indigo | Indigo/purple = category default (Coolify) and AI-slop default; orange = launch signal, instantly distinct |
| 2026-07-05 | IBM Plex vendored, no CDN | Air-gapped rule; Plex fits industrial direction; one-binary philosophy extends to fonts |
| 2026-07-05 | CLI-echo signature pattern | Product differentiator (escape-hatch transparency) expressed in the UI; teaches CLI for free |
| 2026-07-05 | Dark-only, no light theme | Operator tool; halves CSS surface; matches category expectation |
