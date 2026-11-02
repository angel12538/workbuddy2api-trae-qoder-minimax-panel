/**
 * solve-slider5.js — v5：拟人化增强 + 松手前位置自检
 * 改进：① 更长按住时间 ② y 轴更大摆动 ③ 微停顿 ④ 总时长 2.5-3.5s
 *       ⑤ 松手前读取拼图实际位置与缺口比对（诊断用，不额外发请求）
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');
const { execSync } = require('child_process');
const https = require('https');

const BROWSER_WS = 'ws://localhost:9366/devtools/browser/9bb82102-33e6-4191-bde4-f849d63b27aa';
const ART = __dirname + '/../artifacts';
const sleep = (ms) => new Promise(r => setTimeout(r, ms));

function download(url, dest) {
  return new Promise((resolve, reject) => {
    https.get(url, (res) => {
      if (res.statusCode !== 200) return reject(new Error('HTTP ' + res.statusCode));
      const f = fs.createWriteStream(dest);
      res.pipe(f);
      f.on('finish', () => f.close(resolve));
    }).on('error', reject);
  });
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
  const fire = (m, p) => c._fire(m, p);
  const T0 = Date.now();
  const el = () => ((Date.now() - T0) / 1000).toFixed(1) + 's';

  await c.eval(`document.querySelector('#aliyunCaptcha-captcha-body').click()`);
  let ready = false;
  for (let i = 0; i < 16; i++) {
    await sleep(400, 600);
    ready = await c.eval(`(() => {
      const img = document.querySelector('#aliyunCaptcha-img');
      const sl = document.querySelector('#aliyunCaptcha-sliding-slider');
      if (!img || !sl || !img.src) return false;
      const r1 = img.getBoundingClientRect(), r2 = sl.getBoundingClientRect();
      return r1.width > 100 && r2.width > 10 && img.complete;
    })()`);
    if (ready) break;
  }
  if (!ready) { console.error(el(), '未就绪'); process.exit(1); }

  const urls = JSON.parse(await c.eval(`JSON.stringify({
    bg: document.querySelector('#aliyunCaptcha-img').src,
    pz: document.querySelector('#aliyunCaptcha-puzzle').src
  })`));
  await download(urls.bg, ART + '/bg-live.png');
  await download(urls.pz, ART + '/pz-live.png');
  const gap = JSON.parse(execSync(`python3 ${__dirname}/detect_gap.py ${ART}/bg-live.png ${ART}/pz-live.png`).toString());
  log(el(), '缺口:', JSON.stringify(gap));

  const handle = await c.box('#aliyunCaptcha-sliding-slider');
  const imgBox = await c.box('#aliyunCaptcha-img');
  const startX = handle.x, startY = handle.y;
  const dragDist = Math.round(gap.bestX * 1.098);
  log(el(), `手柄(${startX},${startY}) 图片左边界x=${imgBox.x - imgBox.w/2} 拖拽${dragDist}px`);

  // 自然移入（带轻微弧线）
  for (let i = 1; i <= 10; i++) {
    const tt = i / 10;
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX - 40 + 40 * tt + (Math.random() - 0.5) * 3, y: startY - 30 + 30 * tt + (Math.random() - 0.5) * 2 });
    await sleep(18, 38);
  }
  await sleep(200, 450);                                   // 悬停犹豫
  fire('Input.dispatchMouseEvent', { type: 'mousePressed', x: startX, y: startY, button: 'left', clickCount: 1 });
  await sleep(180, 380);                                   // 按住停顿（人类会先稳住）
  // 按下后微小抖动（真实感）
  for (let i = 0; i < 3; i++) {
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + (Math.random() - 0.5) * 1.5, y: startY + (Math.random() - 0.5) * 1.5, button: 'left' });
    await sleep(40, 90);
  }

  // 主拖拽：36 步，总时长 ~2.4s；y 轴 1-2.5px 摆动；偶发微停顿
  const steps = 36;
  const overshoot = 3 + Math.random() * 5;                 // 人类几乎总会过冲
  const total = dragDist + overshoot;
  for (let i = 1; i <= steps; i++) {
    const tt = i / steps;
    // 三段式：慢起(0-0.2) → 快(0.2-0.7) → 减速(0.7-1)
    const ease = tt < 0.2 ? 1.9 * tt * tt
      : tt < 0.7 ? 0.38 + 0.62 * (tt - 0.2) / 0.5
      : 1 - Math.pow(1 - tt, 2.5) * 0.32;
    const yWobble = Math.sin(tt * Math.PI * 2.3) * 1.6 + (Math.random() - 0.5) * 1.2;
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + total * ease + (Math.random() - 0.5) * 0.8, y: startY + yWobble, button: 'left' });
    await sleep(45, 95);
    if (Math.random() < 0.07) await sleep(90, 200);        // 微停顿
  }
  // 回拉对位（人眼校准）
  await sleep(150, 300);
  for (let i = 5; i >= 0; i--) {
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + dragDist + overshoot * i / 6, y: startY + (Math.random() - 0.5), button: 'left' });
    await sleep(50, 110);
  }
  await sleep(250, 500);

  // 松手前自检：拼图实际位置 vs 缺口
  const pieceBox = await c.box('#aliyunCaptcha-puzzle');
  const pieceLeft = pieceBox.x - pieceBox.w / 2;
  const imageLeft = imgBox.x - imgBox.w / 2;
  const pieceOffset = Math.round(pieceLeft - imageLeft);
  log(el(), `自检: 拼图偏移=${pieceOffset}px 缺口=${gap.bestX}px 误差=${pieceOffset - gap.bestX}px`);

  fire('Input.dispatchMouseEvent', { type: 'mouseReleased', x: startX + dragDist, y: startY, button: 'left', clickCount: 1 });
  log(el(), '已松开');
  await sleep(2600, 3600);

  const result = await c.eval(`JSON.stringify({
    text: document.querySelector('#aliyunCaptcha-captcha-text')?.innerText,
    floatCls: document.querySelector('#aliyunCaptcha-window-float')?.className
  })`);
  log(el(), '结果:', result);
  const shot = await c.send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(ART + '/slider-result6.png', Buffer.from(shot.result.data, 'base64'));
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
