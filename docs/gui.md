# Desktop GUI

Run `pkudisk gui` to start a tray and open the local browser interface.
The Nuxt / Nuxt UI interface supports Chinese and English, light and dark
themes, and the same netdisk operations as the CLI/TUI. Choose the language
and theme in the toolbar or Settings. Saved accounts are shared with the CLI.

Files default to folders first, then names in natural order. **Sort** supports
name, modification time and size in either direction, and **Folders first** can
be switched off independently. These preferences are retained across visits.
Material Icon Theme distinguishes file formats; eligible images and media show
the official service's small thumbnail when available, falling back to the file
icon while unavailable. PDFs and Office documents retain their format icons.

Upload and download dialogs show the selected files and destination first.
Overwrite, instant-upload, timeout and desktop local-transfer alternatives are
under **Advanced**. Mobile downloads use a short confirmation and the system's
Downloads folder.

| Interface | Operations |
| --- | --- |
| Accounts | Campus login, CAPTCHA/OTP/SMS verification, account switching, default selection, login replacement and removal |
| Files | Libraries and quota/access information, folder browsing, sorting/filtering, bounded server filename search, mkdir, rename, move, delete to recycle bin |
| Transfers | File/directory upload and download, instant-upload checks, ZIP download, download-and-extract, server copy and extraction, progress, cancellation, operation timeout |
| Sharing | Internal navigation links, user/department search, grants, direct/inherited ACL inspection, direct grant revocation, public link creation/listing/revocation, expiry, permissions, pickup codes, visit limits and phone verification |
| Recycle bin and history | Restore with optional conflict renaming, permanent removal, clearing the reviewed snapshot, file history and version restoration |
| Mounts | Single account/root or all saved accounts, read-only mode, cache and read policy tuning, session refresh, diagnostics, graceful unmount and retry after busy errors |
| Recovery | Offline inspection of retained write journals, copying the local recovery path and uploading retained contents to a selected destination |

The HTML, CSS, JavaScript and tray icon are embedded in the Go executable.
No Node, web server installation, SQLite or C compiler is needed to run it.
Only mounting needs a filesystem driver. Release builds use `CGO_ENABLED=0` for Linux, macOS and
Windows on amd64 and arm64. The tray dependency is `gogpu/systray v0.3.0`;
its macOS backend uses `goffi` to call system frameworks.

## Starting and stopping

| Invocation | Behavior |
| --- | --- |
| No arguments in a terminal, script or SSH session | Show CLI help |
| An existing CLI command, `--help`, or completion | Run that command without starting GUI services |
| `tui` | Open the existing terminal file browser |
| `gui` | Start the tray/browser, or open a new authenticated page in the running instance |
| `gui stop` | Stop the running GUI after active operations finish |
| Windows Explorer double-click, without arguments | Start GUI |
| macOS app or Linux desktop entry below | Explicitly start GUI |

Missing terminal handles alone never select GUI. On Linux and macOS, a terminal
`gui` invocation starts the resident service in the background, prints its PID and
private log path, and returns. Use `gui --foreground` while developing or when
Ctrl-C controlled lifetime is useful. Subsequent invocations open the existing
instance and return. Desktop launchers keep their native application lifecycle.
Closing a browser tab does not stop the service. Use the tray's **Exit**, the
page's **退出桌面服务**, `pkudisk gui stop`, or Ctrl-C in `--foreground`
mode. Startup output includes the stop command. `gui stop` waits for shutdown
and refuses to interrupt active login, transfer or mount work; it also succeeds
when no GUI is running. An instance started with an older binary that lacks the
stop endpoint must be exited from the tray/page once before restarting.

Background logs use mode 0600, outside the configuration directory:

- Linux: `$XDG_STATE_HOME/pkudisk/logs/desktop.log`, defaulting to
  `~/.local/state/pkudisk/logs/desktop.log`. Relative XDG paths are ignored.
- macOS: `~/Library/Logs/pkudisk/desktop.log`.

Existing log files are left in place. Reopening a running macOS app is handled
through its LaunchServices reopen event.

The page checks the local service every two seconds with a 2.5-second timeout,
and checks immediately when a request fails or the page regains focus. Healthy
connections have no status indicator. Service loss or an explicit exit ends the
window's session and shows a non-dismissible, full-screen message: “The service
has exited. Please close this window.” The workspace and business dialogs are
closed, credentials are cleared, and further API requests and toasts are blocked.
Open a fresh interface through the app, tray or `gui` to start another session.

On mobile, the exit action is **Quit** and closes the app. Unexpected service
loss shows **服务未运行，请退出应用** / **The service is not running. Please exit
the app.** Android intentionally releases an idle background service and resumes
it when the user returns; this is distinct from an unexpected failure. Native
task notifications and background lifetime are documented in the
[Android guide](../mobile/android/README.md).

