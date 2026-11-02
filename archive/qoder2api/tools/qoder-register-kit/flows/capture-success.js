/**
 * capture-success.js — 全量网络监听（B 方案：主人手动走流程，只记录）
 * 记录：所有 /api/ 请求（方法/URL/头/体/响应），特别标注 bx-ua 与验证码相关。
 */
const { CDP } = require('./cdp');
const fs = require('fs');
const PAGE_WS = process.argv[2] || 'ws://localhost:9366/devtools/page/5EEBF5919773ECDD41E7E7A394C397B1';
const OUT = __dirname + '/../artifacts/manual-flow-' + new Date().toISOString().replace(/[:.]/g, '-') + '.jsonl';

(async () => {
  const c = new CDP(PAGE_WS);
  await c.ready;
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);
  const store = new Map();
  const out = fs.createWriteStream(OUT, { flags: 'a' });

  c.ws.addEventListener('message', async (ev) => {
    const m = JSON.parse(ev.data);
    if (m.method === 'Network.requestWillBeSent') {
      const r = m.params.request;
      if (!/\/api\/|captcha|umid|baxia|punish|x5sec/.test(r.url)) return;
      store.set(m.params.requestId, {
        ts: new Date().toISOString(),
        id: m.params.requestId,
        method: r.method,
        url: r.url,
        headers: r.headers,
        postData: r.postData || null,
        hasBxUa: !!r.headers['bx-ua'] || (r.postData || '').includes('bx-ua'),
        bxUa: r.headers['bx-ua'] || (r.postData || '').match(/"bx-ua":"([^"]+)"/)?.[1] || null,
      });
      const rec = store.get(m.params.requestId);
      log(`→ ${rec.method} ${rec.url.slice(0, 90)}${rec.bxUa ? ' [bx-ua]' : ''}`);
    }
    if (m.method === 'Network.responseReceived') {
      const rec = store.get(m.params.requestId);
      if (rec) {
        rec.status = m.params.response.status;
        rec.respHeaders = m.params.response.headers;
      }
    }
    if (m.method === 'Network.loadingFinished') {
      const rec = store.get(m.params.requestId);
      if (!rec) return;
      try {
        const r = await c.send('Network.getResponseBody', { requestId: m.params.requestId });
        rec.response = (r.result && r.result.body || '').slice(0, 3000);
      } catch (e) { rec.response = '(unavailable)'; }
      out.write(JSON.stringify(rec) + '\n');
      const flag = /SUCCESS|SUCCESSFUL|"id"|userId|token/i.test(rec.response) && rec.status === 200 ? ' ★★' : '';
      log(`← ${rec.status} ${rec.url.split('?')[0].slice(0, 70)}${flag}`);
      if (flag) log('   响应:', rec.response.slice(0, 300));
      store.delete(m.params.requestId);
    }
  });

  await c.send('Network.enable');
  log('全量监听已启动 →', OUT);
  log('请主人手动完成注册流程（表单/滑块/baxia），我全程只记录');
  process.on('SIGINT', () => { out.end(); process.exit(0); });
  await new Promise(() => {});
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
