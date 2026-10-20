# PKUDisk for Android

This optional app hosts the existing Nuxt GUI in **Android System WebView**.
A Service calls the [shared Go bridge](../README.md) through an AAR
produced by `gomobile bind`. Go runs inside the app process as a JNI library;
there is no packaged executable or child process. The UI stays inside the app
and does not open or depend on the Chrome application. The launcher name is
**PKUDisk**, and the Android application ID is `su.zzi.pkudiskfuse`.

WebView's standard `onShowFileChooser` delegates `<input type="file">` to the
system document picker through AndroidX Activity Result. Standard HTTP
`Content-Disposition: attachment` responses reach a `DownloadListener`, which
starts Android DownloadManager. `CookieManager` supplies a short-lived proof
cookie scoped to that attachment URL. The resource ID alone cannot download a
file, and neither a JavaScript message bridge nor the page's API bearer token is
needed. The shared Go layer exposes HTTP, not platform GUI operations.
[AndroidX URLUtilCompat](https://developer.android.com/reference/androidx/webkit/URLUtilCompat)
parses RFC 6266 `filename*` parameters so Chinese attachment names also work on
the oldest supported Android versions; the shell has no custom header parser.

## Isolation

- Android-specific code lives here. The shared bridge and separate `go.mod` /
  `go.sum` live one level up in `mobile/`; their local `replace` reuses the root
  Go packages. Android and future iOS hosts bind the same library.
- Root `go build`, `go test ./...`, CLI dependencies and desktop release builds
  do not recursively include mobile packages or require Java/Android tools.
  Root `go.work` links both modules for editing and tests; `GOWORK=off` verifies
  the standalone CLI dependency graph. Gomobile tools remain in `mobile/go.mod`.
- Gradle and generated AARs/APKs are ignored. SDKs, NDKs, JDKs, Gradle
  distributions and emulator images belong in user-wide locations outside the repo.
- The checked-in Gradle Wrapper pins and verifies Gradle 8.13; AGP is 8.13.2,
  compile/target SDK 36, minimum SDK 29 (Android 10), NDK 28.2.13676358. Go follows the root
  module's Go version. Java 17 or 21 works.

## Shared toolchain

Install Android Studio or the official [command-line tools][sdkmanager]. On Linux
the SDK convention is `~/Android/Sdk`; on macOS it is `~/Library/Android/sdk`.
Set `ANDROID_HOME` to that directory and add `cmdline-tools/latest/bin`,
`platform-tools` and `emulator` to your user PATH. Put JDKs in a user-wide location
such as `~/.local/share/jdks` (Linux), and set `JAVA_HOME` to the selected JDK.
`local.properties` may instead set `sdk.dir`; it must remain untracked.

Command-line Tools 23 includes the new [Android CLI][android-cli]. Install shared
packages with:

```sh
for package in platform-tools platforms/android-36 build-tools/36.0.0 \
  ndk/28.2.13676358 emulator system-images/android-36/google_apis/x86_64; do
  android --no-metrics sdk install "$package"
done
```

For a local HTTP proxy, set `HTTPS_PROXY=http://localhost:10808` and
`HTTP_PROXY=http://localhost:10808` when running Android CLI. Older command-line
tools use `sdkmanager --licenses` and accept semicolon package IDs (for example
`'ndk;28.2.13676358'`), with `--proxy=http --proxy_host=localhost --proxy_port=10808`.
Version 23's sdkmanager compatibility wrapper ignores those older proxy flags.

Gradle uses JVM proxy properties; put these in **user**
`~/.gradle/gradle.properties` when needed, not in the repository:

```properties
systemProp.http.proxyHost=localhost
systemProp.http.proxyPort=10808
systemProp.https.proxyHost=localhost
systemProp.https.proxyPort=10808
systemProp.http.nonProxyHosts=localhost|127.*|[::1]
```

## Build and use

The root repository already includes the embedded UI. Only run `make ui` at the
root when changing it. From this directory:

```sh
./gradlew :app:assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

Published APKs use the `release` build type, which disables debugging. Signing
keys stay outside the repository. Supply `PKUDISK_ANDROID_KEYSTORE`,
`PKUDISK_ANDROID_STORE_PASSWORD`, `PKUDISK_ANDROID_KEY_ALIAS`, and
`PKUDISK_ANDROID_KEY_PASSWORD`, then run `./gradlew :app:assembleRelease`.
Without those variables, Gradle produces an unsigned APK for downstream signing.
Keep the signing certificate and private key backed up for future releases.
A stable application ID and signing identity allow in-place updates that retain
accounts and app-private data.

The default APK includes arm64 phones and x86_64 emulators. Use
`./gradlew :app:assembleDebug -Pabis=x86_64` to build just the emulator ABI.
Build on Linux/macOS or WSL. Gradle invokes the pinned gomobile/gobind tools from
`../go.mod`, using the shared NDK to build Android JNI libraries. No globally
installed gomobile version is required. Windows users can run the current shell
build script in WSL.

Published APKs include project, Go/gomobile, frontend and Android dependency
license texts and notices under `assets/licenses/`. The build generates them
with `../scripts/licenses.sh` from the same checkout and resolved toolchain;
the intermediate directory is `../build/licenses/`. Android attribution comes
from Gradle's resolved release runtime dependencies and upstream Maven metadata.
Publish just `pkudisk_<version>_android.apk` for Android, with its hash in the
release's single shared `checksums.txt`. No separate license archive, Android
checksum file, symbols or metadata asset is needed.

Launching the app opens the Chinese/English GUI in its WebView. Idle use has no
persistent notification. Active tasks show native progress notifications;
multiple tasks appear in a group with a summary. Grant notification permission
to see them in the notification drawer. The shell promotes its Service to
`dataSync` foreground operation only while tasks, incoming requests or login
operations are active.

Android's standard SplashScreen covers cold launch. The same branded loading
surface covers service reconnection and WebView startup; it fades out after
Nuxt authentication, the target route and a rendered frame are ready. Normal
startup never shows Open/Stop buttons. Retry/Exit actions appear only after a
startup failure. The transition respects the system animation setting.

The current page, account, directory and query options are encoded in the URL.
The shell retains only an allowlisted relative path/query, never the activation
fragment or bearer token, and restores it against the new local origin after
idle suspension or Activity/process recreation. A read-only DOM readiness marker
coordinates the reveal; the web page has no native JavaScript interface.

After the app goes into the background with no work, it releases the Go service
after a one-second idle grace period. Returning opens the service and restores
the last route automatically. If work remains active, returning preserves the current
service and page. The standard system file picker keeps the service available
while a selection is pending. Completed background work also triggers idle
release; the app does not keep an empty foreground service running.

The GUI's **Quit** action gracefully stops the service, closes the app and removes
it from Recents. A graceful stop is refused while work is active. An unexpected
service exit or Android timeout instead leaves the blocking message
**服务未运行，请退出应用** / **The service is not running. Please exit the app.**
It does not silently restart the failed service. Use Android Back to close the
window, then launch the app again. Native recovery controls are reserved for
startup failures or WebView renderer crashes. Force-stopping the app ends its
Go runtime and transfers with the host process. Mount navigation, driver
settings and mount-related prompts are hidden on mobile.

Credentials and renewable sessions use the existing private account store in
`files/service/accounts.json`; they are not encrypted with Android Keystore by
this app. Backup is disabled. Temporary transfer data uses the app cache. Activation links stay in memory between the bridge
and the in-app WebView; they are not sent to other applications or
application logs. The Service is not exported. WebView navigation and attachment
downloads are restricted to the exact loopback origin. Cleartext is permitted only
for `127.0.0.1`; the Go server keeps the same origin checks, one-use activation
and bearer sessions as desktop. WebView debugging is enabled only in debug APKs.

Android 15+ limits `dataSync` foreground operation in the background to six hours
in a 24-hour period. The shell implements `onTimeout` and closes the shared Go
bridge; idle suspension is a separate lifecycle state. Bringing the app to the
foreground resets the system's budget. Background transfers remain subject to
Android limits. Filesystem mounting and a desktop tray are unavailable on Android.
There is no boot receiver or automatic restart after failure; only an intentional
idle suspension resumes automatically when the user returns. DownloadManager
requires a network Android considers validated. The attachment proof expires after one
minute and authorizes at most two GETs: WebView inspects the response, then
DownloadManager receives the stream. Retry if a download was queued while
offline; the app does not provide persistent offline scheduling or resumable
authorization. Proof cookies are HttpOnly, SameSite Strict and scoped to one
attachment path; they cannot authorize ordinary API calls.

## Verification

```sh
(cd .. && go test -race ./... && go vet ./...)
./gradlew :app:assembleDebug :app:lintDebug
printf 'no\n' | avdmanager create avd --name pkudisk-api36 \
  --package 'system-images;android-36;google_apis;x86_64' --device pixel_7
emulator -avd pkudisk-api36 -port 5580 -no-snapshot -no-window \
  -no-audio -gpu swangle -feature -Vulkan
# If this network requires a proxy, add -http-proxy http://localhost:10808 above.
# Wait for Android to finish booting; use only a disposable emulator.
node test/e2e.mjs emulator-5580
```

Emulator rendering is host-dependent. On the Fedora test host,
Emulator 37.2.12's SwiftShader GLES renderer crashed and the host GL renderer
became unstable. `-gpu swangle -feature -Vulkan` uses ANGLE and avoids those
renderers. This is an emulator setting, not an APK requirement.

This network's DNS returns unusable addresses for Google's default HTTPS
connectivity probe. For this disposable AVD we used a real HTTPS 204 probe at
Cloudflare, retaining Android's connectivity validation and normal TLS checks:

```sh
adb -s emulator-5580 shell settings put global captive_portal_https_url https://cp.cloudflare.com/generate_204
adb -s emulator-5580 reboot
```

Avoid a device-wide HTTP proxy for this test: Android DownloadManager can route
loopback requests through it despite proxy exclusions. The emulator's
`-http-proxy http://localhost:10808` flag handles external traffic without changing
the app's loopback requests. These are test-network settings, not app code.

The E2E script uses the root web project's Playwright installation (`npm ci` in
`web/`), or `PLAYWRIGHT_MODULE` can point at another installation. It installs the
real debug APK, clears only the app's emulator data, restores a fake account via a
host-only Go AnyShare fixture shared with CLI and GUI tests
(`internal/anyshare/mockserver`), and checks in-process JNI execution, in-app WebView
activation with Chrome disabled, real system-picker upload, DownloadManager
download, idle notification removal, background idle release and foreground
resume, grouped task progress during background transfers, release after work
completes, Quit removing the app from Recents, force-stop cleanup and the
blocking overlay after Android's foreground-service timeout. It rejects real
device serials. Artifacts go to
ignored `build/e2e/`. Test accounts and endpoints are never compiled into the APK.
The Go tests separately exercise real IAAA RSA/OTP login, persistence, session
refresh and expired-session recovery against a local test server.

