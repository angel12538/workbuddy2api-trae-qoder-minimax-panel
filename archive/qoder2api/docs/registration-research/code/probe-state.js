/**
 * probe-state.js — 探查 React 组件状态（诊断为何不进入密码步）
 */
const { CDP } = require('./cdp');
const WebSocket = require('ws');
const fs = require('fs');

(async () => {
  const bws = new WebSocket('ws://localhost:9366/devtools/browser/9bb82102-33e6-4191-bde4-f849d63b27aa', { perMessageDeflate: false });
  await new Promise(r => bws.addEventListener('open', r));
  const b = new CDP(bws);
  const ts = await b.send('Target.getTargets', {});
  const s = JSON.parse(fs.readFileSync(__dirname + '/.incog.json', 'utf8'));
  const t = (ts.result.targetInfos || []).find(x => x.browserContextId === s.ctxId && x.type === 'page');
  const att = await b.send('Target.attachToTarget', { targetId: t.targetId, flatten: true });
  const c = new CDP(bws, att.result.sessionId);

  const script = [
    'const el = document.querySelector("#basic_email");',
    'const fk = Object.keys(el).find(k => k.startsWith("__reactFiber$"));',
    'let f = el[fk], out = [], d = 0;',
    'while (f && d < 70) {',
    '  const tn = f.type && (typeof f.type === "function" ? (f.type.displayName || f.type.name) : String(f.type));',
    '  if (f.memoizedState && tn) {',
    '    let st = f.memoizedState, vals = [], si = 0;',
    '    while (st && si < 14) {',
    '      const v = st.memoizedState;',
    '      if (typeof v === "string" || typeof v === "boolean" || typeof v === "number") vals.push(v);',
    '      st = st.next; si++;',
    '    }',
    '    if (vals.length) out.push(tn + ": " + JSON.stringify(vals).slice(0, 160));',
    '  }',
    '  f = f.return; d++;',
    '}',
    'return out.join("\\n");',
  ].join('\n');

  const r = await c.eval(`(() => { ${script} })()`);
  console.log(r);
  process.exit(0);
})().catch(e => { console.error('ERR', e.message); process.exit(1); });
