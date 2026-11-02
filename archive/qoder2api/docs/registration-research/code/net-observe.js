const { CDP } = require('./cdp');
const sleep = ms => new Promise(r => setTimeout(r, ms));
const PAGE_WS = 'ws://localhost:9366/devtools/page/5EEBF5919773ECDD41E7E7A394C397B1';

(async () => {
  const c = new CDP(PAGE_WS);
  await c.ready;
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

  const requests = [];
  c.ws.addEventListener('message', (ev) => {
    const m = JSON.parse(ev.data);
    if (m.method === 'Network.requestWillBeSent') {
      const r = m.params.request;
      if (!r.url.includes('.js') && !r.url.includes('.css') && !r.url.includes('.png'))
        requests.push({ url: r.url, method: r.method, postData: (r.postData || '').slice(0, 500), headers: Object.keys(r.headers) });
    }
    if (m.method === 'Network.responseReceived') {
      const last = requests[requests.length - 1];
      if (last && !last.status) last.status = m.params.response.status;
    }
  });
  await c.send('Network.enable');
  log('Network 已启用');

  // 触发提交
  const r = await c.eval(`(() => {
    const i = document.querySelector('input[type=text]');
    const fk = Object.keys(i).find(k => k.startsWith('__reactFiber$'));
    let f = i[fk], d = 0;
    while (f && d < 40) {
      const p = f.memoizedProps;
      if (p && typeof p.onChangeCallback === 'function') { p.onChangeCallback('418251'); return 'called'; }
      f = f.return; d++;
    }
    return 'nf';
  })()`);
  log('触发:', r);
  await sleep(5000);

  log('捕获请求:');
  for (const q of requests) {
    log(`  ${q.method} ${q.url.slice(0, 110)} → ${q.status || '?'}`);
    if (q.postData && q.postData.length > 2) log(`     body: ${q.postData}`);
  }
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
