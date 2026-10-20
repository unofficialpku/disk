# AnyShare protocol notes

These notes describe the protocol boundary implemented by this repository. They are based on the official Peking University NetDisk client 7.0.6-3-281, including read-only inspection of its Debian package unpacked under `/tmp`, and live requests made against the service. They contain no account data or bearer tokens.

## Session

The web client starts at `/anyshare/oauth2/login?lang=zh-cn&redirect=/anyshare/`. The server redirects to `/oauth2/signin` and sets the OAuth challenge and state cookies. The third-party link is `/web/v1/sso?ty=auth`; it posts the official IAAA form to `https://iaaa.pku.edu.cn/iaaa/oauth.jsp`. IAAA account passwords are RSA PKCS#1 v1.5 encrypted with the public key from `/iaaa/getPublicKey.do`, then sent to `/iaaa/oauthlogin.do`. The returned one-time token is sent back to `/oauth2/signin` with the challenge and the `_rand` parameter used by the web client. The resulting `client.oauth2_token` and `client.oauth2_refresh_token` cookies are kept in a private in-memory jar during a session. The command-line account store may persist these two renewable cookies and the login password in an owner-only `0600` file so a selected account can resume without prompting.

The refresh operation is a GET to `/anyshare/oauth2/login/refreshToken?isforced=true`. Scheduled renewal runs hourly, including when the filesystem is idle. A request that receives HTTP 401 also refreshes once and retries with the new token. All renewals use the same cookie jar and are serialized so parallel FUSE operations cannot rotate the same session concurrently. A rejected refresh token causes the mount to exit for supervised login; temporary network failures retain the mount and retry.

## Namespace

`GET /api/efast/v1/entry-doc-lib` returns document libraries. The driver exposes them as the synthetic mount root unless `--root` selects one library or descendant folder. Directory contents use the paginated endpoint:

```text
GET /api/efast/v1/folders/{url-escaped-gns-id}/sub_objects
    ?limit=1000&sort=name&direction=asc[&marker=...]
```

The legacy `POST /api/efast/v1/dir/list` endpoint is useful for diagnostics but is not used by the FUSE namespace. Every selected path is resolved from an allowed library; the driver never concatenates an unchecked user path into an API URL.

The `ls` command uses the same root and paginated folder endpoints. With no selector it lists accessible libraries; with a display path or allowed `gns://...` selector it lists that folder. The terminal UI keeps the resolved parent ID in memory while browsing, so upload and download actions use the same checked namespace.

## Files and mutations

The driver uses these JSON calls:

| Operation | Endpoint |
| --- | --- |
| metadata | `POST /api/efast/v1/file/attribute` or `dir/attribute` |
| permission check | `POST /api/eacp/v1/perm1/checkall` |
| create directory | `POST /api/efast/v1/dir/create` |
| delete | `POST /api/efast/v1/file/delete` or `dir/delete` |
| rename | `POST /api/efast/v1/file/rename` or `dir/rename` |
| move | `POST /api/efast/v1/file/move` or `dir/move` |
| recycle listing | `POST /api/efast/v1/recycle/list` |
| recycle restore/permanent delete | `POST /api/efast/v1/recycle/restore` / `recycle/delete` |
| file history/restore | `POST /api/efast/v1/file/revisions` / `file/restorerevision` |

Downloads begin with `POST /api/efast/v1/file/osdownload`. The returned signed request is restricted to HTTPS on an HTTPS server and is used with a checked `Range` header for `ReadAt`.

The standalone `download` command uses the same `osdownload` request with bounded 512 MiB byte ranges and verifies the exact selected revision size before atomically installing a local file. Each range obtains a fresh signed URL, requests the same revision, and checks `Content-Range` and byte count. No whole-file download size cap was found in this path. The official bundle also includes size errors for preview, conversion, and batch download routes; those do not establish a general object-storage download cap.

For uploads, `POST /api/efast/v1/file/osoption` obtains `partminsize`, `partmaxsize`, and `partmaxnum`. The preferred part size is clamped to the server's size bounds and raised to at least `ceil(length / partmaxnum)`. A file beyond `partmaxsize * partmaxnum` fails before a remote upload is created. Files larger than the negotiated part size use `osinitmultiupload`, one signed `osuploadpart` request per part, `oscompleteupload`, and the signed completion request returned by that endpoint. Small and empty files use `osbeginupload`. Both flows publish at `osendupload`. A file overwrite includes its current `rev`; the driver obtains a fresh revision before upload and reports a conflict if it changed.

PKU's `osbeginupload` rejects lengths beyond 5 GiB with HTTP 400/code `400000000`; multipart avoids that endpoint limit. Administrator file-size policies can still return `403002070` and `detail.file_limit_size` from either upload flow or download authorization. Such errors retain the byte limit in their message and map to `EFBIG` in a mount. See [the investigation](transfer-limits.md) for server-reported capacity and validation.

