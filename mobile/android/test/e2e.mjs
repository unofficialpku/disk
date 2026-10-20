// Run only on a disposable emulator. No fixture hooks are compiled into the APK.
// Usage: node test/e2e.mjs emulator-5580 [path/to/app-debug.apk]
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import { mkdir, writeFile } from 'node:fs/promises'
import { resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { loadPlaywright, until } from '../../../scripts/lib/gui-test.mjs'
import { startAnyShareFixture } from '../../../scripts/lib/anyshare-fixture.mjs'

const run = promisify(execFile)
const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const { _android: android } = loadPlaywright()
const serial = process.argv[2]
assert.match(serial || '', /^emulator-\d+$/, 'Pass the serial of a disposable emulator')
const apk = resolve(process.argv[3] || `${root}/app/build/outputs/apk/debug/app-debug.apk`)
const adb = process.env.ADB || `${process.env.ANDROID_HOME}/platform-tools/adb`
const pkg = 'su.zzi.pkudiskfuse'
const artifacts = resolve(root, 'build/e2e')
await mkdir(artifacts, { recursive: true })
async function device(...args) { return (await run(adb, ['-s', serial, ...args], { maxBuffer: 4 * 1024 * 1024, timeout: 30000 })).stdout.trim() }
async function nativeXML() {
  await device('shell', 'uiautomator', 'dump', '/sdcard/pkudisk-test-window.xml')
  return device('shell', 'cat', '/sdcard/pkudisk-test-window.xml')
}
async function nativeTap(text) {
  const xml = await nativeXML()
  const node = [...xml.matchAll(/<node\b[^>]+/g)].find(([node]) => node.toLowerCase().includes(`text="${text.toLowerCase()}"`) || node.toLowerCase().includes(`content-desc="${text.toLowerCase()}"`))?.[0]
  assert.ok(node, `Native control not found: ${text}`)
  const [, x1, y1, x2, y2] = node.match(/bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"/).map(Number)
  await device('shell', 'input', 'tap', String(Math.round((x1+x2)/2)), String(Math.round((y1+y2)/2)))
}
async function launch() { await device('shell', 'am', 'start', '-W', '-n', `${pkg}/.MainActivity`) }
async function appPID() { return device('shell', 'pidof', pkg).catch(() => '') }
async function instance() {
  try { return JSON.parse(await device('shell', 'run-as', pkg, 'cat', 'files/service/gui/instance.json')) }
  catch { return null }
}
async function screenshot(name) {
  const result = await run(adb, ['-s', serial, 'exec-out', 'screencap', '-p'], { encoding: 'buffer', maxBuffer: 8*1024*1024, timeout: 15000 })
  await writeFile(`${artifacts}/${name}.png`, result.stdout)
}
async function taskNotifications() {
  const dump = await device('shell', 'dumpsys', 'notification', '--noredact')
  const active = dump.split('  Notification attention state:')[0]
  return active.split(/(?=    NotificationRecord\()/).filter(record => record.startsWith('    NotificationRecord(') && record.split('\n')[0].includes(`pkg=${pkg} `))
}
async function download(name) {
  await page.getByRole('button', { name: new RegExp(`^(操作|Actions): ${name.replace('.', '\\.')}$`) }).click()
  await page.getByRole('menuitem', { name: /^(下载|Download)$/ }).click()
  await page.getByRole('dialog').getByRole('button', { name: /^(下载|Download)$/ }).click()
  await page.getByRole('dialog').waitFor({ state: 'hidden' })
}
async function upload(name) {
  await page.getByRole('button', { name: /^(上传|Upload)$/ }).click()
  const dialog = page.getByRole('dialog')
  const submit = dialog.getByRole('button', { name: /^(上传|Upload)$/ })
  assert.equal(await submit.isDisabled(), true, 'upload cannot start before file selection')
  assert.equal(await dialog.getByRole('spinbutton').filter({ visible: true }).count(), 0, 'advanced upload settings stay collapsed')
  await dialog.getByRole('button', { name: /^(选择文件|Select files)$/ }).click()
  let xml = await nativeXML()
  if (xml.includes('text="Files"')) await nativeTap('Files')
  xml = await nativeXML()
  if (!xml.includes(name)) {
    await nativeTap('Show roots')
    await nativeTap('Downloads')
  }
  await nativeTap(name)
  await dialog.getByText(name, { exact: true }).waitFor()
  webEvents.push({ type: 'selected-files', files: await dialog.locator('input[type=file]').evaluate(input => Array.from(input.files || []).map(file => ({ name: file.name, size: file.size, lastModified: file.lastModified, relativePath: file.webkitRelativePath }))) })
  await screenshot('02-native-upload')
  await submit.click()
  await dialog.waitFor({ state: 'hidden' })
}
async function openTransfers() {
  await page.getByRole('button', { name: /^(Open sidebar|打开侧边栏)$/ }).click()
  await page.getByRole('link', { name: /^(Transfers|传输任务)/ }).click()
}
async function openFiles() {
  await page.getByRole('button', { name: /^(Open sidebar|打开侧边栏)$/ }).click()
  await page.getByRole('link', { name: /^(Files|文件)$/, exact: true }).click()
}
async function readyWindow() {
  await until(() => page.evaluate(() => document.documentElement.dataset.pkudiskPhase === 'ready'), 'Nuxt is ready')
  await until(async () => !(await nativeXML()).includes(`${pkg}:id/startup_screen`), 'native startup screen reveals ready WebView')
  const xml = await nativeXML()
  assert.ok(!xml.includes('OPEN INTERFACE') && !xml.includes('打开界面'), 'obsolete native startup controls flashed')
}
const relativeURL = value => {
  const url = new URL(value)
  url.searchParams.sort()
  return url.pathname + (url.searchParams.size ? '?' + url.searchParams.toString() : '')
}
const diagnosticURL = value => { const url = new URL(value); url.hash = ''; return url.href }
let androidDevice
let page
const webEvents = []
const upstream = await startAnyShareFixture({ profile: 'android' })
const upstreamPort = new URL(upstream.origin).port
try {
  assert.equal(await device('shell', 'getprop', 'ro.kernel.qemu'), '1', 'Never run against a real phone')
  const packaged = (await run('unzip', ['-Z1', apk])).stdout
  assert.ok(packaged.includes('lib/x86_64/libgojni.so'), 'gomobile JNI library missing')
  assert.ok(!packaged.includes('libpkudisk.so'), 'obsolete executable was packaged')
  await device('install', '-r', apk)
  await device('shell', 'am', 'force-stop', pkg)
  await device('shell', 'pm', 'clear', pkg)
  await device('shell', 'pm', 'grant', pkg, 'android.permission.POST_NOTIFICATIONS')
  // Prove the app has no dependency on the external Chrome application.
  await device('shell', 'pm', 'disable-user', '--user', '0', 'com.android.chrome')
  await device('reverse', `tcp:${upstreamPort}`, `tcp:${upstreamPort}`)
  await device('shell', 'rm', '-f', '/sdcard/Download/android-report.txt', '/sdcard/Download/移动端下载.txt', '/sdcard/Download/空文件.txt', '/sdcard/Download/课程资料.zip', '/sdcard/Download/background-transfer.bin', '/sdcard/Download/background-second.bin')
  for (const name of (await device('shell', 'ls', '-1', '/sdcard/Download')).split('\n')) {
    if (/^(android-report|移动端下载)(?:-\d+| \(\d+\))?\.txt$/.test(name))
      await device('shell', 'rm', '-f', `/sdcard/Download/${name}`)
  }
  await device('shell', 'run-as', pkg, 'mkdir', '-p', 'files/service')
  const configPath = `${artifacts}/test-accounts.json`
  await writeFile(configPath, JSON.stringify(upstream.config), { mode: 0o600 })
  await device('push', configPath, '/data/local/tmp/pkudisk-test-accounts.json')
  await device('shell', 'run-as', pkg, 'cp', '/data/local/tmp/pkudisk-test-accounts.json', 'files/service/accounts.json')
  await device('shell', 'run-as', pkg, 'chmod', '700', 'files/service')
  await device('shell', 'run-as', pkg, 'chmod', '600', 'files/service/accounts.json')
  await launch()
  const running = await until(instance, 'gomobile service listening')
  const pid = await appPID()
  assert.ok(pid, 'Application process was not started')
  assert.equal(await device('shell', 'pidof', 'libpkudisk.so').catch(() => ''), '')
  androidDevice = (await android.devices()).find(value => value.serial() === serial)
  assert.ok(androidDevice, 'Playwright could not connect to the emulator')
  const webview = await androidDevice.webView({ pkg })
  page = await webview.page()
  await until(() => page.url().startsWith(running.origin + '/'), 'embedded activation')
  page.setDefaultTimeout(20000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  page.on('console', message => { if (message.type() === 'error') webEvents.push({ type: 'console', text: message.text() }) })
  page.on('request', request => webEvents.push({ type: 'request', method: request.method(), url: diagnosticURL(request.url()), resource: request.resourceType() }))
  page.on('requestfailed', request => webEvents.push({ type: 'requestfailed', method: request.method(), url: diagnosticURL(request.url()), error: request.failure() }))
  page.on('response', async response => {
    if (new URL(response.url()).pathname === '/api/uploads') webEvents.push({ type: 'upload-response', status: response.status(), body: await response.text().catch(() => '') })
  })
  page.on('framenavigated', frame => { if (frame === page.mainFrame()) webEvents.push({ type: 'navigation', url: diagnosticURL(frame.url()) }) })
  await page.getByRole('button', { name: 'Android test library', exact: true }).click()
  await page.getByRole('button', { name: 'android-report.txt', exact: true }).waitFor()
  await readyWindow()
  await page.getByRole('button', { name: '照片.png', exact: true }).scrollIntoViewIfNeeded()
  const photo = page.getByRole('img', { name: /^(Thumbnail of 照片\.png|照片\.png 的缩略图)$/ })
  await photo.waitFor()
  await until(() => photo.evaluate(image => image.complete && image.naturalWidth === 8), 'authenticated image thumbnail renders in Android WebView')
  assert.ok((await photo.getAttribute('src')).startsWith('blob:'), 'thumbnail credentials must not be put in image URLs')
  console.log('PASS: Android file browser renders the authenticated thumbnail')
  assert.equal(new URL(page.url()).hash, '', 'activation ticket retained in address bar')
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'mobile horizontal overflow')
  await screenshot('01-files')
  let origin = new URL(page.url()).origin
  const info = await instance()
  assert.equal(info.origin, origin)
  const headers = await page.evaluate(async () => {
    const res = await fetch('/api/status', { headers: { Authorization: `Bearer ${sessionStorage.getItem('pkudisk-gui-session')}` } })
    return { status: res.status, data: await res.json() }
  })
  assert.equal(headers.status, 200)
  assert.equal(headers.data.tray, false)
  assert.equal(headers.data.version, '0.6.0')
  await until(async () => (await taskNotifications()).length === 0, 'idle foreground has no persistent notification')
  assert.deepEqual(headers.data.flags, { isMobile: true, canMount: false, canBrowseLocalFS: false })
  const localAccess = await page.evaluate(async () => {
    const headers = { Authorization: `Bearer ${sessionStorage.getItem('pkudisk-gui-session')}`, 'Content-Type': 'application/json' }
    const results = await Promise.all([
      fetch('/api/local-fs', { headers }),
      fetch('/api/local-fs/mkdir', { method: 'POST', headers, body: JSON.stringify({ path: '/', name: 'test' }) }),
      fetch('/api/recovery', { headers }),
      fetch('/api/tasks', { method: 'POST', headers, body: JSON.stringify({ kind: 'upload', account: 'campus', source: '/', destination: '/' }) })
    ])
    return results.map(response => response.status)
  })
  assert.deepEqual(localAccess, [403, 403, 403, 403], 'mobile must reject local filesystem operations')
  const mountSupport = await page.evaluate(async () => (await fetch('/api/mounts/support', { headers: { Authorization: `Bearer ${sessionStorage.getItem('pkudisk-gui-session')}` } })).json())
  assert.equal(mountSupport.platform, 'android')
  assert.equal(mountSupport.available, false)
  const mountRequests = []
  const trackMount = request => { if (new URL(request.url()).pathname.startsWith('/api/mounts')) mountRequests.push(request.url()) }
  page.on('request', trackMount)
  await page.goto(`${origin}/settings`)
  await page.getByRole('main').waitFor()
  assert.doesNotMatch(await page.getByRole('main').innerText(), /FUSE|WinFsp|Mount driver|挂载驱动|Desktop service|桌面服务/)
  await screenshot('01-mobile-settings')
  await page.goto(`${origin}/mounts`)
  await until(() => new URL(page.url()).pathname === '/', 'mobile mount route is hidden')
  assert.deepEqual(mountRequests, [], 'mobile UI requested mount functionality')
  page.off('request', trackMount)
  await page.getByRole('button', { name: 'Android test library', exact: true }).click()
  assert.ok((await upstream.snapshot()).calls.includes('/anyshare/oauth2/login/refreshToken'))
  assert.equal(await page.evaluate(() => typeof window.pkudiskDownloads), 'undefined', 'custom JavaScript bridge was injected')
  // WebView's system document chooser and Android DownloadManager.
  const uploadPath = `${artifacts}/android-upload.txt`
  await writeFile(uploadPath, 'Android native picker upload\n')
  await device('push', uploadPath, '/sdcard/Download/android-upload.txt')
  await device('shell', 'am', 'broadcast', '-a', 'android.intent.action.MEDIA_SCANNER_SCAN_FILE', '-d', 'file:///sdcard/Download/android-upload.txt')
  await upload('android-upload.txt')
  await page.getByRole('button', { name: 'android-upload.txt', exact: true }).waitFor()
  const uploadedState = await upstream.snapshot()
  const uploaded = [...uploadedState.entries.values()].find(e => e.name === 'android-upload.txt')
  assert.equal(uploadedState.data.get(uploaded.id).toString(), 'Android native picker upload\n')
  await openTransfers()
  const uploadedTask = page.getByRole('article').filter({ has: page.getByText('android-upload.txt', { exact: true }) })
  await uploadedTask.getByText(/^(Completed|已完成)$/).first().waitFor()
  await page.waitForResponse(response => new URL(response.url()).pathname === '/api/tasks')
  assert.equal(await uploadedTask.count(), 1, 'one system-picker upload must appear as one authoritative task')
  console.log('PASS: native picker upload publishes exact bytes with one visible task')
  await page.goBack()
  await page.getByRole('button', { name: 'android-upload.txt', exact: true }).waitFor()
  // Empty files and directory attachments exercise more than a small text
  // response. Validate the ZIP container and entry bytes saved by Android.
  await download('空文件.txt')
  await until(async () => (await device('shell', 'stat', '-c', '%s', '/sdcard/Download/空文件.txt').catch(() => '')) === '0', 'DownloadManager saves the empty Unicode attachment')
  await download('课程资料')
  const savedZIP = `${artifacts}/课程资料.zip`
  await until(async () => {
    try {
      await device('pull', '/sdcard/Download/课程资料.zip', savedZIP)
      const result = await run('python3', ['-c', 'import sys, zipfile; z = zipfile.ZipFile(sys.argv[1]); assert z.namelist() == [sys.argv[2]]; sys.stdout.buffer.write(z.read(sys.argv[2]))', savedZIP, '课程资料/读书笔记.md'], { encoding: 'buffer' })
      return result.stdout.equals(uploadedState.data.get('gns://library/folder/notes'))
    } catch { return false }
  }, 'DownloadManager directory ZIP contains the exact remote file bytes', 20000)
  console.log('PASS: native zero-byte Unicode and directory ZIP attachment integrity')
  // Reusing the same system file in another folder must create a new attempt,
  // not collide with Uppy's retained filename/size/mtime-derived history ID.
  await page.getByRole('button', { name: '课程资料', exact: true }).click()
  let startUpload
  let releaseUpload
  const uploadRequested = new Promise(resolve => { startUpload = resolve })
  const releaseRequest = new Promise(resolve => { releaseUpload = resolve })
  const holdUpload = async route => {
    startUpload()
    await releaseRequest
    await route.continue()
  }
  await page.route('**/api/uploads', holdUpload)
  await upload('android-upload.txt')
  await uploadRequested
  await openTransfers()
  releaseUpload()
  const uploadTasks = page.getByRole('article').filter({ has: page.getByText('android-upload.txt', { exact: true }) })
  await until(async () => (await uploadTasks.getByText(/^(Completed|已完成)$/).count()) === 2, 'repeat upload completes on the Transfers page')
  assert.equal(await uploadTasks.count(), 2, 'repeat upload must have one task per attempt')
  await page.unroute('**/api/uploads', holdUpload)
  const repeatState = await upstream.snapshot()
  const repeats = [...repeatState.entries.values()].filter(entry => entry.name === 'android-upload.txt')
  assert.equal(repeats.length, 2, 'same file was not uploaded to both destinations')
  assert.ok(repeats.some(entry => entry.id.startsWith('gns://library/folder/')), 'second upload used the wrong destination')
  for (const entry of repeats) assert.equal(repeatState.data.get(entry.id).toString(), 'Android native picker upload\n')
  await page.goBack()
  await page.getByRole('button', { name: 'android-upload.txt', exact: true }).waitFor()
  // A name conflict is a failed new attempt, never a recycled completed row.
  await upload('android-upload.txt')
  await openTransfers()
  await until(async () => (await uploadTasks.getByText(/^(Failed|失败)$/).count()) === 1, 'duplicate destination reports upload failure')
  assert.equal(await uploadTasks.getByText(/^(Completed|已完成)$/).count(), 2)
  assert.equal(await uploadTasks.count(), 3)
  assert.equal([...((await upstream.snapshot()).entries.values())].filter(entry => entry.name === 'android-upload.txt').length, 2)
  await screenshot('02-upload-attempts')
  await uploadTasks.filter({ has: page.getByText(/^(Failed|失败)$/) }).getByRole('button', { name: /^(Remove from history|移除历史记录)$/ }).click()
  await until(async () => (await uploadTasks.count()) === 2, 'removing the failed attempt also clears its browser history')
  await page.waitForResponse(response => new URL(response.url()).pathname === '/api/tasks')
  assert.equal(await uploadTasks.count(), 2, 'cleared browser attempt must not reappear after polling')
  console.log('PASS: repeated file uploads, destination routing, conflict failure and history removal')
  await page.goBack()
  await page.getByRole('button', { name: 'android-upload.txt', exact: true }).waitFor()
  await page.getByRole('button', { name: /^(Parent folder|上级目录)$/ }).click()
  await page.getByRole('button', { name: 'android-report.txt', exact: true }).waitFor()
  for (const name of ['android-report.txt', '移动端下载.txt']) {
    await download(name)
    await until(async () => (await device('shell', 'cat', `/sdcard/Download/${name}`).catch(() => '')) === 'Android download test', `DownloadManager attachment ${name}`, 20000)
  }
  for (const name of ['android-report.txt', '移动端下载.txt']) {
    const before = new Set((await device('shell', 'ls', '-1', '/sdcard/Download')).split('\n'))
    await download(name)
    const renamed = await until(async () => {
      const files = (await device('shell', 'ls', '-1', '/sdcard/Download')).split('\n')
      for (const file of files) {
        if (!before.has(file) && file.startsWith(name.slice(0, -4)) && file.endsWith('.txt') &&
          (await device('shell', 'cat', `/sdcard/Download/${file}`).catch(() => '')) === 'Android download test') return file
      }
      return null
    }, `DownloadManager saves a distinct copy of ${name}`, 20000)
    assert.notEqual(renamed, name)
    assert.equal(await device('shell', 'cat', `/sdcard/Download/${name}`), 'Android download test', 'repeat download must preserve existing bytes')
  }
  console.log('PASS: repeated ASCII and Unicode downloads save separate exact copies')
  await page.goto(`${origin}/transfers`)
  for (const name of ['android-report.txt', '移动端下载.txt', '空文件.txt', '课程资料.zip']) {
    const task = page.getByRole('article').filter({ has: page.getByText(name, { exact: true }) })
    const expected = ['android-report.txt', '移动端下载.txt'].includes(name) ? 2 : 1
    await until(async () => await task.getByText(/^(Completed|已完成)$/).count() === expected, `completed download attempts: ${name}`)
    assert.equal(await task.count(), expected, 'WebView handoff must remain a single visible task per user download')
  }
  await screenshot('03-attachment-download')
  const removed = page.getByRole('article').filter({ has: page.getByText('移动端下载.txt', { exact: true }) })
  while (await removed.count()) await removed.first().getByRole('button', { name: /^(Remove from history|移除历史记录)$/ }).click()
  await removed.waitFor({ state: 'hidden' })
  const remaining = await page.evaluate(async () => (await fetch('/api/tasks', { headers: { Authorization: `Bearer ${sessionStorage.getItem('pkudisk-gui-session')}` } })).json())
  assert.equal(remaining.filter(task => task.name === '移动端下载.txt').length, 0, 'clearing a download must remove both handoff records')
  // A fresh origin must restore the complete route, and keep the native splash
  // over the document until async session authentication and rendering finish.
  await page.goto(`${origin}/?account=android-e2e&path=gns%3A%2F%2Flibrary%2Ffolder`)
  await page.getByRole('button', { name: '读书笔记.md', exact: true }).waitFor()
  const savedRoute = relativeURL(page.url())
  await device('shell', 'input', 'keyevent', 'KEYCODE_HOME')
  await until(async () => !(await instance()), 'idle background release')
  assert.equal((await taskNotifications()).length, 0)
  let releaseSession
  let sessionContinued
  let requestedSession = false
  const waitingSession = new Promise(resolve => { releaseSession = resolve })
  const continuedSession = new Promise(resolve => { sessionContinued = resolve })
  const holdSession = async route => {
    requestedSession = true
    await waitingSession
    await route.continue()
    sessionContinued()
  }
  await page.route('**/api/session', holdSession)
  await launch()
  const resumed = await until(instance, 'automatic foreground resume')
  assert.notEqual(resumed.control, running.control, 'idle service was not recreated')
  origin = resumed.origin
  await until(() => page.url().startsWith(origin + '/'), 'fresh embedded activation after idle release')
  await until(() => requestedSession, 'fresh session request held during startup')
  assert.notEqual(await page.evaluate(() => document.documentElement.dataset.pkudiskPhase), 'ready')
  // Capture the actual native loading frame. UIAutomator's global-idle wait is
  // inappropriate while a spinner and a deliberately held request are active.
  await screenshot('04-startup-splash')
  releaseSession()
  await continuedSession
  await page.unroute('**/api/session', holdSession)
  await readyWindow()
  assert.equal(relativeURL(page.url()), savedRoute, 'idle resume lost the selected account/folder')
  await page.getByRole('button', { name: '读书笔记.md', exact: true }).waitFor()
  await screenshot('04-idle-resumed')
  console.log('PASS: splash hides pending authentication and restores the account/folder route')
  // Exercise both sides of the one-second suspension boundary repeatedly.
  for (const delay of [50, 950, 1100, 150, 1300]) {
    await device('shell', 'input', 'keyevent', 'KEYCODE_HOME')
    await new Promise(resolve => setTimeout(resolve, delay))
    await launch()
    await readyWindow()
    assert.equal(relativeURL(page.url()), savedRoute, `rapid ${delay}ms resume reset navigation`)
    assert.equal(await page.getByRole('dialog', { name: /服务未运行|The service is not running/ }).count(), 0)
  }
  console.log('PASS: rapid foreground/background transitions retain the route without stale errors')
  // A stale document must not remain stuck on an old session after the native
  // service has confirmed it is healthy. Re-activate once under the splash.
  await page.evaluate(() => sessionStorage.removeItem('pkudisk-gui-session'))
  await page.reload()
  await until(() => page.evaluate(() => document.documentElement.dataset.pkudiskPhase === 'expired'), 'stale local session')
  await device('shell', 'input', 'keyevent', 'KEYCODE_HOME')
  await launch()
  await readyWindow()
  assert.equal(relativeURL(page.url()), savedRoute, 'session recovery lost its route')
  console.log('PASS: healthy service reactivates a stale WebView session under splash')
  origin = new URL(page.url()).origin
  // Android recreation/rotation should keep the same route and ready frame.
  await device('shell', 'settings', 'put', 'system', 'accelerometer_rotation', '0')
  await device('shell', 'settings', 'put', 'system', 'user_rotation', '1')
  await readyWindow()
  assert.equal(relativeURL(page.url()), savedRoute)
  await device('shell', 'settings', 'put', 'system', 'user_rotation', '0')
  await readyWindow()
  await page.getByRole('button', { name: /^(Parent folder|上级目录)$/ }).click()
  await page.getByRole('button', { name: 'background-transfer.bin', exact: true }).waitFor()
  const transferInstance = await instance()
  // Two genuine throttled transfers exercise grouped native progress and lifetime.
  await download('background-transfer.bin')
  await download('background-second.bin')
  const progress = record => Number(record.match(/android\.progress=Integer \((\d+)\)/)?.[1] || 0)
  const firstProgress = await until(async () => {
    const records = await taskNotifications()
    return records.length >= 3 && records.some(record => progress(record) > 0) ? records : null
  }, 'native per-task progress notifications')
  const beforeProgress = Math.max(...firstProgress.map(progress))
  await writeFile(`${artifacts}/native-task-notifications.txt`, firstProgress.join('\n'))
  await device('shell', 'input', 'keyevent', 'KEYCODE_HOME')
  await until(async () => (await taskNotifications()).some(record => progress(record) > beforeProgress), 'progress advances in background')
  assert.equal((await instance()).control, transferInstance.control, 'active background tasks lost their service')
  await device('shell', 'cmd', 'statusbar', 'expand-notifications')
  // Let the shade animation finish before capturing it. UIAutomator waits for
  // global idleness, which progress notifications intentionally never provide.
  await new Promise(resolve => setTimeout(resolve, 600))
  await device('shell', 'input', 'swipe', '540', '1800', '540', '650', '450')
  await new Promise(resolve => setTimeout(resolve, 600))
  await screenshot('05-task-notifications')
  await device('shell', 'cmd', 'statusbar', 'collapse')
  // DownloadManager preallocates destination files, so size is not completion.
  const expectedHash = createHash('sha256').update(uploadedState.data.get('gns://library/slow-second')).digest('hex')
  await until(async () => (await device('shell', 'sha256sum', '/sdcard/Download/background-second.bin').catch(() => '')).startsWith(expectedHash), 'background transfer bytes complete', 45000)
  await until(async () => !(await instance()), 'completed background service releases itself', 15000)
  assert.equal((await taskNotifications()).length, 0, 'completed task notifications remained ongoing')
  await launch()
  origin = (await until(instance, 'resume after background transfers')).origin
  await until(() => page.url().startsWith(origin + '/'), 'resumed interface')
  await page.getByRole('main').waitFor()
  await readyWindow()
  // Quit through the actual mobile UI, then verify Android removes the task.
  assert.equal(await page.title(), 'PKUDisk')
  await page.getByRole('button', { name: /^(Open sidebar|打开侧边栏)$/ }).click()
  assert.equal(await page.getByRole('button', { name: /^(Quit desktop service|退出桌面服务)$/ }).count(), 0)
  await page.getByRole('button', { name: /^(Quit|退出)$/, exact: true }).click()
  const quitDialog = page.getByRole('dialog', { name: /^(Quit|退出)$/, exact: true })
  await quitDialog.getByRole('button', { name: /^(Confirm|确认)$/, exact: true }).waitFor()
  await screenshot('05-quit-confirm')
  await quitDialog.getByRole('button', { name: /^(Confirm|确认)$/, exact: true }).click()
  await until(async () => !(await instance()), 'Quit stops the shared service')
  await until(async () => {
    const recentTasks = (await device('shell', 'dumpsys', 'activity', 'recents')).split('  Recent tasks:')[1]
    return recentTasks !== undefined && !recentTasks.includes(pkg + '/')
  }, 'Quit removes the app task from Recents')
  await until(async () => !/ResumedActivity[=:][^\n]*su\.zzi\.pkudiskfuse/.test(await device('shell', 'dumpsys', 'activity', 'activities')), 'Quit closes the app window')
  await screenshot('06-quit-closed')
  // Restart, then force-stop the Java package. The Go library dies with the host.
  await launch()
  await until(instance, 'restart')
  const beforeForce = await instance()
  await device('shell', 'am', 'force-stop', pkg)
  await until(async () => !(await appPID()), 'force-stop ends the host process')
  // Set the test budget before the next service starts so Android schedules it.
  await device('shell', 'device_config', 'put', 'activity_manager', 'data_sync_fgs_timeout_duration', '2000')
  await launch()
  await until(async () => { const info = await instance(); return info && info.control !== beforeForce.control ? info : null }, 'restart after force-stop')
  assert.ok(await appPID())
  page = await (await androidDevice.webView({ pkg })).page()
  await readyWindow()
  // The persisted route survives process death, including the library path.
  await page.getByRole('button', { name: 'background-transfer.bin', exact: true }).waitFor()
  await device('shell', 'rm', '-f', '/sdcard/Download/background-transfer.bin')
  await download('background-transfer.bin')
  await until(async () => (await taskNotifications()).length > 0, 'active task starts Android foreground service')
  // Exercise Android's real dataSync timeout callback with a short test budget.
  await device('shell', 'input', 'keyevent', 'KEYCODE_HOME')
  await until(async () => !(await instance()), 'Android foreground-service timeout', 30000)
  // An unexpected timeout keeps the window available for the exit overlay.
  await launch()
  await page.getByRole('dialog', { name: /^(The service is not running\. Please exit the app\.|服务未运行，请退出应用)$/ }).waitFor()
  assert.equal(await page.getByRole('main').count(), 0, 'terminated workspace remained interactive')
  assert.equal(await instance(), null, 'returning to the timeout overlay restarted the service')
  await screenshot('07-unexpected-disconnect')
  assert.deepEqual(errors, [])
  console.log('PASS: Android APK install, in-process gomobile AAR, embedded WebView activation with Chrome disabled, saved login refresh, system picker upload, DownloadManager download, idle release/resume, grouped native background task progress, automatic release after tasks, mobile Quit closes app/Recents, restart, force-stop cleanup and unexpected Android timeout overlay.')
} catch (error) {
  await writeFile(`${artifacts}/web-events.json`, JSON.stringify(webEvents, null, 2))
  await screenshot('failure').catch(() => {})
  if (page) await writeFile(`${artifacts}/jobs.json`, JSON.stringify(await page.evaluate(async () => (await fetch('/api/tasks', { headers: { Authorization: `Bearer ${sessionStorage.getItem('pkudisk-gui-session')}` } })).json()).catch(() => null), null, 2))
  await writeFile(`${artifacts}/upstream-calls.json`, JSON.stringify((await upstream.snapshot()).calls, null, 2))
  await writeFile(`${artifacts}/native-ui.xml`, await nativeXML().catch(() => ''))
  await writeFile(`${artifacts}/native.log`, await device('logcat', '-d', '-s', 'AndroidRuntime', 'PKUDiskLifecycle').catch(() => ''))
  throw error
} finally {
  await androidDevice?.close()
  await device('shell', 'am', 'force-stop', pkg).catch(() => {})
  await device('shell', 'pm', 'enable', 'com.android.chrome').catch(() => {})
  await device('shell', 'device_config', 'delete', 'activity_manager', 'data_sync_fgs_timeout_duration').catch(() => {})
  await device('shell', 'settings', 'put', 'system', 'user_rotation', '0').catch(() => {})
  await device('shell', 'settings', 'put', 'system', 'accelerometer_rotation', '1').catch(() => {})
  await device('reverse', '--remove', `tcp:${upstreamPort}`).catch(() => {})
  await upstream.close()
}
