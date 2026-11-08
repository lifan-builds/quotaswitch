/**
 * Shared CDP + seeded-Chrome helpers for Google OAuth sign-in (`batch-login.mjs` & `onboard-accounts.mjs`).
 */
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import { createServer } from 'node:http';
import os from 'node:os';
import path from 'node:path';

export const CHROME_BIN = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export function getSignedInChromeEmails() {
  try {
    const prefPath = path.join(
      os.homedir(),
      'Library/Application Support/Google/Chrome/Default/Preferences'
    );
    const data = JSON.parse(fs.readFileSync(prefPath, 'utf8'));
    return new Set(
      (data.account_info || []).map((a) => String(a.email || '').toLowerCase()).filter(Boolean)
    );
  } catch {
    return new Set();
  }
}

export function seedChromeProfile(dir) {
  const srcRoot = path.join(os.homedir(), 'Library/Application Support/Google/Chrome');
  fs.mkdirSync(path.join(dir, 'Default'), { recursive: true });
  for (const f of ['Local State']) {
    const s = path.join(srcRoot, f);
    if (fs.existsSync(s)) fs.copyFileSync(s, path.join(dir, f));
  }
  for (const f of ['Cookies', 'Preferences', 'Secure Preferences']) {
    const s = path.join(srcRoot, 'Default', f);
    if (fs.existsSync(s)) fs.copyFileSync(s, path.join(dir, 'Default', f));
  }
}

export function pickFreePort() {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.on('error', reject);
    s.listen(0, '127.0.0.1', () => {
      const p = s.address().port;
      s.close(() => resolve(p));
    });
  });
}

export async function waitForDebuggerReady(debugPort, timeoutMs = 8000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`http://127.0.0.1:${debugPort}/json/version`);
      if (r.ok) return;
    } catch {}
    await sleep(120);
  }
  throw new Error(`Chrome DevTools port ${debugPort} did not become ready`);
}

export async function launchSeededChrome({
  prefix = 'agy-chrome-',
  offscreen = true,
  windowSize = '1100,820',
} = {}) {
  const debugPort = await pickFreePort();
  const userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  seedChromeProfile(userDataDir);

  const args = [
    `--user-data-dir=${userDataDir}`,
    '--profile-directory=Default',
    `--remote-debugging-port=${debugPort}`,
    '--no-first-run',
    '--no-default-browser-check',
    `--window-size=${windowSize}`,
  ];
  if (offscreen) {
    args.push(
      '--disable-backgrounding-occluded-windows',
      '--disable-renderer-backgrounding',
      '--disable-background-timer-throttling',
      '--window-position=3200,3200'
    );
  }
  args.push('about:blank');

  const proc = spawn(CHROME_BIN, args, { stdio: 'ignore' });
  try {
    await waitForDebuggerReady(debugPort);
  } catch (err) {
    proc.kill('SIGTERM');
    fs.rmSync(userDataDir, { recursive: true, force: true });
    throw err;
  }

  return {
    debugPort,
    userDataDir,
    openTab: async (url) =>
      (
        await fetch(`http://127.0.0.1:${debugPort}/json/new?${encodeURIComponent(url)}`, {
          method: 'PUT',
        })
      ).json(),
    closeTab: async (tabId) => {
      await fetch(`http://127.0.0.1:${debugPort}/json/close/${tabId}`).catch(() => null);
    },
    listTargets: async () => (await fetch(`http://127.0.0.1:${debugPort}/json/list`)).json(),
    cleanup: async () => {
      proc.kill('SIGTERM');
      await sleep(300);
      fs.rmSync(userDataDir, { recursive: true, force: true });
    },
  };
}

export function makeSender(ws) {
  let seq = 1;
  return (method, params = {}) =>
    new Promise((res, rej) => {
      const id = seq++;
      const timer = setTimeout(() => {
        ws.removeEventListener('message', h);
        rej(new Error(`CDP timeout on ${method}`));
      }, 8000);
      const h = (ev) => {
        const m = JSON.parse(ev.data.toString());
        if (m.id === id) {
          clearTimeout(timer);
          ws.removeEventListener('message', h);
          if (m.error) rej(new Error(m.error.message));
          else res(m.result);
        }
      };
      ws.addEventListener('message', h);
      ws.send(JSON.stringify({ id, method, params }));
    });
}

