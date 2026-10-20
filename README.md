# PKUDisk

`pkudisk` browses, uploads, downloads and mounts Peking University NetDisk (AnyShare) on Linux, macOS and Windows. Linux/macOS mounts use FUSE; Windows mounts use WinFsp. The CLI and TUI work without a filesystem driver. It is written in Go and uses the same HTTPS API and object-storage flow as the official AnyShare 7.0.6 client. See [platform support](docs/platform-support.md) for driver requirements, paths and validation.

`pkudisk gui` opens a system tray and a Chinese/English browser interface in the same binary. Manage accounts, browse/search files, upload/download, mount drives, share files, manage public links, use the recycle bin and file history, and inspect retained writes. Only mounting needs a filesystem driver. See [desktop GUI](docs/gui.md) for launchers, browser file transfers and the security model.

On Linux and macOS, `gui` detaches by default and prints `pkudisk gui stop` as the shutdown command; use `gui --foreground` for a terminal-attached session.

An optional [Android app](mobile/android/README.md) hosts the same GUI in an in-app
WebView using a shared gomobile library. Mobile dependencies and build tools are
isolated under `mobile/`; CLI builds need no Android or gomobile tooling.
The root `go.work` connects both modules for development. Session authentication,
mount orchestration, AnyShare operations and transfers live in shared internal
packages; see [architecture](docs/architecture.md).

The login uses **PKU unified authentication (IAAA)**. The password is RSA encrypted with the public key served by the login page. Saved accounts are stored in a private file below the user configuration directory (0600 on Unix; a restricted ACL on Windows); the password and renewable AnyShare session are used to resume a login without prompting. NetDisk local accounts are intentionally not supported.

## Install

