/**
 * solve-slider4.js — 一键求解 v4（浏览器外检测）
 * 流程：触发 → 取图 URL → Node 下载 → Python 检测缺口 → CDP 拖拽。
 * 映射修正：dragDist = gap_x × 260/244（拼图位移 = 手柄位移 × 244/260）。
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

  // ========== 1. 触发 ==========
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
  log(el(), '验证码已弹出');

  // ========== 2. 取图 URL 并下载 ==========
  const urls = JSON.parse(await c.eval(`JSON.stringify({
    bg: document.querySelector('#aliyunCaptcha-img').src,
    pz: document.querySelector('#aliyunCaptcha-puzzle').src
  })`));
  log(el(), '图片 URL 已取');
  await download(urls.bg, ART + '/bg-live.png');
  await download(urls.pz, ART + '/pz-live.png');
  log(el(), '图片已下载');

  // ========== 3. Python 检测 ==========
  const detectOut = execSync(`python3 ${__dirname}/detect_gap.py ${ART}/bg-live.png ${ART}/pz-live.png`).toString();
  const gap = JSON.parse(detectOut);
  log(el(), '缺口检测:', JSON.stringify(gap));

  // ========== 4. 拖拽（映射: ×260/244） ==========
  const handle = await c.box('#aliyunCaptcha-sliding-slider');
  const startX = handle.x, startY = handle.y;
  const dragDist = Math.round(gap.bestX * 260 / 244);
  log(el(), `手柄(${startX},${startY}) 拖拽 ${dragDist}px`);

  for (let i = 1; i <= 8; i++) {
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + (Math.random() - 0.5) * 20, y: startY - 50 + i * 6 });
    await sleep(12, 25);
  }
  await sleep(120, 250);
  fire('Input.dispatchMouseEvent', { type: 'mousePressed', x: startX, y: startY, button: 'left', clickCount: 1 });
  await sleep(70, 150);

  const steps = 26;
  const overshoot = Math.random() < 0.5 ? 2 + Math.random() * 4 : 0;
  const total = dragDist + overshoot;
  for (let i = 1; i <= steps; i++) {
    const t = i / steps;
    const ease = t < 0.25 ? 1.6 * t * t : t < 0.75 ? 0.4 + 0.6 * (t - 0.25) / 0.5 : 1 - Math.pow(1 - t, 2.2) * 0.35;
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + total * ease + (Math.random() - 0.5), y: startY + (Math.random() - 0.5), button: 'left' });
    await sleep(35, 60);
  }
  if (overshoot > 0) {
    await sleep(100, 200);
    for (let i = 4; i >= 0; i--) {
      fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + dragDist + overshoot * i / 5, y: startY, button: 'left' });
      await sleep(35, 70);
    }
  }
  await sleep(150, 350);
  fire('Input.dispatchMouseEvent', { type: 'mouseReleased', x: startX + dragDist, y: startY, button: 'left', clickCount: 1 });
  log(el(), '已松开，判定中...');
  await sleep(2500, 3500);

  // ========== 5. 结果 ==========
  const result = await c.eval(`JSON.stringify({
    text: document.querySelector('#aliyunCaptcha-captcha-text')?.innerText,
    floatCls: document.querySelector('#aliyunCaptcha-window-float')?.className,
    bodyCls: (() => { const b = document.querySelector('#aliyunCaptcha-captcha-body'); return b ? String(b.className) : null; })()
  })`);
  log(el(), '结果:', result);
  const shot = await c.send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(ART + '/slider-result5.png', Buffer.from(shot.result.data, 'base64'));
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
