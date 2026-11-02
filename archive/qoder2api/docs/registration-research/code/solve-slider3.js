/**
 * solve-slider3.js — 一键求解（触发→检测→拖拽→判定，单次运行内完成）
 * 教训：验证码有超时（约30-60s），全程必须在一个脚本跑完，中间不能做诊断。
 * 若检测到"验证超时"状态，先关闭再重新触发。
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');

const BROWSER_WS = 'ws://localhost:9366/devtools/browser/9bb82102-33e6-4191-bde4-f849d63b27aa';
const sleep = (ms) => new Promise(r => setTimeout(r, ms));

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

  // ========== 0. 清理超时状态 ==========
  const stuck = await c.eval(`document.body.innerText.includes('验证超时')`);
  if (stuck) {
    log(el(), '检测到超时状态，关闭验证码');
    await c.eval(`(() => { const x = document.querySelector('#aliyunCaptcha-btn-close'); if (x) x.click(); })()`);
    await sleep(1000, 1500);
  }

  // ========== 1. 触发 ==========
  await c.eval(`document.querySelector('#aliyunCaptcha-captcha-body').click()`);
  log(el(), '已触发，等图片加载');
  // 等图片+手柄就绪（轮询，最多 8s）
  let ready = false;
  for (let i = 0; i < 16; i++) {
    await sleep(400, 600);
    ready = await c.eval(`(() => {
      const img = document.querySelector('#aliyunCaptcha-img');
      const sl = document.querySelector('#aliyunCaptcha-sliding-slider');
      if (!img || !sl) return false;
      const r1 = img.getBoundingClientRect(), r2 = sl.getBoundingClientRect();
      return r1.width > 100 && r2.width > 10;
    })()`);
    if (ready) break;
  }
  if (!ready) { console.error(el(), '图片/手柄未就绪'); process.exit(1); }
  log(el(), '图片与手柄就绪');

  // ========== 2. 缺口检测 ==========
  const gapResult = await c.eval(`(async () => {
    const bgImg = document.querySelector('#aliyunCaptcha-img');
    const pzImg = document.querySelector('#aliyunCaptcha-puzzle');
    const load = (img) => new Promise(res => { if (img.complete && img.naturalWidth) return res(); img.onload = () => res(); setTimeout(res, 2000); });
    await load(bgImg); await load(pzImg);
    const W = bgImg.naturalWidth, H = bgImg.naturalHeight, PW = pzImg.naturalWidth;
    const bgC = document.createElement('canvas'); bgC.width = W; bgC.height = H;
    const bgX = bgC.getContext('2d'); bgX.drawImage(bgImg, 0, 0);
    const bgData = bgX.getImageData(0, 0, W, H).data;
    const pzC = document.createElement('canvas'); pzC.width = PW; pzC.height = H;
    const pzX = pzC.getContext('2d'); pzX.drawImage(pzImg, 0, 0);
    const pzData = pzX.getImageData(0, 0, PW, H).data;
    let bestX = -1, bestScore = Infinity;
    for (let x = 0; x <= W - PW; x++) {
      let diff = 0, n = 0;
      for (let py = 0; py < H; py += 2) for (let px = 0; px < PW; px += 2) {
        const pi = (py * PW + px) * 4;
        if (pzData[pi + 3] < 128) continue;
        const bi = (py * W + x + px) * 4;
        diff += Math.abs(pzData[pi] - bgData[bi]) + Math.abs(pzData[pi+1] - bgData[bi+1]) + Math.abs(pzData[pi+2] - bgData[bi+2]);
        n++;
      }
      const score = n ? diff / n : Infinity;
      if (score < bestScore) { bestScore = score; bestX = x; }
    }
    return JSON.stringify({ W, PW, bestX });
  })()`, true);
  const gap = JSON.parse(gapResult);
  log(el(), '缺口:', JSON.stringify(gap));

  // ========== 3. 快速拖拽（~1.5s） ==========
  const handle = await c.box('#aliyunCaptcha-sliding-slider');
  const startX = handle.x, startY = handle.y;
  // 映射修正（实测+理论）: 拼图位移 = 手柄位移 × (W-PW)/(trackW-handleW) = ×244/260
  const dragDist = Math.round(gap.bestX * 260 / 244);
  log(el(), `拖拽 ${dragDist}px`);

  for (let i = 1; i <= 8; i++) { // 移入
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + (Math.random() - 0.5) * 20, y: startY - 50 + i * 6 });
    await sleep(12, 25);
  }
  await sleep(120, 250);
  fire('Input.dispatchMouseEvent', { type: 'mousePressed', x: startX, y: startY, button: 'left', clickCount: 1 });
  await sleep(70, 150);

  const steps = 24;
  const overshoot = Math.random() < 0.6 ? 2 + Math.random() * 5 : 0;
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

  // ========== 4. 结果 ==========
  const result = await c.eval(`JSON.stringify({
    text: document.querySelector('#aliyunCaptcha-captcha-text')?.innerText,
    floatCls: document.querySelector('#aliyunCaptcha-window-float')?.className,
    bodyCls: (() => { const b = document.querySelector('#aliyunCaptcha-captcha-body'); return b ? String(b.className) : null; })()
  })`);
  log(el(), '结果:', result);
  const shot = await c.send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(__dirname + '/../artifacts/slider-result4.png', Buffer.from(shot.result.data, 'base64'));
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