## iOS reuse

The APK contains `libgojni.so` from gomobile, not a Linux executable. The same
`mobile/bridge` package has an Objective-C binding surface and an
[XCFramework build script](../scripts/build-ios.sh). iOS needs a WKWebView host and
iOS lifecycle/download adapters, without porting the backend or Nuxt UI. The
framework must be linked and tested on macOS/Xcode; Linux binding generation does
not establish iOS runtime correctness.

[sdkmanager]: https://developer.android.com/tools/sdkmanager
[android-cli]: https://developer.android.com/tools/agents/android-cli

Additional official references:

- [NDK with other build systems](https://developer.android.com/ndk/guides/other_build_systems)
- [Foreground-service types](https://developer.android.com/develop/background-work/services/fgs/service-types#data-sync)
- [Foreground-service timeouts](https://developer.android.com/develop/background-work/services/fgs/timeout)
- [Emulator command line](https://developer.android.com/studio/run/emulator-commandline)
- [AGP 8.13 compatibility](https://developer.android.com/build/releases/past-releases/agp-8-13-0-release-notes)

- [WebView file chooser](https://developer.android.com/reference/android/webkit/WebChromeClient#onShowFileChooser(android.webkit.WebView,android.webkit.ValueCallback%3Candroid.net.Uri%5B%5D%3E,android.webkit.WebChromeClient.FileChooserParams))
- [WebView DownloadListener](https://developer.android.com/reference/android/webkit/DownloadListener)
- [WebView CookieManager](https://developer.android.com/reference/android/webkit/CookieManager)
- [DownloadManager](https://developer.android.com/reference/android/app/DownloadManager)
