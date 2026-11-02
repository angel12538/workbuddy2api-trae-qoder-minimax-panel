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

  // 设值
  const setRes = await c.eval(`(() => {
    const i = document.querySelector('#basic_email');
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
  await sleep(1200, 1800);

  // 勾选
  const cbOk = await c.eval(`(() => { const cb=document.querySelector('input[type=checkbox]'); if(!cb) return 'no'; if(!cb.checked) cb.click(); return String(cb.checked); })()`);
  log('勾选:', cbOk);
  await sleep(400, 700);

  // 读 DOM 确认
  const dom = await c.eval(`JSON.stringify({fn:document.querySelector('#basic_firstName').value,em:document.querySelector('#basic_email').value,pw:document.querySelector('#basic_password').value})`);
  log('DOM:', dom);

  // 真实鼠标点击按钮
  const box = await c.box('button[type=submit]');
  await c.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: box.x, y: box.y });
  await sleep(120, 220);
  await c.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: box.x, y: box.y, button: 'left', clickCount: 1 });
  await sleep(70, 130);
  await c.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: box.x, y: box.y, button: 'left', clickCount: 1 });
  log('已点击');

  for (let i = 0; i < 12; i++) {
    await sleep(1100, 1400);
    const st = await c.eval(`JSON.stringify({
      cap: (() => { const e=document.querySelector('#aliyunCaptcha-captcha-body'); return e ? Math.round(e.getBoundingClientRect().width) : 0; })(),
      tail: document.body.innerText.slice(-160).replace(/\\n/g,' / ')
    })`);
    log(`  ${i+1}: ${st}`);
    if (JSON.parse(st).cap > 50) { log('★★★ 滑块出现，请主人拖'); process.exit(0); }
  }
  process.exit(1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
