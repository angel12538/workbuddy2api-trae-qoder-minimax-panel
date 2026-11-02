const { CDP } = require('./cdp');
const sleep = ms => new Promise(r => setTimeout(r, ms));
const PAGE_WS = 'ws://localhost:9366/devtools/page/3681DE269541D0AB8303665C31FA1094';
const EXPR = 'JSON.stringify({' +
  'baxia: !!document.querySelector(\'iframe[src*="punish"]\'),' +
  'baxiaW: (function(){ var f = document.querySelector(\'iframe[src*="punish"]\'); return f ? Math.round(f.getBoundingClientRect().width) : 0; })(),' +
  'tail: document.body.innerText.slice(-120).replace(/\\n/g, " / ")' +
  '})';
(async () => {
  const c = new CDP(PAGE_WS);
  await c.ready;
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);
  log('监听中…（拖滑块 → 收码 → 提交 → baxia）');
  let last = '';
  for (let i = 0; i < 400; i++) {
    await sleep(1500, 1800);
    const d = JSON.parse(await c.eval(EXPR));
    const sig = d.baxia + '|' + d.baxiaW + '|' + d.tail.slice(0, 40);
    if (sig !== last) { log(`baxia=${d.baxia} w=${d.baxiaW} | ${d.tail.slice(0, 90)}`); last = sig; }
    if (d.baxia && d.baxiaW > 100) { log('★★★ baxia 弹窗出现！'); process.exit(0); }
  }
  process.exit(1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
