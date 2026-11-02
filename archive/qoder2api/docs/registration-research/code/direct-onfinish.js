/**
 * direct-onfinish.js — 设值 → validateFields → 直接调用组件的 onFinish
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

  const script = `(async () => {
    const input = document.querySelector('#basic_email');
    const fk = Object.keys(input).find(k => k.startsWith('__reactFiber$'));
    let form = null, f = input[fk], d = 0;
    let formOwner = null, ownerDepth = -1;
    while (f && d < 70) {
      for (const cd of [f.memoizedProps, f.stateNode, f.stateNode && f.stateNode.props]) {
        if (cd && typeof cd.setFieldsValue === 'function') { form = cd; formOwner = f; ownerDepth = d; break; }
        if (cd && cd.form && typeof cd.form.setFieldsValue === 'function') { form = cd.form; formOwner = f; ownerDepth = d; break; }
      }
      if (form) break;
      f = f.return; d++;
    }
    if (!form) return 'no form';

    form.setFieldsValue({
      firstName: 'Wei', lastName: 'Chen',
      email: ${JSON.stringify(EMAIL)},
      password: 'CHANGE_ME_PASSWORD'
    });
    const cb = document.querySelector('input[type=checkbox]');
    if (cb && !cb.checked) cb.click();

    // 校验
    let vals;
    try {
      vals = await form.validateFields();
    } catch (e) {
      return 'validate failed: ' + JSON.stringify(e.errorFields || String(e));
    }

    // 从 formOwner 向上找带 onFinish 的组件 props
    let g = formOwner, gd = 0;
    while (g && gd < 40) {
      const p = g.memoizedProps;
      if (p && typeof p.onFinish === 'function') {
        p.onFinish(vals);
        return 'onFinish called at depth ' + (ownerDepth + gd) + ' values=' + JSON.stringify(vals);
      }
      g = g.return; gd++;
    }
    return 'onFinish not found; vals=' + JSON.stringify(vals);
  })()`;

  log(await c.eval(script, true).then(r => '>> ' + r));
  await new Promise(r => setTimeout(r, 4000));
  const st = await c.eval(`JSON.stringify({
    cap: (() => { const e = document.querySelector('#aliyunCaptcha-captcha-body'); return e ? Math.round(e.getBoundingClientRect().width) : 0; })(),
    pw: (() => { const e = document.querySelector('#basic_password'); return e ? Math.round(e.getBoundingClientRect().width) : 0; })(),
    tail: document.body.innerText.slice(-200).replace(/\\n/g, ' / ')
  })`);
  log('状态:', st);
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
