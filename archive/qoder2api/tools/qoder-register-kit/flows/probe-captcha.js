/**
 * probe-captcha.js — 第一轮验证码形态观察（单次、人类节奏、全生命周期自管 session）
 * 架构：连 browser WS → 复用/建无痕上下文 → attach 新 session → 跑流程 → 关闭。
 * 无痕上下文跨运行复用（页面状态保留），session 每次新建。
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const http = require('http');
const fs = require('fs');

const BROWSER_WS = 'ws://localhost:9366/devtools/browser/9bb82102-33e6-4191-bde4-f849d63b27aa';
const IDENT = {
  firstName: 'Wei', lastName: 'Chen',
  email: 'target@example.com',
  password: 'CHANGE_ME_PASSWORD',
};
const sleep = (ms) => new Promise(r => setTimeout(r, ms));

(async () => {
  // 1. browser WS + 复用上下文
  const bws = new WebSocket(BROWSER_WS, { perMessageDeflate: false });
  await new Promise(r => bws.addEventListener('open', r));
  const b = new CDP(bws);
  const bsend = (m, p) => b.send(m, p);

  let ctxId, targetId;
  const stateFile = __dirname + '/.incog.json';
  if (fs.existsSync(stateFile)) {
    const s = JSON.parse(fs.readFileSync(stateFile, 'utf8'));
    ctxId = s.ctxId;
    // 列上下文里的 targets 复用
    const ts = await bsend('Target.getTargets', {});
    const t = (ts.result.targetInfos || []).find(x => x.browserContextId === ctxId && x.type === 'page');
    targetId = t ? t.targetId : (await bsend('Target.createTarget', { url: 'about:blank', browserContextId: ctxId })).result.targetId;
  } else {
    ctxId = (await bsend('Target.createBrowserContext', {})).result.browserContextId;
    targetId = (await bsend('Target.createTarget', { url: 'about:blank', browserContextId: ctxId })).result.targetId;
  }
  const att = await bsend('Target.attachToTarget', { targetId, flatten: true });
  const sessionId = att.result.sessionId;
  fs.writeFileSync(stateFile, JSON.stringify({ ctxId, targetId }));
  console.log('上下文/会话就绪');

  const c = new CDP(bws, sessionId);
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

  // 2. 导航 sign-up 并等表单
  const href = await c.eval('location.href');
  if (!href.includes('/users/sign-up')) {
    log('导航 sign-up');
    await c.send('Page.navigate', { url: 'https://qoder.com/users/sign-up' });
  }
  let ready = false;
  for (let i = 0; i < 25; i++) {
    await c.eval('1'); // 保活
    ready = await c.eval('!!document.querySelector("#basic_firstName")');
    if (ready) break;
    await sleep(800, 1200);
  }
  if (!ready) { console.error('表单 25s 未就绪, href=', await c.eval('location.href')); process.exit(1); }
  log('表单就绪');

  // 3. 拟人浏览 + 填表
  await c.eval('window.scrollTo(0,200)'); await sleep(900, 1800);
  await c.eval('window.scrollTo(0,0)');   await sleep(700, 1400);
  log('填表...');
  await c.humanType('#basic_firstName', IDENT.firstName); await sleep(500, 1100);
  await c.humanType('#basic_lastName', IDENT.lastName);   await sleep(500, 1100);
  await c.humanType('#basic_email', IDENT.email);         await sleep(700, 1500);
  // 注意：密码字段首步隐藏（验证码通过后才出现），此处不填
  const checked = await c.eval("document.querySelector('input[type=checkbox]').checked");
  if (!checked) {
    const cb = await c.box('input[type=checkbox]');
    if (cb && cb.w > 0) { await c.humanClick(cb.x, cb.y); log('已勾选条款'); await sleep(400, 900); }
  } else { log('条款已勾选'); }

  // 4. 点继续 → 等验证码
  const btn = await c.box('button[type=submit]');
  log('点继续', JSON.stringify(btn));
  await c.humanClick(btn.x, btn.y);

  let d = null;
  for (let i = 0; i < 15; i++) {
    await sleep(900, 1200);
    const raw = await c.eval(`(() => {
      const out = { iframes: [], captchaEl: null, modal: null, scene: window.captchaScene || null };
      document.querySelectorAll('iframe').forEach(f => out.iframes.push({src:(f.src||'').slice(0,160), id:f.id, w:f.width, h:f.height, rect:f.getBoundingClientRect().toJSON()}));
      const el = document.querySelector('#captcha-element');
      if (el) out.captchaEl = { html: el.innerHTML.slice(0,800), rect: el.getBoundingClientRect().toJSON() };
      const mask = document.querySelector('[class*=modal],[class*=popup],[class*=dialog],[class*=captcha]');
      if (mask) out.modal = { cls: String(mask.className).slice(0,100), text: mask.innerText.slice(0,200) };
      return JSON.stringify(out);
    })()`);
    d = JSON.parse(raw);
    if (d.captchaEl || d.modal || d.iframes.length) break;
  }
  console.log('=== 验证码 DOM ===');
  console.log(JSON.stringify(d, null, 1));

  const shot = await c.send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(__dirname + '/../artifacts/captcha-round1.png', Buffer.from(shot.result.data, 'base64'));
  log('截图已存 artifacts/captcha-round1.png');
  bws.close();
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
