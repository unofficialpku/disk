# PKUDisk frontend

Nuxt 4.6.1, Nuxt UI 4.11.3 and Nuxt i18n 10.6.0, based on the official Nuxt UI
starter. The starter's MIT license is in `licenses/`. The interface uses Nuxt
UI's dashboard, forms, overlays and TanStack table components, Pinia for shared
state, VueUse for polling/clipboard/debouncing, and Zod for validation. Browser
uploads use Uppy 6, browser-fs-access and Nuxt UI FileUpload. Desktop paths are
selected visually through the local picker and transferred by the shared Go
queue. Mobile downloads use standard streamed HTTP attachments; there is no
JavaScript archive/download engine or native message bridge.

File lists default to folders first with locale-aware, numeric name ordering.
The Sort control also offers modification time and size, with an independent
Folders first switch; preferences persist in cookies. Material Icon Theme's
official manifest supplies file/folder icons, generated locally by Nuxt's
`ready` hook and bundled with the static assets. `npm run test:icons` verifies
association precedence and deterministic generation.

Eligible images and media use the server-generated 96×96 thumbnail endpoint.
Only visible rows fetch authenticated raster bytes, with four concurrent loads
and a bounded in-memory LRU cache scoped by account, file and revision. Pending
generation is checked twice at five-second intervals; unavailable previews keep
their Material icon. Blob URLs are revoked by VueUse when a row disappears.

Use Node 24 (at least 24.15.0) and the committed npm lockfile:

```sh
npm ci
npm run lint
npm run typecheck
npm run build
```

The build runs `nuxt generate` with `ssr: false`, then copies the static output
to `internal/gui/web/`. Rebuild the Go executable afterward. Node and Nitro are
not runtime dependencies. Go serves the page, authenticates browser requests,
and invokes the existing account, transfer and mount implementations.

Commit both source and generated assets. `node scripts/source-hash.mjs --check`
detects stale embedded assets without depending on Nuxt's variable build ID or
prerender timestamp. Frontend CI checks lint, types, source freshness, a fresh
production build, browser workflows and local authentication. The ordinary Go build and `go install` use the checked-in
assets without downloading JavaScript packages.

For integrated development, run `npm run dev` here, then start the Go application
from the repository root in another terminal:

```sh
PKUDISK_GUI_DEV_PROXY=http://127.0.0.1:3000 go run ./cmd/pkudisk gui --foreground
```

Use the browser window opened by Go. Assets and Vite's HMR WebSocket are proxied
to Nuxt, while authentication, `/api/` and `/internal/` stay on the Go service.
Proxy requests strip browser/control credentials. The environment variable only
accepts loopback origins and permits Vite's development CSP for assets; without
it, the embedded production interface retains its nonce-based strict CSP. Run a
separate GUI instance or stop the current instance before changing this setting.
The isolated fixture below supplies populated data without a real account.

## Conventions

- Use Nuxt UI components and semantic theme colors. Follow
  <https://ui.nuxt.com/llms.txt> and its component documentation/MCP metadata.
- All product strings belong in both `i18n/locales/zh-CN.json` and `en.json`.
  Pass the selected locale to `UApp`; Zod's built-in locales cover validation.
  Zod runs with `jitless: true` so validation works under a CSP without eval.
- The original logo is `assets/brand/source.png`. `scripts/generate-icons.py`
  generates web, tray, ICO and ICNS assets; see its accompanying README.
- Icons are bundled locally, with network providers disabled. System fonts are
  used. Product pages must not require third-party assets or a CDN.
- The Go HTML response supplies a CSP nonce. Pass it to `UApp` so Reka UI can
  inject its necessary styles. Do not enable unsafe-inline/unsafe-eval scripts.
  The small `patch-package` patch for Nuxt UI 4.11.3 passes the same nonce to its
  color plugin's temporary and Unhead-managed style tags. Vaul reads the
  standard `meta[property=csp-nonce]` element. Remove the patch when upstream
  provides equivalent nonce support; the existing browser CSP test covers it.
  The same patch removes `role="button"` from a non-interactive FileUpload
  dropzone. With `interactive=false`, only the actual file-picker button is an
  action; announcing its enclosing container as another button duplicates the
  accessible control. Remove this hunk when upstream fixes the role condition.
  Color-mode's `disableTransition` is false because its temporary transition
  override style does not support a nonce.
