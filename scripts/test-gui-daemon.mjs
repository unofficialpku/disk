// Production Linux executable: detached lifetime, terminal service-loss overlay and crash handling.
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { mkdtemp, mkdir, readFile, writeFile, stat, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { loadPlaywright, until, takeScreenshot } from './lib/gui-test.mjs'

if (process.platform !== 'linux' || !process.argv[2]) throw new Error('Usage: node scripts/test-gui-daemon.mjs BINARY [SCREENSHOTS]')
const { chromium } = loadPlaywright()
const binary = resolve(process.argv[2])
const screenshots = process.argv[3] && resolve(process.argv[3])
const temporary = await mkdtemp(join(tmpdir(), 'pkudisk-gui-daemon-'))
const helpers = join(temporary, 'helpers')
const config = join(temporary, 'config')
const state = join(temporary, 'state')
const capture = join(temporary, 'url')
const metadata = join(config, 'pkudisk', 'gui', 'instance.json')
await mkdir(helpers, { mode: 0o700 })
await mkdir(config, { mode: 0o700 })
await writeFile(join(helpers, 'xdg-open'), '#!/bin/sh\numask 077\nprintf "%s" "$1" > "$PKUDISK_TEST_URL"\n', { mode: 0o700 })
const env = { ...process.env, PATH: `${helpers}:${process.env.PATH}`, XDG_CONFIG_HOME: config, XDG_STATE_HOME: state, DISPLAY: ':gui-test', WAYLAND_DISPLAY: '', DBUS_SESSION_BUS_ADDRESS: '', PKUDISK_DESKTOP_LAUNCH: '', PKUDISK_TEST_URL: capture }
const servicePIDs = new Set()
let browser
async function invoke(overrides = {}, args = ['gui']) {
  const child = spawn(binary, args, { env: { ...env, ...overrides }, stdio: ['ignore', 'pipe', 'pipe'] })
  let output = ''
  child.stdout.on('data', data => { output += data })
  child.stderr.on('data', data => { output += data })
  const timeout = setTimeout(() => child.kill('SIGTERM'), 12000)
  const [code] = await once(child, 'close')
  clearTimeout(timeout)
  const pid = Number(output.match(/background \(PID (\d+)\)/)?.[1])
  if (pid) servicePIDs.add(pid)
  assert.equal(output.includes('ticket='), false)
  return { code, pid, output }
}
async function noMetadata() { return stat(metadata).then(() => false, () => true) }
async function quit(page) {
  await page.getByRole('button', { name: '退出桌面服务', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: '确认', exact: true }).click()
}

try {
  const first = await invoke()
  assert.equal(first.code, 0, first.output)
  assert.ok(first.pid, first.output)
  const processStat = (await readFile(`/proc/${first.pid}/stat`, 'utf8')).split(') ')[1].split(' ')
  assert.equal(Number(processStat[3]), first.pid, 'service must own a new OS session')
  assert.equal(Number(processStat[4]), 0, 'service must have no controlling terminal')
  assert.equal((await stat(join(state, 'pkudisk', 'logs', 'desktop.log'))).mode & 0o777, 0o600)
  assert.ok(first.output.includes('Stop: pkudisk gui stop'))
  assert.equal(await stat(join(config, 'pkudisk', 'gui', 'desktop.log')).then(() => true, () => false), false)
  const firstURL = await readFile(capture, 'utf8')
  const second = await invoke()
  assert.equal(second.code, 0, second.output)
  assert.ok(second.output.includes('existing GUI instance'))
  const secondURL = await readFile(capture, 'utf8')
  assert.notEqual(firstURL, secondURL)
  assert.equal(new URL(firstURL).origin, new URL(secondURL).origin)

  browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined, args: ['--no-sandbox'] })
  const context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1200, height: 850 } })
  const page = await context.newPage()
  page.setDefaultTimeout(10000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  await page.goto(secondURL)
  const stoppedTitle = '服务已退出，请关闭本窗口'
  await page.getByRole('main').waitFor()
  assert.equal(await page.getByText('已连接本机服务', { exact: true }).count(), 0)
  // A declarative login dialog must not remain interactive after service loss.
  await page.getByRole('button', { name: '添加账号', exact: true }).first().click()
  await page.getByRole('dialog').waitFor()
  await context.setOffline(true)
  const stopped = page.getByRole('dialog', { name: stoppedTitle, exact: true })
  await stopped.waitFor()
  await until(async () => await page.getByRole('dialog').count() === 1, 'business dialog closed')
  assert.equal(await page.getByRole('main').count(), 0)
  assert.equal(await page.getByRole('button').count(), 0, 'stopped page retained actions')
  assert.equal(await page.evaluate(() => sessionStorage.getItem('pkudisk-gui-session')), null)
  await page.keyboard.press('Escape')
  await page.mouse.click(5, 5)
  await page.keyboard.press('Tab')
  assert.equal(await stopped.isVisible(), true, 'exit overlay was dismissible')
  assert.equal(await stopped.evaluate(element => element.contains(document.activeElement)), true, 'focus escaped the overlay')
  const requests = []
  page.on('request', request => { if (new URL(request.url()).pathname.startsWith('/api/')) requests.push(request.url()) })
  await context.setOffline(false)
  // A confirmed disconnection ends this window even if the old server returns.
  // Wait beyond a heartbeat interval to verify no stale page requests resume.
  await page.waitForTimeout(3000)
  assert.equal(await stopped.isVisible(), true)
  assert.deepEqual(requests, [], 'stopped page continued making API requests')
  if (screenshots) {
    await mkdir(screenshots, { recursive: true })
    await takeScreenshot(page, screenshots, 'service-exited-zh')
  }

  // A fresh activation still works. Test a real process exit in English with
  // a programmatic modal open, ensuring it cannot sit above the exit overlay.
  assert.equal((await invoke()).code, 0)
  const english = await browser.newContext({ locale: 'en', viewport: { width: 390, height: 844 } })
  const interrupted = await english.newPage()
  interrupted.on('pageerror', error => errors.push(error.message))
  await interrupted.goto(await readFile(capture, 'utf8'))
  await interrupted.getByRole('main').waitFor()
  // The mobile sidebar is initially collapsed; use desktop width to open Quit.
  await interrupted.setViewportSize({ width: 1200, height: 850 })
  await interrupted.getByRole('button', { name: 'Quit desktop service', exact: true }).click()
  await interrupted.getByRole('dialog').waitFor()
  process.kill(first.pid, 'SIGTERM')
  await until(noMetadata, 'service cleanup after SIGTERM')
  servicePIDs.delete(first.pid)
  const exited = interrupted.getByRole('dialog', { name: 'The service has exited. Please close this window.', exact: true })
  await exited.waitFor()
  await until(async () => await interrupted.getByRole('dialog').count() === 1, 'quit modal closed')
  assert.equal(await interrupted.getByRole('button').count(), 0)
  assert.equal(await interrupted.getByRole('main').count(), 0)
  await interrupted.keyboard.press('Escape')
  assert.equal(await exited.isVisible(), true)
  await interrupted.setViewportSize({ width: 390, height: 844 })
  assert.equal(await interrupted.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
  if (screenshots) await takeScreenshot(interrupted, screenshots, 'service-exited-en-mobile')
  const restarted = await invoke()
  assert.equal(restarted.code, 0, restarted.output)
  assert.ok(restarted.pid)
  const reopened = await context.newPage()
  await reopened.goto(await readFile(capture, 'utf8'))
  await reopened.getByRole('main').waitFor()
  await quit(reopened)
  await reopened.getByRole('dialog', { name: stoppedTitle, exact: true }).waitFor()
  await until(noMetadata, 'service cleanup after GUI exit')
  servicePIDs.delete(restarted.pid)
  const cliInstance = await invoke()
  assert.equal(cliInstance.code, 0)
  const stopResult = await invoke({}, ['gui', 'stop'])
  assert.equal(stopResult.code, 0, stopResult.output)
  assert.ok(stopResult.output.includes('GUI stopped.'))
  assert.equal(await noMetadata(), true)
  servicePIDs.delete(cliInstance.pid)
  const stopAgain = await invoke({}, ['gui', 'stop'])
  assert.equal(stopAgain.code, 0)
  assert.ok(stopAgain.output.includes('GUI is not running.'))
  const failed = await invoke({ DISPLAY: '', WAYLAND_DISPLAY: '' })
  assert.equal(failed.code, 1, failed.output)
  assert.ok(failed.output.includes('GUI unavailable'), failed.output)
  assert.equal(await noMetadata(), true)
  assert.deepEqual(errors, [])
  console.log('Daemon tests passed: parent exits, session detaches, private logs, repeat activation, heartbeat, non-dismissible exit overlay, blocked stale requests, binary exit detection, restart, GUI exit and startup failure cleanup.')
} finally {
  for (const pid of servicePIDs) { try { process.kill(pid, 'SIGTERM') } catch {} }
  await browser?.close()
  await until(noMetadata, 'final service cleanup').catch(() => {})
  await rm(temporary, { recursive: true, force: true })
}
