/**
 * incog.js — 无痕上下文管理（CDP browser 级）
 * 创建独立无痕 target 跑注册流，不碰主人登录态。
 */
const WebSocket = require('ws');
const http = require('http');

function get(path) {
  return new Promise((resolve, reject) => {
    http.get('http://localhost:9366' + path, (res) => {
      let d = ''; res.on('data', c => d += c); res.on('end', () => { try { resolve(JSON.parse(d)); } catch (e) { reject(e); } });
    }).on('error', reject);
  });
}

class Browser {
  constructor(wsUrl) {
    this.ws = new WebSocket(wsUrl, { perMessageDeflate: false });
    this.id = 0; this.pending = new Map();
    this.ws.addEventListener('message', (ev) => {
      const m = JSON.parse(ev.data);
      if (m.id && this.pending.has(m.id)) { this.pending.get(m.id)(m); this.delete ? 0 : this.pending.delete(m.id); }
    });
    this.ready = new Promise(r => this.ws.addEventListener('open', r));
  }
  send(method, params = {}, sessionId) {
    const mid = ++this.id;
    const msg = { id: mid, method, params };
    if (sessionId) msg.sessionId = sessionId;
    return new Promise((resolve) => { this.pending.set(mid, resolve); this.ws.send(JSON.stringify(msg)); });
  }
}

(async () => {
  const ver = await get('/json/version');
  const b = new Browser(ver.webSocketDebuggerUrl);
  await b.ready;

  // 建无痕上下文 + target
  const ctx = await b.send('Target.createBrowserContext', { });
  const ctxId = ctx.result.browserContextId;
  console.log('browserContextId:', ctxId);

  const tgt = await b.send('Target.createTarget', { url: 'about:blank', browserContextId: ctxId });
  const targetId = tgt.result.targetId;
  console.log('targetId:', targetId);

  // attach 拿 sessionId
  const att = await b.send('Target.attachToTarget', { targetId, flatten: true });
  const sessionId = att.result.sessionId;
  console.log('sessionId:', sessionId);

  // 用 session 导航
  await b.send('Page.enable', {}, sessionId);
  await b.send('Page.navigate', { url: 'https://qoder.com/users/sign-up' }, sessionId);
  console.log('已导航 sign-up（无痕）');
  await new Promise(r => setTimeout(r, 3000));
  b.ws.close();

  // 输出供后续脚本使用
  require('fs').writeFileSync('/Users/zhengyusheng/Documents/qoder-register/code/.incog.json', JSON.stringify({ ctxId, targetId, sessionId }));
})().catch(e => { console.error(e); process.exit(1); });