The page, tray and `gui stop` refuse to quit while login, transfer or mount work is active.
Cancel/finish operations and unmount first. OS signals request cancellation
and driver shutdown using the existing CLI implementation; retained writes
remain recoverable. Job history is in memory and lasts until the service exits.

Linux requires a graphical session and `xdg-open`. A StatusNotifier host is
needed for the tray (for example KDE, or GNOME with an AppIndicator extension).
If it is absent, the browser interface still works and provides the Exit
button. If neither a browser nor a tray can be opened, startup fails and
releases the listener and instance lock. If browser launch fails while a tray
is available, fix the default browser configuration and retry **Open**.

Browser file and folder selection uses Nuxt UI's FileUpload and
`browser-fs-access` with standard file input fallback. The GUI does not launch
native file-picker processes. Mount points, cache directories and desktop
transfer paths use a Nuxt UI picker backed by the authenticated local filesystem
API. It offers standard user folders, directory creation and unused Windows
drive letters for mounting. The tray menu uses **Open** and **Exit** everywhere.

Keep the browser tab open for browser uploads. Uppy handles upload concurrency,
progress and cancellation. HTML file selections contain files and relative paths,
so empty directories are not included. Downloads use standard streamed HTTP
attachments, including ZIP archives for folders, without JavaScript Blob buffering.
Desktop users can also choose a local source or destination visually and let the
shared Go transfer queue finish in the background. This queue handles individual
files, directory trees, ZIP downloads and streamed extraction with the same
progress reporting as other surfaces. Browser and service records with the same
client attempt ID appear as a single task.

The status and accounts APIs return a `flags` object, also available from
`GET /api/flags`. `isMobile` explicitly identifies Android/iOS hosts;
`canMount` and `canBrowseLocalFS` are false on those hosts. Mobile navigation
hides desktop mounting and recovery, and its server rejects local filesystem
browsing and path transfers. UI platform behavior uses `isMobile` directly.

Go receives multipart uploads using a bounded 8 MiB memory buffer and temporary
files for larger bodies, then reuses the shared upload service. Two requests can
stage concurrently, and temporary files are removed on completion or failure.
No campus credentials or signed object-storage URLs reach the browser.

Destructive operations require an explicit confirmation. Emptying a recycle
view submits the IDs reviewed by the user, so newly arriving entries are not
silently included. Sharing approval remains pending until the server grants
it; a successful grant with a failed link creation is reported separately.
Server-side copy/extraction can continue after local cancellation, as with
the CLI. Mount diagnostics go to the launching process's stderr.

Windows deliberately retains a console-subsystem executable. It preserves
normal CLI pipes, exit status, TUI input and Ctrl-C behavior. When launched
from Explorer into GUI, it detaches its own console; a brief console flash
can occur before Go starts. It never hides the invoking shell's window.

## Desktop launchers

Extract the release archive before creating a launcher. It includes the
`desktop/` helper files. From a source checkout, build first with `make build`.
The helper creates files in an output directory; it does not install anything
or replace existing launchers. It accepts an optional final `linux` or `darwin`
argument for packaging an already cross-built executable.

Linux:

```sh
sh desktop/package.sh ./bin/pkudisk ./bin/desktop
mkdir -p ~/.local/share/applications
cp ./bin/desktop/pkudisk.desktop ~/.local/share/applications/
```

Use `./pkudisk` as the binary path when working in an extracted release.
The generated desktop entry contains the binary and logo's absolute paths, including
correct escaping for spaces and reserved characters. Regenerate the entry
if the binary moves.

macOS:

```sh
sh desktop/package.sh ./bin/pkudisk ./bin/desktop
open './bin/desktop/PKUDisk.app'
```

The app bundles a copy of the same executable and a shell launcher that passes
`gui`. It can be moved to Applications. Its internal executable also supports
CLI commands, for example:

```sh
'/Applications/PKUDisk.app/Contents/MacOS/pkudisk' version
```

The app includes the supplied PKU logo as its ICNS application icon. Windows
executables embed the corresponding ICO through architecture-specific Go
resource objects, and the tray/browser use PNG variants of the same artwork.

This helper produces an unsigned app; release signing/notarization and launch
at login are not included. Windows requires no launcher: double-click
the extracted executable, or create a shortcut passing `gui`.

## Local authentication

The server binds only to an OS-assigned `127.0.0.1` port. Each browser activation
creates a random 256-bit ticket, valid for 60 seconds and atomically redeemable
once. The ticket is in a URL fragment (`/#ticket=…`), which browsers do not send
in HTTP requests. JavaScript immediately removes it from the address bar and
exchanges it through a same-origin JSON POST for an independent session token.

