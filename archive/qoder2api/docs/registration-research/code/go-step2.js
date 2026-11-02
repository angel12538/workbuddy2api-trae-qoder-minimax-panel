/**
 * go-step2.js — 用 antd Form 实例填值 → 勾选 → 提交 → 等密码框 → 填密码 → 提交 → 到滑块停
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');
const BROWSER_WS = 'ws://localhost:9366/devtools/browser/9bb82102-33e6-4191-bde4-f849d63b27aa';
const EMAIL = 'target@example.com';
const PASSWORD = 'CHANGE_ME_PASSWORD';
const sleep = ms => new Promise(r => setTimeout(r, ms));
const rnd = (a, b) => a + Math.random() * (b - a);

const SET_FORM = (vals) => `(() => {
  const input = document.querySelector('#basic_email') || document.querySelector('input');
  if (!input) return 'no input';
  const fiberKey = Object.keys(input).find(k => k.startsWith('__reactFiber$'));
  if (!fiberKey) return 'no fiber';
  let f = input[fiberKey], depth = 0;
  while (f && depth < 60) {
    const cands = [f.memoizedProps, f.stateNode, f.stateNode && f.stateNode.props];
    for (const cd of cands) {
      if (cd && typeof cd.setFieldsValue === 'function') { cd.setFieldsValue(${JSON.stringify(vals)}); return 'ok depth ' + depth; }
      if (cd && cd.form && typeof cd.form.setFieldsValue === 'function') { cd.form.setFieldsValue(${JSON.stringify(vals)}); return 'form ok depth ' + depth; }
    }
    f = f.return; depth++;
  }
  return 'not found';
})()`;

(async () => {
  const bws = new WebSocket(BROWSER_WS, { perMessageDeflate: false });
  await new Promise(r => bws.addEventListener('open', r));
  const b = new CDP(bws);
  const ts = await b.send('Target.getTargets', {});
  const s = JSON.parse(fs.readFileSync(__dirname + '/.incog.json', 'utf8'));
  const t = (ts.result.targetInfos || []).find(x => x.browserContextId === s.ctxId && x.type === 'page');
  const att = await b.send('Target.attachToTarget', { targetId: t.targetId, flatten: true });
  const c = new CDP(bws, att.result.sessionId);
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

  // 步骤1：填三字段
  log('填值:', await c.eval(SET_FORM({ firstName: 'Wei', lastName: 'Chen', email: EMAIL })));
  await sleep(800, 1400);

  // 勾选条款（点击真实 checkbox）
  if (!(await c.eval(`document.querySelector('input[type=checkbox]')?.checked`))) {
    // JS 点击 label/checkbox
    await c.eval(`(() => {
      const cb = document.querySelector('input[type=checkbox]');
      if (!cb) return 'no cb';
      cb.click();
      if (!cb.checked) { const lb = cb.closest('label') || cb.parentElement; if (lb) lb.click(); }
      return String(cb.checked);
    })()`);
    await sleep(500, 900);
  }
  log('勾选状态:', await c.eval(`document.querySelector('input[type=checkbox]')?.checked`));

  // 提交
  await sleep(400, 800);
  const sub1 = await c.eval(`(() => {
    const form = document.querySelector('form');
    const btn = [...document.querySelectorAll('button')].find(x=>(x.innerText||'').replace(/\\s/g,'').includes('继续'));
    if (form && form.requestSubmit) { form.requestSubmit(btn || undefined); return 'requestSubmit'; }
    if (btn) { btn.click(); return 'click'; }
    return 'none';
  })()`);
  log('已提交步骤1 via', sub1);

  // 等密码框
  let pw = false;
  for (let i = 0; i < 25; i++) {
    await sleep(700, 1000);
    pw = await c.eval(`(() => { const p=document.querySelector('#basic_password'); return !!(p && p.getBoundingClientRect().width>0); })()`);
    if (pw) break;
  }
  if (!pw) { log('✗ 密码框未出现 | ' + (await c.eval('document.body.innerText.slice(-250)')).replace(/\n/g,' / ')); process.exit(2); }
  log('★ 密码框出现');

  // 步骤2：填密码
  await sleep(700, 1300);
  log('填密码:', await c.eval(SET_FORM({ password: PASSWORD })));
  await sleep(700, 1200);

  // 提交 → 触发验证码
  const sub2 = await c.eval(`(() => {
    const form = document.querySelector('form');
    const btn = [...document.querySelectorAll('button')].find(x=>(x.innerText||'').replace(/\\s/g,'').includes('继续'));
    if (form && form.requestSubmit) { form.requestSubmit(btn || undefined); return 'requestSubmit'; }
    if (btn) { btn.click(); return 'click'; }
    return 'none';
  })()`);
  log('已提交步骤2 via', sub2);

  let cap = false;
  for (let i = 0; i < 25; i++) {
    await sleep(700, 1000);
    cap = await c.eval(`(() => { const el=document.querySelector('#aliyunCaptcha-captcha-body'); return !!(el && el.getBoundingClientRect().width>50); })()`);
    if (cap) break;
  }
  if (cap) { log('★★★ 滑块已弹出，请主人拖动'); process.exit(0); }
  log('无滑块 | ' + (await c.eval('document.body.innerText.slice(-350)')).replace(/\n/g,' / '));
  process.exit(1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
