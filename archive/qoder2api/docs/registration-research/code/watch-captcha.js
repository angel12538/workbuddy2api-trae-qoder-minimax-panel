/**
 * watch-captcha.js — 只读监听：等人手动通过滑块
 * 每 1.5s 读一次 DOM 状态，检测到验证通过（弹窗关闭/进入验证码输入步）即退出。
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

  log('开始监听（请手动拖滑块）...');
  let last = '';
  for (let i = 0; i < 240; i++) {   // 最多 6 分钟
    const st = JSON.parse(await c.eval(`JSON.stringify({
      float: document.querySelector('#aliyunCaptcha-window-float')?.className || 'none',
      text: document.querySelector('#aliyunCaptcha-captcha-text')?.innerText || '',
      bodyTail: document.body.innerText.slice(-200)
    })`));
    const sig = st.float + '|' + st.text;
    if (sig !== last) { log('状态:', st.float, '|', st.text || '(空)'); last = sig; }
    // 通过信号：弹窗隐藏/移除，或页面出现验证码输入框
    const passed = (!st.float.includes('window-show'))
      || st.bodyTail.includes('验证码') || st.bodyTail.includes('verification')
      || st.bodyTail.includes('请输入验证码') || st.bodyTail.includes('输入验证码');
    if (passed) { log('★ 检测到验证通过/进入下一步'); log('页面尾部:', st.bodyTail.replace(/\n/g, ' / ')); process.exit(0); }
    await sleep(1400, 1800);
  }
  log('监听超时');
  process.exit(1);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
