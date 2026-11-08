#!/usr/bin/env node
/**
 * Fast, BotGuard-safe batch OAuth ingestion for agy-auth on macOS.
 *
 * Optimizations:
 * - Single persistent off-screen Chrome instance across the entire batch (no cold-start per account).
 * - Opens a fresh tab per account via CDP HTTP `/json/new` and closes it when done.
 * - Ephemeral local callback port (0) so concurrent/stale runs never collide.
 * - Fast 350ms state polling after initial BotGuard settle, plus box-position stability & value-length verification on `/challenge/pwd` so slide-in animations never drop keystrokes or hang.
 *
 * Usage:
 *   AGY_LOGIN_PASS='<password>' node batch-login.mjs <email-or-alias=email> [...]
 *   node batch-login.mjs --password-env AGY_PASSWORD <email-or-alias=email> [...]
 */
import { execSync, spawn } from 'node:child_process';
import { createServer } from 'node:http';
import {
  clickAt,
  clickNode,
  connectCdpSocket,
  getSignedInChromeEmails,
  launchSeededChrome,
  sleep,
  submitPasswordChallenge,
} from './cdp-google-auth.mjs';

function parseCli(argv) {
  let passEnv = 'AGY_LOGIN_PASS';
  let force = false;
  const rawAccounts = [];
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--password-env' && argv[i + 1]) {
      passEnv = argv[++i];
    } else if (a.startsWith('--password-env=')) {
      passEnv = a.slice('--password-env='.length);
    } else if (a === '--force') {
      force = true;
    } else if (!a.startsWith('-')) {
      rawAccounts.push(a);
    }
  }
  const password = process.env[passEnv] || process.env.AGY_LOGIN_PASS || process.env.AGY_PASSWORD || '';
  const accounts = rawAccounts.map((raw) => {
    if (raw.includes('=')) {
      const [alias, email] = raw.split('=', 2);
      return { alias: alias.trim(), email: email.trim().toLowerCase() };
    }
    const email = raw.trim().toLowerCase();
    return { alias: email.split('@')[0], email };
  });
  return { password, force, accounts };
}

function startAgyLogin(alias, email, redirectUri) {
  return new Promise((resolve, reject) => {
    const child = spawn(
      process.env.QUOTASWITCH_BIN || 'quotaswitch',
      ['login', '--alias', alias, '--login-hint', email, '--no-activate', '--no-browser'],
      {
        stdio: ['pipe', 'pipe', 'pipe'],
        env: { ...process.env, QUOTASWITCH_REDIRECT_URI: redirectUri, QUOTASWITCH_NO_UPDATE_CHECK: '1' },
      }
    );
    let out = '';
    let err = '';
    let resolvedUrl = false;

    child.stdout.on('data', (chunk) => {
      out += chunk.toString();
      if (!resolvedUrl) {
        const m = out.match(/https:\/\/accounts\.google\.com\/o\/oauth2\/auth\S+/);
        if (m && out.includes('Paste the authorization code')) {
          resolvedUrl = true;
          resolve({
            url: m[0],
            submitCallback: (callbackUrl) =>
              new Promise((resDone) => {
                child.on('close', (exitCode) => resDone({ exitCode, out, err }));
                child.stdin.write(callbackUrl + '\n');
                child.stdin.end();
              }),
            kill: () => child.kill(),
          });
        }
      }
    });
    child.stderr.on('data', (chunk) => {
      err += chunk.toString();
    });
    child.on('error', reject);
    child.on('close', (code) => {
      if (!resolvedUrl) reject(new Error(`quotaswitch exited early (${code}): ${out} ${err}`));
    });
  });
}

const { password: PASSWORD, force: FORCE, accounts: parsed } = parseCli(process.argv.slice(2));
if (parsed.length === 0) {
  console.error('Usage: AGY_LOGIN_PASS="<pass>" node batch-login.mjs [--force] <email-or-alias=email> [...]');
  process.exit(1);
}

const qsBin = process.env.QUOTASWITCH_BIN || 'quotaswitch';
const signedInSet = getSignedInChromeEmails();
const existingOut = FORCE ? '' : execSync(`${qsBin} list --no-usage`, { encoding: 'utf8' });
const todo = parsed
  .filter(({ email }) => {
    if (!FORCE && existingOut.includes(`(${email})`)) {
      console.log(`Skipping ${email} (already registered)`);
      return false;
    }
    return true;
  })
  .sort((a, b) => Number(signedInSet.has(b.email)) - Number(signedInSet.has(a.email)));

if (todo.length === 0) {
  console.log('All requested accounts are already registered.');
  process.exit(0);
}

let callbackResolver = null;
const server = createServer((req, res) => {
  const addr = server.address();
  const reqUrl = new URL(req.url, `http://127.0.0.1:${addr.port}`);
  if (reqUrl.pathname === '/oauth-callback') {
    const fullUrl = `http://localhost:${addr.port}/oauth-callback${reqUrl.search}`;
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end('<html><body><h2>OK</h2></body></html>');
    if (callbackResolver) {
      const resolve = callbackResolver;
      callbackResolver = null;
      resolve(fullUrl);
    }
    return;
  }
  res.writeHead(404);
  res.end();
});

await new Promise((r) => server.listen(0, '127.0.0.1', r));
const redirectUri = `http://localhost:${server.address().port}/oauth-callback`;

const chrome = await launchSeededChrome({
  prefix: 'agy-batch-chrome-',
  offscreen: true,
  windowSize: '1100,820',
});

const batchStart = Date.now();
const failures = [];

