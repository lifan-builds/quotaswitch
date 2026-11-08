#!/usr/bin/env node
/**
 * Semi-automated onboarding helper for agy-auth accounts blocked by Google's
 * HTTP 403 `VALIDATION_REQUIRED` (`/v3/signin/challenge/iap/qrcode`).
 *
 * For each unactivated account, this script:
 * 1. Refreshes its OAuth token and fetches the `validation_url` from `:retrieveUserQuotaSummary`.
 * 2. Automatically signs in with the account's email & password and dismisses passkey/selfie prompts
 *    so the browser lands directly on the "Scan the QR code with your phone" screen.
 * 3. Waits for the user to scan the QR code (until the tab reaches `auth_success_gemini` or quota returns 200),
 *    then automatically advances to the next unactivated account.
 *
 * Usage:
 *   AGY_LOGIN_PASS='<password>' node onboard-accounts.mjs [email...]
 */
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  clickAt,
  clickNode,
  connectCdpSocket,
  launchSeededChrome,
  sleep,
  submitPasswordChallenge,
} from './cdp-google-auth.mjs';

const PASSWORD = process.env.AGY_LOGIN_PASS || process.env.AGY_PASSWORD || '';
const filterEmails = new Set(
  process.argv
    .slice(2)
    .filter((a) => !a.startsWith('-'))
    .map((a) => a.trim().toLowerCase())
);

function discoverOAuthClient() {
  const lsBuf = fs.readFileSync('/Applications/Antigravity.app/Contents/Resources/bin/language_server');
  const lsStr = lsBuf.toString('latin1');
  const clientId = lsStr.match(/1071006060591-[a-z0-9]+\.apps\.googleusercontent\.com/)[0];
  let clientSecret = '';
  for (const m of lsStr.matchAll(/GOCSPX-[A-Za-z0-9_-]{13,80}/g)) {
    const run = m[0];
    for (let len = 20; len <= run.length; len++) {
      const cand = run.slice(0, len);
      if (
        createHash('sha256').update(cand).digest('hex') ===
        '1d2f041093fd95aa8995a038c711d50a7960da09a505381c09a745d6ad0ecc60'
      ) {
        clientSecret = cand;
        break;
      }
    }
    if (clientSecret) break;
  }
  return { clientId, clientSecret };
}

async function refreshAccessToken(clientId, clientSecret, refreshToken) {
  const r = await fetch('https://oauth2.googleapis.com/token', {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({
      client_id: clientId,
      client_secret: clientSecret,
      refresh_token: refreshToken,
      grant_type: 'refresh_token',
    }),
  });
  const d = await r.json();
  if (!d.access_token) throw new Error(`OAuth refresh failed: ${JSON.stringify(d)}`);
  return d.access_token;
}

