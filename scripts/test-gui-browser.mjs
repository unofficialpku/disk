// Linux browser smoke test against the built executable. Requires an existing
// Playwright installation and Chromium; neither is a runtime/build dependency.
// PLAYWRIGHT_MODULE may name an absolute playwright(-core) entry point.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { createServer } from "node:http";
import { mkdtemp, mkdir, writeFile, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve, join } from "node:path";
import { loadPlaywright, until, takeScreenshot } from "./lib/gui-test.mjs";

if (process.platform !== "linux" || !process.argv[2]) {
  throw new Error("Usage (Linux): node scripts/test-gui-browser.mjs BINARY [SCREENSHOT-DIRECTORY]");
}
const { chromium } = loadPlaywright()
const binary = resolve(process.argv[2]);
const screenshots = process.argv[3] ? resolve(process.argv[3]) : null;
const temporary = await mkdtemp(join(tmpdir(), "pkudisk-gui-browser-"));
const helpers = join(temporary, "helpers");
const config = join(temporary, "config");
const capture = join(temporary, "browser-url");
await mkdir(helpers, { mode: 0o700 });
await mkdir(config, { mode: 0o700 });
// Replace only the desktop opener. The actual binary, listener, auth protocol,
// single-instance logic, embedded assets, and browser JavaScript all run.
await writeFile(join(helpers, "xdg-open"),
  '#!/bin/sh\numask 077\nprintf "%s" "$1" > "$PKUDISK_TEST_URL"\n', { mode: 0o700 });
const env = {
  ...process.env,
  PATH: `${helpers}:${process.env.PATH}`,
  XDG_CONFIG_HOME: config,
  XDG_STATE_HOME: join(temporary, 'state'),
  DBUS_SESSION_BUS_ADDRESS: "",
  DISPLAY: ":gui-test",
  WAYLAND_DISPLAY: "",
  PKUDISK_TEST_URL: capture,
  PKUDISK_DESKTOP_LAUNCH: "",
};
let output = "";
const child = spawn(binary, ["gui", "--foreground"], { env, stdio: ["ignore", "pipe", "pipe"] });
child.stdout.on("data", (data) => { output += data; });
child.stderr.on("data", (data) => { output += data; });
const exited = once(child, "exit");
const attacker = createServer((request, response) => {
  response.setHeader("Content-Type", "text/html");
  response.end("<!doctype html><title>Foreign origin test</title>");
});
let browser;
const responses = [];

async function capturedURL(previous = "") {
  return until(async () => {
    if (child.exitCode !== null) throw new Error(`GUI exited early: ${output}`);
    const value = await readFile(capture, "utf8").catch(() => "");
    return value && value !== previous ? value : null;
  });
}

