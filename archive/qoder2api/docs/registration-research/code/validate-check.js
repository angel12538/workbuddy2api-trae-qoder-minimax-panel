/**
 * validate-check.js — 检查 antd 表单校验结果
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

  const r = await c.eval(`(async () => {
    const input = document.querySelector('#basic_email');
    const fk = Object.keys(input).find(k => k.startsWith('__reactFiber$'));
    let form = null, f = input[fk], d = 0;
    while (f && d < 70) {
      for (const cd of [f.memoizedProps, f.stateNode, f.stateNode && f.stateNode.props]) {
        if (cd && typeof cd.validateFields === 'function') { form = cd; break; }
        if (cd && cd.form && typeof cd.form.validateFields === 'function') { form = cd.form; break; }
      }
      if (form) break;
      f = f.return; d++;
    }
    if (!form) return 'no form';
    form.setFieldsValue({ firstName: 'Wei', lastName: 'Chen', email: ${JSON.stringify(EMAIL)} });
    const cb = document.querySelector('input[type=checkbox]');
    if (cb && !cb.checked) { cb.click(); }
    await new Promise(r => setTimeout(r, 600));
    try {
      const vals = await form.validateFields();
      return JSON.stringify({ ok: true, vals, cbChecked: cb?.checked });
    } catch (e) {
      return JSON.stringify({ ok: false, err: e && e.errorFields ? e.errorFields : String(e), cbChecked: cb?.checked });
    }
  })()`, true);
  log('校验结果:', r);
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
