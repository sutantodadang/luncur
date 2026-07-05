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
  theme: { extend: {} },
  plugins: [],
}
