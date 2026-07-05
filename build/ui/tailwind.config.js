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
        bg: "#0A0A0B", panel: "#131316", panel2: "#18181C",
        line: "#26262B", row: "#1C1C20",
        ink: "#C9C9D1", inkhi: "#F4F4F6", mut: "#6E6E78",
        signal: "#FF6A00", signalhi: "#FF7E22",
        phosphor: "#3DDC84", build: "#FFB224", fail: "#FF4D4F",
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
