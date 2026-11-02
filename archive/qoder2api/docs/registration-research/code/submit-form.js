/**
 * submit-form.js — 通过 antd Form 实例的 submit() 触发提交
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');
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

  // 找 Form 实例并 submit
  const r = await c.eval(`(() => {
    const el = document.querySelector('#basic_email');
    const fk = Object.keys(el).find(k => k.startsWith('__reactFiber$'));
    let f = el[fk], d = 0;
    while (f && d < 70) {
      for (const cd of [f.memoizedProps, f.stateNode, f.stateNode && f.stateNode.props]) {
        if (!cd) continue;
        if (typeof cd.submit === 'function' && typeof cd.getFieldsValue === 'function') {
          cd.submit(); return 'form.submit() at depth ' + d + ' values=' + JSON.stringify(cd.getFieldsValue());
        }
        if (cd.form && typeof cd.form.submit === 'function') {
          cd.form.submit(); return 'cd.form.submit() at depth ' + d;
        }
      }
      f = f.return; d++;
    }
    return 'not found';
  })()`);
  log('提交:', r);
  await sleep(3000);
  const st = await c.eval(`JSON.stringify({
    hasPw: (() => { const p=document.querySelector('#basic_password'); return !!(p && p.getBoundingClientRect().width>0); })(),
    hasCaptcha: (() => { const e=document.querySelector('#aliyunCaptcha-captcha-body'); return !!(e && e.getBoundingClientRect().width>50); })(),
    body: document.body.innerText.slice(-260).replace(/\\n/g,' / ')
  })`);
  log('状态:', st);
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