- API calls use the injected `$api` client, which adds the bearer token and
  disables automatic retries. Never put credentials in URLs or persistent
  frontend state. Language/theme preferences use cookies; attachment downloads
  additionally use an HttpOnly proof cookie scoped to one resource path. The
  proof cannot authorize API requests.
  The session plugin runs before Nuxt's router so the activation fragment is
  removed before Vue Router captures the initial URL. Browser tests check both
  the address bar and router history before any user navigation, then reload.
- Keep table inputs stable. Inline empty arrays combined with controlled
  TanStack pagination can cause recursive updates. A displayed item and any
  ensuing action must retain their account identity while requests change.
- Data views use the DashboardPanel body and footer slots with a single set of
  cell insets; nested empty states use the library's naked variant. Form actions
  use the Modal footer and the native HTML form attribute so long forms scroll
  without hiding their actions. Semantic status shades must pass contrast checks
  on both plain and tinted surfaces.
- Connection status comes from a bounded heartbeat. Once the service has exited,
  replace the workspace with the blocking exit overlay and prevent further
  operations; do not leave an apparently connected page or a fixed status label.

## Browser tests

From the repository root, with Playwright and Chromium already installed:

```sh
go test -c -o .local/gui.test ./internal/gui
PLAYWRIGHT_MODULE=/path/to/playwright/index.js \
PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chromium \
node scripts/test-gui-workflows.mjs .local/gui.test .local/gui-screenshots
```

The helper exists only in the Go test binary. It starts a private account store,
mock AnyShare HTTP service and mock mount driver, while exercising the real GUI
handlers, transfer service, static assets and browser. It does not contact PKU
or introduce a fixture switch into the distributed executable.

`scripts/test-gui-browser.mjs` separately tests local login, CSRF, replay,
single-instance activation and shutdown against the production binary.
`scripts/test-gui-hmr.mjs BINARY [SCREENSHOTS]` starts Nuxt and an isolated Go GUI,
changes a temporary component attribute, and verifies HMR updates without a
document reload or session replacement. It restores the source and stops its
processes afterward. Avoid running it alongside `nuxt generate` or type checks,
which share Nuxt's generated directory.
`scripts/test-gui-daemon.mjs` tests detached Linux startup, private logs,
repeat activation, XDG state logs, `gui stop`, disconnect/reconnect feedback, service termination, restart
and startup failure cleanup against the same production binary. CI runs all
five browser test scripts, including `scripts/test-gui-ux.mjs` for empty, loading,
failed-request recovery and narrow-screen interactions. Desktop and Android scripts share Playwright loading,
bounded polling and browser screenshots through `scripts/lib/gui-test.mjs`.

## Dependency audit

The lockfile pins the latest stable Nuxt/UI versions checked for this change.
TanStack Table is pinned to 8.21.3 to match Nuxt UI's supported API, rather than
mixing it with the incompatible Table 9 interface.

Tooling overrides update `simple-git` to 4.0.2, `@simple-git/argv-parser` to 2.0.1
and esbuild to 0.28.2 to address their published security advisories without
downgrading Nuxt. Production builds and type checks validate these overrides.

At implementation time, npm still reports transitive high-severity advisories
rooted in `braces` 3.0.3 (GHSA-vfj7-8cjw-p6xm) and `node-forge` 1.4.0
(GHSA-86w9-cpqp-85rv). These remain the registry's latest releases. They are
used by build/development tooling (glob processing and development TLS), not
the embedded browser application or Go HTTP server. Do not use `npm audit fix
--force`, which currently proposes downgrading Nuxt/i18n to older major versions.
Revisit the overrides and these advisories as upstream publishes fixes.
