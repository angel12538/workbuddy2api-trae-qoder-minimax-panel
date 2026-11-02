/**
 * find-submit-handler.js — 找按钮的 React onClick，直接调用
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');
const EMAIL = 'target@example.com';

(async () => {
  const bws = new WebSocket('ws://localhost:9366/devtools/browser/9bb82102-33e6-4191-bde4-f849d63b27aa', { perMessageDeflate: false });
  await new Promise(r => bws.addEventListener('open', r));
  const b = new CDP(bws);
  const ts = await b.send('Target.getTargets', {});
  const s = JSON.parse(fs.readFileSync(__dirname + '/.incog.json', 'utf8'));
  const t = (ts.result.targetInfos || []).find(x => x.browserContextId === s.ctxId && x.type === 'page');
  const att = await b.send('Target.attachToTarget', { targetId: t.targetId, flatten: true });
  const c = new CDP(bws, att.result.sessionId);
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

  // 诊断：列出按钮与 form 相关 React props 的键
  const diag = await c.eval(`(() => {
    const btn = document.querySelector('button[type=submit]');
    const bk = Object.keys(btn).find(k => k.startsWith('__reactProps$'));
    const props = bk ? btn[bk] : null;
    const out = { btnPropsKeys: props ? Object.keys(props) : null, btnType: props && props.type };
    // 向上找 form 元素的 props
    const formEl = document.querySelector('form');
    const fk2 = formEl ? Object.keys(formEl).find(k => k.startsWith('__reactProps$')) : null;
    const fp = fk2 ? formEl[fk2] : null;
    out.formPropsKeys = fp ? Object.keys(fp) : null;
    out.hasOnFinish = !!(fp && fp.onFinish);
    out.hasOnSubmit = !!(fp && fp.onSubmit);
    // fiber 链上的组件名
    const fi = formEl ? formEl[Object.keys(formEl).find(k => k.startsWith('__reactFiber$'))] : null;
    let f = fi, names = [], d = 0;
    while (f && d < 30) {
      const tn = f.type && (typeof f.type === 'function' ? (f.type.displayName || f.type.name) : null);
      if (tn) names.push(tn);
      f = f.return; d++;
    }
    out.chain = names;
    return JSON.stringify(out);
  })()`);
  log('诊断:', diag);
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
