/** Regenerate internal/server/static/app.css after editing templates:
 *  bun x tailwindcss@3.4.17 -c build/ui/tailwind.config.js -i build/ui/input.css -o internal/server/static/app.css --minify
 */
module.exports = {
  content: ["./internal/server/templates/*.html"],
  // Status/cert-status classes are built dynamically in Go templates
  // (class="status-{{.Status}}"), so Tailwind's content scanner never sees
  // the literal class name and would otherwise purge it. Safelist the full
  // enumerated set (matches the status values the server ever renders) plus
  // the chip color variants, which are part of the design system's public
  // API even where no current template applies them directly.
  safelist: [
    "status-live", "status-failed", "status-building", "status-deploying",
    "status-issued", "status-pending", "status-none", "status-external",
    "chip-ok", "chip-warn", "chip-bad", "chip-muted",
  ],
  theme: {
    extend: {
      colors: {
        // Theme-able tokens resolve to CSS custom properties (see
        // build/ui/input.css :root / :root[data-theme="light"]) so the
        // light/dark toggle needs no Tailwind rebuild. termbg/termtext/termts
        // stay fixed literals — the terminal pane is always dark.
        bg: "var(--c-bg)", panel: "var(--c-panel)", panel2: "var(--c-panel2)",
        line: "var(--c-line)", row: "var(--c-row)", rowhover: "var(--c-rowhover)",
        ink: "var(--c-ink)", inkhi: "var(--c-inkhi)", mut: "var(--c-mut)",
        signal: "var(--c-signal)", signalhi: "var(--c-signalhi)",
        phosphor: "var(--c-phosphor)", build: "var(--c-build)", fail: "var(--c-fail)",
        side: "var(--c-side)",
        termbg: "#060607", termtext: "#9BE9BC", termts: "#4A4A52",
      },
      fontFamily: {
        sans: ['"IBM Plex Sans"', "sans-serif"],
        mono: ['"IBM Plex Mono"', "monospace"],
      },
    },
  },
  plugins: [],
}
