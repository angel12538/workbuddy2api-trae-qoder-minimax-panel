/* 智能体配置页（层 3）：点「一键配置」必须真的发出请求。
 *
 * 事故背景（v1.4.0 → v1.5.4）：/settings 只返回掩码（api_keys[].masked），
 * 从不返回明文 key；页面的 Key 下拉却用 esc(k.key) 做 option value ——
 * 该字段不存在 → 每个 option 的 value 都是空串 → 用户选完 Key 点按钮，
 * 只命中 `if(!apiKey)` 的警告 toast，POST /agents/apply 根本没发生
 * （网关侧日志能证实：只有 GET /agents，没有任何 apply）。
 *
 * 修法契约（本套件守护它）：
 *   1. option value = key **id**（明文不随页面数据下发）；
 *   2. apply 前按 id 调一次 /settings/reveal 取明文；
 *   3. POST /agents/apply 的 body.api_key 是 reveal 回来的真实 Key；
 *   4. reveal 被 403（默认面板密码）时给出可读原因且**不发** apply。
 *
 *   node tests/_test_agents_panel.js
 */
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const ROOT = process.env.QD_DASH_ROOT || path.join(__dirname, '..');
const html = fs.readFileSync(path.join(ROOT, 'dashboard.html'), 'utf8');
const SUITE_MD5 = crypto.createHash('md5').update(fs.readFileSync(__filename)).digest('hex');

let PASS = 0, FAIL = 0;
const logs = [];
const origLog = console.log;
console.log = (...args) => { logs.push(args.map(String).join(' ')); };
function say(s){ origLog(s); }
function check(name, cond, extra){
  if(cond){ PASS++; say('  ok   ' + name); }
  else { FAIL++; say('  FAIL ' + name + (extra ? '  ' + extra : '')); }
}

// ---------- 最小 DOM 桩（与 _test_dashboard_full_load.js 同款语义） ----------
function El(tag){
  const el = {
    tagName: String(tag || 'div').toUpperCase(),
    id: '', className: '', _text: '', _html: '', children: [], parentNode: null,
    dataset: {}, style: {}, attrs: {}, listeners: {},
    get classList(){
      const self = el;
      const set = () => new Set(String(self.className).split(/\s+/).filter(Boolean));
      return {
        add(...cs){ const s = set(); cs.forEach(c => s.add(c)); self.className = [...s].join(' '); },
        remove(...cs){ const s = set(); cs.forEach(c => s.delete(c)); self.className = [...s].join(' '); },
        contains(c){ return set().has(c); },
        toggle(c, on){
          const s = set();
          if(on === undefined) on = !s.has(c);
          if(on) s.add(c); else s.delete(c);
          self.className = [...s].join(' ');
          return on;
        },
      };
    },
    appendChild(c){ c.parentNode = el; el.children.push(c); return c; },
    insertBefore(c, ref){ c.parentNode = el; const i = ref ? el.children.indexOf(ref) : -1;
      if(i < 0) el.children.push(c); else el.children.splice(i, 0, c); return c; },
    remove(){ const p = el.parentNode; if(p){ const i = p.children.indexOf(el); if(i >= 0) p.children.splice(i, 1); } },
    querySelector(sel){ return el.querySelectorAll(sel)[0] || null; },
    querySelectorAll(sel){
      const out = [];
      const walk = node => {
        for(const c of node.children){
          if(matchSel(c, sel)) out.push(c);
          walk(c);
        }
      };
      walk(el);
      return out;
    },
    addEventListener(t, fn){ (el.listeners[t] = el.listeners[t] || []).push(fn); },
    setAttribute(k, v){ el.attrs[k] = String(v); if(k === 'id') el.id = String(v); },
    getAttribute(k){ return el.attrs[k] != null ? el.attrs[k] : ''; },
    getBoundingClientRect(){ return { top: 0, left: 0 }; },
    cloneNode(deep){
      const c = El(el.tagName);
      c.className = el.className; c.id = el.id; c._text = el._text; c._html = el._html;
      Object.assign(c.attrs, el.attrs); Object.assign(c.dataset, el.dataset);
      if(deep) el.children.forEach(ch => c.appendChild(ch.cloneNode(true)));
      return c;
    },
    forEach(){}, closest(){ return null; },
  };
  Object.defineProperty(el, 'textContent', { get(){ return el._text; }, set(v){ el._text = String(v == null ? '' : v); } });
  Object.defineProperty(el, 'innerHTML', { get(){ return el._html; }, set(v){ el._html = String(v); } });
  Object.defineProperty(el, 'firstChild', { get(){ return el.children[0] || null; } });
  Object.defineProperty(el, 'offsetHeight', { get(){ return 64; } });
  return el;
}
function matchSel(node, sel){
  if(!sel) return false;
  if(sel[0] === '#') return node.id === sel.slice(1);
  if(sel[0] === '.') return String(node.className).split(/\s+/).includes(sel.slice(1));
  return node.tagName === sel.toUpperCase();
}

