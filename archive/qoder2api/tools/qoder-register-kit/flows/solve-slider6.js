/**
 * solve-slider6.js — v6：闭环自适应拖拽
 * 思路：拖一半 → 等渲染 → 实测「拼图位移/手柄位移」比例 → 按实测比例补足 →
 *       松手前再测一次微调 → 松手。全程人类节奏，位置误差 ≤1px 才松手。
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

  const handle0 = await c.box('#aliyunCaptcha-sliding-slider');
  const img0 = await c.box('#aliyunCaptcha-img');
  const handleLeft0 = handle0.x - handle0.w / 2;
  const imgLeft0 = img0.x - img0.w / 2;
  const startX = handle0.x, startY = handle0.y;

  // 位置读取器：等动画稳定（连续两次读数一致）
  const readOnce = async () => {
    const h = await c.box('#aliyunCaptcha-sliding-slider');
    const p = await c.box('#aliyunCaptcha-puzzle');
    return {
      handleMove: Math.round(h.x - h.w / 2 - handleLeft0),
      pieceMove: Math.round(p.x - p.w / 2 - imgLeft0),
    };
  };
  const measure = async () => {
    await sleep(500, 700);
    let prev = await readOnce();
    for (let i = 0; i < 6; i++) {
      await sleep(350, 450);
      const cur = await readOnce();
      if (cur.pieceMove === prev.pieceMove && cur.handleMove === prev.handleMove) return cur;
      prev = cur;
    }
    return prev;
  };

  // 自然移入 + 按住
  for (let i = 1; i <= 10; i++) {
    const tt = i / 10;
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX - 40 + 40 * tt + (Math.random() - 0.5) * 3, y: startY - 30 + 30 * tt + (Math.random() - 0.5) * 2 });
    await sleep(18, 38);
  }
  await sleep(200, 400);
  fire('Input.dispatchMouseEvent', { type: 'mousePressed', x: startX, y: startY, button: 'left', clickCount: 1 });
  await sleep(180, 350);

  // 阶段1：拖到目标的一半（fire 提速）
  const half = Math.round(gap.bestX * 0.5);
  const steps1 = 14;
  for (let i = 1; i <= steps1; i++) {
    const tt = i / steps1;
    const ease = tt < 0.3 ? 1.4 * tt * tt : 0.42 + 0.58 * (tt - 0.3) / 0.7;
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + half * ease, y: startY + Math.sin(tt * Math.PI) * 1.4, button: 'left' });
    await sleep(45, 85);
  }
  const m1 = await measure();
  const ratio = m1.pieceMove / Math.max(m1.handleMove, 1);
  const effRatio = (ratio > 0.5 && ratio < 1.2) ? ratio : 0.92;   // 异常值回退

  // 阶段2：按实测比例补足
  log(el(), `阶段1 实测: 手柄+${m1.handleMove} 拼图+${m1.pieceMove} 比例=${ratio.toFixed(3)} 采用=${effRatio.toFixed(3)}`);
  let need = Math.round((gap.bestX - m1.pieceMove) / effRatio);
  need = Math.max(0, Math.min(need, 300 - m1.handleMove));        // 不超过轨道
  const steps2 = Math.max(8, Math.round(need / 8));
  const from = m1.handleMove;
  for (let i = 1; i <= steps2; i++) {
    const tt = i / steps2;
    const ease = tt < 0.6 ? 1.2 * tt * tt : 0.43 + 0.57 * (tt - 0.6) / 0.4;
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: startX + from + need * ease, y: startY + (Math.random() - 0.5) * 1.2, button: 'left' });
    await sleep(45, 90);
  }
  const m2 = await measure();
  let err = m2.pieceMove - gap.bestX;
  log(el(), `阶段2 实测: 拼图偏移=${m2.pieceMove} 目标=${gap.bestX} 误差=${err}px`);

  // 阶段3：微调（±2px 内逐像素）
  let handlePos = startX + m2.handleMove;
  for (let k = 0; k < 3 && Math.abs(err) > 1; k++) {
    const adjust = Math.round(-err / effRatio);
    handlePos += adjust;
    fire('Input.dispatchMouseEvent', { type: 'mouseMoved', x: handlePos, y: startY, button: 'left' });
    await sleep(120, 220);
    const m3 = await measure();
    err = m3.pieceMove - gap.bestX;
    log(el(), `  微调${k + 1}: 误差=${err}px`);
  }

  await sleep(200, 400);
  fire('Input.dispatchMouseEvent', { type: 'mouseReleased', x: handlePos, y: startY, button: 'left', clickCount: 1 });
  log(el(), `已松开 (最终误差 ${err}px)`);
  await sleep(2600, 3600);

  const result = await c.eval(`JSON.stringify({
    text: document.querySelector('#aliyunCaptcha-captcha-text')?.innerText,
    floatCls: document.querySelector('#aliyunCaptcha-window-float')?.className
  })`);
  log(el(), '结果:', result);
  const shot = await c.send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(ART + '/slider-result7.png', Buffer.from(shot.result.data, 'base64'));
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
