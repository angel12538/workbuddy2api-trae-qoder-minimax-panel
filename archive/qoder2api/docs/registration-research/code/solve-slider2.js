/**
 * solve-slider2.js — 阿里云滑块求解器 v2
 * 修正：① 拖拽改发射后不等（~1.5s 完成，避免 40s 异常时长被时序检测）
 *       ② 触发用 JS click（CDP 鼠标事件对该 SDK 无效，已实测）
 *       ③ 缺口检测保留模板匹配（实测 237 与视觉一致）
 * 单次尝试，人类节奏。
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

  // ========== 1. 刷新 + 触发新验证码 ==========
  await c.eval(`(() => { const rf = document.querySelector('#aliyunCaptcha-btn-refresh'); if (rf) rf.click(); })()`);
  await sleep(800, 1200);
  const trig = await c.eval(`(() => { const el = document.querySelector('#aliyunCaptcha-captcha-body'); if (!el) return 'NOT_FOUND'; el.click(); return 'ok'; })()`);
  log('触发验证码:', trig);
  await sleep(2500, 4000); // 等图片加载

  // ========== 2. 缺口检测 ==========
  const gapResult = await c.eval(`(async () => {
    const bgImg = document.querySelector('#aliyunCaptcha-img');
    const pzImg = document.querySelector('#aliyunCaptcha-puzzle');
    if (!bgImg || !pzImg) return JSON.stringify({error: 'images not found'});
    const load = (img) => new Promise(res => { if (img.complete && img.naturalWidth) return res(); img.onload = () => res(); setTimeout(res, 3000); });
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
    return JSON.stringify({ W, H, PW, bestX, bestScore: Math.round(bestScore) });
  })()`, true);
  const gap = JSON.parse(gapResult);
  log('缺口检测:', JSON.stringify(gap));
  if (gap.error || gap.bestX < 0) { console.error('检测失败'); process.exit(1); }

  // ========== 3. 快速拟人拖拽（~1.5s） ==========
  const handle = await c.box('#aliyunCaptcha-sliding-slider');
  if (!handle || handle.w === 0) { console.error('手柄不可见'); process.exit(1); }
  const startX = handle.x, startY = handle.y;
  const trackW = 334 - 40;                    // 轨道宽
  const dragDist = Math.round(gap.bestX * trackW / gap.W);
  log(`手柄 (${startX}, ${startY}) 拖拽距离 ${dragDist}px`);

  // 从上方移入（hover 意图）
  for (let i = 1; i <= 8; i++) {
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + (Math.random() - 0.5) * 30, y: startY - 60 + i * 7 });
    await sleep(15, 35);
  }
  await sleep(150, 300);
  fire('Input.dispatchMouseEvent', { type: 'mousePressed', x: startX, y: startY, button: 'left', clickCount: 1 });
  await sleep(80, 180);

  // 主拖拽：人类加速度（起步慢→中段快→减速），~25 步 × 40-60ms ≈ 1.3s
  const steps = 25;
  const overshoot = Math.random() < 0.6 ? 2 + Math.random() * 6 : 0;
  const total = dragDist + overshoot;
  for (let i = 1; i <= steps; i++) {
    const t = i / steps;
    const ease = t < 0.25 ? 1.6 * t * t : t < 0.75 ? 0.4 + 0.6 * (t - 0.25) / 0.5 : 1 - Math.pow(1 - t, 2.2) * 0.35;
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + total * ease + (Math.random() - 0.5), y: startY + (Math.random() - 0.5) * 1.2, button: 'left' });
    await sleep(35, 65);
  }
  // 过冲回拉
  if (overshoot > 0) {
    await sleep(120, 250);
    for (let i = 4; i >= 0; i--) {
      fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + dragDist + overshoot * i / 5, y: startY, button: 'left' });
      await sleep(40, 90);
    }
  }
  await sleep(180, 400);
  fire('Input.dispatchMouseEvent', { type: 'mouseReleased', x: startX + dragDist, y: startY, button: 'left', clickCount: 1 });
  log('拖拽完成，等待判定（3s）...');
  await sleep(3000, 4000);

  // ========== 4. 结果 ==========
  const result = await c.eval(`JSON.stringify({
    text: document.querySelector('#aliyunCaptcha-captcha-text')?.innerText,
    floatCls: document.querySelector('#aliyunCaptcha-window-float')?.className,
    bodyCls: (() => { const b = document.querySelector('#aliyunCaptcha-captcha-body'); return b ? String(b.className) : null; })()
  })`);
  log('结果:', result);
  const shot = await c.send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(__dirname + '/../artifacts/slider-result2.png', Buffer.from(shot.result.data, 'base64'));
  log('截图已存');
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
