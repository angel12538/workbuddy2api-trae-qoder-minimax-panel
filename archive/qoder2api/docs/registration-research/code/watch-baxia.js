const { CDP } = require('./cdp');
const sleep = ms => new Promise(r => setTimeout(r, ms));
const PAGE_WS = 'ws://localhost:9366/devtools/page/3681DE269541D0AB8303665C31FA1094';
(async () => {
  const c = new CDP(PAGE_WS);
  await c.ready;
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);
  log('监听 baxia 弹窗（请主人手动完成验证）...');
  let last = '';
  for (let i = 0; i < 300; i++) {
    await sleep(1800, 2200);
    const st = await c.eval(`JSON.stringify({
      baxia: !!document.querySelector('.baxia-dialog'),
      baxiaVisible: (() => { const e = document.querySelector('.baxia-dialog-content'); return e ? Math.round(e.getBoundingClientRect().width) : 0; })(),
      href: location.href,
      body: document.body.innerText.slice(0, 300)
    })`);
    const d = JSON.parse(st);
    const sig = d.baxia + '|' + d.baxiaVisible + '|' + d.href;
    if (sig !== last) { log(`baxia=${d.baxia} w=${d.baxiaVisible} href=${d.href.slice(0, 60)}`); last = sig; }
    if (!d.baxia || d.baxiaVisible < 50) {
      log('★★ baxia 弹窗已消失，页面:', d.body.replace(/\n/g, ' / ').slice(0, 200));
      process.exit(0);
    }
  }
  process.exit(1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
