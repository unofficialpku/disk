# Architecture and development workspace

The root module supplies the headless core and CLI, TUI and desktop browser
adapters. `mobile/` is a separate Go module containing the gomobile tools and
runtime boundary. Root `go.work` connects both modules for development; the
mobile module's local `replace` also permits independent module commands.
Published CLI installs and `GOWORK=off` builds use only root dependencies.

| Package | Responsibility |
| --- | --- |
| `internal/session` | Account selection, restored sessions, refresh locks, state persistence and login fallback; callers supply input callbacks |
| `internal/accounts`, `internal/auth` | Private account storage and IAAA/AnyShare authentication protocol |
| `internal/anyshare` | Remote HTTP protocol and shared deletion, recycle snapshot, revision and library statistics operations |
| `internal/mount` | Shared mount defaults and validation, account backend assembly, driver support and mount lifetime |
| `internal/vfs` | Scoped account routing, cache/recovery, common errors and node attributes, FUSE and WinFsp adapters |
| `internal/transfer` | Shared file/tree/archive transfer operations and progress |
| `internal/gui` | Authenticated loopback HTTP, task queue, local filesystem browsing, embedded assets, background desktop launch and development proxy |
| `internal/tui`, `internal/display` | Terminal I/O, reusable asynchronous operations, forms/confirmation, and consistent table/JSON rendering |
| `internal/desktop` | Desktop tray integration; the only native GUI in desktop Go code |
| `cmd/pkudisk` | Cobra commands and terminal input/output adapters |
| `web/` | Nuxt UI browser surface, Chinese/English messages and visual local path picker |
| `mobile/bridge`, `mobile/android` | In-process runtime lifetime and the Android WebView host |

Account namespaces carry the owning account in `account://` IDs. The VFS account
backend unwraps these IDs before remote calls and scopes returned entries, so
file transfers, sharing, recycle operations and history retain account identity
without surface-specific routing. Extraction across accounts is rejected.

Go does not open desktop file dialogs. The authenticated local filesystem API
supplies folders and available Windows drive letters to a Nuxt UI picker.
Desktop transfer paths enter the same Go task queue as other surfaces. Browser
uploads use standard file input/multipart data, and mobile attachments flow
through WebView's standard download callback. Status and account responses expose
host `flags`; Android and iOS report `isMobile: true`, `canMount: false` and
`canBrowseLocalFS: false`. Platform-specific presentation uses `isMobile`,
independently of filesystem access.

The Android runtime uses a gomobile AAR inside the app process. A future iOS
host can bind the same package into an XCFramework and use WKWebView/WKDownload.
The shared mobile bridge exposes task snapshots and atomic idle suspension;
native shells own lifecycle and notification policy. Android shows native task
progress while work is active, releases an idle background service and resumes
only intentional idle suspension on return. Idle visible use has no persistent
notification. Neither approach launches a packaged executable. iOS linking and runtime tests
still require macOS/Xcode; the current Linux environment does not establish iOS
runtime support.

Run each module's tests explicitly; `./...` stops at nested module boundaries:

```sh
go test -race ./...
(cd mobile && go test -race ./...)
GOWORK=off go build -o bin/pkudisk ./cmd/pkudisk
npm --prefix web run lint
npm --prefix web run typecheck
make ui
node web/scripts/source-hash.mjs --check
```

`internal/anyshare/mockserver` supplies shared protocol fixtures to CLI and GUI
unit tests and browser/Android E2E runs. `scripts/lib/anyshare-fixture.mjs` starts
the Go fixture process for JavaScript tests. Test endpoints, fake accounts and
mock mount drivers are confined to explicit test tools, never application
builds. See [frontend development](../web/README.md) for HMR and browser tests,
and [Android](../mobile/android/README.md) for shared toolchain installation and
emulator verification.
