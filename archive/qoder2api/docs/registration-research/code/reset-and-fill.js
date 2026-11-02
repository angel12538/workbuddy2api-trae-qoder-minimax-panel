/**
 * reset-and-fill.js — 清空表单 → 修正字符映射 → 逐键输入 → 提交 → 到滑块停
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
const rnd = (a, b) => a + Math.random() * (b - a);

// 字符 → (code, vk, needsShift)
function keyInfo(ch) {
  if (/[a-z]/.test(ch)) return { code: 'Key' + ch.toUpperCase(), vk: ch.toUpperCase().charCodeAt(0), shift: false };
  if (/[A-Z]/.test(ch)) return { code: 'Key' + ch, vk: ch.charCodeAt(0), shift: true };
  if (/[0-9]/.test(ch)) return { code: 'Digit' + ch, vk: ch.charCodeAt(0), shift: false };
  const map = {
    '@': { code: 'Digit2', vk: 50, shift: true },
    '#': { code: 'Digit3', vk: 51, shift: true },
    '.': { code: 'Period', vk: 190, shift: false },
    '-': { code: 'Minus', vk: 189, shift: false },
    '_': { code: 'Minus', vk: 189, shift: true },
  };
  return map[ch] || { code: '', vk: ch.charCodeAt(0), shift: false };
}

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

  // 0. 回到干净页面
  await c.send('Page.navigate', { url: 'https://qoder.com/users/sign-up' });
  for (let i = 0; i < 25; i++) {
    await sleep(600, 900);
    if (await c.eval('!!document.querySelector("#basic_firstName")')) break;
  }
  log('页面就绪');

  // 全部字段清空（保险）
  await c.eval(`(() => {
    for (const id of ['#basic_firstName','#basic_lastName','#basic_email','#basic_password']) {
      const el = document.querySelector(id);
      if (!el) continue;
      el.focus();
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      if (el._valueTracker) el._valueTracker.setValue('__x__');
      setter.call(el, '');
      el.dispatchEvent(new Event('input', { bubbles: true }));
      el.blur();
    }
    return 'cleared';
  })()`);

  const type = async (sel, val) => {
    // 滚动到元素 + 取精确坐标
    await c.eval(`document.querySelector(${JSON.stringify(sel)}).scrollIntoView({block:'center'})`);
    await sleep(300, 500);
    const box = await c.box(sel);
    log(`  ${sel} box=${JSON.stringify(box)}`);
    // 真实鼠标点击
    await c.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: box.x, y: box.y });
    await sleep(80, 160);
    await c.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: box.x, y: box.y, button: 'left', clickCount: 1 });
    await sleep(50, 100);
    await c.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: box.x, y: box.y, button: 'left', clickCount: 1 });
    await sleep(250, 450);
    let active = await c.eval('document.activeElement?.id');
    log(`  焦点=${active}`);
    if (active !== sel.replace('#', '')) {
      // 兜底：JS focus
      await c.eval(`document.querySelector(${JSON.stringify(sel)}).focus()`);
      await sleep(200, 300);
      active = await c.eval('document.activeElement?.id');
      log(`  兜底焦点=${active}`);
    }
    // 清空
    await c.send('Input.dispatchKeyEvent', { type: 'keyDown', modifiers: 2, windowsVirtualKeyCode: 65, key: 'a', code: 'KeyA' });
    await c.send('Input.dispatchKeyEvent', { type: 'keyUp', modifiers: 2, windowsVirtualKeyCode: 65, key: 'a', code: 'KeyA' });
    await sleep(80, 150);
    await c.send('Input.dispatchKeyEvent', { type: 'keyDown', windowsVirtualKeyCode: 8, key: 'Backspace', code: 'Backspace' });
    await c.send('Input.dispatchKeyEvent', { type: 'keyUp', windowsVirtualKeyCode: 8, key: 'Backspace', code: 'Backspace' });
    await sleep(150, 300);
    // 逐字符输入
    for (const ch of val) {
      await c.send('Input.insertText', { text: ch });
      await sleep(rnd(30, 70));
    }
    // ★ 关键：重置 React valueTracker 并派发 input，让 antd Form 感知
    await c.eval(`(() => {
      const el = document.querySelector(${JSON.stringify(sel)});
      if (el._valueTracker) el._valueTracker.setValue('__stale__');
      el.dispatchEvent(new Event('input', { bubbles: true }));
      el.dispatchEvent(new Event('change', { bubbles: true }));
    })()`);
    await sleep(300, 600);
    const got = await c.eval(`document.querySelector(${JSON.stringify(sel)}).value`);
    log(`  结果 = ${JSON.stringify(got)} ${got === val ? '✓' : '✗'}`);
    return got === val;
  };

  log('输入');
  await type('#basic_firstName', IDENT.firstName);
  await type('#basic_lastName', IDENT.lastName);
  await type('#basic_email', IDENT.email);

  if (!(await c.eval(`document.querySelector('input[type=checkbox]')?.checked`))) {
    await c.eval(`document.querySelector('input[type=checkbox]').click()`);
    await sleep(400, 700);
  }
  log('已勾选');

  await sleep(500, 900);
  await c.eval(`(() => { const b2=[...document.querySelectorAll('button')].find(x=>(x.innerText||'').replace(/\\s/g,'').includes('继续')); if(b2) b2.click(); })()`);
  log('点继续');

  let pw = false;
  for (let i = 0; i < 20; i++) {
    await sleep(700, 1000);
    pw = await c.eval(`(() => { const p=document.querySelector('#basic_password'); return !!(p && p.getBoundingClientRect().width>0); })()`);
    if (pw) break;
  }
  if (!pw) { log('密码框未出现 | ' + (await c.eval('document.body.innerText.slice(-220)')).replace(/\n/g, ' / ')); process.exit(2); }
  log('密码框出现');

  await sleep(800, 1500);
  await type('#basic_password', IDENT.password);

  await sleep(500, 900);
  await c.eval(`(() => { const b2=[...document.querySelectorAll('button')].find(x=>(x.innerText||'').replace(/\\s/g,'').includes('继续')); if(b2) b2.click(); })()`);
  log('点继续，等验证码');

  let cap = false;
  for (let i = 0; i < 20; i++) {
    await sleep(700, 1000);
    cap = await c.eval(`(() => { const el=document.querySelector('#aliyunCaptcha-captcha-body'); return !!(el && el.getBoundingClientRect().width>50); })()`);
    if (cap) break;
  }
  if (cap) { log('★★ 验证码已弹出，请主人拖动滑块'); process.exit(0); }
  log('无验证码 | ' + (await c.eval('document.body.innerText.slice(-300)')).replace(/\n/g, ' / '));
  process.exit(1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
