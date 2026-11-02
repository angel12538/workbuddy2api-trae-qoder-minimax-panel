/**
 * fill-and-submit.js — 填表+提交（快速但保留拟人节奏）
 * 用原生 setter + input 事件写入（React 兼容），字符级延迟由脚本控制。
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');

const BROWSER_WS = 'ws://localhost:9366/devtools/browser/9bb82102-33e6-4191-bde4-f849d63b27aa';
const IDENT = {
  firstName: 'Wei',
  lastName: 'Chen',
  email: 'target@example.com',
  password: 'CHANGE_ME_PASSWORD',
};
const sleep = (ms) => new Promise(r => setTimeout(r, ms));
const this_sleep = (a, b) => sleep(a + Math.random() * (b - a));

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

  // 等表单
  let ok = false;
  for (let i = 0; i < 20; i++) {
    ok = await c.eval('!!document.querySelector("#basic_firstName")');
    if (ok) break;
    await sleep(700, 1100);
  }
  if (!ok) { console.error('表单未就绪'); process.exit(1); }
  log('表单就绪');

  // 拟人：滚动看看
  await c.eval('window.scrollTo(0,180)'); await sleep(800, 1500);
  await c.eval('window.scrollTo(0,0)');   await sleep(600, 1200);

  // 填表（原生 setter + input 事件，逐字段带停顿）
  const fill = async (sel, val) => {
    // React 兼容写入：原生 setter + 绕过 value tracker + input 事件
    await c.eval(`(() => {
      const el = document.querySelector(${JSON.stringify(sel)});
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      const tracker = el._valueTracker;
      if (tracker) tracker.setValue('');
      setter.call(el, '');
      el.dispatchEvent(new Event('input', { bubbles: true }));
    })()`);
    await sleep(120, 260);
    // 逐键输入：带 key/code 的完整事件，并等待回包（保证顺序）
    for (const ch of val) {
      const isUpper = ch >= 'A' && ch <= 'Z';
      const code = ch === '@' ? 'Digit2' : ch === '#' ? 'Digit3' : ch === '.' ? 'Period'
        : /[a-z]/i.test(ch) ? 'Key' + ch.toUpperCase() : /[0-9]/.test(ch) ? 'Digit' + ch : '';
      await c.send('Input.dispatchKeyEvent', {
        type: 'keyDown', text: ch, key: ch, code,
        windowsVirtualKeyCode: ch.toUpperCase().charCodeAt(0),
        nativeVirtualKeyCode: ch.toUpperCase().charCodeAt(0),
        modifiers: isUpper ? 8 : 0,
      });
      await this_sleep(25, 55);
      await c.send('Input.dispatchKeyEvent', {
        type: 'keyUp', key: ch, code,
        windowsVirtualKeyCode: ch.toUpperCase().charCodeAt(0),
        nativeVirtualKeyCode: ch.toUpperCase().charCodeAt(0),
        modifiers: isUpper ? 8 : 0,
      });
      await this_sleep(25, 55);
    }
    // 补：确保 React/antd Form store 拿到最终值
    await c.eval(`(() => {
      const el = document.querySelector(${JSON.stringify(sel)});
      const tracker = el._valueTracker;
      if (tracker) tracker.setValue('__force__');
      el.dispatchEvent(new Event('input', { bubbles: true }));
    })()`);
    await sleep(350, 700);
  };
  log('填 firstName');
  await fill('#basic_firstName', IDENT.firstName);
  log('填 lastName');
  await fill('#basic_lastName', IDENT.lastName);
  log('填 email');
  await fill('#basic_email', IDENT.email);
  log('填完，勾选条款');

  // 勾选条款（仅当未勾选）
  const checked = await c.eval(`document.querySelector('input[type=checkbox]')?.checked`);
  if (!checked) {
    await c.eval(`(() => { const cb = document.querySelector('input[type=checkbox]'); cb.click(); })()`);
    await sleep(500, 900);
  }

  // 点继续（JS click，实测 SDK 认 click 事件）
  log('点继续');
  await c.eval(`(() => {
    const btns = [...document.querySelectorAll('button')];
    const b2 = btns.find(x => (x.innerText || '').includes('继') && (x.innerText || '').includes('续'));
    if (b2) b2.click();
  })()`);

  // 等密码框出现（邮箱校验通过）
  let pwOk = false;
  for (let i = 0; i < 20; i++) {
    await sleep(800, 1200);
    pwOk = await c.eval(`(() => { const p = document.querySelector('#basic_password'); if (!p) return false; const r = p.getBoundingClientRect(); return r.width > 0; })()`);
    if (pwOk) break;
    const err = await c.eval(`document.body.innerText.includes('有效的邮箱') || document.body.innerText.includes('不能为空')`);
    if (err) { console.error('表单校验失败:', await c.eval('document.body.innerText.slice(-200)')); process.exit(2); }
  }
  if (!pwOk) { console.error('密码框未出现'); process.exit(1); }
  log('密码框已出现');

  // 填密码
  await sleep(1000, 2000);
  await fill('#basic_password', IDENT.password);
  log('密码已填，点继续触发验证码');

  await sleep(600, 1200);
  await c.eval(`(() => {
    const btns = [...document.querySelectorAll('button')];
    const b2 = btns.find(x => (x.innerText || '').includes('继') && (x.innerText || '').includes('续'));
    if (b2) b2.click();
  })()`);

  // 等验证码弹窗
  let cap = false;
  for (let i = 0; i < 20; i++) {
    await sleep(700, 1000);
    cap = await c.eval(`(() => { const el = document.querySelector('#aliyunCaptcha-captcha-body'); return !!(el && el.getBoundingClientRect().width > 50); })()`);
    if (cap) break;
  }
  log(cap ? '★ 验证码已弹出，请主人手动拖动' : '验证码未弹出');
  process.exit(cap ? 0 : 1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
