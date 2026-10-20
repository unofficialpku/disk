// Verify an actual installed release upgrade on a disposable, rootable emulator.
// Usage: node test/upgrade.mjs emulator-5580 previous.apk [debug.apk] [release.apk]
import assert from 'node:assert/strict'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import { mkdir, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { loadPlaywright, until } from '../../../scripts/lib/gui-test.mjs'
import { startAnyShareFixture } from '../../../scripts/lib/anyshare-fixture.mjs'

const run = promisify(execFile)
const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const serial = process.argv[2]
assert.match(serial || '', /^emulator-\d+$/, 'Use a disposable emulator')
assert.ok(process.argv[3], 'Pass the previously distributed APK')
const previous = resolve(process.argv[3])
const debug = resolve(process.argv[4] || `${root}/app/build/outputs/apk/debug/app-debug.apk`)
const release = resolve(process.argv[5] || `${root}/app/build/outputs/apk/release/app-release.apk`)
const adb = process.env.ADB || `${process.env.ANDROID_HOME}/platform-tools/adb`
const buildTools = `${process.env.ANDROID_HOME}/build-tools/36.0.0`
const pkg = 'su.zzi.pkudiskfuse'
const data = `/data/user/0/${pkg}`
const artifacts = resolve(root, 'build/upgrade-e2e')
await mkdir(artifacts, { recursive: true })
async function device(...args) { return (await run(adb, ['-s', serial, ...args], { maxBuffer: 4 * 1024 * 1024, timeout: 30000 })).stdout.trim() }
async function shellRoot(...args) { return device('shell', 'su', '0', ...args) }
async function nativeXML() {
  await device('shell', 'uiautomator', 'dump', '/sdcard/pkudisk-upgrade-window.xml')
  return device('shell', 'cat', '/sdcard/pkudisk-upgrade-window.xml')
}
async function nativeTap(text) {
  const xml = await nativeXML()
  const node = [...xml.matchAll(/<node\b[^>]+/g)].find(([node]) => node.includes(`text="${text}"`) || node.includes(`content-desc="${text}"`))?.[0]
  assert.ok(node, `Native control not found: ${text}`)
  const [, x1, y1, x2, y2] = node.match(/bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"/).map(Number)
  await device('shell', 'input', 'tap', String(Math.round((x1 + x2) / 2)), String(Math.round((y1 + y2) / 2)))
}
async function screenshot(name) {
  const result = await run(adb, ['-s', serial, 'exec-out', 'screencap', '-p'], { encoding: 'buffer', maxBuffer: 8 * 1024 * 1024, timeout: 15000 })
  await writeFile(`${artifacts}/${name}.png`, result.stdout)
}
async function launch() { await device('shell', 'am', 'start', '-W', '-n', `${pkg}/.MainActivity`) }
async function savedAccount() { return JSON.parse(await shellRoot('cat', `${data}/files/service/accounts.json`)) }
async function savedRoute() { return shellRoot('cat', `${data}/shared_prefs/MainActivity.xml`) }
async function signature(apk) {
  const output = (await run(`${buildTools}/apksigner`, ['verify', '--print-certs', apk])).stdout
  return output.match(/Signer #1 certificate SHA-256 digest: (\w+)/)?.[1]
}
const upstream = await startAnyShareFixture({ profile: 'android' })
const port = new URL(upstream.origin).port
let androidDevice
try {
  assert.equal(await device('shell', 'getprop', 'ro.kernel.qemu'), '1')
  assert.match(await shellRoot('id'), /^uid=0\(/, 'The previous non-debuggable release requires a rootable test emulator')
  const originalSignature = await signature(previous)
  assert.ok(originalSignature)
  assert.equal(await signature(debug), originalSignature, 'Debug test APK cannot update the distributed APK')
  assert.equal(await signature(release), originalSignature, 'Release signing identity changed')
  const manifest = (await run(`${buildTools}/aapt`, ['dump', 'badging', release])).stdout
  assert.match(manifest, /package: name='su\.zzi\.pkudiskfuse' versionCode='5' versionName='0\.6\.0'/)
  assert.match(manifest, /application-label:'PKUDisk'/)
  assert.ok(!manifest.includes('application-debuggable'), 'Release must disable WebView debugging')
  const packaged = (await run('unzip', ['-Z1', release])).stdout
  for (const license of ['LICENSE', 'NOTICE', 'android/ATTRIBUTION.txt', 'go/Go-LICENSE', 'web/THIRD_PARTY_LICENSES.txt']) {
    assert.ok(packaged.includes(`assets/licenses/${license}`), `APK omits ${license}`)
  }
  await device('uninstall', pkg).catch(() => {})
  await device('install', previous)
  await device('shell', 'pm', 'grant', pkg, 'android.permission.POST_NOTIFICATIONS')
  await device('reverse', `tcp:${port}`, `tcp:${port}`)
  const accountPath = `${artifacts}/test-accounts.json`
  await writeFile(accountPath, JSON.stringify(upstream.config), { mode: 0o600 })
  await device('push', accountPath, '/data/local/tmp/pkudisk-upgrade-accounts.json')
  const owner = await shellRoot('stat', '-c', '%u:%g', data)
  await shellRoot('mkdir', '-p', `${data}/files/service`)
  await shellRoot('cp', '/data/local/tmp/pkudisk-upgrade-accounts.json', `${data}/files/service/accounts.json`)
  await shellRoot('chown', '-R', owner, `${data}/files`)
  await shellRoot('chmod', '700', `${data}/files/service`)
  await shellRoot('chmod', '600', `${data}/files/service/accounts.json`)
  await shellRoot('restorecon', '-RF', `${data}/files`)
  await launch()
  await until(async () => (await nativeXML()).includes('Android test library'), 'previous release restores the account', 30000)
  await nativeTap('Android test library')
  await until(async () => (await nativeXML()).includes('课程资料'), 'previous release opens the library')
  await nativeTap('课程资料')
  await until(async () => (await nativeXML()).includes('读书笔记.md'), 'previous release selects a nested folder')
  await screenshot('01-previous-release')
  await device('shell', 'input', 'keyevent', 'KEYCODE_HOME')
  const before = await savedAccount()
  const route = await savedRoute()
  assert.match(decodeURIComponent(route), /gns:\/\/library\/folder/)
  await device('shell', 'am', 'force-stop', pkg)
  await device('install', '-r', debug)
  assert.deepEqual(await savedAccount(), before, 'Installing the update changed saved accounts/session')
  assert.equal(await savedRoute(), route, 'Installing the update changed the saved route')
  await launch()
  const { _android: android } = loadPlaywright()
  androidDevice = (await android.devices()).find(value => value.serial() === serial)
  assert.ok(androidDevice)
  const page = await (await androidDevice.webView({ pkg })).page()
  page.setDefaultTimeout(20000)
  await page.getByRole('button', { name: '读书笔记.md', exact: true }).waitFor()
  assert.equal(await page.title(), 'PKUDisk')
  assert.equal(new URL(page.url()).searchParams.get('path'), 'gns://library/folder')
  assert.equal(new URL(page.url()).searchParams.get('account'), 'android-e2e')
  assert.equal(await page.getByRole('dialog', { name: /添加账号|Add account/ }).count(), 0)
  assert.ok((await upstream.snapshot()).calls.includes('/anyshare/oauth2/login/refreshToken'), 'Saved session was not reused')
  await screenshot('02-updated-debug')
  await device('shell', 'input', 'keyevent', 'KEYCODE_HOME')
  const afterDebug = await savedAccount()
  const afterRoute = await savedRoute()
  await device('shell', 'am', 'force-stop', pkg)
  await androidDevice.close()
  androidDevice = null
  await device('install', '-r', release)
  assert.deepEqual(await savedAccount(), afterDebug, 'Release update changed saved accounts/session')
  assert.equal(await savedRoute(), afterRoute, 'Release update changed the saved route')
  await launch()
  await until(async () => (await nativeXML()).includes('读书笔记.md'), 'signed release restores the same account and folder', 30000)
  await screenshot('03-updated-release')
  await device('shell', 'am', 'start', '-a', 'android.settings.APPLICATION_DETAILS_SETTINGS', '-d', `package:${pkg}`)
  await until(async () => (await nativeXML()).includes('text="PKUDisk"'), 'Android shows the new PKUDisk application name')
  await writeFile(`${artifacts}/system-app-name.xml`, await nativeXML())
  await screenshot('04-system-app-name')
  console.log('PASS: previous release → v0.6.0 debug → signed release, unchanged package/signing, saved account/session and nested folder, PKUDisk title/launcher label, bundled license notices, non-debuggable release.')
} catch (error) {
  await screenshot('failure').catch(() => {})
  await writeFile(`${artifacts}/native-ui.xml`, await nativeXML().catch(() => ''))
  throw error
} finally {
  await androidDevice?.close()
  await device('shell', 'am', 'force-stop', pkg).catch(() => {})
  await device('reverse', '--remove', `tcp:${port}`).catch(() => {})
  await upstream.close()
}