async function checkQuotaOrValidationUrl(accessToken) {
  const r = await fetch('https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary', {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${accessToken}`,
      'Content-Type': 'application/json',
      'User-Agent': 'antigravity/2.28.2 darwin/arm64',
    },
    body: '{}',
  });
  const data = await r.json().catch(() => ({}));
  let validationUrl = '';
  for (const d of data?.error?.details || []) {
    if (d.metadata?.validation_url) validationUrl = d.metadata.validation_url;
  }
  return { status: r.status, validationUrl };
}

const { clientId, clientSecret } = discoverOAuthClient();
const exportDir = fs.mkdtempSync(path.join(os.tmpdir(), 'agy-onboard-export-'));
const candidates = [];

try {
  execFileSync('agy-auth', ['export', '--dir', exportDir], { stdio: 'ignore' });
  for (const f of fs.readdirSync(exportDir).sort()) {
    if (!f.endsWith('.json')) continue;
    const email = f.split('-acct_')[0].toLowerCase();
    if (filterEmails.size > 0 && !filterEmails.has(email)) continue;
    const tok = JSON.parse(fs.readFileSync(path.join(exportDir, f), 'utf8'));
    const accessToken = await refreshAccessToken(clientId, clientSecret, tok.token.refresh_token);
    const { status, validationUrl } = await checkQuotaOrValidationUrl(accessToken);
    if (status === 403 && validationUrl) {
      candidates.push({ email, refreshToken: tok.token.refresh_token, validationUrl });
    } else {
      console.log(`[OK] ${email} is already onboarded (HTTP ${status})`);
    }
  }
} finally {
  fs.rmSync(exportDir, { recursive: true, force: true });
}

if (candidates.length === 0) {
  console.log('All accounts are already onboarded!');
  process.exit(0);
}

console.log(`\nFound ${candidates.length} account(s) requiring one-time phone QR verification:`);
for (const c of candidates) console.log(`  - ${c.email}`);

// Open a VISIBLE window so the user can scan the QR code once the script auto-logs in
const chrome = await launchSeededChrome({
  prefix: 'agy-onboard-chrome-',
  offscreen: false,
  windowSize: '980,820',
});

try {
  for (const { email, refreshToken } of candidates) {
    const at = await refreshAccessToken(clientId, clientSecret, refreshToken);
    const { status, validationUrl } = await checkQuotaOrValidationUrl(at);
    if (status === 200 || !validationUrl) {
      console.log(`[OK] ${email} is already onboarded!`);
      continue;
    }

    const cleanVal = validationUrl.replace(/&authuser$/, '');
    const startUrl = `https://accounts.google.com/AccountChooser?Email=${encodeURIComponent(email)}&continue=${encodeURIComponent(cleanVal)}`;
    console.log(`\n=== Preparing ${email} (auto-signing in to reach QR screen) ===`);

    const tab = await chrome.openTab(startUrl);
    try {
      await sleep(2400);
      const { ws, send } = await connectCdpSocket(tab.webSocketDebuggerUrl);

      let pwdTypedAt = 0;
      let loggedQr = false;
      const deadline = Date.now() + 180000; // 3 minutes per QR scan

      while (Date.now() < deadline) {
        const curTargets = await chrome.listTargets();
        const curPage = curTargets.find((x) => x.id === tab.id);
        const curUrl = curPage?.url || '';

        if (curUrl.startsWith('https://developers.google.com/')) {
          console.log(`  [SUCCESS] ${email} verified and onboarded!`);
          await sleep(1200);
          break;
        }

        const doc = await send('DOM.getDocument', {}).catch(() => null);
        if (!doc?.root?.nodeId) {
          await sleep(300);
          continue;
        }

        if (curUrl.includes('accountchooser')) {
          await clickNode(send, doc.root.nodeId, `[data-identifier="${email}"], [data-email="${email}"]`);
          await sleep(450);
          continue;
        }

        if (curUrl.includes('/signin/confirmidentifier')) {
          await send('Runtime.evaluate', {
            expression: `(() => {
              const b = Array.from(document.querySelectorAll('#identifierNext button, button')).find(e => /^(next|continue)$/i.test((e.innerText || '').trim()));
              if (b) b.click();
            })()`,
            returnByValue: true,
          }).catch(() => null);
          await sleep(500);
          continue;
        }

        if (curUrl.includes('/signin/identifier')) {
          const hasVal = await send('Runtime.evaluate', {
            expression: `Boolean(document.querySelector('input#identifierId, input[type="email"]')?.value)`,
            returnByValue: true,
          }).catch(() => null);
          if (!hasVal?.result?.value) {
            if (await clickNode(send, doc.root.nodeId, 'input#identifierId, input[type="email"]')) {
              await sleep(60);
              for (const ch of email) {
                await send('Input.insertText', { text: ch });
                await sleep(14);
              }
              await sleep(80);
            }
          }
          await clickNode(send, doc.root.nodeId, '#identifierNext button');
          await sleep(550);
          continue;
        }

        if (curUrl.includes('/challenge/pwd') && PASSWORD && (!pwdTypedAt || Date.now() - pwdTypedAt > 3500)) {
          if (await submitPasswordChallenge(send, doc.root.nodeId, PASSWORD)) {
            pwdTypedAt = Date.now();
            await sleep(500);
            continue;
          }
        }

        if (curUrl.includes('myaccount.google.com/verification/selfie') || curUrl.includes('/speedbump/')) {
          const nextParam = new URL(curUrl).searchParams.get('next');
          if (nextParam) {
            await send('Page.navigate', { url: nextParam });
            await sleep(1500);
            continue;
          }
          const pos = await send('Runtime.evaluate', {
            expression: `(() => {
              const btns = Array.from(document.querySelectorAll('button, a, div[role="button"]'));
              for (const b of btns) {
                const t = (b.innerText || '').trim();
                if (/^(not now|skip|no thanks|later|cancel)$/i.test(t)) {
                  const r = b.getBoundingClientRect();
                  if (r.width > 8 && r.height > 8) return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) };
                }
              }
              return null;
            })()`,
            returnByValue: true,
          }).catch(() => null);
          if (pos?.result?.value) {
            await clickAt(send, pos.result.value.x, pos.result.value.y, 40);
            await sleep(1000);
            continue;
          }
        }

        if (curUrl.includes('/challenge/iap/qrcode') && !loggedQr) {
          const shortLink = await send('Runtime.evaluate', {
            expression: `(document.body?.innerText || '').match(/g\\.co\\/verify\\/[A-Za-z0-9]+/)?.[0] || ''`,
            returnByValue: true,
          }).catch(() => null);
          const linkStr = shortLink?.result?.value ? ` (or open https://${shortLink.result.value} on your phone)` : '';
          console.log(`  >>> READY FOR PHONE SCAN: Scan the QR code in the Chrome window for ${email}${linkStr} <<<`);
          loggedQr = true;
        }

        await sleep(600);
      }
      ws.close();
    } finally {
      await chrome.closeTab(tab.id);
    }
  }
} finally {
  await chrome.cleanup();
}