These measures only remove client/request-size limits. Account quota, administrator file policies, forbidden types, storage-service limits, and server-side object-storage limits remain authoritative and are reported as errors.

## Server utilities and quota

Per-library capacity uses the authenticated endpoint:

```text
GET /api/efast/v1/quota/doc-lib/{url-escaped-full-gns-library-id}
POST /api/eacp/v1/perm1/checkall
{"docid":"gns://LIBRARY-OR-SELECTED-FOLDER"}
```

Each library returns `allocated` and `used`. Permissions come from the named
`allow` list: `create` or `modify` makes the selected location writable.
`allowvalue` uses a different representation and is not used for this check.
Writable libraries contribute `max(allocated,used)` and `used`; read-only
libraries contribute `used` to both totals. Available is the sum of
`max(allocated-used,0)` over writable libraries. This includes shared custom
libraries independently of the personal account quota.

FUSE and WinFsp report these totals. A selected `--root` counts its containing
library, checking permissions at the selected folder; `--read-only` clamps every
library to used/used. No capacity is inferred from the cache volume. Concurrent
queries share a request and successful results are cached for five seconds.
Mutations invalidate the cache and detach older requests so they cannot repopulate
it. Failed quota/permission queries are reported instead of retaining stale free
space. Fixtures cover writable custom/user libraries alongside a read-only
public library; live acceptance verified public and personal library responses.

With `mount --all-accounts`, virtual account directories route requests to
separate authenticated clients. Internal IDs include the account name and
normalized server to isolate inodes, metadata and cached contents. Shared
libraries are deduplicated by normalized server and original library ID for
capacity reporting, using writable access from any participating account.
Cross-account rename is rejected before flushing or temporarily renaming a
source. Recovery journals record the account/server and original remote IDs.
`tui --all-accounts` reuses this scoped listing namespace. Transfers and sharing
resolve the owning account locally and use that client's original IDs; returned
entry metadata is scoped again to preserve UI selection across shared libraries.
Virtual account nodes cannot start file operations. Extraction strips the
selected account's path prefix and rejects another account's destination.

The top-level `quota`, `search`, `copy`, `share` and `link` commands use these
server APIs:

```text
POST /api/ecosearch/v1/file-search
{"quick_search":true,"dimension":["basename"],"keyword":"...",
 "rows":25,"range":[],"start":0,"type":"doc"}
```

`search` resolves an optional folder scope first, then requests one bounded
filename page. It validates returned IDs, names and sizes before printing them. The deployed
search index returns `created_at`/`modified_at` as integer Unix microseconds;
RFC3339 strings are also accepted, and null/missing times remain absent. Folder
results use `size:-1` and an empty extension. The filename is basename+extension,
and `parent_path` is a gns:// parent ID. `dimension:["basename"]` searches files
and folders; adding `"folder"` or `"file"` restricts the type rather than widening
the query. Empty output uses a JSON array. Newly uploaded files may remain absent
until the asynchronous index catches up.

```text
POST /api/efast/v1/file/copy
{"docid":"SOURCE-ID","destparent":"DESTINATION-ID","ondup":1}
```

For directories the path is `dir/copy` and includes
`check_upload_process:true`. A directory copy can return a server job ID; the
client polls:

```text
POST /api/efast/v1/dir/copyprogress
{"id":"JOB-ID"}
```

A directory initiation returns both the new `docid` and a distinct job `id`.
The destination exists before its contents finish copying, so the client polls
even when `docid` is present. Progress uses `sizecopied/size` (or copied entry
counts for empty trees), and success requires `status:"finished"` with `success`.
The final entry uses the destination docid, not the job ID. Same-folder copies
use `ondup:2` and obtain their server-generated name from metadata.

`copy` waits for the server to finish and never moves file bytes through
the local machine. Existing-name behavior remains the server's policy. Server
permissions, approval requirements and storage-service restrictions can reject
copy even when ordinary upload/download is allowed.

Recycle-bin, version-history and instant-upload APIs are exposed by the CLI and
TUI. Recycle entries use the IDs returned by `/recycle/list`, which are separate
from the active object's ID. `recycle restore` uses `ondup:1` by default and
`ondup:2` for `--rename`; `recycle remove` calls the permanent delete endpoint.
The server may delay list updates and its offset-based recycle listing has no
stable cursor; the client deduplicates repeated rows and stops when a page makes
no progress. It may retain only the current revision when version history is
disabled for a library. `upload --fast` probes the first
204800 bytes and length, then hashes the whole file with bounded memory and
sends the official `md5`, eight-digit hexadecimal `crc32`, `client_mtime` in
Unix milliseconds, destination and duplicate policy to `dupload`. A mismatch
falls back to the ordinary revision-checked uploader; existing files never use
the shortcut. No offline URL, BitTorrent or magnet task API was found in the
official client.