// ---------- 真实数据形状：/settings 只有掩码，没有明文 key ----------
const PLAIN_KEY = 'aa20e14bddf750782ae2d881a7cb3ecdcc7f';
const ORIGIN = 'http://127.0.0.1:8790';
const SETTINGS = { api_keys: [
  { id: 'k0', name: 'QODER', realm: '', enabled: false, masked: 'fa54******f0c7' },
  { id: 'k1', name: 'DSH', realm: '', enabled: true, masked: 'aa20******cc7f' },
] };
const AGENTS = { clients: [
  { id: 'claude-code', label: 'Claude Code', desc: 'Anthropic', protocol: 'anthropic',
    installed: true, configured: false, applied_at: null },
  { id: 'dsh', label: 'DSH (DeepSeek Harness)', desc: 'llm-pi-ai', protocol: 'openai',
    installed: true, configured: false, applied_at: null },
  { id: 'opencode', label: 'OpenCode', desc: 'openai-compatible', protocol: 'openai',
    installed: false, configured: false, applied_at: null },
] };
const MODELS = { models: [{ id: 'Qwen3.8-Max' }, { id: 'Qwen3.8-Flash' }] };

// ---------- fetch 桩：记录每次请求；可按场景返回 403 ----------
let scenario = 'ok';
const calls = [];
function resp(status, body){
  return {
    status, ok: status >= 200 && status < 300,
    text: async () => JSON.stringify(body),
    json: async () => body,
  };
}
async function fakeFetch(url, opts){
  const u = String(url);
  calls.push({ url: u, opts: opts || {} });
  if(u === '/agents') return resp(200, AGENTS);
  if(u === '/settings') return resp(200, SETTINGS);
  if(u === '/agents/models') return resp(200, MODELS);
  if(u.startsWith('/settings/reveal')){
    if(scenario === 'reveal403'){
      return resp(403, { error: { message: '面板仍在使用默认密码，请先修改密码' } });
    }
    return resp(200, { id: 'k1', key: PLAIN_KEY });
  }
  if(u === '/agents/apply'){
    return resp(200, { label: 'DSH (DeepSeek Harness)',
                       files: [{ path: 'C:/users/x/.dsh/settings.yaml' }] });
  }
  return resp(200, {});
}

// ---------- 建 DOM 树并 eval 整个主 script ----------
const body = El('body');
const docEl = El('html');
const byId = {};
['agentBaseURL', 'agentApiKeySelect', 'agentModelSelect', 'agentDetectState',
 'agentConnState', 'agentClientList', 'agentKeySel', 'agentModelSel'].forEach(id => {
  const el = El('div'); el.id = id; byId[id] = el; body.appendChild(el);
});
const win = {
  addEventListener(){},
  location: { search: '', href: ORIGIN + '/', hash: '', pathname: '/', origin: ORIGIN },
  matchMedia: q => ({ matches: false, addEventListener(){} }),
  localStorage: (() => { const s = {}; return {
    getItem: k => (k in s ? s[k] : null), setItem: (k, v) => { s[k] = String(v); },
    removeItem: k => { delete s[k]; } }; })(),
  sessionStorage: { getItem: () => null, setItem(){}, removeItem(){} },
  pageYOffset: 0, innerHeight: 800, scrollTo(){},
  history: { replaceState(){} }, URL: URL,
  __MAIN_TAB__: 'agents',
};
const doc = {
  getElementById: id => byId[id] || null,
  querySelector: sel => (sel[0] === '.' ? body.querySelectorAll(sel)[0] || null : null),
  querySelectorAll: sel => body.querySelectorAll(sel),
  createElement: t => El(t),
  addEventListener(){},
  body, documentElement: docEl, readyState: 'complete',
};