Standard attachment downloads are prepared through the authenticated API. The
response names a random resource URL, `/download/{id}`, and sets a separate
HttpOnly, SameSite Strict proof cookie scoped to that exact path for 60 seconds.
The URL alone does not authorize access. The server stores only the proof hash
and permits at most two GETs: embedded WebView may first inspect the attachment
headers before its platform download manager retrieves the stream. The second
retrieval consumes the authorization and clears the cookie. Desktop browsers
can retrieve the attachment directly. No download authority is placed in a URL,
JSON response, JavaScript bridge or Authorization header.

The page's API session lasts at most 12 hours and is stored in `sessionStorage`,
so reloads in the same tab work. API requests explicitly send it in the
Authorization header; attachment cookies never authorize API access. New tabs
should be opened through the tray or `gui`; expired sessions require a fresh
activation. Restarting the service invalidates tickets, sessions and attachment
proofs. Download responses use `Referrer-Policy: no-referrer`.

Language, theme and size-format cookies contain preferences only. Saved account
passwords, server access tokens and refresh tokens stay in Go and the private
account store; they are never returned in browser responses. Interactive verification attempts are bound to the browser
session that started them and expire after five minutes.

The server checks the exact Host and port, rejects foreign/null Origins and
cross-site Fetch Metadata, and requires a matching Origin for mutations. It
does not enable cross-origin CORS. CSP forbids external scripts and embedding;
authentication responses are not cached. Per-response nonces authorize Nuxt's
inline bootstrap and the component library's dynamic styles without enabling
unsafe scripts. Credentials are not logged, printed
to the terminal, or included in query strings.

The GUI is single-instance per user configuration directory. Discovery state
is below the existing configuration directory in `pkudisk/gui/`, using
0700 directories / 0600 files on Unix and restricted ACLs on Windows. An OS
process lock determines ownership; a stale file or PID cannot confer ownership.
A separate control secret authenticates subsequent `gui` activations and is
never sent to browser JavaScript. CLI/TUI processes remain independent.

This protects against cross-site web requests and other ordinary local users.
It is not an isolation boundary against malicious code already running as the
same OS user or a browser extension with access to the page.

## Validation

Automated Go tests cover ticket replay and expiration, concurrent redemption,
Host/Origin/Fetch Metadata checks, form and CORS requests, separation of browser
and control credentials, simultaneous launches, graceful exit, process-lock
contention, crash recovery, and CLI help/completion without GUI initialization.
Business integration tests run the real AnyShare and transfer clients against
the shared `internal/anyshare/mockserver` HTTP fixture, covering account isolation, consent checks, sharing,
file management, transfers and archives, verification ownership, queue timeout
semantics, guarded exit and retryable unmount errors.
When `dbus-daemon` is available, Linux also exercises the actual tray backend
against a private test bus and a StatusNotifier host fixture, including icon
registration, menu activation and shutdown. CI installs this test dependency.
The normal CI matrix tests all three OSes and cross-builds all six targets. Additional tests exercise
browser multipart uploads, spooled-file cleanup, attachment proof expiry and
replay limits, cookie/API authority separation, streamed ZIPs, and authenticated
command-line shutdown. CLI, GUI and Android E2E tests reuse the same simulated
AnyShare service; it is never included in application binaries.

An optional Linux browser smoke test uses a built binary, Chromium and an
existing Playwright installation. It replaces only `xdg-open` with a URL
capture helper inside a temporary private directory and then drives the real
HTTP server and embedded interface:

```sh
PLAYWRIGHT_MODULE=/path/to/playwright/index.js \
PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chromium \
node scripts/test-gui-browser.mjs ./bin/pkudisk ./.local/gui-screenshots
```

It checks login, fragment removal, reload, replay rejection, cross-origin
requests, repeated activation, browser closure and shutdown. Playwright is
only a development test dependency; it is not needed to build or run the app.

The fuller workflow test drives a separate Go test executable with mock
accounts and a mock driver. See [frontend development](../web/README.md) for
its command, asset regeneration, dependency auditing and the Nuxt UI CSP patch.

Before declaring native desktop support verified, perform these checks on each
OS (compilation and headless tests do not replace them):

- Start from the terminal and the desktop; confirm the tray and browser open.
- Close the browser and reopen from the tray and a second launch; only one
  resident GUI process/listener should remain.
- Reload the authenticated page; try an old one-use link in a fresh tab.
- Exit from the page, tray and Ctrl-C; ensure the tray, listener and discovery
  metadata disappear, and restart successfully.
- On Windows, also check redirected CLI output, pipelines, exit codes and TUI
  from PowerShell and cmd; only desktop GUI launch may detach the console.
- On macOS, reopen an already-running `.app` and exercise the menu and exit on
  both Intel and Apple Silicon. On Linux, test a StatusNotifier desktop and a
  desktop without a tray host.
