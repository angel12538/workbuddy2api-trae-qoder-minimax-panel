/**
 * solve-slider.js — 阿里云滑块验证码求解器
 * 步骤：浏览器内模板匹配找缺口 → 拟人拖拽 → 检测结果。
 * 人类节奏：拖拽轨迹带加速/减速/微抖动/过冲，单次尝试，不 hammer。
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

  // ========== 1. 缺口检测（浏览器内模板匹配） ==========
  const gapResult = await c.eval(`(async () => {
    const bgImg = document.querySelector('#aliyunCaptcha-img');
    const pzImg = document.querySelector('#aliyunCaptcha-puzzle');
    if (!bgImg || !pzImg) return JSON.stringify({error: 'images not found'});
    const load = (img) => new Promise(res => {
      if (img.complete && img.naturalWidth) return res();
      img.onload = () => res();
      setTimeout(res, 3000);
    });
    await load(bgImg); await load(pzImg);
    const W = bgImg.naturalWidth, H = bgImg.naturalHeight;
    const PW = pzImg.naturalWidth;
    const bgC = document.createElement('canvas'); bgC.width = W; bgC.height = H;
    const bgX = bgC.getContext('2d'); bgX.drawImage(bgImg, 0, 0);
    const bgData = bgX.getImageData(0, 0, W, H).data;
    const pzC = document.createElement('canvas'); pzC.width = PW; pzC.height = H;
    const pzX = pzC.getContext('2d'); pzX.drawImage(pzImg, 0, 0);
    const pzData = pzX.getImageData(0, 0, PW, H).data;
    // 模板匹配：拼图块不透明像素 vs 背景 [x, x+PW]
    let bestX = -1, bestScore = Infinity;
    const scores = [];
    for (let x = 0; x <= W - PW; x++) {
      let diff = 0, n = 0;
      for (let py = 0; py < H; py += 2) {
        for (let px = 0; px < PW; px += 2) {
          const pi = (py * PW + px) * 4;
          if (pzData[pi + 3] < 128) continue; // 跳过透明像素
          const bi = (py * W + x + px) * 4;
          diff += Math.abs(pzData[pi] - bgData[bi]) + Math.abs(pzData[pi+1] - bgData[bi+1]) + Math.abs(pzData[pi+2] - bgData[bi+2]);
          n++;
        }
      }
      const score = n ? diff / n : Infinity;
      scores.push([x, Math.round(score)]);
      if (score < bestScore) { bestScore = score; bestX = x; }
    }
    // 输出 top5 候选
    scores.sort((a, b2) => a[1] - b2[1]);
    return JSON.stringify({ W, H, PW, bestX, bestScore: Math.round(bestScore), top5: scores.slice(0, 5) });
  })()`, true);
  const gap = JSON.parse(gapResult);
  log('缺口检测:', JSON.stringify(gap));
  if (gap.error || gap.bestX < 0) { console.error('检测失败'); process.exit(1); }

  // ========== 2. 拟人拖拽 ==========
  const handle = await c.box('.slider-move');
  log('手柄:', JSON.stringify(handle));
  const startX = handle.x, startY = handle.y;
  // 映射：图片宽 300 → 轨道宽（popup 334 - 手柄 40 ≈ 294）；缺口在图片坐标 → 屏幕坐标
  const trackW = 334 - 40;
  const imgW = gap.W;
  const dragDist = Math.round(gap.bestX * trackW / imgW);
  log(`拖拽距离: ${dragDist}px (缺口图内 x=${gap.bestX})`);

  // 人类式拖拽：按下 → 加速 → 减速 → 微过冲 → 回拉 → 稳定 → 松开
  await c.humanMove(startX - 100, startY - 50, startX, startY, 15); // 从别处移过来
  await sleep(200, 400);
  await c.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: startX, y: startY, button: 'left', clickCount: 1 });
  await sleep(100, 250);

  const steps = 30 + Math.floor(Math.random() * 15);
  const overshoot = Math.random() < 0.7 ? 3 + Math.random() * 8 : 0; // 70% 概率过冲
  const total = dragDist + overshoot;
  for (let i = 1; i <= steps; i++) {
    const t = i / steps;
    // 人类加速度曲线：起步慢、中段快、结尾减速
    const ease = t < 0.3 ? 1.5 * t * t : t < 0.8 ? 0.45 + 0.55 * (t - 0.3) / 0.5 : 1 - Math.pow(1 - t, 2) * 0.3;
    const x = startX + total * ease + (Math.random() - 0.5) * 1.2;
    const y = startY + (Math.random() - 0.5) * 1.5;
    await c.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y, button: 'left' });
    await sleep(12, 40);
  }
  // 过冲回拉
  if (overshoot > 0) {
    await sleep(150, 350);
    for (let i = 0; i < 5; i++) {
      const x = startX + dragDist + (overshoot * (5 - i) / 5) + (Math.random() - 0.5);
      await c.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y: startY, button: 'left' });
      await sleep(30, 80);
    }
  }
  await sleep(200, 500); // 停顿后松开（像人在确认）
  await c.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: startX + dragDist, y: startY, button: 'left', clickCount: 1 });
  log('拖拽完成，等待判定...');
  await sleep(3000, 4500);

  // ========== 3. 结果检测 ==========
  const result = await c.eval(`(() => {
    const txt = document.querySelector('#aliyunCaptcha-captcha-text');
    const float = document.querySelector('#aliyunCaptcha-window-float');
    return JSON.stringify({
      text: txt ? txt.innerText : null,
      floatCls: float ? String(float.className) : null,
      bodyCls: (() => { const b = document.querySelector('#aliyunCaptcha-captcha-body'); return b ? String(b.className) : null; })(),
    });
  })()`);
  log('结果:', result);
  const shot = await c.send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(__dirname + '/../artifacts/slider-result.png', Buffer.from(shot.result.data, 'base64'));
  log('截图已存 artifacts/slider-result.png');
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