const blocks = [...html.matchAll(/<script[^>]*>([\s\S]*?)<\/script>/g)].map(x => x[1]);
const mainJs = blocks.length ? blocks[blocks.length - 1] : '';
check('主 script 块可抽取且非空', !!mainJs && mainJs.length > 100000);

let evalErr = null;
let api = null;
global.document = doc; global.window = win; global.localStorage = win.localStorage;
global.sessionStorage = win.sessionStorage; global.navigator = { userAgent: 'node' };
global.fetch = fakeFetch;
global.getComputedStyle = () => ({ display: 'block' });
global.MutationObserver = class { constructor(cb){ this.cb = cb; } observe(){} };
global.history = win.history; global.location = win.location;
try {
  api = new Function('document', 'window', 'localStorage', 'sessionStorage', 'navigator',
    'fetch', 'getComputedStyle', 'MutationObserver', 'history', 'location',
    mainJs + ';\nreturn {loadAgentsPage:loadAgentsPage, applyAgentClient:applyAgentClient,'
    + ' onAgentKeyChange:onAgentKeyChange};')(
    doc, win, win.localStorage, win.sessionStorage, global.navigator, fakeFetch,
    global.getComputedStyle, global.MutationObserver, win.history, win.location);
} catch(e){ evalErr = e; }
check('整个主 script 块在 DOM 桩上 eval 不抛异常', evalErr === null,
  evalErr ? String(evalErr && evalErr.stack || evalErr).slice(0, 300) : '');

// ---------- 源级契约：不再读不存在的 k.key ----------
check('源码级：智能体页不再引用 esc(k.key)（该字段从不存在）',
  !/esc\(k\.key\)/.test(html));
const _applyFnStart = mainJs.indexOf('async function applyAgentClient');
const _applyFn = _applyFnStart >= 0
  ? mainJs.slice(_applyFnStart, mainJs.indexOf('async function restoreAgentClient', _applyFnStart))
  : '';
check('源码级：apply 前显式走 /settings/reveal',
  _applyFnStart >= 0 && /settings\/reveal\?id=/.test(_applyFn));

