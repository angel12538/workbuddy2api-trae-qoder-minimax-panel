/**
 * call-onsubmit.js — 设值 → 直接调用 form 元素的 onSubmit
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
    // 1. 设值
    const input = document.querySelector('#basic_email');
    const fk = Object.keys(input).find(k => k.startsWith('__reactFiber$'));
    let form = null, f = input[fk], d = 0;
    while (f && d < 70) {
      for (const cd of [f.memoizedProps, f.stateNode, f.stateNode && f.stateNode.props]) {
        if (cd && typeof cd.setFieldsValue === 'function') { form = cd; break; }
        if (cd && cd.form && typeof cd.form.setFieldsValue === 'function') { form = cd.form; break; }
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
    await new Promise(r => setTimeout(r, 200));

    // 2. 取 form 元素的 onSubmit 并调用
    const formEl = document.querySelector('form');
    const pk = Object.keys(formEl).find(k => k.startsWith('__reactProps$'));
    const props = formEl[pk];
    if (!props || typeof props.onSubmit !== 'function') return 'no onSubmit';
    const vals = form.getFieldsValue ? form.getFieldsValue() : null;
    props.onSubmit({ preventDefault() {}, stopPropagation() {} });
    return 'onSubmit called; values=' + JSON.stringify(vals);
  })()`;

  log(await c.eval(script, true).then(r => '>> ' + r));
  for (let i = 0; i < 10; i++) {
    await new Promise(r => setTimeout(r, 1200));
    const st = await c.eval(`JSON.stringify({
      cap: (() => { const e = document.querySelector('#aliyunCaptcha-captcha-body'); return e ? Math.round(e.getBoundingClientRect().width) : 0; })(),
      pw: (() => { const e = document.querySelector('#basic_password'); return e ? Math.round(e.getBoundingClientRect().width) : 0; })(),
      tail: document.body.innerText.slice(-150).replace(/\\n/g, ' / ')
    })`);
    log(`  ${i + 1}: ${st}`);
    if (JSON.parse(st).cap > 50) { log('★★★ 滑块出现'); process.exit(0); }
  }
  process.exit(1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
