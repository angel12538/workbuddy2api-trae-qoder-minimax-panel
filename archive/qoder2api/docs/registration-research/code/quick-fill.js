/**
 * quick-fill.js — 直接用 CDP 逐键输入（带 key/code，等回包保证顺序），到滑块停
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');
const BROWSER_WS = 'ws://localhost:9366/devtools/browser/9bb82102-33e6-4191-bde4-f849d63b27aa';
const IDENT = { firstName: 'Wei', lastName: 'Chen', email: 'target@example.com', password: 'CHANGE_ME_PASSWORD' };
const sleep = (ms) => new Promise(r => setTimeout(r, ms));
const rnd = (a, b) => a + Math.random() * (b - a);

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
  for (let i = 0; i < 20; i++) {
    if (await c.eval('!!document.querySelector("#basic_firstName")')) break;
    await sleep(700, 1100);
  }
  log('表单就绪');

  // 打字（真实键盘事件序列）
  const type = async (sel, val) => {
    await c.eval(`document.querySelector(${JSON.stringify(sel)}).focus()`);
    await sleep(150, 300);
    for (const ch of val) {
      const code = ch === '@' ? 'Digit2' : ch === '#' ? 'Digit3' : ch === '.' ? 'Period'
        : /[a-z]/i.test(ch) ? 'Key' + ch.toUpperCase()
        : /[0-9]/.test(ch) ? 'Digit' + ch : 'Minus';
      const vk = /[a-z]/i.test(ch) ? ch.toUpperCase().charCodeAt(0)
        : ch === '@' ? 50 : ch === '#' ? 51 : ch === '.' ? 190 : ch.charCodeAt(0);
      const isUpper = ch >= 'A' && ch <= 'Z';
      await c.send('Input.dispatchKeyEvent', { type: 'keyDown', text: ch, unmodifiedText: ch, key: ch, code, windowsVirtualKeyCode: vk, nativeVirtualKeyCode: vk, modifiers: isUpper ? 8 : 0 });
      await sleep(rnd(20, 45));
      await c.send('Input.dispatchKeyEvent', { type: 'keyUp', key: ch, code, windowsVirtualKeyCode: vk, nativeVirtualKeyCode: vk, modifiers: isUpper ? 8 : 0 });
      await sleep(rnd(20, 45));
    }
    await sleep(300, 600);
    log(`  ${sel} = ${await c.eval(`document.querySelector(${JSON.stringify(sel)}).value`)}`);
  };

  log('输入名字/姓/邮箱');
  await type('#basic_firstName', IDENT.firstName);
  await type('#basic_lastName', IDENT.lastName);
  await type('#basic_email', IDENT.email);

  // 勾选条款
  if (!(await c.eval(`document.querySelector('input[type=checkbox]')?.checked`))) {
    await c.eval(`document.querySelector('input[type=checkbox]').click()`);
    await sleep(400, 700);
  }
  log('已勾选条款');

  // 点继续
  await sleep(500, 900);
  await c.eval(`(() => {
    const b2 = [...document.querySelectorAll('button')].find(x => (x.innerText||'').replace(/\\s/g,'').includes('继续'));
    if (b2) b2.click();
  })()`);
  log('已点继续，等密码框...');

  // 等密码框
  let pw = false;
  for (let i = 0; i < 20; i++) {
    await sleep(700, 1000);
    pw = await c.eval(`(() => { const p = document.querySelector('#basic_password'); if (!p) return false; return p.getBoundingClientRect().width > 0; })()`);
    if (pw) break;
  }
  if (!pw) { log('密码框未出现 | ' + (await c.eval('document.body.innerText.slice(-200)')).replace(/\n/g,' / ')); process.exit(2); }
  log('密码框出现');

  await sleep(800, 1500);
  await type('#basic_password', IDENT.password);

  // 点继续触发验证码
  await sleep(500, 900);
  await c.eval(`(() => {
    const b2 = [...document.querySelectorAll('button')].find(x => (x.innerText||'').replace(/\\s/g,'').includes('继续'));
    if (b2) b2.click();
  })()`);
  log('已点继续，等验证码...');

  let cap = false;
  for (let i = 0; i < 20; i++) {
    await sleep(700, 1000);
    cap = await c.eval(`(() => { const el = document.querySelector('#aliyunCaptcha-captcha-body'); return !!(el && el.getBoundingClientRect().width > 50); })()`);
    if (cap) break;
  }
  if (cap) { log('★★ 验证码已弹出，请主人拖动滑块'); process.exit(0); }
  log('验证码未出现 | ' + (await c.eval('document.body.innerText.slice(-300)')).replace(/\n/g,' / '));
  process.exit(1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