(async () => {
  if(api && evalErr === null){
    // 模拟真实浏览器：用户按**文案**选中下拉项，value 取自选项 HTML 的 value 属性
    // （旧 bug 下所有 option 的 value 都是空串，选完仍是未选择——本套件的关键还原点）。
    function selectAgentKeyByLabel(label){
      const opt = [...byId['agentApiKeySelect'].innerHTML
        .matchAll(/<option value="([^"]*)">([\s\S]*?)<\/option>/g)]
        .find(m => m[2].includes(label));
      byId['agentApiKeySelect'].value = opt ? opt[1] : '';
      return byId['agentApiKeySelect'].value;
    }
    await api.loadAgentsPage(false);
    const keySel = byId['agentApiKeySelect'];
    const keyHtml = keySel.innerHTML;
    check('Key 下拉 option 的 value 是 key id（不是空串 / "undefined"）',
      /value="k1"/.test(keyHtml) && !/value="undefined"/.test(keyHtml) && !/value=""/.test(keyHtml.replace(/<option value="">选择 API Key…<\/option>/, '')),
      keyHtml.slice(0, 200));
    check('Key 下拉文案带掩码（用户能确认选的是哪一把）',
      /aa20\*+cc7f/.test(keyHtml));
    check('只有一个启用的 Key 进入下拉（禁用 k0 不出现）', !/k0/.test(keyHtml));
    check('已安装的客户端出现「一键配置」按钮', /一键配置/.test(byId['agentClientList'].innerHTML));
    check('未安装的客户端不出按钮' + '（OpenCode 无权操作）',
      !/applyAgentClient\('opencode'/.test(byId['agentClientList'].innerHTML));

    // —— 场景 1：选 Key → 一键配置（正常路径）——
    const picked = selectAgentKeyByLabel('DSH');
    check('按文案选中 DSH 后下拉 value = k1（真实浏览器里的选择语义）', picked === 'k1',
      JSON.stringify(picked));
    byId['agentModelSelect'].value = '';
    scenario = 'ok';
    calls.length = 0; logs.length = 0;
    await api.applyAgentClient('dsh', null);
    const revealCall = calls.find(c => c.url === '/settings/reveal?id=k1');
    check('apply 前按 id 取明文：GET /settings/reveal?id=k1', !!revealCall);
    const applyCall = calls.find(c => c.url === '/agents/apply');
    check('点击「一键配置」真的发出 POST /agents/apply（旧 bug 卡在这一步之前）', !!applyCall);
    const sent = applyCall ? JSON.parse(applyCall.opts.body) : {};
    check('POST body.api_key 是 reveal 回来的真实 Key', sent.api_key === PLAIN_KEY);
    check('POST body 不再出现空串 / "undefined"',
      sent.api_key && sent.api_key !== 'undefined');
    check('POST body 的 client / base_url / model 正确',
      sent.client === 'dsh' && sent.base_url === ORIGIN && sent.model === '',
      JSON.stringify({ client: sent.client, base_url: sent.base_url, model: sent.model }));
    check('没有误弹「请先选择 API Key」', !logs.some(m => m.includes('请先在上方选择一个 API Key')));
    check('成功 toast 带客户端名', logs.some(m => m.includes('已配置')));

    // —— 场景 2：reveal 被 403（默认面板密码）→ 可读错误、不发 apply ——
    selectAgentKeyByLabel('DSH');
    scenario = 'reveal403';
    calls.length = 0; logs.length = 0;
    await api.applyAgentClient('dsh', null);
    check('reveal 403 时不发 /agents/apply', !calls.some(c => c.url === '/agents/apply'));
    check('reveal 403 时 toast 透出服务端可读原因',
      logs.some(m => m.includes('配置失败') && m.includes('默认密码')));

    // —— 场景 3：没选 Key → 只警告、不发任何请求 ——
    keySel.value = '';
    scenario = 'ok';
    calls.length = 0; logs.length = 0;
    await api.applyAgentClient('dsh', null);
    check('未选 Key 时只弹警告、不发请求',
      calls.length === 0 && logs.some(m => m.includes('请先在上方选择一个 API Key')));
  }

  say('');
  say('SUITE: ' + path.basename(__filename) + ' md5=' + SUITE_MD5);
  say('FILE: dashboard.html md5=' + crypto.createHash('md5').update(html).digest('hex')
    + ' bytes=' + Buffer.byteLength(html, 'utf8') + ' as-of=' + new Date().toISOString());
  say('');
  say('SUMMARY: TOTAL ' + (PASS + FAIL) + ' checks, ' + PASS + ' passed, ' + FAIL + ' failed');
  say('RESULT: ' + (FAIL ? 'RED' : 'GREEN') + ' (exit ' + (FAIL ? 1 : 0) + ')');
  process.exit(FAIL ? 1 : 0);
})().catch(err => {
  origLog('  FAIL 套件自身异常: ' + (err && err.stack || err));
  origLog('SUMMARY: TOTAL ' + (PASS + FAIL + 1) + ' checks, ' + PASS + ' passed, ' + (FAIL + 1) + ' failed');
  origLog('RESULT: RED (exit 1)');
  process.exit(1);
});