Download a binary archive for your platform from [GitHub Releases](https://github.com/unofficialpku/disk/releases/latest), verify it against the accompanying `checksums.txt`, and extract the executable to a directory on your `PATH`. `amd64` is Intel/AMD x86-64; `arm64` includes Apple Silicon and Windows on ARM. Linux/macOS archives use `.tar.gz`; Windows archives use `.zip`. These binaries do not require Go.

With Go 1.26.9 or newer, install the command with:

```sh
go install github.com/unofficialpku/disk/cmd/pkudisk@latest
```

Use a version such as `@v0.6.0` instead of `@latest` to select a release. Add `GOBIN` (normally `~/go/bin`, or `%USERPROFILE%\go\bin` on Windows) to your `PATH` if needed.

```sh
pkudisk version
pkudisk account add
pkudisk ls
pkudisk tui
```

Only `mount` needs a filesystem driver: FUSE on Linux, macFUSE on macOS or WinFsp on Windows. See [platform support](docs/platform-support.md) for installation and validation details. Archives also contain shell completion scripts in `completions/`.

## Build

Go 1.26.9 or newer is required and must be available as `go` on `PATH`.

```sh
make build
make test
make check
```

The Nuxt GUI is checked in as static embedded assets, so Go builds and
`go install` do not require Node. After changing the frontend in `web/`, run
`make ui` with Node 24, then `make build`. `make ui-check` checks frontend types,
lint and the embedded asset fingerprint. See [frontend development](web/README.md).

The binary is `bin/pkudisk`. No real account credentials or private keys are
checked in; automated tests use explicit local fixtures.

The workspace contains two modules, so run Go tests in each:

```sh
go test -race ./...
(cd mobile && go test -race ./...)
```

`GOWORK=off go build -o bin/pkudisk ./cmd/pkudisk` checks the standalone CLI module.
Installing a published CLI version also resolves only the root module; Android
and gomobile tools remain declared in `mobile/go.mod`.

Without Make, build directly with `go build -trimpath -o pkudisk ./cmd/pkudisk`; on Windows use `-o pkudisk.exe`. Cross-builds for Linux/macOS/Windows amd64 and arm64 support `CGO_ENABLED=0`. Native Windows mounts require WinFsp to be installed separately.

For maintainers, [releasing](docs/releasing.md) describes the tagged CI workflow and local packaging rehearsal.

## Built-in help

The CLI uses Cobra; text listings use Lip Gloss tables to align Chinese names,
emoji and numeric columns by terminal display width. Every command includes its purpose, argument meanings, defaults, behavior, and examples in its help. Flags can appear before or after arguments. Use `--` before a file name beginning with a dash.

```sh
bin/pkudisk --help
bin/pkudisk help recover
bin/pkudisk help account add
bin/pkudisk download --help
```

Help and shell completion work without a saved account or network connection. Completion scripts are available for Bash, Zsh, Fish, and PowerShell:

```sh
source <(bin/pkudisk completion bash)
bin/pkudisk completion zsh > ~/.zsh/completions/_pkudisk
```

Sizes use readable IEC units by default (for example, `12 GiB`). Use
`-b/--bytes` for exact integer byte counts in listings, quota, recovery records
and transfer output. JSON always keeps numeric byte values. Mount diagnostics
also honor `-b`; the TUI applies it to remote entries and transfer progress.

Common short flags are `-a/--account`, `-j/--json`, `-f/--overwrite`,
`-v/--verbose`, `-t/--io-timeout` and `-u/--username`, wherever the corresponding
long flag is available. `-h/--help` remains help on every command.

Commands are top-level and help groups them by purpose. In v0.4.0, `quota`,
`search` and `copy` replace `util quota/search/copy`; the `util` group and its
old forms are removed. `share` manages internal access, and `link` manages
external links. `delete` and `recycle` manage the server recycle bin;
`revision` manages retained file versions. Commands and listing verbs have no
aliases.

`--account/-a` selects one saved account by its exact name. Use
`mount --all-accounts` or `tui --all-accounts` to aggregate saved accounts;
the wildcard `*` has no special meaning.

```sh
bin/pkudisk ls -a work
bin/pkudisk ls -b -a work
bin/pkudisk quota -b -a work
bin/pkudisk download '/My library/report.pdf' ./report.pdf -f -t 30m
bin/pkudisk delete '/My library/old-report.pdf'
bin/pkudisk recycle ls
bin/pkudisk revision ls '/My library/report.pdf'
bin/pkudisk upload ./report.pdf '/My library/report.pdf' --fast
```

Exit codes are `0` for success, `1` for an execution failure, and `2` for invalid commands, arguments, or flags. Results and requested help go to stdout; account announcements, errors, and CLI transfer progress go to stderr. `--json` provides JSON arrays for `ls`, `account ls`, and `recover`, including `[]` for empty results.

## Authentication

An interactive mount prompts for the campus username and hides the password input. On Linux/macOS:

```sh
mkdir -m 700 ~/mnt/pkudisk
bin/pkudisk mount ~/mnt/pkudisk
```

On Windows, after installing WinFsp, use an unused drive letter: `pkudisk.exe mount P:`. `pkudisk account add` and `pkudisk tui` work on each platform without a mount driver.

For a non-interactive command, use an owner-only password file. The file must be a regular file owned by the current user with mode `0600`:

```sh
umask 077
printf '%s\n' 'YOUR-PASSWORD' > ~/.config/pkudisk/password
bin/pkudisk mount --username YOUR-CAMPUS-ID \
  --password-file ~/.config/pkudisk/password ~/mnt/pkudisk
```

`--password-stdin` is also available. A JSON credential file can be supplied with `--credentials-file`; it must be an owner-only `0600` file. Run `account add --help` for its format. The first successful login is saved under the campus username and becomes the default account. Use `--no-save` for a one-off login and `--non-interactive` to fail instead of prompting when credentials or required verification are missing.

Use `account` to add a login or manage saved accounts:

```sh
bin/pkudisk account ls
bin/pkudisk account add --username YOUR-CAMPUS-ID --password-file ~/.config/pkudisk/password work
bin/pkudisk account default work
bin/pkudisk account remove work
```

`account` without a subcommand shows help. Account labels and transfer destinations use positional arguments. Commands have no compatibility aliases.

`mount`, `ls`, `tui`, `upload`, `download`, and `extract` reuse the default account. Pass `--account NAME` to choose another saved account. If the saved session has expired, the stored password is used to log in again and the renewed session is saved.

`mount`, `ls`, `upload`, `download`, and `extract` print the selected account and server on standard error before restoring the session or logging in. JSON results remain on standard output. `account` and `tui` omit this announcement.

If IAAA requires a CAPTCHA or OTP/SMS verification, the driver prints a prompt. CAPTCHA images are written to a temporary file for the user to inspect and are removed after the answer is entered.

HTTP clients use Go's default TLS and HTTP protocol negotiation. Transport failures are returned immediately without application-level retries. Refresh-token rotation is serialized across processes using the same account store. Transport errors identify the server and endpoint without including query parameters or credentials.

## Choosing the mounted root

By default the mount root contains every accessible document library. Use `ls` without a path (or `ls /`) to inspect the libraries, and pass a library/folder path to `ls` for its contents:

```sh
bin/pkudisk ls --username YOUR-CAMPUS-ID --password-file ~/.config/pkudisk/password
bin/pkudisk ls --username YOUR-CAMPUS-ID --password-file ~/.config/pkudisk/password '/My library/Reports'
bin/pkudisk ls '/My library/Reports'
```

Library listings show human-readable `USED` and `RW`/`RO` access. Writable
libraries include quota and usage percentage; read-only libraries show only
used space. Usage or permission failures are marked unavailable/unknown,
with details on stderr, so other libraries remain visible. Inside a library,
regular files show readable sizes (`-b` selects bytes), and folders display `-`. `ls --json` keeps
the original entry schema, including the remote directory size sentinel `-1`.

`tui` opens a Bubble Tea terminal browser showing the selected account and remote path. The remote list scrolls, paginates, and supports fuzzy filtering with `/`. Use `j`/`k` or the arrows to move, Enter to open a folder, Backspace to go up, `u` to pick a local file or directory to upload, `d` to download the selected file or folder, `a` to save a folder as a server ZIP, `v` to download a folder through one ZIP and extract locally, `x` to extract an existing remote archive, `r` to refresh, and `?` for keyboard help. In the upload picker, Enter opens a directory, Space selects a file or directory, and `p` lets you type a local path. `~` is expanded in local path inputs.

```sh
bin/pkudisk tui
bin/pkudisk tui --all-accounts
```

With `--all-accounts`, the TUI root contains saved account directories. Entering
one shows its libraries and updates the header to that account/server; `g`
returns to the account root. Uploads, downloads, ZIP operations, extraction and
sharing use the owning account's session. Virtual account directories cannot
be uploaded into, downloaded or shared; open a library first. Extraction paths
include the account name (`/work/My library/Folder`), and extraction cannot
target another account. A raw `gns://` destination uses the current account.
Local download defaults and overwrite prompts remain the same.

All saved accounts must authenticate and list their libraries before the TUI
opens. The default account and saved account set are unchanged. This mode uses
the same directory-name checks as the mount and is mutually exclusive with
`--account`, `--server`, explicit credentials and `--no-save`. Session errors
identify the account. Default TUI behavior remains a single account.

Directory reads and transfers run asynchronously so the screen remains responsive. The TUI adapts to terminal resizing and shows transfer progress and speed. Esc cancels an active transfer or dialog; `q` quits, cancelling an active operation first. Existing files require an explicit overwrite choice. Bubble Tea manages terminal input and restoration on exit.

Upload and download without mounting the filesystem. Remote paths are library-relative (`/Library/folder/file`) or an allowed `gns://...` ID:

```sh
bin/pkudisk upload ./report.pdf '/My library/Reports/report.pdf'
bin/pkudisk download --overwrite '/My library/Reports/report.pdf' ./report.pdf
bin/pkudisk upload ./Photos '/My library/'
bin/pkudisk download '/My library/Photos' ~/Downloads/
bin/pkudisk download '/My library/Photos' ./photos.zip --archive
bin/pkudisk download '/My library/Photos' ./photos --via-archive
bin/pkudisk extract '/My library/photos.zip' '/My library/Photos-new'
bin/pkudisk quota
bin/pkudisk search report --in '/My library/Reports'
bin/pkudisk copy '/My library/report.pdf' '/My library/Archive'
bin/pkudisk delete '/My library/old-report.pdf'
bin/pkudisk recycle ls
bin/pkudisk revision ls '/My library/report.pdf'
bin/pkudisk upload ./report.pdf '/My library/report.pdf' --fast
```

Directories transfer recursively, preserving nested and empty folders. A remote folder destination uses the local basename; a local directory destination uses the remote basename. A new destination path can rename the result, and its parent must already exist. Omitting a download destination saves the remote name in the current directory, including for a `gns://` selector. Uploads reject symlinks and special files.

Uploads refuse to replace an existing remote file unless `--overwrite` is supplied. Downloads use an atomic temporary file and refuse to replace an existing local file unless `--overwrite` is supplied. Interactive terminals show a maintained progress bar with percentage, transferred/total bytes, speed, and ETA. The bar reaches 100% after an upload is published or a download is saved, and marks failed transfers. Progress is disabled when redirected; `--io-timeout` bounds the entire transfer, including a directory tree or archive preparation. Ctrl-C cancels CLI transfers; cancelled downloads preserve an existing destination and remove the temporary file.

For directories, `--overwrite` merges trees and replaces matching files while keeping unrelated entries. Each file installs or publishes individually; cancellation can leave a partial tree. Total byte progress covers the tree, and completion reports file and directory counts.

`download --archive` asks the server to package a folder as a ZIP, keeping its top-level name. `--via-archive` downloads the same ZIP, validates its paths and file types, and extracts it into a local folder. This reduces per-file requests for many small files; the observed server ZIP uses stored entries, so it does not reduce payload bytes. These modes need local space for the ZIP, and extraction also needs space for the output. Server archive limits apply; use direct downloads to avoid the archive service's limits. Neither mode silently falls back after a server error.

`extract` runs the server's archive extractor without downloading the archive. It creates a new remote folder and waits for the server job. The server can drop empty directories and sometimes produces duplicate nested folders; duplicate output paths are detected and reported. Directory uploads therefore use the reliable direct path. Extraction errors can leave partial remote output. Cancelling extraction stops waiting, and the server job may continue. See the [archive protocol findings](docs/anyshare-protocol.md#directory-transfers-and-archives).

Uploads negotiate the server's minimum and maximum part sizes and maximum part count, then grow the part size as needed. Large files use the desktop client's multipart protocol to avoid the single-request upload limit. Small and empty files use a single request. `upload --fast` can reuse server-side content for new files after a bounded-memory hash pass; a non-match uses the ordinary uploader and existing files always retain revision checks. Downloads stream 512 MiB ranges with fresh signed URLs and verify the selected revision and exact size. Server quotas, administrator policies, and storage-service limits still apply; increase `--io-timeout` for long transfers. See [the size-limit investigation](docs/transfer-limits.md) for the observed limits and verification.

## Remote deletion and history

`delete PATH` moves a remote file or recursive directory into AnyShare's recycle
bin. `recycle ls` shows the recycle IDs and original paths; pass one of those
IDs to `recycle restore`, or use `--rename` when the original name is occupied.
`recycle remove` and `recycle empty` permanently delete retained objects and
require `--yes` with `--non-interactive`. `revision ls FILE` lists versions and
`revision restore FILE REV` restores one after confirmation. Server retention
and library policy determine which versions are available.

The `--root` option accepts either the displayed path or an AnyShare `gns://...` object ID. A folder can therefore be mounted directly:

```sh
bin/pkudisk mount \
  --root '/My library/Reports' \
  --username YOUR-CAMPUS-ID \
  --password-file ~/.config/pkudisk/password \
  ~/mnt/pkudisk
```

The selected root is checked against the libraries returned for the authenticated account. A `gns://` ID from another library is rejected.

## Server utilities and mounted free space

`quota` sums the accessible document libraries. Writable libraries contribute
their allocation and usage; read-only libraries contribute their used bytes to
both totals. Available is the sum of writable libraries' remaining capacity. `search` performs a bounded server-side
filename search, and `copy` duplicates a file or folder within NetDisk
without downloading its bytes through this program. These operations use the
selected account and never accept arbitrary URL or BitTorrent sources.

FUSE `statfs`/Windows drive free-space queries use the same library totals.
`--root` counts only its containing library and checks write permission at the
selected folder. `--read-only` reports zero available capacity and counts each
library as used/used. The local cache volume is not remote capacity. Quota queries
are shared and cached for five seconds, then invalidated after mount mutations.
A quota or permission failure returns an error instead of stale writable space.

## Internal sharing and external links

```sh
# An internal navigation link requires login and existing file permissions.
bin/pkudisk share link '/My library/report.pdf'

# Find the recipient's AnyShare ID, then grant access explicitly.
bin/pkudisk share search 'Recipient name'
bin/pkudisk share add '/My library/report.pdf' \
  --user USER-ID --permission preview,download --expires-in 168h
bin/pkudisk share ls '/My library/report.pdf'
bin/pkudisk share remove '/My library/report.pdf' --user USER-ID

# External links work without an AnyShare account.
bin/pkudisk link add '/My library/report.pdf' \
  --code Abcd1234 --expires-in 168h
bin/pkudisk link ls '/My library/report.pdf'
bin/pkudisk link remove LINK-ID
```

`share add/remove` accepts repeated or comma-separated `--user` and
`--department` IDs. Recipient names and campus login numbers are not IDs; the
directory search shows names, accounts, departments and explicit IDs. Internal
links are reused and grant no permission on their own. Removal affects only
selected direct rules, preserving unrelated recipients and inherited access.
No API to delete an internal navigation link was found; remove its access rules
instead. The official revoke API replaces the full ACL snapshot and has no
conditional update, so avoid concurrent permission edits from another client.

External links support `--permission`, `--expires-in`, `--code`, `--title`,
`--limit` for visits and `--verify-mobile`. Omitted optional settings use server
defaults. `--expires-in 0` requests no expiry; `--limit -1` requests unlimited
visits, subject to policy. Upload links require a folder and
`--permission create,modify`. Text/JSON results include the URL and effective
settings; `link ls` fetches all pages and shows pickup codes. Pending server
approvals are reported explicitly, not as active links or permissions. None of
these operations sends email or recipient notifications.

In a terminal, missing paths are prompted; `share add` without recipient flags
searches and offers an explicit selection, and `share remove` offers current
direct recipients. A server-required missing pickup code is prompted too.
Commands with all required input execute directly. Use `--non-interactive` for
scripts; non-terminal invocations always fail on missing input. There is no
separate `--interactive` flag. `-a/--account`, `-t/--io-timeout` and `-j/--json`
apply where shown in help. Account announcements go to stderr.

In the TUI, select a file/folder and press `s`. The sharing menu provides
internal links, recipient search/grants, direct/inherited permission inspection
and revocation, external link creation/listing and revocation. Mutations show a
review screen before applying; Esc cancels. Link results have a scrollable URL
and `c` requests clipboard copying through the terminal's OSC52 support.

## Long-lived mounts

The driver renews idle sessions every hour and retains rotated refresh cookies in memory. `--refresh-interval` changes this interval; `0` disables scheduled renewal. If an API call returns HTTP 401, the driver also renews the session and retries that request once. Concurrent requests share one serialized refresh.

Temporary network failures retry after a minute. When the server rejects the refresh token, the driver unmounts and exits with an error. A supervisor can then restart it using a private credential file. Accounts requiring CAPTCHA or second-factor verification may need interactive login again.

A user service with `Restart=on-failure` can keep a mount alive across the current user session; it does not put credentials in this repository. The service used for the local read-only mount is outside the repository under `~/.config/systemd/user/`.

The optional live test uses a separate session and reads remote data only:

```sh
PKUDISK_TEST_CREDENTIALS="$HOME/.config/pkudisk/credentials.json" \
  go test -run '^TestLiveSessionRefresh$' -v ./internal/auth
```

For live CLI transfer and utility acceptance with a saved account:

```sh
PKUDISK_TEST_LIVE=1 go test -count=1 -timeout=7m \
  -run '^TestLiveTransfers$' -v ./cmd/pkudisk
```

`PKUDISK_TEST_ACCOUNT` selects a saved account. `PKUDISK_TEST_BINARY` optionally
selects the executable to test; otherwise the test invokes the CLI entry point.
It creates and removes a small temporary remote tree, compares transfer/copy
hashes, and requires already indexed file and folder search matches. Indexing
new uploads is asynchronous, so existing metadata supplies the matched-search
checks. The regular CI suite exercises these response shapes without credentials
and tests single-Enter prompt submission in a private Windows console.

Sharing has its own opt-in acceptance with two saved accounts on the same
server:

```sh
PKUDISK_TEST_LIVE_SHARING=1 go test -count=1 -timeout=4m \
  -run '^TestLiveSharing$' -v ./cmd/pkudisk
```

It shares only a new personal-library fixture with the second saved account,
verifies that recipient can download, revokes access, tests external file/folder
links and removes the fixture. It sends no notifications. The regular suite
also covers populated ACLs, preservation of unrelated/inherited rules,
approval responses, link pagination, scripts, help and TUI review/cancellation.

## Mount multiple accounts

```sh
mkdir -p ~/PKUDisk
bin/pkudisk mount ~/PKUDisk --all-accounts
# Windows: pkudisk.exe mount P: --all-accounts
```

The first level contains each saved account name, followed by that account's
accessible libraries. For example, `~/PKUDisk/work/My library/Reports`.
Account directories are virtual and read-only; files inside use the owning
account's permissions and session. Each account has separate inode and cache
identities even when several accounts can see the same shared library.
Reads and writes share the mount's existing cache budget and read concurrency.

All saved accounts must authenticate and list their libraries before the mount
starts. Account names must be usable directory names on the current platform;
Windows also rejects names that collide ignoring case. Each session renews
independently, and an expired session ends the mount with the account named in
the error. The account set stays fixed until the next mount; the saved default
is unchanged.

`--all-accounts` is available for `mount` and `tui`, and cannot be combined with
`--account`, `--server`, explicit credentials, or `--no-save`. Mounts also
exclude `--root` in this mode.
Cross-account moves return `EXDEV` before changing any remote file. Use a copy
followed by deletion when needed. Capacity counts a shared library on the same
server once; its free space counts if any mounted account can write there.
Read-only libraries contribute only used space, and `--read-only` reports zero
available space. Recovery records for this mode include the owning account and
server alongside the original remote IDs; select that account when reuploading.

## File behavior

Mount reads fetch files up to 16 MiB whole on first access, if the file fits in the configured disk budget. A 5 MiB photo uses one authorization call and one data GET, including when a viewer starts at its tail. Larger files start with 4 MiB ranges. Consecutive reads double the fetch size up to 16 MiB; seeking resets it to the base range. A read spanning adjacent blocks is batched into one range when the batch fits. Already cached or in-flight neighbours stop the batch, avoiding duplicate transfers.

Requested bytes return as soon as their part of the shared stream arrives. A 64 KiB header read does not wait for the rest of a 16 MiB batch. The fetch continues filling the bounded range, and subsequent reads reuse its received prefix or wait only for their missing bytes. Reads reaching the range end wait for final byte-count/EOF validation. Only complete, verified ranges enter the disk cache; interrupted ranges are discarded. Full decoding or a seek near the end still needs the bytes leading up to that position in the selected range.

Fetches are limited to four at a time per mount. Overlapping requests share one fetch, and queued reads allocate no fetch buffer before admission. Cancelling one waiter leaves a useful shared transfer running; an unused cold transfer is cancelled when its last waiter leaves. Unmount cancels and joins active fills before removing the cache. Metadata queries and warm reads proceed independently of network I/O and its queue. Repeated reads and reopening the same revision reuse the private disk cache. Memory-mapped reads are supported on Linux, macOS and Windows.

Tune the policy with the same mount flags on all platforms:

| Flag | Default | Effect |
| --- | --- | --- |
| `--read-whole-file` | `16MiB` | Fetch files up to this size whole; `0` disables this threshold. |
| `--read-block-size` | `4MiB` | Base range for large files and random seeks. |
| `--read-ahead` | `16MiB` | Maximum fetch size for sequential reads and contiguous demand; `0` disables growth and batching beyond the base range. |
| `--read-concurrency` | `4` | Maximum simultaneous remote fetches; valid range is 1–64. |

Size flags accept bytes, decimal units such as `5MB`, and binary units such as `8MiB` or `2GiB`. Each read size is capped at 512 MiB; a read-ahead value below the base range leaves the base range unchanged. The disk budget can further reduce ranges and batches. Fetch buffers use memory as well as disk: increasing the threshold, batch size or concurrency increases peak memory use. The OS page cache uses additional memory.

For fewer round trips on a fast connection, or smaller fetches on a slower connection:

```sh
pkudisk mount ~/PKUDisk --read-whole-file 32MiB --read-ahead 64MiB
pkudisk mount ~/PKUDisk --read-whole-file 0 --read-block-size 1MiB --read-ahead 0 --read-concurrency 2
```

On Windows use an unused drive letter, such as `P:`, as the mountpoint. `mount --help` explains the policy and includes these examples without requiring an account or a network connection.

Windows directory handles load contents only when the application enumerates them. Attribute-only opens reuse known paths without listing each ancestor. Fresh listing metadata is reused until `--cache-ttl` expires; concurrent directory listings and metadata refreshes share a request. Metadata HTTP requests release the file lock, so cached reads remain available during a refresh. An open directory handle keeps one enumeration snapshot, while namespace changes invalidate listings for subsequent handles.

For a slow or frozen application, capture mount decisions and requests in a log stored in a local folder:

```powershell
.\pkudisk.exe mount P: --verbose 2> mount.log
```

For Windows PowerShell 5, redirect through `cmd.exe` to preserve native UTF-8 text without PowerShell's error-record wrapping and UTF-16 conversion:

```powershell
cmd.exe /d /c ".\pkudisk.exe mount P: --verbose 2>mount.log"
```

On Linux/macOS use an empty directory as the mountpoint. `--verbose` logs to stderr with timestamps and numbered `START`/`END` operations. Child operations identify their parent; an unmatched `START` is still pending. A fetch can remain pending after its header read returns while its suffix continues filling; the log marks this as `requested bytes ready`. The log shows file paths/handles, metadata refreshes, file/cache lock waits, whole-file versus sequential/range planning, cache hits/misses, shared fetches, queue admission, eviction, retention, uploads and unmount. HTTP entries show endpoint hosts, trusted byte ranges, connection reuse/setup, response status, first body bytes and completion timing. Waiting for response headers is distinguished from waiting for the body.

Logs include file names/IDs and local paths, but omit credentials, cookies, request/response bodies, URL queries and signed object paths. Diagnostics are disabled by default. `--debug` can additionally enable the filesystem driver's raw protocol messages. These logs report requests reaching this process; operations stalled inside Photos or the Windows driver before a callback cannot appear in the application log.

Reads and writes share one per-mount `--cache-limit` budget. Clean data is evicted in order of use to make room; pending writes and recovery journals are never evicted. A complete file held in one cached range, including a whole-file fetch or batch, or a previously flushed write file, can become writable without another download or allocation. Flushed write contents remain available for subsequent reads until eviction or unmount. Reads still work when the budget is occupied by pending writes, but may not retain new ranges. Clean cache data is removed on unmount.

New files, overwrites, random writes, truncation, append, mkdir, unlink, rmdir, rename, and same-library moves are supported. Existing-file writes are compared with the remote revision before upload; a concurrent edit returns `EBUSY` and leaves the local write cache for recovery.

Writes are held in a private `0700` cache and published on `fsync`/close. A failed upload is never acknowledged as successful. The journal and cached data remain available under `--cache-dir`; inspect them with:

```sh
bin/pkudisk recover --cache-dir ~/.cache/pkudisk
```

`recover` lists retained local contents and their recorded remote names/IDs, sizes, revisions, and update times. It runs offline and does not upload, delete, or automatically restore anything. Stop the affected mount, copy needed cached files to a permanent location, then use `upload` with a chosen destination. `recover --help` includes a complete example; `recover --json` returns `[]` when no records exist.

Set `--read-only` when a mount must not issue any mutating API request. The default shared disk-cache limit is 10 GiB; `--cache-limit 0` disables it. This limit does not cap the size of files that can be read. `--part-size` sets a preferred upload chunk size (at least 5 MiB), which is adjusted to the server's limits and the file size.

Ctrl-C unmounts on all platforms. Linux also supports `fusermount3 -u MOUNTPOINT` (or `fusermount -u MOUNTPOINT`); macOS supports `umount MOUNTPOINT`.

## License

Licensed under [Apache-2.0](LICENSE). The license is the verbatim [official Apache text](https://www.apache.org/licenses/LICENSE-2.0.txt); project attribution is in [NOTICE](NOTICE). Binary archives include third-party licenses in `licenses/`.

## Implementation notes

The installed desktop client was inspected from `/opt/pku-netdisk/current/resources/app.asar`. The relevant API calls are documented in [docs/anyshare-protocol.md](docs/anyshare-protocol.md). Cobra handles command parsing and help; Bubble Tea, Bubbles, and Lip Gloss provide the terminal UI; progressbar renders CLI transfer progress. Both interfaces use the same transfer service for destination selection, streaming, revision checks, and atomic downloads. Authentication, remote object-storage URLs, path selectors, and recovery metadata are validated before use; signed object-storage URLs are never included in errors or logs.