## Internal sharing and anonymous links

The official client inspected for sharing is the Linux 7.0.6-3-281 package,
unpacked under `/tmp` for read-only analysis without installing or running it.
Internal links are authenticated navigation, independent from ACL grants:

```text
GET  /api/shared-link/v1/document/{file|folder}/{escaped-full-gns-id}?type=realname
POST /api/shared-link/v1/document/realname
{"item":{"id":"gns://LIBRARY/OBJECT","type":"file"}}
```

An existing link is reused; creation returns `id`. URLs use the configured
server base followed by `/anyshare/zh-cn/link/ID` with item/type routing fields
and no authentication token or pickup code added. PKU's root `/link/ID` route
serves its landing page instead of the netdisk. The
bundle exposes no internal-link deletion route. Permission listing/removal is
separate from link creation.

```text
POST /api/eacp/v1/department/search
{"key":"NAME","start":0,"limit":50}

GET /api/doc-share/v1/sharing-configuration-scope/user
POST /api/doc-share/v1/doc-perm
{"doc_id":"gns://LIBRARY/OBJECT","configs":[{
 "allow":["display","preview","download"],"deny":[],
 "accessor":{"id":"USER-ID","type":"user"},
 "expires_at":"2026-12-01T00:00:00Z"}]}
```

Recipient search returns `userinfos` (`userid/name/account/deppath`) and
`depinfos` (`depid/name/path`). Explicit `userid/depid` identifies recipients;
campus login numbers need not match AnyShare directory accounts. `user` and
`department` are accessor type strings. `validity_period` and `perm` from the
policy define defaults and allowed settings. Epoch zero represents no expiry.
HTTP 202 means approval pending and must not be presented as active access.

```text
POST /api/eacp/v1/perm2/get {"docid":"gns://LIBRARY/OBJECT"}
POST /api/eacp/v1/perm2/set
{"docid":"gns://LIBRARY/OBJECT","inherit":true,"perminfos":[...]}
```

ACL entries identify `accessorid/accessortype`, named `allow/deny`, `endtime`
(Unix microseconds; `-1` is permanent), `inheritdocid/inheritpath`, and optional
read policies. Removal retains every unrelated/inherited entry as raw JSON and
the original inheritance flag. The official protocol replaces the snapshot;
no server revision, ETag or conditional update was found. Avoid concurrent
ACL edits by other clients. HTTP 200 with `result:1` also means pending approval;
`result:0` means effective. Selected recipients without direct rules cause no
mutation. Removing direct denies can still leave inherited permissions.

External links use these independent APIs:

```text
GET /api/doc-share/v1/sharing-configuration-scope/anyone
POST /api/shared-link/v1/document/anonymous
{"item":{"id":"gns://LIBRARY/OBJECT","type":"file",
         "allow":["display","preview","download"]},
 "title":"report.pdf","expires_at":"2026-12-01T00:00:00Z",
 "password":"PICKUP-CODE","verify_mobile":false,"limited_times":-1}

GET /api/shared-link/v1/document/{file|folder}/{escaped-full-gns-id}?type=anonymous&offset=0&limit=20
DELETE /api/shared-link/v1/document/anonymous/LINK-ID
```

Listings return arrays containing `id/title/item.allow/expires_at/password/
limited_times/verify_mobile`. The CLI fetches all pages. Epoch-zero expiry and
`limited_times:-1` mean unlimited. Optional settings default to policy;
`must_set_password`, allowed permissions, validity/visit maxima and forced phone
verification are enforced before creation. Folder upload links use
`display/create/modify`; file upload permissions are rejected. The result is a
purpose-built share URL, not an object-storage signed URL.

The client never invokes `/doc-center/v2/share-doc-message` or sends mail.
Acceptance on two saved PKU accounts created only temporary personal-library
files, verified recipient downloads and subsequent loss of access, retained an
unrelated direct rule, and created/revoked pickup-code file and folder links.
CLI and TUI reuse these APIs. Anonymous link metadata can briefly be cached by
the service after deletion; the client uses the official revoke operation and
does not add network retries or delay workarounds.

## Directory transfers and archives

Direct directory uploads/downloads walk the selected tree and preserve nested and empty folders. Local symlinks and special files are rejected on upload. Downloads validate every path before creating local files, reject conflicting names on the destination platform, and use `os.Root` to confine tree writes/renames. Each file uses the existing revision-aware upload or checked atomic download path. `--overwrite` merges trees, keeping unrelated entries. Completion is per file; interrupted tree transfers can leave partial output.

