/**
 * fill-and-click.js — 同一 tick 内：setFieldsValue → 勾选 → 真实按钮 click
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');
const EMAIL = 'target@example.com';
const sleep = ms => new Promise(r => setTimeout(r, ms));

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
    await new Promise(r => setTimeout(r, 300));
    // 用 Form 实例校验通过后手动调 onFinish？先试按钮原生 click
    const btn = document.querySelector('button[type=submit]');
    btn.click();
    return JSON.stringify({ clicked: true, cbChecked: cb ? cb.checked : null });
  })()`;

  log('执行:', await c.eval(script, true));
  for (let i = 0; i < 12; i++) {
    await sleep(1000, 1400);
    const st = await c.eval(`JSON.stringify({
      cap: !!document.querySelector('#aliyunCaptcha-captcha-body'),
      capW: (() => { const e = document.querySelector('#aliyunCaptcha-captcha-body'); return e ? Math.round(e.getBoundingClientRect().width) : 0; })(),
      pwW: (() => { const e = document.querySelector('#basic_password'); return e ? Math.round(e.getBoundingClientRect().width) : 0; })(),
      tail: document.body.innerText.slice(-120).replace(/\\n/g, ' / ')
    })`);
    log(`  ${i + 1}: ${st}`);
    if (JSON.parse(st).capW > 50) { log('★★★ 滑块已弹出'); process.exit(0); }
  }
  process.exit(1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
