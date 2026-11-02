const { CDP } = require('./cdp');
const sleep = ms => new Promise(r => setTimeout(r, ms));
const PAGE_WS = 'ws://localhost:9366/devtools/page/5EEBF5919773ECDD41E7E7A394C397B1';

(async () => {
  const c = new CDP(PAGE_WS);
  await c.ready;
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);
  const pending = new Map();

  c.ws.addEventListener('message', async (ev) => {
    const m = JSON.parse(ev.data);
    if (m.method === 'Network.requestWillBeSent' && m.params.request.url.includes('/api/v1/users')) {
      log('请求:', m.params.request.method, m.params.request.url);
      pending.set(m.params.requestId, true);
    }
    if (m.method === 'Network.loadingFinished' && pending.has(m.params.requestId)) {
      const r = await c.send('Network.getResponseBody', { requestId: m.params.requestId });
      log('响应体:', (r.result && r.result.body || '').slice(0, 600));
      pending.delete(m.params.requestId);
    }
  });
  await c.send('Network.enable');
  log('监听中，重新触发提交...');

  await c.eval(`(() => {
    const i = document.querySelector('input[type=text]');
    const fk = Object.keys(i).find(k => k.startsWith('__reactFiber$'));
    let f = i[fk], d = 0;
    while (f && d < 40) {
      const p = f.memoizedProps;
      if (p && typeof p.onChangeCallback === 'function') { p.onChangeCallback('418251'); return 'ok'; }
      f = f.return; d++;
    }
    return 'nf';
  })()`);
  await sleep(6000);
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
