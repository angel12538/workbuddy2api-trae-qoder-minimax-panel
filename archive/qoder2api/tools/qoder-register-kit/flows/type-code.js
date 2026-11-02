const { CDP } = require('./cdp');
const sleep = ms => new Promise(r => setTimeout(r, ms));
const PAGE_WS = 'ws://localhost:9366/devtools/page/5EEBF5919773ECDD41E7E7A394C397B1';
const CODE = process.argv[2] || '418251';

(async () => {
  const c = new CDP(PAGE_WS);
  await c.ready;
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

  // 点击输入框
  const box = await c.eval(`(() => {
    const i = document.querySelector('input[type=text]');
    const r = i.getBoundingClientRect();
    return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 });
  })()`);
  const p = JSON.parse(box);
  await c.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: p.x, y: p.y, button: 'left', clickCount: 1 });
  await sleep(50, 90);
  await c.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: p.x, y: p.y, button: 'left', clickCount: 1 });
  await sleep(300, 500);

  // 全选删除（Cmd+A + Delete）
  for (const m of [4, 0]) { // 4=Meta
    await c.send('Input.dispatchKeyEvent', { type: 'keyDown', modifiers: m, windowsVirtualKeyCode: 65, key: 'a', code: 'KeyA' });
    await c.send('Input.dispatchKeyEvent', { type: 'keyUp', modifiers: m, windowsVirtualKeyCode: 65, key: 'a', code: 'KeyA' });
    await sleep(120, 200);
  }
  await c.send('Input.dispatchKeyEvent', { type: 'keyDown', windowsVirtualKeyCode: 8, key: 'Backspace', code: 'Backspace' });
  await c.send('Input.dispatchKeyEvent', { type: 'keyUp', windowsVirtualKeyCode: 8, key: 'Backspace', code: 'Backspace' });
  await sleep(400, 700);
  log('清空后:', await c.eval(`document.querySelector('input[type=text]').value`));

  // 逐位输入
  for (const d of CODE) {
    const vk = d.charCodeAt(0);
    await c.send('Input.dispatchKeyEvent', { type: 'keyDown', text: d, unmodifiedText: d, key: d, code: 'Digit' + d, windowsVirtualKeyCode: vk, nativeVirtualKeyCode: vk });
    await sleep(70, 130);
    await c.send('Input.dispatchKeyEvent', { type: 'keyUp', key: d, code: 'Digit' + d, windowsVirtualKeyCode: vk, nativeVirtualKeyCode: vk });
    await sleep(100, 180);
  }
  await sleep(600, 900);
  log('当前值:', await c.eval(`document.querySelector('input[type=text]').value`));

  // DOM 层也同步一次（防止 React state 没收到）
  await c.eval(`(() => {
    const i = document.querySelector('input[type=text]');
    if (i.value !== ${JSON.stringify(CODE)}) {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      if (i._valueTracker) i._valueTracker.setValue('__x__');
      setter.call(i, ${JSON.stringify(CODE)});
      i.dispatchEvent(new Event('input', { bubbles: true }));
    }
  })()`);
  await sleep(400, 700);
  log('同步后:', await c.eval(`document.querySelector('input[type=text]').value`));

  // 等待自动提交
  for (let i = 0; i < 12; i++) {
    await sleep(1400, 1800);
    const st = await c.eval(`JSON.stringify({ href: location.href, body: document.body.innerText.slice(0, 260).replace(/\\n/g, ' / ') })`);
    log(i + 1, st);
    const d = JSON.parse(st);
    if (!d.body.includes('验证邮箱')) { log('★ 已离开验证码页'); break; }
  }
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
