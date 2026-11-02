/**
 * cdp.js — CDP 驱动（人类节奏版）
 * 硬约束：单线程、随机延迟、拟人轨迹。禁止高并发、禁止紧密循环请求。
 */
const WebSocket = require('ws');
const _counters = new WeakMap(); // ws → 共享 id 计数器

class CDP {
  /**
   * @param {string|WebSocket} wsOrUrl 已有 WebSocket 实例（session 绑连接，必须复用）或 URL
   * @param {string} [sessionId] flatten session（browser 级连接时使用）
   */
  constructor(wsOrUrl, sessionId) {
    this.sessionId = sessionId || null;
    if (typeof wsOrUrl === 'string') {
      this.ws = new WebSocket(wsOrUrl, { perMessageDeflate: false });
    } else {
      this.ws = wsOrUrl;
    }
    if (!_counters.has(this.ws)) _counters.set(this.ws, 0);
    this.pending = new Map();
    this._onMsg = (ev) => {
      const m = JSON.parse(ev.data);
      if (m.id && this.pending.has(m.id)) { this.pending.get(m.id)(m); this.pending.delete(m.id); }
    };
    this.ws.addEventListener('message', this._onMsg);
    this.ready = this.ws.readyState === 1 ? Promise.resolve() : new Promise((res) => this.ws.addEventListener('open', res));
  }
  send(method, params = {}, timeoutMs = 15000) {
    const counter = _counters.get(this.ws);
    const mid = counter + 1;
    _counters.set(this.ws, mid);
    const msg = { id: mid, method, params };
    if (this.sessionId) msg.sessionId = this.sessionId;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { this.pending.delete(mid); reject(new Error(`CDP timeout: ${method}`)); }, timeoutMs);
      this.pending.set(mid, (m) => { clearTimeout(timer); resolve(m); });
      this.ws.send(JSON.stringify(msg));
    });
  }
  // 人类节奏延迟 [min,max] ms
  static sleep(min, max) {
    const ms = min + Math.random() * (max - min);
    return new Promise((r) => setTimeout(r, ms));
  }
  async eval(expression, awaitPromise = false) {
    const r = await this.send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise });
    if (r.result.exceptionDetails) throw new Error('eval error: ' + JSON.stringify(r.result.exceptionDetails));
    return r.result.result.value;
  }
  // 拟人鼠标：贝塞尔曲线轨迹 + 微抖动 + 随机过冲
  async humanMove(x1, y1, x2, y2, steps = 25) {
    const cx = (x1 + x2) / 2 + (Math.random() - 0.5) * 120;
    const cy = (y1 + y2) / 2 + (Math.random() - 0.5) * 120;
    for (let i = 1; i <= steps; i++) {
      const t = i / steps;
      const ease = t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2; // easeInOutQuad
      const x = (1 - ease) * (1 - ease) * x1 + 2 * (1 - ease) * ease * cx + ease * ease * x2 + (Math.random() - 0.5) * 1.5;
      const y = (1 - ease) * (1 - ease) * y1 + 2 * (1 - ease) * ease * cy + ease * ease * y2 + (Math.random() - 0.5) * 1.5;
      await this.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y });
      await CDP.sleep(8, 28);
    }
  }
  async humanClick(x, y) {
    await this.humanMove(200 + Math.random() * 400, 300 + Math.random() * 300, x, y);
    await CDP.sleep(60, 180);
    await this.send('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button: 'left', clickCount: 1 });
    await CDP.sleep(45, 120);
    await this.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button: 'left', clickCount: 1 });
  }
  // 拟人输入：先清空（React 兼容的原生 setter）再逐字符输入 + 随机停顿
  async humanType(selector, text) {
    await this.eval(`(() => {
      const el = document.querySelector(${JSON.stringify(selector)});
      el.focus();
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      setter.call(el, '');
      el.dispatchEvent(new Event('input', { bubbles: true }));
    })()`);
    await CDP.sleep(150, 300);
    for (const ch of text) {
      this._fire('Input.dispatchKeyEvent', { type: 'keyDown', text: ch });
      this._fire('Input.dispatchKeyEvent', { type: 'keyUp' });
      await CDP.sleep(45, ch === '@' || ch === '.' ? 180 : 110);
      if (Math.random() < 0.06) await CDP.sleep(400, 1200); // 思考停顿
    }
    await CDP.sleep(200, 400); // 等页面 React 状态更新
  }
  // 发射后不等回包
  _fire(method, params = {}) {
    const counter = _counters.get(this.ws);
    const mid = counter + 1;
    _counters.set(this.ws, mid);
    const msg = { id: mid, method, params };
    if (this.sessionId) msg.sessionId = this.sessionId;
    this.ws.send(JSON.stringify(msg));
  }
  // 元素坐标（viewport 相对）
  async box(selector) {
    return await this.eval(`(() => { const r = document.querySelector(${JSON.stringify(selector)}).getBoundingClientRect();
      return JSON.stringify({x: r.x + r.width/2, y: r.y + r.height/2, w: r.width, h: r.height}); })()`).then(JSON.parse);
  }
  async close() { this.ws.close(); }
}

module.exports = { CDP };