try {
  const firstURL = await capturedURL();
  const origin = new URL(firstURL).origin;
  browser = await chromium.launch({
    headless: true,
    executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined,
    args: ["--no-sandbox"],
  });
  const context = await browser.newContext({ locale: "zh-CN", viewport: { width: 1100, height: 850 } });
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  page.on("response", (response) => responses.push({ path: new URL(response.url()).pathname, status: response.status() }));
  const errors = [];
  page.on("pageerror", (error) => errors.push(error));
  await page.goto(firstURL);
  await page.getByRole("main").waitFor();
  assert.equal(new URL(page.url()).hash, "");
  assert.equal(await page.evaluate(() => JSON.stringify(history.state).includes('ticket=')), false);
  const session = await page.evaluate(() => sessionStorage.getItem("pkudisk-gui-session"));
  assert.equal(session.length, 43);
  const status = await context.request.get(`${origin}/api/status`, { headers: { Authorization: `Bearer ${session}` } });
  assert.deepEqual((await status.json()).flags, { isMobile: false, canMount: true, canBrowseLocalFS: true });
  assert.equal((await context.cookies()).some(cookie => /auth|session|token/i.test(cookie.name) || cookie.value === session), false);
  await page.reload();
  await page.getByRole("main").waitFor();

  // Empty accounts should have one clear action, both in the file onboarding
  // state and on the account management page. The sidebar footer stays compact.
  await page.getByRole("button", { name: "添加账号", exact: true }).waitFor();
  assert.equal(await page.getByRole("button", { name: "添加账号", exact: true }).count(), 1);
  assert.equal(await page.getByRole("button", { name: "上传", exact: true }).count(), 0);
  async function checkSidebarFooter() {
    const quit = await page.getByRole("button", { name: "退出桌面服务", exact: true }).boundingBox();
    const version = await page.getByTestId("sidebar-version").filter({ visible: true }).boundingBox();
    assert.ok(quit && version, "Sidebar quit action and version are visible");
    assert.ok(version.x >= quit.x + quit.width, "Version follows quit on the right");
    assert.ok(Math.abs(quit.y + quit.height / 2 - version.y - version.height / 2) < 2,
      "Version shares the quit action's row");
  }
  await checkSidebarFooter();
  await page.getByRole("button", { name: "收起侧边栏", exact: true }).click();
  await page.getByTestId("sidebar-version").waitFor({ state: "hidden" });
  assert.ok(await page.getByRole("button", { name: "退出桌面服务", exact: true }).isVisible());
  await page.getByRole("button", { name: "展开侧边栏", exact: true }).click();
  await page.getByTestId("sidebar-version").waitFor();
  await page.getByRole("link", { name: "账号", exact: true }).click();
  await page.getByRole("main", { name: "我的账号", exact: true }).waitFor();
  assert.equal(await page.getByRole("button", { name: "添加账号", exact: true }).count(), 1);
  await page.getByRole("button", { name: "添加账号", exact: true }).click();
  await page.getByRole("dialog", { name: "连接 PKUDisk", exact: true }).waitFor();
  await page.keyboard.press("Escape");
  await page.getByRole("dialog").waitFor({ state: "hidden" });
  if (screenshots) await takeScreenshot(page, screenshots, "gui-empty-accounts");
  await page.getByRole("link", { name: "文件", exact: true }).click();
  await page.getByRole("main", { name: "我的文件", exact: true }).waitFor();
  if (screenshots) {
    await mkdir(screenshots, { recursive: true });
    await takeScreenshot(page, screenshots, "gui-desktop");
    await page.setViewportSize({ width: 390, height: 844 });
    await takeScreenshot(page, screenshots, "gui-mobile");
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    await page.getByRole("button", { name: "打开侧边栏", exact: true }).click();
    await page.getByTestId("sidebar-version").filter({ visible: true }).waitFor();
    await checkSidebarFooter();
    await takeScreenshot(page, screenshots, "gui-mobile-sidebar");
    await page.getByRole("link", { name: "账号", exact: true }).click();
    await page.getByRole("main", { name: "我的账号", exact: true }).waitFor();
    assert.equal(await page.getByRole("button", { name: "添加账号", exact: true }).count(), 1);
    await takeScreenshot(page, screenshots, "gui-mobile-empty-accounts");
  }
  const fresh = await browser.newContext({ locale: "zh-CN" });
  const replay = await fresh.newPage();
  await replay.goto(firstURL);
  await replay.getByRole("status").filter({ hasText: "请重新打开界面" }).waitFor();
  assert.equal(new URL(replay.url()).hash, "");
  assert.equal((await fresh.request.get(`${origin}/api/status`)).status(), 401);

  // Even a foreign page given a valid bearer token cannot submit a mutating
  // request through CORS. A simple no-CORS request also has no authority.
  attacker.listen(0, "127.0.0.1");
  await once(attacker, "listening");
  const evil = await fresh.newPage();
  await evil.goto(`http://127.0.0.1:${attacker.address().port}`);
  const cors = await evil.evaluate(async ({ origin, session }) => {
    try {
      await fetch(`${origin}/api/quit`, { method: "POST", headers: { Authorization: `Bearer ${session}` } });
      return "allowed";
    } catch { return "blocked"; }
  }, { origin, session });
  assert.equal(cors, "blocked");
  await evil.evaluate(async (origin) => {
    try { await fetch(`${origin}/api/quit`, { method: "POST", mode: "no-cors" }); } catch { /* Browser may also block it. */ }
  }, origin);
  assert.equal((await context.request.get(`${origin}/api/status`, {
    headers: { Authorization: `Bearer ${session}` },
  })).status(), 200);

  const second = spawn(binary, ["gui"], { env, stdio: "ignore" });
  assert.equal((await once(second, "exit"))[0], 0);
  const secondURL = await capturedURL(firstURL);
  assert.equal(new URL(secondURL).origin, origin);
  const reopened = await fresh.newPage();
  await reopened.goto(secondURL);
  await reopened.getByRole("main").waitFor();
  await page.close();
  assert.equal((await context.request.get(`${origin}/api/status`, {
    headers: { Authorization: `Bearer ${session}` },
  })).status(), 200);
  await reopened.getByRole("button", { name: "退出桌面服务" }).click();
  await reopened.getByRole("dialog").getByRole("button", { name: "确认", exact: true }).click();
  await reopened.getByRole("dialog", { name: "服务已退出，请关闭本窗口", exact: true }).waitFor();
  assert.equal((await exited)[0], 0);
  assert.equal(await stat(join(config, "pkudisk", "gui", "instance.json")).then(() => true, () => false), false);
  assert.equal(output.includes("ticket="), false);
  assert.deepEqual(errors, []);
  console.log("Browser smoke test passed: login, fragment cleanup, reload, replay, CSRF, activation, residency, exit.");
} catch (error) {
  console.error("HTTP responses:", responses);
  for (const context of browser?.contexts() || []) {
    for (const page of context.pages()) {
      console.error("Visible status:", await page.getByRole("status").allTextContents().catch(() => []));
      if (screenshots) {
        await mkdir(screenshots, { recursive: true });
        await takeScreenshot(page, screenshots, "gui-failure").catch(() => {});
      }
    }
  }
  throw error;
} finally {
  if (child.exitCode === null) {
    child.kill("SIGTERM");
    await Promise.race([exited, new Promise((done) => setTimeout(done, 2000))]);
    if (child.exitCode === null) child.kill("SIGKILL");
    await exited;
  }
  await browser?.close();
  if (attacker.listening) await new Promise((done) => attacker.close(done));
  await rm(temporary, { recursive: true, force: true });
}
