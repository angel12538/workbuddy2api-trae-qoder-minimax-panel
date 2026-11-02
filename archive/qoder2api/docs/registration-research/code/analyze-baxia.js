const { CDP } = require('./cdp');
const PAGE_WS = 'ws://localhost:9366/devtools/page/3681DE269541D0AB8303665C31FA1094';

(async () => {
  const c = new CDP(PAGE_WS);
  await c.ready;
  const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

  // 1. 定位 punish iframe
  const iframeInfo = await c.eval(`JSON.stringify((() => {
    const f = document.querySelector('iframe[src*="punish"]');
    if (!f) return { found: false };
    const r = f.getBoundingClientRect();
    return { found: true, id: f.id, name: f.name, src: f.src.slice(0, 200),
             rect: [Math.round(r.x), Math.round(r.y), Math.round(r.width), Math.round(r.height)],
             sameOrigin: (() => { try { return !!f.contentWindow.document; } catch (e) { return 'blocked: ' + e.name; } })() };
  })())`);
  log('iframe:', iframeInfo);
  const info = JSON.parse(iframeInfo);
  if (!info.found) { console.log('无 iframe'); process.exit(1); }

  // 2. 尝试跨入 iframe 读 DOM
  const inner = await c.eval(`JSON.stringify((() => {
    const f = document.querySelector('iframe[src*="punish"]');
    try {
      const d = f.contentWindow.document;
      const canvases = [...d.querySelectorAll('canvas')].map(cv => {
        const r = cv.getBoundingClientRect();
        return { id: cv.id, cls: String(cv.className).slice(0,50), w: cv.width, h: cv.height,
                 rect: [Math.round(r.x), Math.round(r.y), Math.round(r.width), Math.round(r.height)] };
      });
      const imgs = [...d.querySelectorAll('img')].map(im => ({ id: im.id, src: (im.src||'').slice(0,60), len: (im.src||'').length }));
      return {
        title: d.title,
        bodyText: d.body.innerText.slice(0, 500),
        canvases, imgs,
        hasSecCaptcha: typeof f.contentWindow.SecCaptcha,
        secKeys: f.contentWindow.SecCaptcha ? Object.keys(f.contentWindow.SecCaptcha) : null,
        globals: Object.keys(f.contentWindow).filter(k => /sec|captcha|bx|baxia/i.test(k)).slice(0, 30),
        gridEls: d.querySelectorAll('[class*=grid],[class*=item],[class*=cell]').length
      };
    } catch (e) { return { error: e.name + ': ' + e.message }; }
  })())`);
  log('iframe 内:', inner);
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