export async function connectCdpSocket(webSocketDebuggerUrl) {
  const ws = new WebSocket(webSocketDebuggerUrl);
  await new Promise((r, rej) => {
    ws.addEventListener('open', r, { once: true });
    ws.addEventListener('error', rej, { once: true });
  });
  return { ws, send: makeSender(ws) };
}

export async function getStableCenter(send, rootNodeId, selector) {
  const q = await send('DOM.querySelectorAll', { nodeId: rootNodeId, selector }).catch(() => null);
  if (!q?.nodeIds?.length) return null;

  for (const nodeId of q.nodeIds) {
    const bm1 = await send('DOM.getBoxModel', { nodeId }).catch(() => null);
    if (!bm1?.model?.border) continue;
    const [ax1, ay1, , , ax3, ay3] = bm1.model.border;
    const aw = ax3 - ax1;
    const ah = ay3 - ay1;
    const acx = Math.round((ax1 + ax3) / 2);
    const acy = Math.round((ay1 + ay3) / 2);
    if (aw < 8 || ah < 8 || acx <= 0 || acx >= 1100 || acy <= 0 || acy >= 820) continue;

    await sleep(70);
    const bm2 = await send('DOM.getBoxModel', { nodeId }).catch(() => null);
    if (!bm2?.model?.border) continue;
    // Reject if element is still sliding horizontally/vertically during Google's card transition
    if (
      Math.abs(bm1.model.border[0] - bm2.model.border[0]) > 2 ||
      Math.abs(bm1.model.border[1] - bm2.model.border[1]) > 2
    ) {
      continue;
    }
    const [x1, y1, , , x3, y3] = bm2.model.border;
    const cx = Math.round((x1 + x3) / 2);
    const cy = Math.round((y1 + y3) / 2);
    if (cx <= 0 || cx >= 1100 || cy <= 0 || cy >= 820 || x3 - x1 < 8 || y3 - y1 < 8) continue;
    return { cx, cy, nodeId };
  }
  return null;
}

export async function clickAt(send, x, y, delayMs = 35) {
  await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y });
  await sleep(delayMs);
  await send('Input.dispatchMouseEvent', {
    type: 'mousePressed',
    x,
    y,
    button: 'left',
    clickCount: 1,
  });
  await sleep(delayMs);
  await send('Input.dispatchMouseEvent', {
    type: 'mouseReleased',
    x,
    y,
    button: 'left',
    clickCount: 1,
  });
}

export async function clickNode(send, rootNodeId, selector) {
  const pos = await getStableCenter(send, rootNodeId, selector);
  if (!pos) return false;
  await clickAt(send, pos.cx, pos.cy);
  return true;
}

export async function submitPasswordChallenge(send, rootNodeId, password) {
  const pwdSel = 'input[type="password"], input[name="Passwd"]';
  if (!(await clickNode(send, rootNodeId, pwdSel))) return false;
  await sleep(60);
  await send('Runtime.evaluate', {
    expression: `(() => {
      const el = Array.from(document.querySelectorAll('input[type="password"], input[name="Passwd"]')).find((e) => {
        const r = e.getBoundingClientRect();
        return r.width >= 8 && r.height >= 8 && r.left >= 0 && r.left < window.innerWidth;
      });
      if (el && el.value) el.value = '';
    })()`,
    returnByValue: true,
  }).catch(() => null);

  for (const ch of password) {
    await send('Input.insertText', { text: ch });
    await sleep(14);
  }

  const lenCheck = await send('Runtime.evaluate', {
    expression: `(() => {
      const el = Array.from(document.querySelectorAll('input[type="password"], input[name="Passwd"]')).find((e) => {
        const r = e.getBoundingClientRect();
        return r.width >= 8 && r.height >= 8 && r.left >= 0 && r.left < window.innerWidth;
      });
      return el ? el.value.length : 0;
    })()`,
    returnByValue: true,
  }).catch(() => null);

  if ((lenCheck?.result?.value || 0) !== password.length) return false;
  await sleep(60);
  await clickNode(send, rootNodeId, '#passwordNext button');
  return true;
}
