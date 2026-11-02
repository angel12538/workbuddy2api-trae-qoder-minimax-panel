const { CDP } = require('./cdp');
const sleep = ms => new Promise(r => setTimeout(r, ms));
const PAGE_WS = 'ws://localhost:9366/devtools/page/5EEBF5919773ECDD41E7E7A394C397B1';
(async () => {
  const c = new CDP(PAGE_WS);
  await c.ready;
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);
  log('监听中（请手动点绿色按钮并拖滑块）...');
  let last = '';
  for (let i = 0; i < 200; i++) {
    await sleep(1500, 1900);
    const st = await c.eval(`JSON.stringify({
      f: document.querySelector('#aliyunCaptcha-window-float')?.className || 'none',
      t: document.querySelector('#aliyunCaptcha-captcha-text')?.innerText || '',
      body: document.body.innerText
    })`);
    const d = JSON.parse(st);
    const sig = d.f + '|' + d.t;
    if (sig !== last) { log('状态:', d.f, '|', d.t || '(空)'); last = sig; }
    if (d.body.includes('验证通过') || d.body.includes('请输入验证码') || d.body.includes('验证码已发送')) {
      log('★★★ 通过! 尾部:', d.body.slice(-260).replace(/\n/g, ' / '));
      process.exit(0);
    }
  }
  process.exit(1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
