const { CDP } = require('./cdp');
const sleep = ms => new Promise(r => setTimeout(r, ms));
const PAGE_WS = 'ws://localhost:9366/devtools/page/3681DE269541D0AB8303665C31FA1094';
const EMAIL = 'target@example.com';

(async () => {
  const c = new CDP(PAGE_WS);     // page 级连接，无 sessionId
  await c.ready;
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);
  log('已连接 page');

  const href = await c.eval('location.href');
  log('当前:', href);

  const setRes = await c.eval(`(() => {
    const i = document.querySelector('#basic_email');
    if (!i) return 'no input';
    const fk = Object.keys(i).find(k => k.startsWith('__reactFiber$'));
    let f = i[fk], d = 0;
    while (f && d < 70) {
      for (const cd of [f.memoizedProps, f.stateNode, f.stateNode && f.stateNode.props]) {
        if (cd && typeof cd.setFieldsValue === 'function') { cd.setFieldsValue({firstName:'Wei',lastName:'Chen',email:${JSON.stringify(EMAIL)},password:'CHANGE_ME_PASSWORD'}); return 'ok'; }
        if (cd && cd.form && typeof cd.form.setFieldsValue === 'function') { cd.form.setFieldsValue({firstName:'Wei',lastName:'Chen',email:${JSON.stringify(EMAIL)},password:'CHANGE_ME_PASSWORD'}); return 'ok2'; }
      }
      f = f.return; d++;
    }
    return 'nf';
  })()`);
  log('设值:', setRes);
  await sleep(1000, 1500);

  log('勾选:', await c.eval(`(() => { const cb=document.querySelector('input[type=checkbox]'); if(!cb) return 'no'; if(!cb.checked) cb.click(); return String(cb.checked); })()`));
  await sleep(400, 700);

  const box = await c.box('button[type=submit]');
  log('按钮:', JSON.stringify(box));
  await c.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: box.x, y: box.y });
  await sleep(150, 250);
  await c.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: box.x, y: box.y, button: 'left', clickCount: 1 });
  await sleep(80, 140);
  await c.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: box.x, y: box.y, button: 'left', clickCount: 1 });
  log('已点击');

  for (let i = 0; i < 15; i++) {
    await sleep(1100, 1400);
    const st = await c.eval(`JSON.stringify({
      cap: (() => { const e=document.querySelector('#aliyunCaptcha-captcha-body'); return e ? Math.round(e.getBoundingClientRect().width) : 0; })(),
      pw: (() => { const e=document.querySelector('#basic_password'); return e ? Math.round(e.getBoundingClientRect().width) : 0; })(),
      tail: document.body.innerText.slice(-140).replace(/\\n/g,' / ')
    })`);
    log(`  ${i+1}: ${st}`);
    if (JSON.parse(st).cap > 50) { log('★★★ 滑块出现'); process.exit(0); }
  }
  process.exit(1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
