// End-to-end UI tests use the Go test executable's isolated AnyShare fixture.
// No real credentials, network account, or filesystem driver is used.
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { mkdtemp, readFile, writeFile, mkdir, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { loadPlaywright, until, takeScreenshot } from './lib/gui-test.mjs'

const { chromium } = loadPlaywright()
if (!process.argv[2]) throw new Error('Usage: node scripts/test-gui-workflows.mjs GUI-TEST-BINARY [SCREENSHOTS]')
const temporary = await mkdtemp(join(tmpdir(), 'pkudisk-gui-workflows-'))
const capture = join(temporary, 'url')
const screenshots = process.argv[3] && resolve(process.argv[3])
const child = spawn(resolve(process.argv[2]), ['-test.run=^TestGUIBrowserFixtureProcess$', '-test.timeout=180s'], {
  env: { ...process.env, PKUDISK_GUI_BROWSER_CAPTURE: capture }, stdio: ['ignore', 'pipe', 'pipe']
})
let output = ''
child.stdout.on('data', data => { output += data })
child.stderr.on('data', data => { output += data })
const exited = once(child, 'exit')
let browser
let page
const errors = []
const requests = []
const violations = []
const styleRecords = []

async function screenshot(name) { await takeScreenshot(page, screenshots, name) }
async function tasks() {
  return page.evaluate(async () => {
    const response = await fetch('/api/tasks', { headers: { Authorization: `Bearer ${sessionStorage.getItem('pkudisk-gui-session')}` } })
    return response.json()
  })
}
async function completed(kind) {
  return until(async () => {
    const all = await tasks()
    const task = all.find(value => value.kind === kind)
    if (task?.state === 'failed') throw new Error(`${kind}: ${task.error?.message}`)
    return task?.state === 'completed' ? task : null
  }, `${kind} completion`)
}
async function confirm() { await page.getByRole('dialog').last().getByRole('button', { name: '确认', exact: true }).click() }
async function advanced() {
  await page.getByRole('dialog').getByRole('button', { name: '高级选项', exact: true }).click()
}
async function fileMenu(name, action) {
  await page.getByRole('button', { name: `操作: ${name}`, exact: true }).click()
  const item = page.getByRole('menuitem', { name: action, exact: true })
  await until(() => item.isVisible().catch(() => false), `${action} menu item`)
  await item.click()
}
async function browseLocal(path) {
  const picker = page.getByRole('dialog').last()
  await picker.getByRole('button', { name: '文件系统', exact: true }).click()
  for (const part of path.split('/').filter(Boolean)) {
    await picker.getByRole('button', { name: part, exact: true }).click()
  }
  return picker
}
async function createLocalFolder(picker, name) {
  await picker.getByRole('button', { name: '新建文件夹', exact: true }).click()
  await page.getByRole('textbox', { name: '名称', exact: true }).fill(name)
  await page.getByRole('button', { name: '创建文件夹', exact: true }).click()
  await picker.getByText(new RegExp(`/${name}$`)).first().waitFor()
}
async function closeDialog(title) {
  const dialog = page.getByRole('dialog', { name: title, exact: true })
  await dialog.getByRole('button', { name: /Close|关闭/ }).first().click()
  await dialog.waitFor({ state: 'hidden' })
}

try {
  const address = await until(async () => {
    if (child.exitCode !== null) throw new Error(`Fixture failed: ${output}`)
    return readFile(capture, 'utf8').catch(() => null)
  }, 'fixture startup')
  const info = JSON.parse(await readFile(`${capture}.info`, 'utf8'))
  const origin = new URL(address).origin
  browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined, args: ['--no-sandbox'] })
  const context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 1000 }, permissions: ['clipboard-read', 'clipboard-write'] })
  page = await context.newPage()
  page.setDefaultTimeout(12000)
  await page.exposeFunction('recordCSP', directive => violations.push(directive))
  await page.exposeFunction('recordStyle', style => styleRecords.push(style))
  await page.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', event => window.recordCSP({ directive: event.violatedDirective, source: event.sourceFile, line: event.lineNumber }))
    new MutationObserver(records => {
      for (const record of records) for (const node of record.addedNodes) {
        if (node.nodeName === 'STYLE') window.recordStyle({ text: node.textContent, nonce: node.nonce })
      }
    }).observe(document, { subtree: true, childList: true })
  })
  page.on('pageerror', error => errors.push(error.message))
  page.on('crash', () => errors.push('Browser page crashed'))
  page.on('console', message => { if (message.type() === 'error') console.error('Browser console:', message.text()) })
  page.on('request', request => {
    const url = new URL(request.url())
    if (url.protocol.startsWith('http')) requests.push({ origin: url.origin, path: url.pathname })
  })
  await page.goto(address)
  await page.getByRole('main').waitFor()
  assert.equal(new URL(page.url()).hash, '')
  assert.equal(await page.title(), 'PKUDisk')
  assert.equal(await page.evaluate(() => JSON.stringify(history.state).includes('ticket=')), false)
  await page.getByRole('button', { name: '个人文档库', exact: true }).click()
  await page.getByRole('button', { name: '研究报告.txt', exact: true }).waitFor()
  assert.equal(new URL(page.url()).hash, '')
  await screenshot('files-zh')

  await page.getByRole('button', { name: '语言', exact: true }).click()
  await page.getByRole('option', { name: /English/ }).click()
  await page.getByRole('link', { name: 'Files', exact: true }).waitFor()
  assert.equal(await page.locator('html').getAttribute('lang'), 'en')
  assert.equal(await page.title(), 'PKUDisk')
  await screenshot('files-en')
  await page.getByRole('button', { name: 'Language', exact: true }).click()
  await page.getByRole('option', { name: /简体中文/ }).click()
  await page.getByRole('button', { name: '主题', exact: true }).click()
  await until(() => page.locator('html').evaluate(node => node.classList.contains('dark')), 'dark theme')
  await screenshot('files-dark')
  await page.getByRole('button', { name: '主题', exact: true }).click()
  await page.setViewportSize({ width: 390, height: 844 })
  assert.ok((await page.getByRole('textbox', { name: '搜索', exact: true }).boundingBox()).width >= 220, 'mobile search input was squeezed')
  assert.ok((await page.getByRole('textbox', { name: '筛选当前目录…', exact: true }).boundingBox()).width >= 220, 'mobile filter input was squeezed')
  await screenshot('files-mobile')
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
  await page.setViewportSize({ width: 1440, height: 1000 })

  await page.getByRole('textbox', { name: '筛选当前目录…', exact: true }).fill('no-such-file-ux-test')
  await page.getByRole('heading', { name: '没有匹配的文件', exact: true }).waitFor()
  await page.getByRole('button', { name: '清除', exact: true }).click()
  await page.getByRole('button', { name: '研究报告.txt', exact: true }).waitFor()

  await page.getByRole('button', { name: '新建文件夹', exact: true }).click()
  await page.getByRole('dialog').getByLabel('新名称', { exact: true }).fill('GUI 新目录')
  await page.getByRole('dialog').getByRole('button', { name: '保存', exact: true }).click()
  await page.getByRole('button', { name: 'GUI 新目录', exact: true }).waitFor()
  await page.getByRole('button', { name: '上传', exact: true }).click()
  assert.equal(await page.getByRole('dialog').getByRole('button', { name: '上传', exact: true }).isDisabled(), true)
  assert.equal(await page.getByRole('dialog').getByRole('spinbutton').filter({ visible: true }).count(), 0, 'advanced upload controls start collapsed')
  await page.getByRole('dialog').locator('input[type=file]').setInputFiles(info.upload)
  await screenshot('browser-upload')
  await page.getByRole('dialog').getByRole('button', { name: '上传', exact: true }).click()
  await completed('upload')
  // Completed writes refresh the listing without a manual reload.
  await page.getByRole('button', { name: 'upload.txt', exact: true }).waitFor()

  // Exercise browser-fs-access's directory picker with a real browser handle.
  await page.evaluate(async () => {
    const root = await navigator.storage.getDirectory()
    const source = await root.getDirectoryHandle('browser-selected-folder', { create: true })
    const nested = await source.getDirectoryHandle('nested', { create: true })
    const writer = await (await nested.getFileHandle('chosen.txt', { create: true })).createWritable()
    await writer.write('selected through the browser directory API')
    await writer.close()
    window.showDirectoryPicker = async () => source
  })
  await page.getByRole('button', { name: '上传', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: '选择文件夹', exact: true }).click()
  await page.getByRole('dialog').getByText('chosen.txt', { exact: true }).waitFor()
  await page.getByRole('dialog').getByRole('button', { name: '上传', exact: true }).click()
  await page.getByRole('button', { name: 'browser-selected-folder', exact: true }).waitFor()

  await fileMenu('研究报告.txt', '下载')
  assert.equal(await page.getByRole('dialog').getByRole('spinbutton').filter({ visible: true }).count(), 0, 'browser download does not need transfer settings')
  await screenshot('browser-download')
  const downloaded = page.waitForEvent('download')
  await page.getByRole('dialog').getByRole('button', { name: '下载', exact: true }).click()
  const download = await downloaded
  assert.equal(download.suggestedFilename(), '研究报告.txt')
  await download.saveAs(join(info.downloads, 'browser-download.txt'))
  await completed('download')
  assert.equal((await readFile(join(info.downloads, 'browser-download.txt'))).length, 1536)

  // Directory downloads use the same Go transfer queue on the desktop.
  for (const [mode, directoryName] of [['逐个下载文件和目录', 'direct-local'], ['下载 ZIP 并在本机解压', 'zip-local']]) {
    await fileMenu('课程资料', '下载')
    await advanced()
    await page.getByRole('dialog').getByRole('button', { name: '保存到本机', exact: true }).click()
    const picker = await browseLocal(info.downloads)
    await createLocalFolder(picker, directoryName)
    await picker.getByRole('button', { name: '选择', exact: true }).click()
    await page.getByRole('dialog').getByRole('combobox', { name: '下载方式', exact: true }).click()
    await page.getByRole('option', { name: mode, exact: true }).click()
    await page.getByRole('dialog').getByRole('button', { name: '下载', exact: true }).click()
    await until(async () => (await readFile(join(info.downloads, directoryName, '课程资料', '读书笔记.md')).catch(() => null))?.length === 1536, `${directoryName} queued transfer`)
  }
  await writeFile(join(info.downloads, 'queued-upload.txt'), 'uploaded by the shared desktop queue')
  await page.getByRole('button', { name: '上传', exact: true }).click()
  await advanced()
  await page.getByRole('dialog').getByRole('button', { name: '从本机选择', exact: true }).click()
  const uploadPicker = await browseLocal(info.downloads)
  await uploadPicker.getByRole('button', { name: 'queued-upload.txt', exact: true }).click()
  await uploadPicker.getByRole('button', { name: '选择', exact: true }).click()
  // Reopening a file choice must browse its parent, not try to list the file.
  await page.getByRole('dialog').getByRole('button', { name: '从本机选择', exact: true }).click()
  const reopenedUploadPicker = page.getByRole('dialog').last()
  await reopenedUploadPicker.getByRole('button', { name: 'queued-upload.txt', exact: true }).click()
  await reopenedUploadPicker.getByRole('button', { name: '选择', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: '上传', exact: true }).click()
  await page.getByRole('button', { name: 'queued-upload.txt', exact: true }).waitFor()

  // A desktop fallback uses the same server transfer queue, with no path input.
  await fileMenu('研究报告.txt', '下载')
  await advanced()
  await page.getByRole('dialog').getByRole('button', { name: '保存到本机', exact: true }).click()
  const downloadPicker = await browseLocal(info.downloads)
  await screenshot('local-picker-download')
  await downloadPicker.getByRole('button', { name: '选择', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: '下载', exact: true }).click()
  await until(async () => (await readFile(join(info.downloads, '研究报告.txt')).catch(() => null))?.length === 1536, 'desktop queued download')

  await fileMenu('研究报告.txt', '分享管理')
  await page.getByRole('dialog').getByRole('button', { name: '获取内部链接', exact: true }).click()
  await page.getByRole('dialog').getByRole('textbox', { name: '成员与部门', exact: true }).waitFor()
  await screenshot('sharing')
  await page.getByRole('tab', { name: '公开链接', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: '创建公开链接', exact: true }).click()
  await page.getByRole('dialog').getByRole('textbox', { name: '提取码', exact: true }).fill('Test1234')
  await page.getByRole('dialog').getByRole('button', { name: '创建公开链接', exact: true }).last().click()
  await confirm()
  await page.getByRole('dialog').getByRole('textbox', { name: '公开链接', exact: true }).waitFor()
  await closeDialog('分享管理')

  await fileMenu('研究报告.txt', '历史版本')
  await page.getByRole('dialog').getByRole('button', { name: '恢复此版本', exact: true }).last().click()
  await confirm()
  await closeDialog('历史版本')
  await page.getByRole('link', { name: '回收站', exact: true }).click()
  await page.getByRole('cell').getByText('已删除的笔记.txt', { exact: true }).waitFor()
  await page.getByRole('checkbox', { name: '选择此项', exact: true }).first().check()
  await page.getByRole('button', { name: '恢复', exact: true }).click()
  await confirm()
  await completed('recycle_restore')

  await page.getByRole('link', { name: '账号', exact: true }).click()
  await page.getByTestId('add-account').click()
  await page.getByRole('dialog').getByLabel('校园用户名', { exact: true }).fill('verify')
  await page.getByRole('dialog').getByLabel('密码', { exact: true }).fill('fixture-password')
  await page.getByRole('dialog').getByLabel('账号名称', { exact: true }).fill('verified')
  await page.getByRole('dialog').getByRole('button', { name: '登录', exact: true }).click()
  await page.getByRole('dialog').getByLabel('动态口令', { exact: true }).fill('123456')
  await screenshot('verification')
  await page.getByRole('dialog').getByRole('button', { name: '继续', exact: true }).click()
  await page.getByRole('dialog').waitFor({ state: 'hidden' })
  await page.getByText('verified', { exact: true }).first().waitFor()

  await page.getByRole('link', { name: '挂载管理', exact: true }).click()
  await page.getByRole('button', { name: '挂载网盘', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: '浏览…', exact: true }).first().click()
  const mountPicker = await browseLocal(info.downloads)
  await createLocalFolder(mountPicker, 'mount')
  await screenshot('local-picker-mount')
  await mountPicker.getByRole('button', { name: '选择', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: '挂载', exact: true }).click()
  await page.getByText('已挂载', { exact: true }).waitFor()
  await screenshot('mounts')
  await page.getByRole('button', { name: '退出桌面服务', exact: true }).click()
  await confirm()
  await page.getByText('请先完成或取消进行中的操作，并卸载文件系统。', { exact: true }).waitFor()
  assert.equal(child.exitCode, null)
  await page.getByRole('button', { name: '卸载', exact: true }).click()
  await confirm()
  await completed('mount')

  assert.deepEqual(violations, [])
  assert.deepEqual(errors, [])
  assert.equal(requests.every(request => request.origin === origin), true, 'app fetched external assets')
  await page.getByRole('button', { name: '退出桌面服务', exact: true }).click()
  await confirm()
  await page.getByRole('dialog', { name: '服务已退出，请关闭本窗口', exact: true }).waitFor()
  assert.equal((await exited)[0], 0, output)
  console.log('GUI workflows passed: languages, theme, responsive layout, mkdir, upload/download, links, revisions, recycle, OTP, mount, guarded shutdown; no CSP violations or external assets.')
} catch (error) {
  console.error('Workflow failure:', error.message)
  console.error('Fixture output:', output)
  console.error('Page errors:', errors, 'CSP:', violations)
  console.error('Unnonced styles:', styleRecords.filter(style => !style.nonce).map(style => ({ hash: createHash('sha256').update(style.text || '').digest('base64'), text: style.text?.slice(0, 500) })))
  console.error('Requests:', requests.slice(-25))
  if (page) {
    console.error('Inline styles:', await page.locator('style').evaluateAll(nodes => nodes.map(node => ({ nonce: node.nonce, text: node.textContent?.slice(0, 250) }))).catch(() => []))
    console.error('Visible page:', (await page.locator('body').innerText({ timeout: 1000 }).catch(() => 'No body')).slice(0, 6000))
    await Promise.race([screenshot('workflow-failure').catch(() => {}), new Promise(done => setTimeout(done, 1000))])
  }
  throw error
} finally {
  if (child.exitCode === null) {
    child.kill('SIGTERM')
    await Promise.race([exited, new Promise(done => setTimeout(done, 3000))])
    if (child.exitCode === null) child.kill('SIGKILL')
    await exited
  }
  await browser?.close()
  await rm(temporary, { recursive: true, force: true })
}
