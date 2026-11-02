/**
 * one-shot.js — 单次页面内连贯操作：填值 → 勾选 → 提交（同步执行，不给重渲染窗口）
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');
const EMAIL = 'target@example.com';
const sleep = ms => new Promise(r => setTimeout(r, ms));

const ONE_SHOT = `(() => {
  const input = document.querySelector('#basic_email');
  if (!input) return 'no input';
  const fk = Object.keys(input).find(k => k.startsWith('__reactFiber$'));
  if (!fk) return 'no fiber';

  // 向上找 Form 实例
  let form = null, f = input[fk], d = 0;
  while (f && d < 70) {
    for (const cd of [f.memoizedProps, f.stateNode, f.stateNode && f.stateNode.props]) {
      if (cd && typeof cd.setFieldsValue === 'function' && typeof cd.submit === 'function') { form = cd; break; }
      if (cd && cd.form && typeof cd.form.setFieldsValue === 'function' && typeof cd.form.submit === 'function') { form = cd.form; break; }
    }
    if (form) break;
    f = f.return; d++;
  }
  if (!form) return 'form not found';

  // 1. 赋值
  form.setFieldsValue({ firstName: 'Wei', lastName: 'Chen', email: ${JSON.stringify(EMAIL)}, password: 'CHANGE_ME_PASSWORD' });

  // 2. 勾选条款（找 React 受控 checkbox 的 onChange）
  const cb = document.querySelector('input[type=checkbox]');
  if (cb && !cb.checked) {
    cb.click();
    if (!cb.checked) {
      const pk = Object.keys(cb).find(k => k.startsWith('__reactProps$'));
      if (pk && typeof cb[pk].onChange === 'function') cb[pk].onChange({ target: { checked: true } });
    }
  }

  // 3. 立即提交（同步，避免重渲染清空）
  const vals = form.getFieldsValue ? form.getFieldsValue() : null;
  form.submit();

  return JSON.stringify({ depth: d, values: vals, cbChecked: cb ? cb.checked : null });
})()`;

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

  log('单次操作:', await c.eval(ONE_SHOT));
  await sleep(3500, 4500);
  const st = await c.eval(`JSON.stringify({
    hasPw: (() => { const p=document.querySelector('#basic_password'); return !!(p && p.getBoundingClientRect().width>0); })(),
    hasCaptcha: (() => { const e=document.querySelector('#aliyunCaptcha-captcha-body'); return !!(e && e.getBoundingClientRect().width>50); })(),
    body: document.body.innerText.slice(-300).replace(/\\n/g,' / ')
  })`);
  log('结果:', st);
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
