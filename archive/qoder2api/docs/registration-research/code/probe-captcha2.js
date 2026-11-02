/**
 * probe-captcha2.js — 第二步：填密码 → 点继续 → 观察第一轮验证码（滑块）
 * 承接 probe-captcha.js 的页面状态（无痕上下文复用，已在密码步）。
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');

const BROWSER_WS = 'ws://localhost:9366/devtools/browser/9bb82102-33e6-4191-bde4-f849d63b27aa';
const PASSWORD = 'CHANGE_ME_PASSWORD';
const sleep = (ms) => new Promise(r => setTimeout(r, ms));

(async () => {
  const bws = new WebSocket(BROWSER_WS, { perMessageDeflate: false });
  await new Promise(r => bws.addEventListener('open', r));
  const b = new CDP(bws);
  const ts = await b.send('Target.getTargets', {});
  const s = JSON.parse(fs.readFileSync(__dirname + '/.incog.json', 'utf8'));
  const t = (ts.result.targetInfos || []).find(x => x.browserContextId === s.ctxId && x.type === 'page');
  const att = await b.send('Target.attachToTarget', { targetId: t.targetId, flatten: true });
  const c = new CDP(bws, att.result.sessionId);
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

  // 等密码字段可见
  let vis = false;
  for (let i = 0; i < 15; i++) {
    vis = await c.eval(`(() => { const p = document.querySelector('#basic_password'); if (!p) return false; const r = p.getBoundingClientRect(); return r.width > 0; })()`);
    if (vis) break;
    await sleep(800, 1200);
  }
  if (!vis) { console.error('密码字段未出现'); process.exit(1); }
  log('密码字段已可见');

  // 拟人：停顿（像人在想密码）→ 输入
  await sleep(1200, 2500);
  await c.humanType('#basic_password', PASSWORD);
  log('密码已输入');
  await sleep(600, 1200);

  // 点继续
  const btn = await c.box('button[type=submit]');
  log('点继续', JSON.stringify(btn));
  await c.humanClick(btn.x, btn.y);

  // 观察验证码（15 次 × ~1.2s）
  let d = null;
  for (let i = 0; i < 15; i++) {
    await sleep(900, 1200);
    const raw = await c.eval(`(() => {
      const out = { iframes: [], captchaEl: null, modal: null };
      document.querySelectorAll('iframe').forEach(f => { const r = f.getBoundingClientRect();
        if (r.width > 0) out.iframes.push({src:(f.src||'').slice(0,200), id:f.id, w:f.width, h:f.height, rect:r.toJSON()}); });
      const el = document.querySelector('#captcha-element');
      if (el) { const r = el.getBoundingClientRect(); if (r.width > 0) out.captchaEl = { html: el.innerHTML.slice(0,1200), rect: r.toJSON() }; }
      const masks = document.querySelectorAll('[class*=modal],[class*=popup],[class*=dialog],[class*=captcha],[class*=slider],[class*=verify]');
      masks.forEach(m => { const r = m.getBoundingClientRect(); if (r.width > 50 && m.innerText && m.innerText.length > 2) out.modal = { cls: String(m.className).slice(0,120), text: m.innerText.slice(0,300), rect: r.toJSON() }; });
      return JSON.stringify(out);
    })()`);
    d = JSON.parse(raw);
    if (d.captchaEl || d.modal || d.iframes.length) { log(`第 ${i + 1} 次轮询发现验证码元素`); break; }
  }
  console.log('=== 验证码 DOM ===');
  console.log(JSON.stringify(d, null, 1));

  const shot = await c.send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(__dirname + '/../artifacts/captcha-round1-pw.png', Buffer.from(shot.result.data, 'base64'));
  log('截图已存 artifacts/captcha-round1-pw.png');
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
