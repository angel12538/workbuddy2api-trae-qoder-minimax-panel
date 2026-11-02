/**
 * fill-via-antd.js — 通过 antd Form 实例直接赋值（绕过 DOM/事件层）
 * 思路：从 React fiber 树找到 Form 实例，调用 setFieldsValue，再触发校验/提交。
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');

const BROWSER_WS = 'ws://localhost:9366/devtools/browser/9bb82102-33e6-4191-bde4-f849d63b27aa';
const IDENT = {
  firstName: 'Wei',
  lastName: 'Chen',
  email: 'target@example.com',
  password: 'CHANGE_ME_PASSWORD',
};
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

  await c.send('Page.navigate', { url: 'https://qoder.com/users/sign-up' });
  for (let i = 0; i < 25; i++) {
    await sleep(600, 900);
    if (await c.eval('!!document.querySelector("#basic_email")')) break;
  }
  log('页面就绪');

  // 在页面内探测 antd Form 实例
  const found = await c.eval(`(() => {
    const input = document.querySelector('#basic_email');
    if (!input) return 'no input';
    // React 17: __reactEventHandlers$xxx / __reactFiber$xxx
    const fiberKey = Object.keys(input).find(k => k.startsWith('__reactFiber$'));
    const propsKey = Object.keys(input).find(k => k.startsWith('__reactProps$') || k.startsWith('__reactEventHandlers$'));
    return JSON.stringify({ fiberKey: !!fiberKey, propsKey: !!propsKey, keys: Object.keys(input).filter(k=>k.startsWith('__react')).slice(0,4) });
  })()`);
  log('React 内部键:', found);

  // 从 fiber 向上找 Form 实例（含 setFieldsValue 的对象）
  const setRes = await c.eval(`(() => {
    const input = document.querySelector('#basic_email');
    const fiberKey = Object.keys(input).find(k => k.startsWith('__reactFiber$'));
    if (!fiberKey) return 'no fiber';
    let f = input[fiberKey];
    let depth = 0;
    const seen = [];
    while (f && depth < 40) {
      // 检查 memoizedProps / stateNode 上是否有 setFieldsValue
      const cands = [f.memoizedProps, f.stateNode, f.stateNode && f.stateNode.props];
      for (const cd of cands) {
        if (cd && typeof cd.setFieldsValue === 'function') {
          cd.setFieldsValue({ firstName: 'Wei', lastName: 'Chen', email: 'target@example.com' });
          return 'setFieldsValue OK at depth ' + depth;
        }
        if (cd && cd.form && typeof cd.form.setFieldsValue === 'function') {
          cd.form.setFieldsValue({ firstName: 'Wei', lastName: 'Chen', email: 'target@example.com' });
          return 'form.setFieldsValue OK at depth ' + depth;
        }
      }
      // 记录类型名便于诊断
      const tn = f.type && (typeof f.type === 'function' ? (f.type.displayName || f.type.name) : String(f.type));
      if (tn) seen.push(tn);
      f = f.return; depth++;
    }
    return 'not found; chain=' + seen.slice(0,20).join('>');
  })()`);
  log('Form 注入结果:', setRes);

  await sleep(1500, 2500);
  const vals = await c.eval(`JSON.stringify({
    fn: document.querySelector('#basic_firstName')?.value,
    ln: document.querySelector('#basic_lastName')?.value,
    em: document.querySelector('#basic_email')?.value
  })`);
  log('字段值:', vals);
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