The official client's folder download path repeats this request once per second until `progress` reaches 100:

```text
POST /api/open-doc/v1/file-download
{"name":"folder.zip","doc":[{"id":"LAST-COMPONENT-OF-GNS-ID"}]}
```

The response includes `items` with completion/error status and a `package_address`. The client rejects failed items instead of installing a partial ZIP. The package download uses an account `token` query parameter. This token is sent only to the configured server origin; the server's explicit default port (`:443`) is equivalent to the implicit HTTPS port. Redirects are refused, and package URLs/tokens are omitted from errors and diagnostics. Package bytes stream to a local temporary file. A ZIP download installs that file atomically; a folder download checks the ZIP root, traversal, duplicates, symlinks/special files and platform filenames before extracting through the confined local tree writer. No local path is created from an unchecked member.

The live server ZIP preserved Chinese names, zero-length files, nested folders and empty directories. Observed entries used ZIP STORE, so packaging reduces authorization/request overhead rather than bandwidth. `download --archive` saves the ZIP; `download --via-archive` extracts it locally. Default folder downloads use direct file transfers and do not depend on the archive service. Archive failures are explicit; callers can choose the direct route rather than receiving a silent fallback.

Remote extraction follows the same polling pattern:

```text
POST /api/open-doc/v1/file-unarchiver
{"doc_id":"LAST-COMPONENT-OF-ARCHIVE-ID","name":"new-folder",
 "target_path":"gns://LIBRARY/PARENT","ondup":1}
```

The response reports `progress`, `status` (`extracting` / `complete`), and `unarchive_path`. The client requires a completed job and an output directory directly under the chosen parent. `extract` then lists the output tree, rejecting duplicate local paths. Extraction creates a new folder and never requests overwrite. A live flat ZIP round trip passed. However, the deployed server omitted empty directories and sometimes created multiple nested folders with the same name. That makes server extraction unsuitable for reliable directory uploads, so uploads always send the tree directly. We do not try to merge ambiguous server output. Formats and size policies remain server-controlled; ZIP is verified, and other formats are not claimed as tested.

Neither API exposed a job cancellation endpoint in the official bundle. Cancelling or timing out stops polling; a remote extraction can continue and errors can leave partial output. Error codes `400008002` (official download message: 2 GB) and `400008003` (extraction maximum) describe archive service limits, not direct file limits; the deployed archive thresholds were not measured. `500008012` reports partial extraction, and `403043013` reports different source/destination storage services. These errors are described without leaking response bodies.

Live acceptance compared file SHA-256 across recursive upload/download, server ZIP download, local extraction, remote extraction and overwrite merging; unrelated local files and all direct-transfer empty directories survived. Temporary remote fixtures were deleted. The official client/package was only unpacked under `/tmp` and inspected read-only, never installed or executed.

## File thumbnails

The official AnyShare 7.0.6 desktop bundle uses this read-only request for file
icons (list size `24*24`, medium `96*96`, large `228*120`):

```text
GET /api/open-doc/v1/file-thumbnail?id=LAST-COMPONENT-OF-GNS-ID&type=96*96&redirect=false
Authorization: Bearer ACCOUNT-TOKEN
```

It returns `{"url":"SIGNED-OBJECT-STORAGE-URL"}`. The official browser client
also supplies a token query parameter, but the deployed API accepts the bearer
header alone. HTTP 503 with code `503008001` means the thumbnail is being
generated. The official icon support list includes images, video, Photoshop,
Illustrator and CAD files; office documents and PDFs keep ordinary file icons.

A read-only live check on 2026-10-14 confirmed a JPEG thumbnail: API HTTP 200,
an HTTPS signed URL on a different storage origin, then 5,758 bytes of
`image/jpeg`. No original file was uploaded or modified for this check. The
implemented core method was checked against the same deployed request.

The GUI fetches `/api/thumbnail?account=...&path=...` with its normal bearer
session and receives only raster bytes. It never accepts or exposes a remote
URL. Upstream storage requests carry no account bearer or cookies, refuse
redirects and HTTPS downgrades, and limit thumbnail bodies to 2 MiB. Detected
JPEG/PNG/GIF/WebP content is accepted; HTML and SVG are rejected. Requests have
a 15-second deadline and follow caller cancellation. Private responses use
`Cache-Control: no-store`; thumbnail-generation delays return
`ERR_THUMBNAIL_PENDING` so the UI can retain its file icon without busy polling.

## Recovery boundary

The local cache is created below `--cache-dir` with mode `0700`. A dirty inode has an adjacent JSON journal containing only the remote ID, parent ID, revision, cache basename, and update time. It does not contain credentials or signed URLs. A failed upload returns an error to the caller and leaves the cache and journal in place for `recover` inspection.