try {
  for (const { alias, email } of todo) {
    const t0 = Date.now();
    const inSession = signedInSet.has(email);
    console.log(`\n=== Logging in ${email} (alias: ${alias}, inChromeSession: ${inSession}) ===`);
    const session = await startAgyLogin(alias, email, redirectUri);

    let callbackUrl = null;
    new Promise((r) => {
      callbackResolver = r;
    }).then((url) => {
      callbackUrl = url;
    });

    const tab = await chrome.openTab(session.url);
    let accountErr = null;
    try {
      // Wait briefly before attaching CDP so BotGuard's initial identifier VM finishes undisturbed
      await sleep(inSession ? 1600 : 2600);

      if (!callbackUrl) {
        const { ws, send } = await connectCdpSocket(tab.webSocketDebuggerUrl);

        let lastLoggedUrl = '';
        let passwordTypedAt = 0;
        let anchorClicked = false;
        const deadline = Date.now() + 45000;

        while (!callbackUrl && Date.now() < deadline) {
          const curTargets = await chrome.listTargets();
          const curPage =
            curTargets.find((x) => x.id === tab.id) ||
            curTargets.find((x) => x.type === 'page' && x.url.startsWith('http'));
          const curUrl = curPage?.url || '';

          if (curUrl && curUrl !== lastLoggedUrl) {
            console.log(`  [+${((Date.now() - t0) / 1000).toFixed(1)}s] ${curUrl.split('?')[0]}`);
            lastLoggedUrl = curUrl;
          }

          const doc = await send('DOM.getDocument', {}).catch(() => null);
          if (!doc?.root?.nodeId) {
            await sleep(250);
            continue;
          }

          if (curUrl.includes('accountchooser')) {
            const clicked = await clickNode(
              send,
              doc.root.nodeId,
              `[data-identifier="${email}"], [data-email="${email}"]`
            );
            await sleep(clicked ? 450 : 250);
            continue;
          }

          if (curUrl.includes('/signin/identifier')) {
            await clickNode(send, doc.root.nodeId, '#identifierNext button');
            await sleep(450);
            continue;
          }

          if (curUrl.includes('/challenge/recaptcha') && !anchorClicked) {
            await sleep(6200);
            const freshTargets = await chrome.listTargets();
            const anchorTarget = freshTargets.find(
              (t) => t.url.includes('recaptcha') && t.url.includes('anchor')
            );
            if (anchorTarget) {
              const { ws: aws, send: asend } = await connectCdpSocket(
                anchorTarget.webSocketDebuggerUrl
              );
              await clickAt(asend, 28, 38, 50);
              await sleep(1200);
              await asend('Runtime.evaluate', {
                expression: 'document.querySelector("#recaptcha-anchor")?.click()',
                returnByValue: true,
              });
              aws.close();
              anchorClicked = true;
              await sleep(2000);
            }
            continue;
          }

          if (curUrl.includes('/challenge/pwd')) {
            if (!PASSWORD) {
              throw new Error(
                `Google requested password for ${email} (/challenge/pwd), but AGY_LOGIN_PASS is empty`
              );
            }
            if (!passwordTypedAt || Date.now() - passwordTypedAt > 3500) {
              if (await submitPasswordChallenge(send, doc.root.nodeId, PASSWORD)) {
                passwordTypedAt = Date.now();
                await sleep(450);
                continue;
              }
            }
          }

          if (
            curUrl.includes('/consent') ||
            curUrl.includes('/oauth/id') ||
            curUrl.includes('/speedbump') ||
            curUrl.includes('/firstparty/nativeapp')
          ) {
            await send('Runtime.evaluate', {
              expression: `(() => {
                const isVisible = (el) => {
                  if (!el) return false;
                  const s = getComputedStyle(el);
                  if (s.display === 'none' || s.visibility === 'hidden' || s.opacity === '0') return false;
                  const r = el.getBoundingClientRect();
                  return r.width > 0 && r.height > 0;
                };
                const buttons = Array.from(document.querySelectorAll('button, div[role="button"], input[type="submit"]')).filter(isVisible);
                for (const b of buttons) {
                  const txt = (b.innerText || b.value || '').trim();
                  if (/^(continue|allow|sign in|i understand|not now|skip|继续|允许|登录|我了解|确认)$/i.test(txt)) {
                    b.click();
                    return txt;
                  }
                }
                return null;
              })()`,
              returnByValue: true,
            }).catch(() => null);
            await sleep(350);
            continue;
          }

          await sleep(300);
        }

        if (!callbackUrl) {
          const txt = await send('Runtime.evaluate', {
            expression: 'document.body.innerText',
            returnByValue: true,
          }).catch(() => null);
          console.log(
            `  [stuck] ${String(txt?.result?.value || '').replace(/\s+/g, ' ').slice(0, 350)}`
          );
        }
        ws.close();
      }
    } catch (err) {
      accountErr = err?.message || String(err);
    } finally {
      await chrome.closeTab(tab.id);
    }

    if (!callbackUrl) {
      session.kill();
      console.error(`FAILED ${email}: ${accountErr || 'timed out waiting for OAuth callback'}`);
      failures.push(email);
      continue;
    }

    const done = await session.submitCallback(callbackUrl);
    const elapsed = ((Date.now() - t0) / 1000).toFixed(1);
    console.log(`  [done in ${elapsed}s] ${done.out.trim().split('\n').pop()}`);
    if (done.exitCode !== 0) {
      console.error(`FAILED ${email}: ${done.err}`);
      failures.push(email);
    }
  }

  const totalSec = ((Date.now() - batchStart) / 1000).toFixed(1);
  console.log(
    `\nBatch finished ${todo.length - failures.length}/${todo.length} account(s) in ${totalSec}s.`
  );
  if (failures.length) {
    console.error(`Failed accounts: ${failures.join(', ')}`);
    process.exitCode = 1;
  }
} finally {
  server.close();
  await chrome.cleanup();
}
