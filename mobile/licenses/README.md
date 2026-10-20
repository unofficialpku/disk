These license and notice texts are copied verbatim from the upstream versions
used by the Android runtime. Do not rewrite their copyright or appendix text.

- `kotlin-LICENSE.txt`: <https://github.com/JetBrains/kotlin/blob/v2.1.20/license/LICENSE.txt>
- `kotlin-NOTICE.txt`: <https://github.com/JetBrains/kotlin/blob/v2.1.20/license/NOTICE.txt>
- `kotlinx-coroutines-LICENSE.txt`: <https://github.com/Kotlin/kotlinx.coroutines/blob/1.9.0/LICENSE.txt>

The Kotlin notice identifies the upstream compiler distribution; it is retained
unchanged alongside the Kotlin standard library's license.

`scripts/licenses.sh` also copies the project's verbatim Apache-2.0 license,
Go and gomobile licenses, frontend dependency licenses, Gradle's exact resolved
Android runtime dependency report, upstream Maven POMs (including inherited
license metadata), and license/notice files found in the resolved JARs/AARs.
`android/ATTRIBUTION.txt` records the upstream names, developers and license URLs
declared in those POMs. The resulting directory is bundled in the APK under
`assets/licenses/`. Publish one Android APK and include its hash in the release's
shared `checksums.txt`; do not publish a separate Android license archive.
