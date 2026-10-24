'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'wb2api.key', LS_THEME = 'wb2api.theme';
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let view = 'checkin';   // 账号池视图已移除（2026-10-24）：签到池是默认首页
let overviewData = null, cfgLoaded = null;
let logPin = true, loginState = null, loginTimer = null;
let traeLoginState = null, traeTimer = null;   // Trae 授权登录（与 WorkBuddy 那套分开）
let refTimer = null;
/* 视图级筛选状态（模块级声明放在文件顶部，避免顶层 go() 早于声明执行时踩 TDZ）。 */
let mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
let mdAll = [], mdProbes = {}, mdProbeOf = () => undefined;
let mdBusyByKey = {}, mdBusyMedianByRealm = {};
let reqFilter = { q: '', outcome: '' };
let reqEntries = [];
let usDim = 'account', usCreditDim = 'account', usSort = 'total';
let usageData = null;
let reqRangeState = null; // 请求记录的时间范围（用量页的见 trangeState）

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 两态翻转（浅/深），首次访问跟随系统偏好；点击总是切换可见外观，符合直觉。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = eff === 'light' ? '切换到深色' : '切换到浅色';
}
addEventListener('change', applyTheme);
$('btnTheme').onclick = () => {
  theme = effTheme() === 'light' ? 'dark' : 'light';
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
};
applyTheme();

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  // fetch 的请求头只允许 ISO-8859-1；密钥含中文等字符时跳过，避免整个请求抛异常。
  if (k && /^[\x00-\xff]*$/.test(k)) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}
function parseAPITime(value) {
  const text = String(value || '');
  if (!text || text.startsWith('0001-')) return 0;
  const ms = Date.parse(text);
  return Number.isFinite(ms) ? ms : 0;
}
function fmtLocalDateTime(ms) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}

/* ── 时间范围控件（用量 / 请求记录共用）────────────────────────────────
   预设项：今天 / 近 24 小时 / 近 3 天 / 近 7 天 / 近 30 天 / 全部历史 / 自定义。

   为什么区间一律由前端算好再发：
     - 「今天」必须是**浏览器本地时区**的 00:00 起。服务端时区未必与浏览器一致
       （容器常挂 TZ=Asia/Shanghai，而浏览器可能在任何时区），让服务端算"今天"
       会在跨时区时切错日子。
     - 「自定义」本来就是用户挑的具体时刻，没有任何服务端推导空间。

   滚动预设（近 N 小时/天）则保留 hours 参数：服务端按整点对齐的滚动窗口与旧
   行为逐位一致，前端自己减 N 小时会多算/少算一个边界桶。 */
const TRANGE_PRESETS = [
  ['today', '今天'],
  ['24', '近 24 小时'],
  ['72', '近 3 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];
const TRANGE_DEFAULT = '72';
const trangeStates = new Map(); // hostId → { preset, from: Date|null, to: Date|null }

// dtLocalValue / dtLocalParse 与 <input type=datetime-local> 的取值格式互转
// （YYYY-MM-DDTHH:mm，本地时区；ES 里"带时间的日期串"按本地解析，正是我们要的）。
function dtLocalValue(d) {
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}
function dtLocalParse(s) {
  if (!s) return null;
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}

// trangeMidnight 今天 00:00（本地时区）。
function trangeMidnight() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

function trangeState(id) {
  if (!trangeStates.has(id)) {
    // 「自定义」的初始值给一段有意义的默认：今天 00:00 → 现在。
    trangeStates.set(id, { preset: TRANGE_DEFAULT, from: trangeMidnight(), to: new Date() });
  }
  return trangeStates.get(id);
}

// trangeRender 画出控件骨架（幂等：重复调用会保留当前状态）。
function trangeRender(id) {
  const host = $(id);
  if (!host) return;
  const st = trangeState(id);
  const custom = st.preset === 'custom';
  host.innerHTML =
    '<select class="tr-preset" aria-label="时间范围">' +
    TRANGE_PRESETS.map(([v, label]) =>
      '<option value="' + v + '"' + (v === st.preset ? ' selected' : '') + '>' + esc(label) + '</option>').join('') +
    '</select>' +
    '<span class="tr-custom"' + (custom ? '' : ' hidden') + '>' +
    '<input type="datetime-local" class="tr-from" value="' + esc(st.from ? dtLocalValue(st.from) : '') + '" aria-label="起始时间">' +
    '<span class="tr-sep">→</span>' +
    '<input type="datetime-local" class="tr-to" value="' + esc(st.to ? dtLocalValue(st.to) : '') + '" aria-label="结束时间">' +
    '</span>';
  const preset = host.querySelector('.tr-preset');
  if (preset) preset.onchange = () => {
    st.preset = preset.value;
    // 从别的预设切到自定义时，把区间重置为"今天 00:00 → 现在"，
    // 免得用户上次留下的半年区间被无声沿用。
    if (st.preset === 'custom' && (!st.from || !st.to)) { st.from = trangeMidnight(); st.to = new Date(); }
    trangeRender(id);
    trangeEmit(id);
  };
  const fromEl = host.querySelector('.tr-from');
  const toEl = host.querySelector('.tr-to');
  const readCustom = () => {
    st.from = dtLocalParse(fromEl.value);
    st.to = dtLocalParse(toEl.value);
    // 起止颠倒就地标红（不静默纠正：用户可能正输到一半）。
    const bad = st.from && st.to && st.from > st.to;
    fromEl.classList.toggle('tr-bad', !!bad);
    toEl.classList.toggle('tr-bad', !!bad);
    if (bad) return;
    trangeEmit(id);
  };
  if (fromEl) fromEl.onchange = readCustom;
  if (toEl) toEl.onchange = readCustom;
}

const trangeHandlers = new Map();
// trangeBind 渲染控件并登记变化回调。**不**在绑定时触发回调：各视图的首次加载
// 由 go() 统一驱动，这里再触发一次会让打开页面时打两遍接口。
function trangeBind(id, onChange, preset) {
  trangeHandlers.set(id, onChange);
  if (preset) trangeState(id).preset = preset;
  trangeRender(id);
}
function trangeEmit(id) {
  const fn = trangeHandlers.get(id);
  if (fn) fn();
}

// trangeQuery 把当前选择翻译成查询参数。
//   rolling=true  → 滚动预设发 hours（服务端整点对齐），今天/自定义发 from/to
//   rolling=false → 一律发 from/to（归档是线性日志，前端算区间更直观）
// 「全部历史」两者都不发。
function trangeQuery(id, rolling) {
  const st = trangeState(id);
  const q = new URLSearchParams();
  const sec = d => Math.floor(d.getTime() / 1000);
  if (st.preset === 'custom') {
    if (st.from) q.set('from', sec(st.from));
    if (st.to) q.set('to', sec(st.to));
    return q;
  }
  if (st.preset === 'today') {
    q.set('from', sec(trangeMidnight()));
    return q;
  }
  // 全部历史：滚动端点（用量）必须**显式**传 hours=0。
  //
  // 后端对「什么都不给」的缺省是 72 小时（见 panel.go 的说明：
  // 「都不给：等同于 hours=72（保持旧调用方行为）」），所以这里返回空 query 会被
  // 当成「近 3 天」—— 正是 issue #121 报的现象：选了「全部历史」，数字却和
  // 「近 3 天」一模一样。
  //
  // 非滚动端点（请求记录）没有缺省窗口：不传 from/to 即"不限起点"，保持空 query。
  if (st.preset === '0') {
    if (rolling) q.set('hours', '0');
    return q;
  }
  if (rolling) { q.set('hours', st.preset); return q; }
  q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
  return q;
}

// trangeLabel 人读口径，用于「用量总览」右上角这类需要回显区间的位置。
function trangeLabel(id) {
  const st = trangeState(id);
  const found = TRANGE_PRESETS.find(p => p[0] === st.preset);
  if (st.preset !== 'custom') return found ? found[1] : '';
  if (!st.from && !st.to) return '自定义';
  const f = d => d ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0') : '…';
  return f(st.from) + ' → ' + f(st.to);
}
function rateLimitMeta(row, now) {
  const model = String(row && row.model || '未知模型');
  const kind = String(row && row.kind || 'rate_limit');
  const resetAt = parseAPITime(row && row.reset_at);
  const until = parseAPITime(row && row.until);
  const deadline = resetAt || until;
  const remaining = deadline > now ? Math.round((deadline - now) / 1000) : 0;
  if (kind === 'model_unavailable') {
    return {
      model,
      kind,
      detail: remaining ? '预计 ' + dur(remaining) + ' 后重试' : '等待重新探测',
      title: model + '\n模型当前不可用' + (deadline ? '\n最早重试：' + fmtLocalDateTime(deadline) : ''),
    };
  }
  let detail = resetAt
    ? '预计 ' + fmtLocalDateTime(resetAt) + ' 解封' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '')
    : (until ? '预计 ' + fmtLocalDateTime(until) + ' 恢复（剩余 ' + dur(remaining) + '）' : '预计解封时间未知');
  const title = [model, resetAt ? '上游重置：' + fmtLocalDateTime(resetAt) : '上游重置：时间未知'];
  if (until && resetAt && until < resetAt) {
    detail += ' · 网关最快 ' + dur(Math.max(0, Math.round((until - now) / 1000))) + ' 后重试';
    title.push('网关最早重试：' + fmtLocalDateTime(until));
  }
  return { model, kind, detail, title: title.join('\n') };
}
function rateLimitRowsHtml(rows, now) {
  const list = Array.isArray(rows) ? rows.filter(row => row && row.model) : [];
  if (!list.length) return '';
  return '<div class="rate-limits">' + list.map(row => {
    const m = rateLimitMeta(row, now);
    return '<div class="rate-limit ' + (m.kind === 'model_unavailable' ? 'model-unavailable' : '') +
      '" title="' + esc(m.title) + '"><b>' + esc(m.model) + '</b><span>' + esc(m.detail) + '</span></div>';
  }).join('') + '</div>';
}

function formatTokenCount(tokens) {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
  let unit = units[0];
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let value = n / unit[1];
  let rounded = Number(value.toFixed(1));
  // 999999 → 1m，而不是 1000k；四舍五入后自动升级单位。
  const next = units[units.indexOf(unit) + 1];
  if (next && rounded >= 1000) {
    unit = next;
    value = n / unit[1];
    rounded = Number(value.toFixed(1));
  }
  return rounded + unit[0];
}

function formatLatency(ms) {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { $('keyVeil').classList.add('on'); setTimeout(() => $('keyInput').focus(), 60); }
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview');
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();
  } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });

/* ── 路由 ─────────────────────────────────────────────────────────── */
/* 积分包明细共享缓存：「积分构成」视图与首页「积分到期提醒」卡片共用同一份
   /panel/api/packages 数据（逐账号实时查上游，能省一次是一次）。声明在路由区
   是因为 go() 的初始调用就会触发首页卡片的渲染，必须先于它就位。 */
let lastPackages = null;
let lastPackagesAt = 0;
let expFetching = false;                       // 到期卡片在途标记（防重复打上游）
const EXP_FRESH_MS = 2 * 60 * 1000;            // 缓存新鲜窗口：2 分钟内复用

const TITLES = { checkin: '签到池', usage: '用量', packages: '积分构成', taskscenter: '任务中心', ideaccounts: 'IDE 账号池', models: '模型与档位', config: '配置', logs: '运行日志' };
function go(v) {
  if (!(v in TITLES)) v = 'checkin';
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  if (v === 'models') { if (!$('mdBody').children.length) loadModels(); loadModelQueue(); }
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'usage') { loadUsage(); loadUsageOfficial(); }
  if (v === 'packages') { loadPackages(); loadPkIdea(true); }
  if (v === 'taskscenter') { reattachQueueView(); loadIdeaTasks(true); }
  if (v === 'checkin') { loadTraeStatus(true); loadTraeCredits(true); loadCheckinPool(true); loadQkCredits(true); loadWkCredits(true); }
  // 冻结期内进 IDE 账号页不自动重取：保留切换前那份列表与滚动位置。
  if (v === 'ideaccounts' && !viewFrozen()) { loadQoderAccounts(true); loadTraeIdeAccounts(true); loadWorkBuddyAccounts(true); }
  saveViewState();
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
/* 首次进入延到本轮脚本求值之后再 go()。
   原因：go() 会同步触发视图的数据加载（loadUsage/loadLogs/loadPackages…），而这些
   函数读到的模块级 let/const（usageRateWarmAt、PK_* 等）在文件后半段才初始化——
   直接深链 #usage / #packages 打开页面时会踩 TDZ（"Cannot access 'x' before
   initialization"），表现为该页永远显示"读取失败"，而点导航进去一切正常。
   延迟 0ms 让整份脚本先求值完，是修这一类问题最省事也最不容易再犯的办法。 */
setTimeout(() => {
  // 优先还原上次停留的视图与滚动位置（切号后按 F5、或从别的页切回来都不回顶部）；
  // 没有历史记录时再按 URL hash 进入。
  if (!restoreViewState()) {
    const hash = (location.hash || '#checkin').slice(1);
    go(hash in TITLES ? hash : 'checkin');
  }
}, 0);

/* ── 账号池 ───────────────────────────────────────────────────────── */
function renderAccounts(list) {
  const tb = $('accBody');
  if (!tb) return;
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="9"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div></td></tr>';
    return;
  }
  // 有总额度（credits_total）→ 进度条按自身 剩余/总额 百分比；旧数据无总额 → 退回池内最高=100%
  const maxCred = Math.max(1, ...list.map(s => s.credits || 0));
  tb.innerHTML = list.map(s => {
    const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
    const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
    const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
    let cls = '', tag;
    if (s.disabled) { cls = 'off'; tag = '<span class="tag bad">已禁用</span>'; }
    else if (s.paused) { cls = 'off'; tag = '<span class="tag warn">已暂停选号</span>'; }
    else if (cool > 0) {
      cls = 'cool';
      const kind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? '熔断'
        : (dg > (s.cool_remaining_sec || 0) ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
      tag = '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>';
    } else tag = '<span class="tag ok">可用</span>' + (s.in_flight ? '' : '');
    const note = s.reason ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.reason) + '</div>' : '';
    const rateLimits = rateLimitRowsHtml(s.rate_limited_models, Date.now());
    const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
    // 企业版不限量：上游 limitNum == -1，网关以 credits_total=-1 透出（见 upstream
    // enterpriseUnlimitedTotal）。此时剩余额度不参与展示，直接标「不限」。
    const unlimited = s.credits_total === -1;
    const cred = unlimited ? '不限'
      : (s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits)));
    const pct = unlimited ? 100
      : (s.credits_total > 0
        ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
        : Math.round((s.credits || 0) / maxCred * 100));
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
    // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
    let credTip;
    if (unlimited) credTip = '企业版不限量（上游 limitNum=-1）';
    else if (s.credits_total > 0) {
      credTip = (s.enterprise ? '企业版剩余额度 ' : '剩余 ')
        + s.credits + ' / ' + (s.enterprise ? '分配 ' : '总额 ') + s.credits_total + '（' + pct + '%）';
    } else credTip = '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    const tu = s.token_usage || {};
    const req = tu.request_count || 0;
    const totalTok = formatTokenCount(tu.total_tokens);
    const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
    const latency = formatLatency(tu.last_latency_ms);
    const rate = formatRate(tu.last_tokens_per_second);
    const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
    return '<tr class="' + cls + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + (s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + (s.enterprise ? ' <span class="realm-tag">企业版</span>' : '') + '</div><div class="id">' + esc(short) + '</div></td>' +
      '<td>' + tag + note + rateLimits + '</td>' +
      '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
      '<td class="num">' + (s.in_flight || 0) + '</td>' +
      '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
        '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
        '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
        '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
        '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
      '</span></td>' +
      '<td class="num" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
      '<td class="acts">' +
        // 企业版无个人成长体系（签到 400「企业账号不支持该操作」/ 成长任务 403）：
        // 不渲染「签到」「任务」按钮，只留「额度」——点它走 /balance，企业额度由
        // upstream 的 get-enterprise-user-usage 口径填充。
        (s.enterprise ? '' :
          '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '"' + (s.checkin_done ? ' title="今日已签到；点击可重新签到并刷新余额"' : '') + '>' + (s.checkin_done ? '已签' : '签到') + '</button>') +
        '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '"' + (s.enterprise ? ' title="刷新企业版已分配额度（上游 get-enterprise-user-usage）"' : '') + '>' + (s.enterprise ? '额度' : '余额') + '</button>' +
        (s.enterprise ? '' :
          '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">任务</button>') +
        (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '">解冻</button>'
                : (s.paused ? '<button class="xs primary" data-a="resume" data-u="' + esc(s.uid) + '">恢复选号</button>'
                            : '<button class="xs ghost" data-a="pause" data-u="' + esc(s.uid) + '" title="' + (s.enterprise ? '退出选号，但照常保活 / 刷新额度' : '退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额') + '">暂停选号</button>')) +
        (s.disabled ? '' : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '">禁用</button>') +
        '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">移除</button>' +
      '</td></tr>';
  }).join('');
}

/* ── 模型排队与冷却 ────────────────────────────────────────────────
   三款 IDE 各自的模型排队/冷却面：
   - WorkBuddy（本代理上游）：池内逐（域, 模型）模型级冷却 + 可服务号数（选号实况）；
   - Trae：v1 额度接口的模型请求限额（快速/慢速请求 = 优先/排队）；
   - Qoder：无模型级接口，仅提示。 */
function fmtNum(n) { n = Number(n); return Number.isFinite(n) ? String(Math.round(n * 100) / 100) : '—'; }

function renderMqLocks(rows) {
  const tb = $('mqLockBody');
  if (!tb) return;
  const STATE = {
    locked: ['bad', '整池不可用'], starved: ['warn', '没号可用'], partial: ['warn', '部分限流'],
    observed: ['mute', '观察到限流（解锁未知）']
  };
  const left = iso => {
    const ms = parseAPITime(iso);
    return ms ? dur(Math.max(0, Math.round((ms - Date.now()) / 1000))) : '—';
  };
  if (!rows || !rows.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">当前没有模型级冷却 —— 所有模型均可选</div></td></tr>';
    return;
  }
  tb.innerHTML = rows.map(r => {
    const st = STATE[r.state] || ['mute', r.state || '—'];
    const realm = r.realm === 'global' ? '国际版' : '国内版';
    const observed = Number(r.observed || 0);
    const lockedText = (r.locked || 0) + (observed ? '（另观测 ' + observed + '）' : '');
    const firstUnlock = r.state === 'observed' ? '未知' : left(r.unlock_at || r.fully_unlock_at);
    const allUnlock = r.state === 'observed' ? '未知' : left(r.fully_unlock_at);
    return '<tr>' +
      '<td>' + esc(r.model) + '</td>' +
      '<td><span class="realm-tag">' + realm + '</span></td>' +
      '<td><span class="tag ' + st[0] + '">' + st[1] + '</span></td>' +
      '<td class="num">' + (r.servable || 0) + ' / ' + (r.total || 0) + '</td>' +
      '<td class="num">' + lockedText + '</td>' +
      '<td class="num">' + firstUnlock + '</td>' +
      '<td class="num">' + allUnlock + '</td>' +
      '<td>' + (r.reason ? '<div class="note">' + esc(r.reason) + '</div>' : '—') + '</td>' +
      '</tr>';
  }).join('');
}

function renderMqTrae(accts) {
  const tb = $('mqTraeBody');
  if (!tb) return;
  if (!accts || !accts.length) {
    tb.innerHTML = '<tr><td colspan="5"><div class="empty">未找到 Trae 账号</div></td></tr>';
    return;
  }
  let html = '';
  accts.forEach(a => {
    const name = a.name || a.user_id || '未命名';
    if (a.error) {
      html += '<tr><td>' + esc(name) + '</td><td colspan="4"><span class="tag ' +
        (a.expired ? 'warn' : 'bad') + '">' + esc(a.error) + '</span></td></tr>';
      return;
    }
    const limits = a.limits || [];
    // 只展示有意义的类目（启用/有额度/有用量）；全 0 的「未启用」行折叠成一句说明。
    const active = limits.filter(l => l.parallel || l.unlimited || Number(l.limit) > 0 || Number(l.used) > 0);
    const parallel = limits.find(l => l.parallel);
    if (!active.length) {
      const p = parallel && parallel.limit > 0 ? '（并行上限 ' + parallel.limit + '）' : '';
      html += '<tr><td>' + esc(name) + '</td><td colspan="4"><div class="note">' +
        '该账号为积分计费，未启用模型请求限额' + p + '</div></td></tr>';
      return;
    }
    active.forEach((l, i) => {
      const acctCell = i === 0 ? '<td rowspan="' + active.length + '">' + esc(name) + '</td>' : '';
      let quota, used, remain;
      if (l.parallel) {
        quota = l.limit > 0 ? String(l.limit) : '—'; used = '—'; remain = '—';
      } else if (l.unlimited) {
        quota = '不限'; used = fmtNum(l.used); remain = '∞';
      } else if (l.limit > 0) {
        quota = fmtNum(l.limit); used = fmtNum(l.used); remain = fmtNum(l.remaining);
      } else {
        quota = '未启用'; used = fmtNum(l.used); remain = '—';
      }
      html += '<tr>' + acctCell + '<td>' + esc(l.label) + '</td>' +
        '<td class="num">' + quota + '</td><td class="num">' + used + '</td><td class="num">' + remain + '</td></tr>';
    });
  });
  tb.innerHTML = html;
}

/* Trae 模型通道（trae: 前缀转发）的**本网关上**并发占用与冷却。
   与上面 mqTraeBody 的上游额度互补：这里显示的是此刻有几个请求正占着账号名额。 */
function renderMqTraeChat(chat) {
  const tb = $('mqTraeChatBody');
  const note = $('mqTraeChatNote');
  if (!tb) return;
  chat = chat || {};
  if (!chat.enabled) {
    tb.innerHTML = '<tr><td colspan="5"><div class="empty">未启用（config.json → trae.chat.enabled）</div></td></tr>';
    if (note) note.textContent = '关闭后 /v1/chat/completions 不接受 trae: 前缀模型。';
    return;
  }
  if (note) {
    const extra = [];
    if (chat.in_flight) extra.push('在途 ' + chat.in_flight);
    if (chat.cooling) extra.push('冷却账号 ' + chat.cooling);
    extra.push('已暴露模型 ' + (chat.model_count || 0) + ' 个');
    note.textContent = '调用方式：' + (chat.endpoint || '') + '　·　' + extra.join('　·　') +
      '　·　上游并行上限决定排队，满员时请求在此等待。';
  }
  const rows = chat.accounts || [];
  if (!rows.length) {
    tb.innerHTML = '<tr><td colspan="5"><div class="empty">无 Trae 账号（在「Trae 签到」里添加）</div></td></tr>';
    return;
  }
  tb.innerHTML = rows.map(a => {
    const cool = a.cooling ? '冷却中' : '可用';
    const cls = a.cooling ? 'no' : 'ok';
    return '<tr><td>' + esc(a.name || a.user_id) + '</td>' +
      '<td class="num">' + (a.in_flight || 0) + ' / ' + (a.max_in_flight || 0) + '</td>' +
      '<td class="' + cls + '">' + cool + '</td>' +
      '<td>' + (a.cooldown_until ? esc(String(a.cooldown_until).replace('T', ' ').slice(0, 19)) : '—') + '</td>' +
      '<td>' + (a.last_error ? esc(a.last_error) : '—') + '</td></tr>';
  }).join('');
}

async function loadModelQueue() {
  const lb = $('mqLockBody');
  if (!lb) return;
  lb.innerHTML = '<tr><td colspan="8"><div class="empty">正在查询…</div></td></tr>';
  if ($('mqTraeBody')) $('mqTraeBody').innerHTML = '<tr><td colspan="5"><div class="empty">正在查询…</div></td></tr>';
  if ($('mqTraeChatBody')) $('mqTraeChatBody').innerHTML = '<tr><td colspan="5"><div class="empty">正在查询…</div></td></tr>';
  let d;
  try {
    d = await api('model_queue');
  } catch (e) {
    lb.innerHTML = '<tr><td colspan="8"><div class="empty">' + esc(e.message) + '</div></td></tr>';
    if ($('mqTraeBody')) $('mqTraeBody').innerHTML = '<tr><td colspan="5"><div class="empty">—</div></td></tr>';
    if ($('mqTraeChatBody')) $('mqTraeChatBody').innerHTML = '<tr><td colspan="5"><div class="empty">—</div></td></tr>';
    return;
  }
  renderMqLocks(d.locks || []);
  renderMqTrae(d.trae || []);
  renderMqTraeChat(d.trae_chat || {});
  const q = d.qoder || {};
  if ($('mqQoder')) $('mqQoder').textContent = q.has_model_queue ? '' : ('Qoder：' + (q.note || '无模型级排队/冷却接口'));
  if ($('mqNote')) $('mqNote').textContent = 'WorkBuddy 池内实况 · Trae 请求额度 · Trae 通道并发 · 更新于 ' + new Date().toLocaleTimeString();
}

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    // 账号池页已恢复：账号表 / 状态标签 / 单号运维按钮 / 批量操作都重新挂上 DOM，
    // 这里的全局计数同时喂给账号池页的统计条。
    if ($('sTotal')) $('sTotal').textContent = d.total;
    if ($('sHealthy')) $('sHealthy').textContent = d.healthy;
    if ($('sCooling')) $('sCooling').textContent = d.cooling;
    if ($('sDisabled')) $('sDisabled').textContent = d.disabled;
    if ($('sPaused')) $('sPaused').textContent = d.paused == null ? '-' : d.paused;
    const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
    const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
    if ($('sCredits')) $('sCredits').textContent = totSum > 0 ? remSum + ' / ' + totSum : remSum;
    if ($('sSticky')) $('sSticky').textContent = d.sticky_sessions;
    // 积分到期提醒卡片：渲染本地快照（overview 自带 credits_earliest_remaining，
    // 由签到与余额后台刷新写入 state.json）——零成本，跟 overview 一起刷新。
    // 此前该卡片只有渲染器、没有触发点，永远不显示。
    renderExpirySnapshot(d);
    $('navSub').textContent = 'v' + d.version;
    $('navVer').textContent = 'v' + d.version;
    $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    const p = $('navPulse');
    p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    if ($('accNote')) $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
    const up = Math.floor(d.uptime_sec);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    if ($('accBody')) renderAccounts(d.accounts || []);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

if ($('accBody')) $('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'pause') {
      await api('accounts/' + encodeURIComponent(u) + '/pause', { method: 'POST' });
      toast('已暂停选号（签到 / 保活照常）', 'ok');
    } else if (a === 'resume') {
      await api('accounts/' + encodeURIComponent(u) + '/resume', { method: 'POST' });
      toast('已恢复选号', 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
});

if ($('btnCheckinAll')) $('btnCheckinAll').onclick = async () => {
  try { await api('checkin_all', { method: 'POST' }); toast('全部签到已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
if ($('btnKeepaliveAll')) $('btnKeepaliveAll').onclick = async () => {
  try { await api('keepalive_all', { method: 'POST' }); toast('全部保活已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
if ($('btnTravelAll')) $('btnTravelAll').onclick = async () => {
  try { await api('travel_all', { method: 'POST' }); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
if ($('btnActivityAll')) $('btnActivityAll').onclick = async () => {
  try { await api('activity_all', { method: 'POST' }); toast('活跃上报已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* rateCell 倍率列：牌价 vs 生效价。上游 credits 是牌价（转正后基准倍率），
   modelPromotions 给当前生效折扣（限时免费 factor=0 / 夜间五折 0.5 等）——
   WorkBuddy 客户端显示的正是生效价。有折扣：生效价大字 + 标签 + 划线牌价，
   悬停带时段说明；无 factor 只有标签（错峰类）：牌价 + 标签。 */
function rateCell(m) {
  const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
    return '<span' + tip + ' style="cursor:help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span' + tip + ' style="cursor:help">' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
  }
  return m.credits ? esc(m.credits) : '—';
}

async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="8"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 探测/用量数据是可选增强：拉取失败不影响模型目录本身。
    const [d, pr, usageView] = await Promise.all([
      api('models'), api('model_probes').catch(() => ({})), api('usage?hours=72').catch(() => ({}))
    ]);
    mdAll = d.models || [];
    mdProbes = pr.probes || {};
    mdBusyByKey = {};
    mdBusyMedianByRealm = {};
    const busyRows = usageView.by_realm_model || [];
    busyRows.forEach(x => { mdBusyByKey[x.key] = x; });
    for (const realm of ['cn', 'global']) {
      const vals = busyRows.filter(x => x.realm === realm && Number(x.requests) >= 5 && Number(x.avg_latency_ms) > 0)
        .map(x => Number(x.avg_latency_ms)).sort((a, b) => a - b);
      if (vals.length) {
        const mid = Math.floor(vals.length / 2);
        mdBusyMedianByRealm[realm] = vals.length % 2 ? vals[mid] : (vals[mid - 1] + vals[mid]) / 2;
      }
    }
    if (!mdAll.length) {
      tb.innerHTML = '<tr><td colspan="8"><div class="empty">上游未返回模型</div></td></tr>';
      $('mdCount').textContent = '';
      $('mdNote').textContent = '上游未返回模型';
      return;
    }
    // 探测键带域前缀（cn:glm-5.2），模型表显示裸名，按「精确命中或 :后缀」关联。
    const probeKeys = Object.keys(mdProbes);
    mdProbeOf = id => mdProbes[id] || mdProbes[probeKeys.find(k => k.endsWith(':' + id))];
    const hit = mdAll.filter(m => mdProbeOf(m.id)).length;
    $('mdNote').textContent = mdAll.length + ' 个模型 · 本地 72h 响应延迟估计（非官方实时负载） · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : '');
    renderModels();
  } catch (e) {
    mdAll = [];
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">' + esc(e.message) + '</div></td></tr>';
    $('mdCount').textContent = '';
  }
}

/* ── 模型筛选（按条件查询）───────────────────────────────────────────
   模型目录一次拉全（几十条），筛选与排序全部在前端完成：改条件零延迟，且不会
   因为调一次筛选就打一次上游——/panel/api/models 是直连上游的实时查询，很贵。
   条件之间是 AND；每个条件为空即不参与判定。 */
// mdRateValue 当前生效的积分倍率数值：优先促销价（限时免费 = 0），无倍率记为
// Infinity 排到最后（排序时"没有价格"不该冒充最便宜）。
function mdRateValue(m) {
  const raw = (m.promo_credits != null && m.promo_credits !== '') ? m.promo_credits : m.credits;
  const n = parseFloat(String(raw == null ? '' : raw).replace(/[^\d.]/g, ''));
  return Number.isFinite(n) ? n : Infinity;
}

// mdSearchText 参与关键字搜索的字段（ID / 展示名 / 厂商 / 描述 / 标签）。
function mdSearchText(m) {
  return [m.id, m.name, m.vendor, m.description, (m.tags || []).join(' ')]
    .filter(Boolean).join(' ').toLowerCase();
}

// mdMatch 单个模型是否满足全部筛选条件。
function mdMatch(m, f) {
  f = f || mdFilter;
  if (f.q) {
    const text = mdSearchText(m);
    // 空格分词后逐个匹配：多关键词是 AND，便于"cn 视觉"这类组合查询。
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.realm && !String(m.id || '').startsWith(f.realm + ':')) return false;
  if (f.cap === 'tool' && !m.supports_tool_call) return false;
  if (f.cap === 'vision' && !m.supports_images) return false;
  if (f.cap === 'reasoning' && !m.supports_reasoning) return false;
  if (f.cap === 'default' && !m.is_default) return false;
  if (f.effort === 'off') {
    if (!m.can_disable_thinking) return false;
  } else if (f.effort && !(m.supported_efforts || []).includes(f.effort)) {
    return false;
  }
  const factor = m.promo_factor == null ? null : Number(m.promo_factor);
  if (f.promo === 'promo' && factor == null && !m.promo_label) return false;
  if (f.promo === 'free' && !(factor === 0)) return false;
  if (f.promo === 'discount' && !(factor != null && factor > 0)) return false;
  return true;
}

// mdSortList 按当前排序条件返回新数组（不改动入参，保持上游原始顺序可回溯）。
function mdSortList(list, f) {
  f = f || mdFilter;
  const out = list.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'rate') out.sort((a, b) => mdRateValue(a) - mdRateValue(b));
  else if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}

// mdBusyCell 用近 72 小时本地请求尝试延迟相对同域中位数估计繁忙程度。
// 该指标不是官方负载；低于 5 次样本不分类，避免小样本制造确定性印象。
function mdBusyCell(id) {
  const parts = String(id || '').split(':');
  const realm = parts.length > 1 && (parts[0] === 'cn' || parts[0] === 'global') ? parts[0] : '';
  const key = realm ? realm + ':' + parts.slice(1).join(':') : '';
  const row = key ? mdBusyByKey[key] : null;
  if (!row || Number(row.requests) < 5 || !Number(row.avg_latency_ms)) {
    const samples = row ? Number(row.requests || 0) : 0;
    return '<td><span class="tag mute">样本不足</span><div class="note">' + samples + ' 次尝试</div></td>';
  }
  const base = mdBusyMedianByRealm[realm];
  if (!base) return '<td><span class="tag mute">缺少同域基线</span><div class="note">' + fmtMs(row.avg_latency_ms) + ' · ' + row.requests + ' 次尝试</div></td>';
  const ratio = Number(row.avg_latency_ms) / base;
  const grade = ratio < 0.8 ? ['ok', '响应较快'] : ratio <= 1.5 ? ['mute', '常规'] : ratio <= 2.5 ? ['warn', '偏忙'] : ['bad', '较忙'];
  const tip = '近72小时本地请求尝试平均延迟，相对同域有足够样本模型的中位数 ' + fmtMs(base) + '；包含失败/重试，不是上游官方实时负载。';
  return '<td title="' + esc(tip) + '"><span class="tag ' + grade[0] + '">' + grade[1] + '</span><div class="note">' + fmtMs(row.avg_latency_ms) + ' · ' + row.requests + ' 次</div></td>';
}

// mdRowHtml 单个模型行（纯渲染，便于独立测试）。
function mdRowHtml(m, pr) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
  const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
    : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
  // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）
  const caps = [];
  if (m.is_default) caps.push('<span class="tag ok">默认</span>');
  if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
  if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
  const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
  const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
  return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm">' + esc(m.id) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
    '<td class="num">' + rateCell(m) + '</td>' +
    '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
    '<td class="efs" style="white-space:normal">' + effs + '</td>' +
    '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
    outCell(m, pr) + mdBusyCell(m.id) + '</tr>';
}

function renderModels() {
  const tb = $('mdBody');
  const list = mdSortList(mdAll.filter(m => mdMatch(m)));
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">没有符合当前筛选条件的模型</div></td></tr>';
  } else {
    tb.innerHTML = list.map(m => mdRowHtml(m, mdProbeOf(m.id))).join('');
  }
  const filtered = list.length !== mdAll.length;
  $('mdCount').textContent = !mdAll.length ? ''
    : filtered ? '命中 ' + list.length + ' / ' + mdAll.length + ' 个模型'
    : mdAll.length + ' 个模型';
  $('mdCount').className = filtered ? 'note src-off' : 'note';
}

function resetModelFilter() {
  mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
  $('mdQ').value = ''; $('mdRealm').value = ''; $('mdCap').value = '';
  $('mdEffort').value = ''; $('mdPromo').value = ''; $('mdSort').value = 'default';
  renderModels();
}

// 筛选控件：输入框防抖 120ms（长列表逐字符重排不必每键一次），下拉即时。
let mdQTimer = null;
$('mdQ').oninput = () => {
  clearTimeout(mdQTimer);
  mdQTimer = setTimeout(() => { mdFilter.q = $('mdQ').value.trim(); renderModels(); }, 120);
};
for (const [id, key] of [['mdRealm', 'realm'], ['mdCap', 'cap'], ['mdEffort', 'effort'], ['mdPromo', 'promo'], ['mdSort', 'sort']]) {
  const el = $(id);
  if (!el) continue;
  el.onchange = () => { mdFilter[key] = el.value; renderModels(); };
}
$('mdReset').onclick = resetModelFilter;
$('btnModels').onclick = loadModels;
if ($('btnModelQueue')) $('btnModelQueue').onclick = loadModelQueue;

/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
let logCh = 'all';
$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  loadLogs();
});
async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  const limit = ($('reqLimit') && $('reqLimit').value) || 100;
  // 时间范围由归档侧过滤（不是前端筛已拉取的条目）：区间落在更早的时间段时，
  // 「最近 N 条」里根本不会有那些记录，必须让服务端按时间取。
  const rq = trangeQuery('reqRange', false);
  rq.set('limit', limit);
  try {
    const [d, metrics, requestRows] = await Promise.all([
      api('logs'),
      api('request_metrics').catch(() => ({})),
      api('request_logs?' + rq.toString()).catch(() => ({ entries: [] })),
    ]);
    // 归档开启时以归档为准——「区间内没有记录」是一个真实结果，不能回落成内存里
    // 的最近 100 条（那会把筛选条件之外、时间范围之外的请求显示出来）。
    // 只有归档关闭时才回落到内存指标，保证没有归档的部署仍能看到最近请求。
    const archiveOn = !!(metrics && metrics.archive && metrics.archive.enabled);
    const recent = archiveOn ? (requestRows.entries || []) : (metrics.recent || []);
    renderRequestMetrics(metrics, recent);
    const entries = (d.entries || []).filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
        const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
        const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>' : '';
        return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
      }).join('')
      : '<span style="color:var(--ink-3)">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + entries.length + ' 行';
  } catch (e) { /* 概览已提示 */ }
}

function renderRequestMetrics(m, entries) {
  m = m || {};
  const a = m.archive || {};
  $('reqSummary').textContent =
    '已完成 ' + fmtTok(m.completed) +
    ' · 成功 ' + (m.success_rate == null ? '—' : Number(m.success_rate).toFixed(1) + '%') +
    ' · HTTP ' + (m.http_success_rate == null ? '—' : Number(m.http_success_rate).toFixed(1) + '%') +
    ' · 平均 ' + fmtMs(m.avg_duration_ms) +
    ' · 进行中 ' + String(m.in_flight || 0);
  $('reqNote').textContent = a.enabled
    ? 'JSONL 归档 ' + fmtBytes(a.bytes) + (a.dropped_writes ? ' · 丢弃 ' + a.dropped_writes + ' 条' : '') +
      (a.last_error ? ' · 错误：' + a.last_error : '')
    : '仅内存指标，JSONL 归档已关闭';

  reqEntries = entries || [];
  renderRequestTable();
}

/* reqMatch 请求记录筛选：q 对 IP/UA/模型/账号/请求 ID 做空格分词的 AND 包含匹配，
   outcome 精确匹配。两者都在已拉取的条目上做（最多 1000 条），不发新请求。 */
function reqMatch(e, f) {
  f = f || reqFilter;
  if (f.outcome && String(e && e.outcome || '') !== f.outcome) return false;
  if (f.q) {
    const text = [e && e.client_ip, e && e.user_agent, e && e.model, e && e.account, e && e.request_id]
      .filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  return true;
}

function reqOutcomeTag(e) {
  const outcome = String(e && e.outcome || '');
  const label = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' }[outcome] || outcome || '—';
  const cls = outcome === 'success' ? 'ok'
    : outcome === 'interrupted' ? 'warn'
    : outcome ? 'bad' : 'mute';
  return '<span class="tag ' + cls + '">' + esc(String(e && e.status || '—') + ' ' + label) + '</span>';
}

function reqTokenCell(e) {
  const total = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  return total ? fmtTok(total) : '—';
}

function reqCreditCell(e) {
  if (!e || !e.credit_known) return '<span class="muted">—</span>';
  const v = Number(e.credit);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '<span class="muted">—</span>';
}

/* renderRequestTable 渲染请求记录表。来源列是这一版的重点：IP 用等宽字体方便扫，
   UA 单行截断（完整值在 title 里，行本身用 requestLogText 作 tooltip）。 */
function renderRequestTable() {
  const list = reqEntries.filter(e => reqMatch(e));
  const tb = $('reqBody');
  if (!tb) return;
  tb.innerHTML = list.map(e => {
    const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
    const ip = e && e.client_ip ? e.client_ip : '';
    const ua = e && e.user_agent ? e.user_agent : '';
    const rid = e && e.request_id ? e.request_id : '';
    return '<tr title="' + esc(requestLogText(e)) + '">' +
      '<td class="num">' + esc(when) + '</td>' +
      '<td>' + reqOutcomeTag(e) + '</td>' +
      '<td>' + esc(e && e.model || '—') + '</td>' +
      '<td>' + esc(e && e.account || '—') + '</td>' +
      '<td>' + (ip ? '<span class="clip ip" title="' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td>' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="num">' + fmtMs(e && e.duration_ms) + '</td>' +
      '<td class="num">' + reqTokenCell(e) + '</td>' +
      '<td class="num">' + reqCreditCell(e) + '</td>' +
      '<td>' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="10" class="empty">' +
      (reqEntries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录') + '</td></tr>';

  const filtered = list.length !== reqEntries.length;
  // 归档里的旧条目没有来源字段（该功能上线前写入）：这时提示开关/历史原因，
  // 而不是让人以为筛选坏了。
  const hasSource = reqEntries.some(e => e && (e.client_ip || e.user_agent));
  $('reqCount').textContent = !reqEntries.length ? ''
    : (filtered ? '命中 ' + list.length + ' / ' + reqEntries.length + ' 条' : reqEntries.length + ' 条') +
      (hasSource ? '' : ' · 来源未记录');
  $('reqCount').className = (filtered || !hasSource) ? 'note src-off' : 'note';
}

/* 请求记录筛选控件。搜索框防抖 150ms：最多 1000 行重渲染，不必每键一次。
   这段顶层绑定放在 requestLogText 之前，是为了让"纯函数切片"式前端测试
   （slice requestLogText → fmtBytes）只拿到无副作用的格式化函数。 */
let reqQTimer = null;
if ($('reqQ')) $('reqQ').oninput = () => {
  clearTimeout(reqQTimer);
  reqQTimer = setTimeout(() => { reqFilter.q = $('reqQ').value.trim(); renderRequestTable(); }, 150);
};
if ($('reqOutcome')) $('reqOutcome').onchange = () => {
  reqFilter.outcome = $('reqOutcome').value;
  renderRequestTable();
};
if ($('reqLimit')) $('reqLimit').onchange = loadLogs;
if ($('btnReqReload')) $('btnReqReload').onclick = loadLogs;
// 时间范围：默认「全部历史」——请求记录页的历史行为就是"取最近 N 条"，
// 加一个默认收窄的区间会让打开页面时看到的条数凭空变少。
if ($('reqRange')) trangeBind('reqRange', loadLogs, '0');

function requestLogText(e) {
  const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
  const outcomeLabel = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' };
  const token = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  let credit = 'credit —';
  if (e && e.credit_known) {
    const value = Number(e.credit);
    if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
  }
  return [
    when,
    String(e && e.status || '—') + ' ' + (outcomeLabel[e && e.outcome] || (e && e.outcome) || '—'),
    e && e.model || '—',
    e && e.account || '—',
    e && e.client_ip || '—',
    e && e.user_agent || '—',
    fmtMs(e && e.duration_ms),
    fmtTok(token) + ' tok',
    credit,
    cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens) === '—' ? '' : '命中 ' + cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens),
    e && e.request_id || '—',
  ].filter(Boolean).join(' | ');
}

/* 缓存命中率纯文本（issue #92）：requestLogText 与积分表/kpi 卡共用。
   自包含（不依赖 trimFixed）：前端纯函数切片测试只截取本段。 */
function cacheRateText(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '—';
  return String(Math.round(h / total * 1000) / 10) + '%';
}

function fmtBytes(bytes) {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  listen: ['listen'], api_key: ['api_key'],
  package_detail_limit: ['panel', 'package_detail_limit'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'], growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  credit_floor: ['pool', 'credit_floor'],
  prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  trae_enabled: ['trae', 'enabled'], trae_auto_hours: ['trae', 'auto_hours'],
  request_client_info: ['logging', 'request_client_info'],
};
/* 「覆盖型」文本字段：空串本身是有意义的取值（= 回落到内置默认），必须照发。
 *
 * 其余文本字段保持「空 = 不下发」的既有语义——那是防误清空的保护，不是 bug：
 * 表单里某个框没填，通常意味着"没改"，把它当成"请清空"会静默抹掉配置。
 *
 * 但覆盖型字段正好相反：清空 = 明确要求回到默认。漏发它们会让面板显示"已保存"
 * 而值其实没变（issue #102 附带发现 2：user_agent 清空后 config.json 里仍是旧值）。
 *
 * 刻意不含 api_key：清空它 = 关闭整个鉴权，误触代价是网关变成无鉴权公开服务。
 * 该字段（以及提示文案"留空 = 不鉴权"与现状不符的问题）单独处理。
 */
const CLEARABLE_CFG = new Set(['user_agent', 'prompt_file']);

function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? '' : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      // trae_auto_hours 的「空」本身是有意义的值：后端把显式 [] 解释为
      // 「只要手动按钮、不要自动签到」（见 config.go normalize 的注释）。
      // 若也按「空=跳过发送」处理，面板就永远关不掉 Trae 自动签到，
      // 用户只能手改 config.json。故这一个字段空值必须显式送 []。
      if (name === 'trae_auto_hours') {
        v = raw === '' ? [] : raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      }
      // 覆盖型字段空串照发（见 CLEARABLE_CFG）；其余空 = 不下发。
      else if (raw === '') v = CLEARABLE_CFG.has(name) ? '' : undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
    const n = (r.restart_required || []).length;
    toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const k = $('cfgKey').value.trim();
    if (k) localStorage.setItem(LS_KEY, k);
    loadConfig();
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function openAdd() {
  $('addVeil').classList.add('on');
  // 重置到 WorkBuddy 标签
  switchAddTab('wb');
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('importDone').hidden = true; $('importErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  resetTraePanel();
  resetQoderPanel();
  resetMinimaxAddPanel();
  resetDoubaoAddPanel();
  stopPoll(); stopTraePoll();
}
function switchAddTab(tab) {
  document.querySelectorAll('#addTabs .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === tab));
  $('addTabWb').hidden = tab !== 'wb';
  $('addTabTrae').hidden = tab !== 'trae';
  $('addTabQoder').hidden = tab !== 'qoder';
  $('addTabMinimax').hidden = tab !== 'minimax';
  $('addTabDoubao').hidden = tab !== 'doubao';
  $('addTabImport').hidden = tab !== 'import';
  // 页脚那组按钮只服务 WorkBuddy 设备码流程；其它流程有自己的按钮（在面板内）。
  const wb = tab === 'wb';
  $('btnStartLogin').hidden = !wb || !$('addReady').hidden;
  $('btnCopyUrl').hidden = !wb || $('addReady').hidden;
  $('btnOpenUrl').hidden = !wb || $('addReady').hidden;
  if (tab === 'trae') stopPoll(); else stopTraePoll();
  if (tab !== 'minimax') stopMmLoginPoll();
  if (tab === 'qoder') loadQoderAddTab();
  if (tab === 'minimax') loadMinimaxAddTab();
  if (tab === 'doubao') resetDoubaoAddPanel();
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
  b.onclick = () => switchAddTab(b.dataset.tab);
});
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      $('addDone').textContent = '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中';
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    $('addReady').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message + '（关闭后重新添加）';
  }
}
function closeAdd() { stopPoll(); stopTraePoll(); stopMmLoginPoll(); loginState = null; traeLoginState = null; $('addVeil').classList.remove('on'); }
$('btnCloseAdd').onclick = closeAdd;

/* ── Trae 账号添加（面板自拉浏览器 + CDP 取凭证）────────────────────────
   与上面 WorkBuddy 的设备码流程是两套独立体系：账号落在 Trae 助手的
   checkin_accounts.json，凭证是 Cloud-IDE-JWT，接口在 api.trae.com.cn。
   走「自拉浏览器」而不是授权页 OAuth：授权页会在「认证中」卡住，且复用浏览器
   登录态 → 多账号没法加。自拉浏览器每次全新会话，可反复加号。 */
function resetTraePanel() {
  $('addTraeLoad').hidden = true; $('addTraeReady').hidden = true;
  $('addTraeDone').hidden = true; $('addTraeErr').hidden = true;
  $('btnTraeStart').disabled = false; $('btnTraeStart').hidden = false;
  $('btnTraeStop').hidden = true;
}
function stopTraePoll() { if (traeTimer) { clearInterval(traeTimer); traeTimer = null; } }
async function startTraeLogin() {
  $('btnTraeStart').disabled = true;
  $('addTraeLoad').hidden = false; $('addTraeErr').hidden = true; $('addTraeDone').hidden = true;
  try {
    await api('trae/browser/start', { method: 'POST' });
    $('addTraeLoad').hidden = true; $('addTraeReady').hidden = false;
    $('btnTraeStart').hidden = true; $('btnTraeStop').hidden = false;
    stopTraePoll();
    traeTimer = setInterval(pollTraeLogin, 2000);
  } catch (e) {
    $('addTraeLoad').hidden = true;
    $('btnTraeStart').disabled = false;
    $('addTraeErr').hidden = false;
    $('addTraeErr').textContent = e.message;
  }
}
async function stopTraeBrowser() {
  try { await api('trae/browser/stop', { method: 'POST' }); } catch (e) { /* 忽略 */ }
  stopTraePoll();
  resetTraePanel();
}
async function pollTraeLogin() {
  try {
    const r = await api('trae/browser/poll');
    if (r.expired) { stopTraePoll(); resetTraePanel(); $('addTraeErr').hidden = false; $('addTraeErr').textContent = r.message; return; }
    if (!r.done) return;
    stopTraePoll();
    $('addTraeReady').hidden = true;
    $('btnTraeStart').hidden = true; $('btnTraeStop').hidden = true;
    if (!r.ok) { $('addTraeErr').hidden = false; $('addTraeErr').textContent = r.message || '添加失败'; return; }
    $('addTraeDone').hidden = false;
    $('addTraeDone').textContent = (r.message || '添加成功') + '，已写入 Trae 账号文件';
    setTimeout(() => { closeAdd(); loadTraeStatus(true); }, 1600);
  } catch (e) {
    stopTraePoll();
    $('addTraeReady').hidden = true;
    $('addTraeErr').hidden = false;
    $('addTraeErr').textContent = e.message + '（关闭后重新添加）';
  }
}
$('btnTraeStart').onclick = startTraeLogin;
$('btnTraeStop').onclick = stopTraeBrowser;

/* ── Qoder 添加（把本机当前登录态存入号池）────────────────────────── */
function resetQoderPanel() {
  const dn = $('addQoderDone'), er = $('addQoderErr');
  if (dn) dn.hidden = true;
  if (er) er.hidden = true;
  const b = $('btnQoderCapture');
  if (b) { b.disabled = false; b.textContent = '添加当前 Qoder 账号'; }
}
async function loadQoderAddTab() {
  resetQoderPanel();
  const cur = $('addQoderCur');
  if (!cur) return;
  cur.className = 'state';
  cur.innerHTML = '<span class="dots">读取中</span>';
  try {
    const d = await api('ide/qoder/accounts');
    cur.className = 'state';
    cur.textContent = d.current_email
      ? '本机当前登录：' + (d.current_name || '') + ' <' + d.current_email + '>'
      : '本机 Qoder 未登录 —— 请先登录 Qoder，再回来点「添加当前 Qoder 账号」。';
  } catch (e) { cur.className = 'state err'; cur.textContent = e.message; }
}
async function doQoderCapture() {
  const b = $('btnQoderCapture');
  b.disabled = true; b.textContent = '添加中…';
  $('addQoderDone').hidden = true; $('addQoderErr').hidden = true;
  try {
    const d = await api('ide/qoder/add', { method: 'POST', body: '{}' });
    $('addQoderDone').hidden = false;
    $('addQoderDone').textContent = (d.added ? '已添加：' : '已更新：') + (d.name || '') + ' <' + d.email + '>';
    loadQoderAccounts(true);
  } catch (e) {
    $('addQoderErr').hidden = false; $('addQoderErr').textContent = e.message;
  } finally { b.disabled = false; b.textContent = '添加当前 Qoder 账号'; }
}
if ($('btnQoderCapture')) $('btnQoderCapture').onclick = doQoderCapture;

/* ── MiniMax 添加（客户端捕获 / 粘贴 Token）────────────────────────── */
function resetMinimaxAddPanel() {
  const dn = $('addMmDone'), er = $('addMmErr');
  if (dn) dn.hidden = true;
  if (er) er.hidden = true;
  const man = $('addMmManual');
  if (man) man.hidden = true;
  const tg = $('btnMmToggleManual');
  if (tg) tg.textContent = '改用粘贴 Token';
  const b = $('btnMmCapture');
  if (b) { b.disabled = false; b.textContent = '添加当前 MiniMax 账号'; }
  resetMmLoginPanel();
}
async function loadMinimaxAddTab() {
  resetMinimaxAddPanel();
  const cur = $('addMmCur');
  if (!cur) return;
  cur.className = 'state';
  cur.innerHTML = '<span class="dots">读取中</span>';
  try {
    const d = await api('ide/minimax/accounts');
    cur.className = 'state';
    if (d.has_client) {
      cur.textContent = '本机 MiniMax Code 已登录，点「添加当前账号」即可（已有 ' + (d.accounts || []).length + ' 个账号在号池）。';
    } else {
      cur.textContent = '本机 MiniMax Code 未登录。可先用其它账号的 Token 粘贴添加（下方「改用粘贴 Token」）。';
    }
  } catch (e) { cur.className = 'state err'; cur.textContent = e.message; }
}
async function doMmCapture() {
  const b = $('btnMmCapture');
  b.disabled = true; b.textContent = '添加中…';
  $('addMmDone').hidden = true; $('addMmErr').hidden = true;
  try {
    const d = await api('ide/minimax/add', { method: 'POST', body: '{}' });
    $('addMmDone').hidden = false;
    $('addMmDone').textContent = (d.added ? '已添加：' : '已更新：') + (d.label || d.id);
    loadCkMiniMax(true);
  } catch (e) {
    $('addMmErr').hidden = false; $('addMmErr').textContent = e.message;
  } finally { b.disabled = false; b.textContent = '添加当前 MiniMax 账号'; }
}
async function doMmAddManual() {
  const b = $('btnMmAddManual');
  b.disabled = true;
  $('addMmDone').hidden = true; $('addMmErr').hidden = true;
  try {
    const d = await api('ide/minimax/add_manual', {
      method: 'POST',
      body: JSON.stringify({ access_token: $('mmAccessInput').value.trim(), refresh_token: $('mmRefreshInput').value.trim() }),
    });
    $('addMmDone').hidden = false;
    $('addMmDone').textContent = d.msg || ('已保存：' + (d.label || d.id));
    $('mmAccessInput').value = ''; $('mmRefreshInput').value = '';
    loadCkMiniMax(true);
  } catch (e) {
    $('addMmErr').hidden = false; $('addMmErr').textContent = e.message;
  } finally { b.disabled = false; }
}
if ($('btnMmCapture')) $('btnMmCapture').onclick = doMmCapture;
if ($('btnMmToggleManual')) $('btnMmToggleManual').onclick = () => {
  const man = $('addMmManual');
  man.hidden = !man.hidden;
  $('btnMmToggleManual').textContent = man.hidden ? '改用粘贴 Token' : '收起粘贴框';
};
if ($('btnMmAddManual')) $('btnMmAddManual').onclick = doMmAddManual;

/* ── MiniMax 浏览器登录（OAuth 设备码，RFC 8628）─────────────────────
   服务端申请 device_code → 用户在浏览器授权 → 面板轮询换 token → 入池。
   两个实测约束决定了这段逻辑的形状：
     · 授权窗口只有 300 秒（上游 expires_in=300）→ 必须显示倒计时并允许重发；
     · 轮询快于服务端给的 interval 会拿到 slow_down → 轮询节奏跟着服务端走。 */
let mmLoginState = null, mmLoginTimer = null, mmLoginCdTimer = null, mmLoginInterval = 3;
function stopMmLoginPoll() {
  if (mmLoginTimer) { clearTimeout(mmLoginTimer); mmLoginTimer = null; }
  if (mmLoginCdTimer) { clearInterval(mmLoginCdTimer); mmLoginCdTimer = null; }
}
function resetMmLoginPanel() {
  stopMmLoginPoll();
  mmLoginState = null;
  const box = $('addMmLoginBox');
  if (box) box.hidden = true;
  const er = $('addMmLoginErr');
  if (er) { er.hidden = true; er.textContent = ''; }
  const b = $('btnMmLogin');
  if (b) { b.disabled = false; b.textContent = '浏览器登录添加账号'; }
}
function startMmCountdown(sec) {
  if (mmLoginCdTimer) clearInterval(mmLoginCdTimer);
  let left = Math.max(0, sec | 0);
  const tick = () => {
    const el = $('mmLoginCountdown');
    if (!el) return;
    if (left <= 0) {
      el.textContent = '已过期，请重新申请';
      stopMmLoginPoll();
      return;
    }
    const m = Math.floor(left / 60), s = left % 60;
    el.textContent = m + ':' + String(s).padStart(2, '0');
    left--;
  };
  tick();
  mmLoginCdTimer = setInterval(tick, 1000);
}
async function startMmLogin() {
  const b = $('btnMmLogin');
  const er = $('addMmLoginErr');
  b.disabled = true; b.textContent = '正在申请…';
  er.hidden = true; er.textContent = '';
  const realm = (document.querySelector('input[name="mmRealm"]:checked') || {}).value || 'cn';
  try {
    const r = await api('ide/minimax/login/start', { method: 'POST', body: JSON.stringify({ region: realm }) });
    mmLoginState = r.state;
    mmLoginInterval = Math.max(2, r.interval || 3);
    $('addMmLoginBox').hidden = false;
    $('mmLoginUrl').textContent = r.verification_uri_complete || r.verification_uri || '';
    $('mmLoginCode').textContent = r.user_code || '';
    b.disabled = false; b.textContent = '重新申请';
    startMmCountdown(r.expires_in || 300);
    mmLoginTimer = setTimeout(pollMmLogin, mmLoginInterval * 1000);
  } catch (e) {
    b.disabled = false; b.textContent = '浏览器登录添加账号';
    er.hidden = false; er.textContent = e.message;
  }
}
async function pollMmLogin() {
  if (!mmLoginState) return;
  try {
    const r = await api('ide/minimax/login/poll?state=' + encodeURIComponent(mmLoginState));
    if (r.done) {
      stopMmLoginPoll();
      $('addMmLoginBox').hidden = true;
      $('addMmDone').hidden = false;
      $('addMmDone').textContent = (r.msg || ('已添加：' + (r.label || r.id))) + (r.checkin ? ' · ' + r.checkin : '');
      loadCkMiniMax(true);
      return;
    }
    if (r.fatal) {
      stopMmLoginPoll();
      $('addMmLoginErr').hidden = false;
      $('addMmLoginErr').textContent = r.error || '授权失败';
      return;
    }
    if (r.interval) mmLoginInterval = Math.max(2, r.interval);
    if (r.message) {
      const er = $('addMmLoginErr');
      er.hidden = false; er.textContent = r.message;
    }
    mmLoginTimer = setTimeout(pollMmLogin, mmLoginInterval * 1000);
  } catch (e) {
    stopMmLoginPoll();
    $('addMmLoginErr').hidden = false;
    $('addMmLoginErr').textContent = e.message + '（关闭后重新添加）';
  }
}
if ($('btnMmLogin')) $('btnMmLogin').onclick = startMmLogin;
if ($('btnMmLoginOpen')) $('btnMmLoginOpen').onclick = () => window.open($('mmLoginUrl').textContent, '_blank');
if ($('btnMmLoginCopy')) $('btnMmLoginCopy').onclick = () => navigator.clipboard.writeText($('mmLoginCode').textContent)
  .then(() => toast('验证码已复制', 'ok'), () => toast('复制失败，请手动复制', 'err'));

/* ── 豆包添加（客户端提取 / 粘贴 Cookie）──────────────────────────── */
function resetDoubaoAddPanel() {
  const dn = $('addDbDone'), er = $('addDbErr');
  if (dn) dn.hidden = true;
  if (er) er.hidden = true;
  const man = $('addDbManual');
  if (man) man.hidden = true;
  const tg = $('btnDbToggleManual');
  if (tg) tg.textContent = '改用粘贴 Cookie';
  const b = $('btnDbExtract');
  if (b) { b.disabled = false; b.textContent = '从客户端提取当前账号'; }
}
async function doDbExtractAdd() {
  const b = $('btnDbExtract');
  b.disabled = true; b.textContent = '提取中（会重启豆包客户端）…';
  $('addDbDone').hidden = true; $('addDbErr').hidden = true;
  try {
    const d = await api('ide/doubao/add_extract', { method: 'POST', body: '{}' });
    $('addDbDone').hidden = false;
    $('addDbDone').textContent = (d.msg || '已提取') + (d.note ? '（' + d.note + '）' : '');
    loadCkDoubao(true);
  } catch (e) {
    $('addDbErr').hidden = false; $('addDbErr').textContent = e.message;
  } finally { b.disabled = false; b.textContent = '从客户端提取当前账号'; }
}
/* 豆包「登录窗口」：登录在客户端自己的 UI 里（网页资源加密打包，没有可复用的
   OAuth 端点），所以这里拉起客户端本体，在客户端里登录完再点「提取」。 */
async function doDbLaunch() {
  const b = $('btnDbLaunch');
  b.disabled = true; b.textContent = '正在启动…';
  $('addDbDone').hidden = false; $('addDbErr').hidden = true;
  try {
    const d = await api('ide/doubao/launch', { method: 'POST', body: '{}' });
    $('addDbDone').textContent = d.msg || '已启动豆包客户端';
  } catch (e) {
    $('addDbErr').hidden = false; $('addDbErr').textContent = e.message;
  } finally { b.disabled = false; b.textContent = '启动豆包客户端登录'; }
}
if ($('btnDbLaunch')) $('btnDbLaunch').onclick = doDbLaunch;
async function doDbAddCookie() {
  const b = $('btnDbAddCookie');
  b.disabled = true;
  $('addDbDone').hidden = true; $('addDbErr').hidden = true;
  try {
    const d = await api('ide/doubao/add_cookie', {
      method: 'POST',
      body: JSON.stringify({ cookie: $('dbCookieInput').value }),
    });
    $('addDbDone').hidden = false;
    $('addDbDone').textContent = d.msg || '已保存';
    $('dbCookieInput').value = '';
    loadCkDoubao(true);
  } catch (e) {
    $('addDbErr').hidden = false; $('addDbErr').textContent = e.message;
  } finally { b.disabled = false; }
}
if ($('btnDbExtract')) $('btnDbExtract').onclick = doDbExtractAdd;
if ($('btnDbToggleManual')) $('btnDbToggleManual').onclick = () => {
  const man = $('addDbManual');
  man.hidden = !man.hidden;
  $('btnDbToggleManual').textContent = man.hidden ? '改用粘贴 Cookie' : '收起粘贴框';
};
if ($('btnDbAddCookie')) $('btnDbAddCookie').onclick = doDbAddCookie;

$('btnStartLogin').onclick = startAddLogin;
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => navigator.clipboard.writeText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));
$('importFile').onchange = async () => {
  const file = $('importFile').files[0];
  if (!file) return;
  $('importDone').hidden = true; $('importErr').hidden = true;
  const fd = new FormData();
  fd.append('file', file);
  const h = {};
  const k = localStorage.getItem(LS_KEY);
  // 同上：非 ISO-8859-1 密钥跳过，避免 FormData 上传请求抛异常。
  if (k && /^[\x00-\xff]*$/.test(k)) h['Authorization'] = 'Bearer ' + k;
  try {
    const r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd, headers: h });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
    $('importDone').hidden = false;
    $('importDone').textContent = '导入完成：成功 ' + d.imported + ' 个' + (d.skipped ? '，跳过 ' + d.skipped + ' 个' : '');
    if (d.errors && d.errors.length) {
      console.warn('import errors:', d.errors);
    }
    loadOverview(true);
  } catch (e) {
    $('importErr').hidden = false;
    $('importErr').textContent = '导入失败：' + e.message;
  }
  $('importFile').value = '';
};

/* ── 切号冻结：只换后端登录态，界面 100% 停在切换前 ───────────────────
   问题：切 A→B 后立刻整表重绘，加上每 5 秒的全局刷新，列表顺序、滚动位置、
   额度数字、连「当前登录」徽章都会变成 B 的——观感上就是「页面被切走了」。
   做法（完整版，不是最小改动）：
     1) 切号成功后**不重载任何数据、不改任何 DOM**：画面 100% 保持切换前，
        连「当前登录」标记都留在 A 上（面板会提示：这是冻结态，点一下即刷新）；
     2) 冻结无时间上限，直到用户显式刷新才解冻——避免「过一会儿自己就变了」；
     3) 冻结期间统一闸门 autoRefreshBlocked()：overview 轮询、日志轮询、
        任务队列轮询、进出 IDE 账号页的自动加载全部跳过；
     4) 顶部常驻一条可点提示条，写明切到了哪个号、数据已暂停，点它才刷新；
     5) 视图位置持久化（sessionStorage）：视图/tab、整页滚动、三个列表内部滚动、
        WorkBuddy 版本筛选，刷新页面或切号后都回到原处；
     6) 解冻时统一 reloadCurrentView()，按当前视图精确补数据，并恢复滚动位置。
   边界：只冻结「展示层」，后端切号/签到/写入凭据完全不受影响。 */
const VIEW_STATE_KEY = 'wb2a.viewstate.v1';
const FREEZE = { active: false, kind: '', target: '', at: 0 };

function viewFrozen() { return FREEZE.active; }
// autoRefreshBlocked 是所有「自动刷新」的唯一闸门。
function autoRefreshBlocked() { return FREEZE.active; }

// freezeView 进入冻结态：画面停在当前这一刻，不再自动跟随账号变化。
function freezeView(kind, target) {
  FREEZE.active = true;
  FREEZE.kind = kind || '';
  FREEZE.target = target || '';
  FREEZE.at = Date.now();
  showFreezeBar();
  saveViewState();
}

// thawView 解冻。opts.reload=true 时按当前视图重新取数并恢复滚动位置。
// 返回是否确实从冻结态解冻（用于按钮做「原来没冻结就别多刷一次」）。
function thawView(opts) {
  const was = FREEZE.active;
  FREEZE.active = false; FREEZE.kind = ''; FREEZE.target = ''; FREEZE.at = 0;
  hideFreezeBar();
  if (opts && opts.reload && was) reloadCurrentView();
  return was;
}

// reloadCurrentView 按当前视图精确补数据（解冻/手动刷新共用），并保持滚动位置。
// 同时快照三个列表的内部滚动：重载会重建 innerHTML，不恢复就会「回到列表顶部」。
function reloadCurrentView(skipOverview) {
  const y = window.scrollY || 0;
  const lists = {};
  ['qoderList', 'traeIdeList', 'wbList', 'accBody'].forEach(id => {
    const el = $(id);
    if (el && typeof el.scrollTop === 'number') lists[id] = el.scrollTop;
  });
  if (!skipOverview) loadOverview(true);
  if (view === 'ideaccounts') { loadQoderAccounts(true); loadTraeIdeAccounts(true); loadWorkBuddyAccounts(true); }
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') { reattachQueueView(); loadIdeaTasks(true); }
  else if (view === 'checkin') { loadTraeStatus(true); loadTraeCredits(true); loadCheckinPool(true); loadQkCredits(true); loadWkCredits(true); }
  else if (view === 'usage') { loadUsage(); loadUsageOfficial(); }
  else if (view === 'packages') { loadPackages(); loadPkIdea(true); }
  else if (view === 'models') { loadModels(); loadModelQueue(); }
  restoreScrollTo(y);
  // 列表是异步渲染的：分两拍补内部滚动（首拍覆盖首屏，次拍覆盖慢请求）。
  const snap = { lists: lists };
  setTimeout(() => applyListScroll(snap), 120);
  setTimeout(() => applyListScroll(snap), 500);
}

function showFreezeBar() {
  const bar = $('freezeBar');
  if (!bar) return;
  const name = FREEZE.target ? esc(FREEZE.target) : '新账号';
  bar.innerHTML = '<span class="ic">✓</span>' +
    '<span>已切换到 <b>' + name + '</b> · 界面停留在切换前，数据暂停更新</span>' +
    '<span class="go">点此立即刷新</span>';
  bar.hidden = false;
  bar.onclick = () => { thawView({ reload: true }); toast('已刷新为最新数据', 'ok'); };
}
function hideFreezeBar() {
  const bar = $('freezeBar');
  if (bar) { bar.hidden = true; bar.onclick = null; }
}

// onSwitchDone 三处切号共用的收尾：不重载、不改标记，只冻结 + 提示。
function onSwitchDone(kind, target, note) {
  freezeView(kind, target);
  toast('已切到 ' + target + (note ? '，' + note : '') + '（界面已冻结，点顶部提示可刷新）', 'ok');
}

/* ── 视图位置持久化 ────────────────────────────────────────────────
   目的：切号、误按 F5、或从别的视图切回来时，不会「回到页面顶部、
   列表回到第一行」。属于纯前端体验优化，失败（隐私模式）静默忽略。 */
function saveViewState() {
  try {
    const lists = {};
    ['qoderList', 'traeIdeList', 'wbList', 'accBody'].forEach(id => {
      const el = $(id);
      if (el && typeof el.scrollTop === 'number') lists[id] = el.scrollTop;
    });
    const wbEd = $('wbEdition');
    sessionStorage.setItem(VIEW_STATE_KEY, JSON.stringify({
      view: view, y: window.scrollY || 0, lists: lists,
      wbEdition: wbEd ? wbEd.value : '', at: Date.now(),
    }));
  } catch (_) { /* 配额/隐私模式：忽略 */ }
}
function readViewState() {
  try { return JSON.parse(sessionStorage.getItem(VIEW_STATE_KEY) || 'null'); } catch (_) { return null; }
}
function restoreScrollTo(y) {
  if (!y) return;
  requestAnimationFrame(() => window.scrollTo(0, y));
}
// restoreViewState 还原上次的视图与滚动位置；返回是否成功还原。
function restoreViewState() {
  const st = readViewState();
  if (!st || !st.view || !(st.view in TITLES)) return false;
  go(st.view);
  restoreScrollTo(st.y);
  // 列表内容异步渲染，分两拍恢复内部滚动（首拍覆盖首屏，次拍覆盖慢请求）
  setTimeout(() => applyListScroll(st), 120);
  setTimeout(() => applyListScroll(st), 500);
  return true;
}
function applyListScroll(st) {
  Object.keys((st && st.lists) || {}).forEach(id => {
    const el = $(id);
    if (el) el.scrollTop = st.lists[id] || 0;
  });
}

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  b.disabled = true; b.textContent = '刷新中…';
  try {
    // 顶部「刷新」= 用户明确要看最新数据：解冻 + 余额刷新 + 当前视图整体重取。
    thawView();
    await api('balance_all', { method: 'POST' });
    reloadCurrentView(true); // 已经在下面拿过 overview
    await loadOverview(true);
    toast('余额已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { b.disabled = false; b.textContent = '刷新'; }
  if (view === 'logs') loadLogs();
};

/* ── 轮询 ─────────────────────────────────────────────────────────── */
function refreshVisible() {
  // 切号后的冻结窗口内不刷新全局数字：避免刚切到 B 号，面板上的额度/统计
  // 立刻变成 B 的，把用户正在看的 A 号视图「切走」。手动刷新会解冻。
  if (viewFrozen()) return;
  // overview 每 5s 刷新一次，账号池视图的账号表与统计条随之更新。
  loadOverview(true);
  if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') { reattachQueueView(); loadIdeaTasks(true); }
}
function start() {
  loadOverview(true);
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
  // 滚动位置持久化：停止滚动 250ms 后记一次，避免每帧写 sessionStorage。
  let vsTimer = null;
  window.addEventListener('scroll', () => {
    clearTimeout(vsTimer);
    vsTimer = setTimeout(saveViewState, 250);
  }, { passive: true });
  checkAuthGate();
}
async function checkAuthGate() {
  try { await api('overview'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）',
  'school_season': '校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_1': '小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_2': '小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）',
  'Sequential_Tasks_3': '小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）',
  'Sequential_Tasks_4': '小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_5': '小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_6': '小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖',
  'Sequential_Tasks_7': '体验灵感功能（预留，疑 PC 口径）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）'
};

function openTasks(uid) {
  taskUID = uid;
  $('taskWho').textContent = uid.slice(0, 16);
  $('taskVeil').classList.add('on');
  $('btnTaskReload').hidden = false;
  loadTasks();
}
function closeTasks() { $('taskVeil').classList.remove('on'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
//
// 耗时提示：普通任务约 1-2 分钟；但「小程序多次对话」任务（Sequential_Tasks_3/6
// 等）每条对话事件之间必须间隔约 45 秒（上游反作弊：数秒级连发会被判无效整体
// 回滚），Tasks_6「10 次对话」单项就要约 8 分钟。后端按待办量动态放宽超时，
// 前端把已用时长打在按钮上——长时间「执行中」是正常的，不要当卡死关掉。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAutoAll');
  if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n\n' +
    '普通任务约 1-2 分钟；若含「小程序多次对话」任务，每条对话需间隔约 45 秒，\n' +
    '最长可能十几分钟。执行期间请保持本页打开，确认继续？')) return;
  btn.disabled = true;
  const t0 = Date.now();
  const label = () => '执行中… ' + Math.round((Date.now() - t0) / 1000) + 's';
  btn.textContent = label();
  const tick = setInterval(() => { btn.textContent = label(); }, 1000);
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all', { method: 'POST' });
    const rs = r.results || [];
    const okN = rs.filter(x => x.status === 'done').length;
    const skipN = rs.filter(x => x.status === 'skipped').length;
    const errN = rs.filter(x => x.status === 'error').length;
    const lockN = rs.filter(x => x.status === 'locked').length;
    const manN = rs.filter(x => x.status === 'manual').length;
    const secs = Math.round((Date.now() - t0) / 1000);
    // 未解锁项不是失败：上游把任务真锁到某个时刻（accept 直接回 task locked until），
    // 到点后由面板的成长队列自动执行，所以单独提示、不计入「失败」。
    // 「需手动」也不是失败：那是关注公众号 / 真实捐款 / 国际版成长路径这类**网关
    // 做不了真实行为**的任务，如实列出来（否则用户只看到 25 项全完成、却在任务中心
    // 看到还有剩余，会以为两边对不上）。
    const lockTip = lockN ? `，${lockN} 项未解锁（到点自动执行）` : '';
    const manTip = manN ? `，${manN} 项需真实操作（面板做不了）` : '';
    toast(`执行完成（${secs}s）：成功 ${okN} 项，跳过 ${skipN} 项${lockTip}${manTip}${errN ? '，失败 ' + errN + ' 项' : ''}`,
      errN ? 'err' : 'ok');
    console.log('auto_all results:', r.results);
  } catch (e) { toast(e.message, 'err'); }
  finally {
    clearInterval(tick);
    btn.disabled = false; btn.textContent = '一键完成可自动任务'; loadTasks();
  }
};

// fmtUnlock 上游解锁时刻（RFC3339）→ 「MM-DD HH:mm」（空/异常返回 ''）。
// 未解锁任务（Sequential 链条每日零点解锁一环）要把它显示出来，否则用户只看到
// 一个「未解锁」标签，不知道什么时候才会轮到。
function fmtUnlock(s) {
  const m = String(s || '').match(/^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/);
  return m ? `${m[2]}-${m[3]} ${m[4]}:${m[5]}` : '';
}

async function loadTasks() {
  if (!taskUID) return;
  const st = $('taskState'), tb = $('taskTable');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  $('taskProgress').hidden = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
    const list = d.tasks || [];
    // 国际版（workbuddy.ai）返回的是「成长路径」清单型任务：没有报名环节，
    // 也不能由网关代做（需官方客户端行为点亮）。这一模式换一套说明与按钮。
    const checklist = d.mode === 'checklist';
    if (!list.length) {
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    $('taskHint').textContent = checklist
      ? '国际版「成长路径」任务：没有报名环节，也无法由网关代做——需在官方 WorkBuddy 客户端完成对应操作（真实对话 / 安装技能 / 链接微信 / 召唤专家 / 使用模板），完成后回到这里点「领取」。'
      : '查询上游任务进度；「接受」为报名（幂等），「领取」在进度达标后可用。所有操作走网关，无需外部脚本。';
    $('btnTaskAcceptAll').hidden = checklist;
    $('btnTaskAutoAll').hidden = checklist;
    const prof = d.profile;
    if (checklist && prof) {
      const done = prof.completed ?? 0, tot = prof.total ?? list.length;
      $('taskProgress').hidden = false;
      $('taskBarFill').style.width = (tot ? Math.round(done * 100 / tot) : 0) + '%';
      $('taskProgressText').textContent = '成长路径已完成 ' + done + '/' + tot +
        (prof.max_level ? ' · 已满级' : (prof.level_name ? ' · ' + prof.level_name : ''));
    } else {
      $('taskProgress').hidden = true;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push('+' + t.credit + ' 分');
      if (t.energy) parts.push('+' + t.energy + ' 能');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' ') : '—';
      const lk = fmtUnlock(t.unlock_at); // 未解锁任务的解锁时刻（空串 = 上游没给）
      let badge, acted;
      if (t.checklist) {
        // 清单型：状态词表是 available / completed / claimed，没有 accept_status。
        // 「未完成」不能渲染成「未接受」——那会让人去找一个根本不存在的报名按钮。
        const done = !t.claimed && t.status === 'completed';
        badge = t.claimed ? '<span class="tag ok">已领取</span>'
          : done ? '<span class="tag warn">可领取</span>'
          : '<span class="tag mute">未完成</span>';
        acted = t.claimed ? ''
          : done ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
          : '<span class="hint" style="font-size:11.5px">客户端完成</span>';
      } else {
        badge = t.claimed ? '<span class="tag ok">已领取</span>'
          : t.claimable ? '<span class="tag warn">可领取</span>'
          : t.locked ? '<span class="tag mute" title="' + esc(lk ? '上游锁定至 ' + lk + '，到点由面板自动执行' : '上游未解锁') + '">' + (lk ? lk + ' 解锁' : '未解锁') + '</span>'
          : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
          : '<span class="tag mute">未接受</span>';
        acted = t.claimed || t.locked ? ''
          : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
          : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
          : t.accept_status === 'accepted' ? ''
          : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      }
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const sub = t.checklist ? (t.level_name || t.task_code) : (t.task_code + (t.tag ? ' · ' + t.tag : ''));
      const lockTip = t.locked
        ? (lk ? '上游锁定至 ' + lk + '，到点由面板自动执行' : '上游未解锁，到点由面板自动执行')
        : '';
      const tip = [t.title, t.task_desc || t.description, lockTip, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(sub) + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      loadOverview(true);
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      const r = await api(path, { method: 'POST', body: JSON.stringify(body) });
      if (kind === 'claim') {
        toast('已领取奖励', 'ok');
        loadOverview(true);
      } else {
        // 上游对 accept 恒返回 200，真实成败在 results[] 里——按它提示，
        // 不要一见 HTTP 200 就报「已接受」（国际版账号上会谎报成功）。
        const res = (r.results || [])[0];
        if (res && res.status === 'error') toast('接受失败：' + (res.message || '上游拒绝'), 'err');
        else if (res && res.status === 'already_accepted') toast('该任务此前已接受', 'ok');
        else if (res && res.status === 'unknown') toast('已提交，但上游未返回确认结果', 'err');
        else toast('已接受任务', 'ok');
      }
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});

/* ── Trae 签到 ──────────────────────────────────────────────────────
   账号来自 Trae Work 助手的 checkin_accounts.json（服务端直接读同一份文件，
   这里不做任何本地存储）。签到走 POST trae/checkin，服务端先 status 预检、
   已签跳过，再 claim，账号间 1.2s 节流。 */
async function loadTraeStatus(quiet) {
  const st = $('traeState'), list = $('traeList');
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const d = await api('trae/accounts');
    const arr = d.accounts || [];
    if (d.enabled === false) {
      st.hidden = false; st.className = 'state';
      st.textContent = 'Trae 签到已在配置中关闭（trae.enabled=false）';
      list.innerHTML = ''; $('traeSummary').textContent = '已关闭';
      if ($('btnTraeCheckinAll')) $('btnTraeCheckinAll').disabled = true;
      return;
    }
    if ($('btnTraeCheckinAll')) $('btnTraeCheckinAll').disabled = false;
    if (!arr.length) {
      st.hidden = false; st.className = 'state';
      st.textContent = '未找到 Trae 账号。请先在「Trae Work 助手」里添加账号，然后回来点刷新。';
      list.innerHTML = ''; $('traeSummary').textContent = '';
      return;
    }
    let done = 0, errs = 0, expired = 0;
    const head = '<div class="thead"><div>账号</div><div>今日状态</div><div>积分</div><div>操作</div></div>';
    list.innerHTML = head + arr.map(a => {
      if (a.expired) expired++;
      if (a.error) errs++; else if (a.checked_in) done++;
      const status = a.expired
        ? '<span class="stask todo"><span class="mark">!</span>凭证过期</span>'
        : (a.error
          ? '<span class="stask todo"><span class="mark">!</span>查询失败</span>'
          : (a.checked_in
            ? '<span class="stask ok"><span class="mark">✓</span>今日已签</span>'
            : '<span class="stask warn"><span class="mark">○</span>未签到</span>'));
      const credits = a.credits == null ? '—' : a.credits;
      // 续期能力：refresh_token 通道（最快）> 浏览器会话通道 > 都无（需重新登录）。
      const renewTip = a.refreshable ? '可自动续期（refresh_token 通道）'
        : (a.has_profile ? '可自动续期（浏览器会话通道）' : '暂不可自动续期，请在「添加账号 → Trae 登录」重新登录一次');
      const idTitle = (a.expires_at ? '凭证到期 ' + a.expires_at + ' · ' : '') + renewTip;
      const renewBtn = (a.refreshable || a.has_profile || a.expired)
        ? '<button class="xs" data-trae-renew="' + esc(a.user_id || '') + '">续期</button>' : '';
      return '<div class="trow">' +
        '<div class="who"><div class="nm" title="' + esc(a.name || '') + '">' + esc(a.name || '未命名') + '</div>' +
        '<div class="id" title="' + esc(idTitle) + '">' + esc(a.user_id || '') + '</div></div>' +
        '<div>' + status + '</div>' +
        '<div class="credits" title="当前积分">' + esc(credits) + '</div>' +
        '<div><button class="xs" data-trae-uid="' + esc(a.user_id || '') + '"' + (a.checked_in || a.expired ? ' disabled' : '') + '>签到</button>' + renewBtn + '</div>' +
        (a.error ? '<div class="err">' + esc(a.error) + '</div>' : '') +
        '</div>';
    }).join('');
    const autoN = arr.filter(a => a.refreshable || a.has_profile).length;
    const renewNote = ' · ' + autoN + '/' + arr.length + ' 可自动续期';
    $('traeSummary').textContent = expired
      ? done + '/' + arr.length + ' 已签 · ' + expired + ' 个凭证过期' + renewNote
      : (errs
        ? done + '/' + arr.length + ' 已签 · ' + errs + ' 个查询失败' + renewNote
        : (done === arr.length ? '今日全部完成 🎉' + renewNote : done + '/' + arr.length + ' 已签' + renewNote));
    st.hidden = true;
    list.querySelectorAll('button[data-trae-uid]').forEach(b => {
      b.onclick = () => doTraeCheckin([b.dataset.traeUid]);
    });
    list.querySelectorAll('button[data-trae-renew]').forEach(b => {
      b.onclick = () => doTraeRenew([b.dataset.traeRenew]);
    });
  } catch (e) {
    st.hidden = false; st.className = 'state err'; st.textContent = e.message;
  }
}

async function doTraeCheckin(userIds) {
  const one = !!(userIds && userIds.length === 1);
  if (!one && !confirm('将对全部 Trae 账号执行签到。确认继续？')) return;
  const btn = $('btnTraeCheckinAll');
  if (btn) btn.disabled = true;
  try {
    const d = await api('trae/checkin', {
      method: 'POST',
      body: JSON.stringify(userIds ? { user_ids: userIds } : {}),
    });
    const ok = d.total_ok || 0, already = d.already || 0, failed = d.failed || 0;
    if (failed) toast('签到完成：成功 ' + ok + ' / 已签 ' + already + ' / 失败 ' + failed, 'err');
    else if (ok) toast('签到成功 ' + ok + ' 个账号' + (already ? '（' + already + ' 个已签过）' : ''), 'ok');
    else toast('全部账号今日已签到', 'ok');
    loadTraeStatus(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) btn.disabled = false; }
}
/* 续期凭证：走 POST trae/refresh。服务端对「仍在有效期内」的账号会自动跳过，
   所以整页续期不会白刷；想强制刷新单个账号时点行内的「续期」即可。 */
async function doTraeRenew(userIds) {
  const one = !!(userIds && userIds.length === 1);
  if (!one && !confirm('将对全部 Trae 账号检查并续期凭证。确认继续？')) return;
  const btn = $('btnTraeRenew');
  const rowBtn = one
    ? document.querySelector('button[data-trae-renew="' + CSS.escape(userIds[0]) + '"]')
    : null;
  const btnOld = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '续期中…'; }
  const rowOld = rowBtn ? rowBtn.textContent : '';
  if (rowBtn) { rowBtn.disabled = true; rowBtn.textContent = '续期中…'; }
  // 浏览器会话通道要拉起无头浏览器，单个账号就可能耗时 30s~5min；给用户明确提示，
  // 避免请求挂起时看起来像「点了没反应」。
  toast('正在续期（浏览器通道较慢，最长约 5 分钟，请勿关闭页面）…', 'ok');
  try {
    const d = await api('trae/refresh', {
      method: 'POST',
      body: JSON.stringify(userIds ? { user_ids: userIds, force: true } : {}),
    });
    const rs = d.results || [];
    const okN = rs.filter(x => x.status === 'refreshed').length;
    const failN = rs.filter(x => x.status === 'fail').length;
    if (failN) {
      const first = rs.find(x => x.status === 'fail');
      toast('续期完成：成功 ' + okN + ' / 失败 ' + failN + (first ? '（' + first.message + '）' : ''), 'err');
    } else if (okN) {
      toast('续期成功 ' + okN + ' 个账号', 'ok');
    } else {
      toast('凭证仍在有效期内，无需续期', 'ok');
    }
    loadTraeStatus(true);
  } catch (e) { toast(e.message, 'err'); }
  finally {
    if (btn) { btn.disabled = false; btn.textContent = btnOld || '续期凭证'; }
    if (rowBtn) { rowBtn.disabled = false; rowBtn.textContent = rowOld || '续期'; }
  }
}
if ($('btnTraeCheckinAll')) $('btnTraeCheckinAll').onclick = () => doTraeSignAll($('btnTraeCheckinAll'));

/* 从本机已登录的 Trae 客户端提取当前账号令牌（免浏览器续期通道）。
   客户端在线时会自己刷新 JWT，提取即把最新令牌 upsert 进号池；
   若客户端登录的是新账号，也会直接入池。 */
async function doTraeClientSync() {
  const btn = $('btnTraeClientSync');
  const old = btn ? btn.textContent : '从客户端提取';
  if (btn) { btn.disabled = true; btn.textContent = '提取中…'; }
  try {
    const d = await api('trae/client_sync', { method: 'POST', body: JSON.stringify({ force: true }) });
    if (d.ok) toast(d.message || ('已提取客户端令牌' + (d.expires_at ? '（到期 ' + d.expires_at + '）' : '')), 'ok');
    else toast(d.message || '客户端暂无可提取的登录态', d.action === 'fresh' ? 'ok' : 'err');
    loadTraeStatus(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = old; } }
}
if ($('btnTraeClientSync')) $('btnTraeClientSync').onclick = doTraeClientSync;

/* 一键签到：先尝试续期凭证（有效期内自动跳过），再对全部 Trae 账号签到。 */
async function doTraeSignAll(btn) {
  btn = btn || $('btnTraeCheckinAll');
  const old = btn ? btn.textContent : '一键签到';
  if (btn) { btn.disabled = true; btn.textContent = '签到中…'; }
  try {
    try { await api('trae/refresh', { method: 'POST', body: JSON.stringify({ force: false }) }); } catch (_) { /* 续期失败不阻断签到 */ }
    const d = await api('trae/checkin', { method: 'POST', body: '{}' });
    const ok = d.total_ok || 0, already = d.already || 0, failed = d.failed || 0;
    if (failed) toast('Trae 签到完成：成功 ' + ok + ' / 已签 ' + already + ' / 失败 ' + failed, 'err');
    else if (ok) toast('Trae 签到成功 ' + ok + ' 个账号' + (already ? '（' + already + ' 个已签过）' : ''), 'ok');
    else toast('Trae 全部账号今日已签到', 'ok');
    loadTraeStatus(true); loadTraeCredits(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = old || '一键签到'; } }
}

/* ── Trae 积分 ──────────────────────────────────────────────────────
   余额走上游 ide_user_ent_usage 实时口径；实时失败才回落本地快照并标注来源。
   统一三列：账号 / 积分余额 / 签到可得。 */
async function loadTraeCredits(quiet) {
  const st = $('tcState'), tb = $('tcBody');
  if (!tb) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; tb.innerHTML = ''; }
  try {
    const d = await api('trae/credits');
    const arr = d.accounts || [];
    if (!arr.length) {
      st.hidden = false; st.className = 'state';
      st.textContent = '未找到 Trae 账号。';
      tb.innerHTML = ''; $('tcNote').textContent = '';
      return;
    }
    tb.innerHTML = arr.map(a => {
      let bal;
      if (a.has_balance) {
        const n = Number(a.balance).toLocaleString('zh-CN', { maximumFractionDigits: 2 });
        const src = a.balance_source === 'local'
          ? ' <span class="tag mute">快照</span>' : '';
        bal = '<b>' + n + '</b>' + src;
      } else {
        bal = '<span style="color:var(--ink-3)">—</span>';
      }
      const ck = a.has_checkin
        ? '<b>+' + Number(a.checkin_credits).toLocaleString('zh-CN', { maximumFractionDigits: 0 }) + '</b>'
        : '<span style="color:var(--ink-3)">—</span>';
      return '<tr>' +
        '<td class="who"><div class="nm">' + esc(a.name || '未命名') + '</div>' +
        '<div class="id">' + esc(a.user_id || '') + '</div></td>' +
        '<td class="num">' + bal + '</td>' +
        '<td class="num">' + ck + '</td>' +
        '</tr>';
    }).join('');
    const live = arr.filter(a => a.checkin_source === 'live').length;
    const balLive = arr.filter(a => a.balance_source === 'live').length;
    $('tcNote').textContent = arr.length + ' 个账号 · 余额实时 ' + balLive + ' · 签到额度实时 ' + live;
    st.hidden = true;
  } catch (e) {
    st.hidden = false; st.className = 'state err'; st.textContent = e.message;
  }
}
if ($('btnTcRefresh')) $('btnTcRefresh').onclick = () => loadTraeCredits(false);

/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证（强制 byte
   模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性；券码为短文本，v1-5（26 字节起）绰绰有余。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);


  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  $('vcVeil').classList.add('on');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">🎟️</div>还没有抽到券</div>';
    $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note" style="color:var(--warn);margin-top:8px">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(e.message) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
  }
}
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => $('vcVeil').classList.remove('on');
$('btnVcRefresh').onclick = loadSchoolVouchers;

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）
$('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  // 停掉队列轮询：显式扫描 = 切到待办视图。否则在途队列的下一 tick 会把扫描
  // 结果冲掉重渲染回队列视图（服务端执行不受影响，只是不再实时回写本视图）。
  if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
  b.disabled = true; b.textContent = '扫描中…';
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    renderQueue(groupItems(d), null, '没有待办任务 🎉', '全部账号已无未完成任务（未解锁 / 需手动的任务也会一并列出）。Sequential 链条每日零点解锁下一环，可明天再来。');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '扫描待办'; }
};
$('btnRunQueue').onclick = async () => {
  const conc = Number($('qcConc').value) || 1;
  if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
  const b = $('btnRunQueue');
  b.disabled = true; b.textContent = '启动中…';
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) { toast(r.message || '没有待办任务', 'ok'); return; }
    lastQueueSeq = r.seq || 0;
    toast('队列已启动：' + r.total + ' 项（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '执行全部待办'; }
};
// 扫描结果 → 分组条目（无执行状态）
//
// 三类合起来 = 每个账号的全部未完成任务，保证不漏项：
//   growth      可自动化 → 待执行
//   growth_locked 有自动动作但上游未解锁（Sequential 链条每日零点解锁下一环）
//   manual      未完成但面板无自动动作（需真实世界行为 / 客户端才能点亮）
// 曾经只列 growth/growth_locked，manual 类任务在扫描结果里完全消失，用户会以为
// 「扫描不出来任务」——所以这里必须把 manual 也渲染出来。
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
    }
    for (const t of (a.growth_locked || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'locked', unlock: t.unlock_at });
    }
    for (const t of (a.manual || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'manual', message: t.task_desc || t.description || '' });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行', locked: '未解锁', manual: '需手动' };
const LOCKED_MSG = '上游尚未解锁（Sequential 链条每日零点解锁下一环，解锁后自动执行）';
const MANUAL_MSG = '需真实操作（关注公众号 / 真实捐款 / 客户端行为），网关无法代为完成';
function qrowHTML(it) {
  const isSchool = it.kind === 'school';
  const title = isSchool ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const dotCls = it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  const lk = it.status === 'locked' ? fmtUnlock(it.unlock) : '';
  const msg = it.status === 'locked'
    ? (lk ? '上游锁定至 ' + lk + '，到点由面板自动执行' : LOCKED_MSG)
    : it.status === 'manual'
      ? (it.message || MANUAL_MSG)
      : (it.message || '');
  return '<div class="qrow" title="' + esc(msg) + '">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">开学季</span>' : '') + (it.status === 'locked' ? '<span class="tag mute">' + (lk ? lk + ' 解锁' : '未解锁') + '</span>' : '') + (it.status === 'manual' ? '<span class="tag mute">需手动</span>' : '') + '</span>' +
    '<span class="prog">' + esc(it.prog || '') + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg">' + esc(msg) + '</span>' +
    '</div>';
}
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  const empty = $('tcEmpty'), list = $('qcList');
  if (!groups.length) {
    empty.style.display = '';
    if (emptyTitle) empty.querySelector('.t').textContent = emptyTitle;
    if (emptyDesc) empty.querySelector('.d').textContent = emptyDesc;
    list.innerHTML = '';
    $('qProg').hidden = true; $('qcSummary').textContent = '';
    return;
  }
  empty.style.display = 'none';
  let total = 0, lockedN = 0, manualN = 0;
  list.innerHTML = groups.map(g => {
    const runnable = g.rows.filter(r => r.status !== 'locked' && r.status !== 'manual').length;
    const locked = g.rows.filter(r => r.status === 'locked').length;
    const manual = g.rows.filter(r => r.status === 'manual').length;
    total += runnable; lockedN += locked; manualN += manual;
    const parts = [runnable + ' 项待办'];
    if (locked) parts.push(locked + ' 项未解锁');
    if (manual) parts.push(manual + ' 项需手动');
    const cnt = parts.join(' · ');
    return '<div class="qgroup"><header><span class="nm">' + esc(g.nick || g.uid.slice(0, 12)) + '</span><span class="cnt">' + cnt + '</span></header>' +
      g.rows.map(qrowHTML).join('') + '</div>';
  }).join('');
  const sum = [total + ' 项'];
  if (lockedN) sum.push(lockedN + ' 项未解锁');
  if (manualN) sum.push(manualN + ' 项需手动');
  $('qcSummary').textContent = sum.join(' · ');
  updateProgress(progress);
}
function updateProgress(q) {
  if (!q || !q.items) { $('qProg').hidden = true; return; }
  const total = q.items.length;
  const done = q.items.filter(it => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;
  $('qProg').hidden = false;
  $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  $('qProgText').textContent = (q.running ? '执行中 ' : '已结束 ') + done + ' / ' + total;
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code,
      prog: it.kind === 'school' ? '—' : '',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
function startQueuePolling() {
  if (queueTimer) clearInterval(queueTimer);
  queueTimer = setInterval(async () => {
    // 切号冻结期间不轮询队列：避免把用户正看着的进度视图冲掉。
    if (autoRefreshBlocked()) return;
    let q;
    try { q = await api('tasks/queue'); } catch (e) { return; }
    // 两个提前返回都必须**停表**，否则本页会每 3s 永久空轮询下去：
    //   · q.started=false —— 队列根本没在跑（已被别处结束），没有任何可等的
    //   · seq 变了 —— 那是**另一个会话**启动的新一轮队列，本页不该接管
    if (!q.started) { clearInterval(queueTimer); queueTimer = null; return; }
    // 只渲染本页启动过的那轮队列（刷新页面后不再接管旧队列）。
    if (lastQueueSeq && q.seq !== lastQueueSeq) { clearInterval(queueTimer); queueTimer = null; return; }
    if (q.running) {
      renderQueue(groupsFromQueue(q.items || []), q);
      return;
    }
    // 结束：终态只渲染这一次，随即停表。此后残留的 items（running=false）不再
    // 回写视图——曾把用户刚点开的「扫描待办」结果在下一个 tick 冲掉。
    renderQueue(groupsFromQueue(q.items || []), q);
    clearInterval(queueTimer); queueTimer = null;
    toast('任务队列执行结束', 'ok');
  }, 3000);
}
// reattachQueueView 切回任务中心视图时恢复队列进度：仅当本页启动的队列仍在
// 执行才重新开轮询（残留态/别页队列不接管——视图不被旧结果冲掉）。
function reattachQueueView() {
  // 全程异步：go() 在顶层（app.js ~143 行）被调用时，本文件下方 let/const
  //（queueTimer/lastQueueSeq 等）尚未初始化——同步读取即 TDZ ReferenceError
  // 使整个脚本中断。await 之后才碰它们（旧 pollQueueOnce 正是靠开头的 await
  // 侥幸安全）。queueTimer 的"已在跑"判定也挪到 await 后，语义不变。
  (async () => {
    try {
      const q = await api('tasks/queue');
      if (queueTimer) return; // 轮询已在跑（跨视图不中断）
      if (q.started && q.running && (!lastQueueSeq || q.seq === lastQueueSeq)) startQueuePolling();
    } catch (e) { /* 静默 */ }
  })();
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */

function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }
function trimFixed(s) {
  if (!String(s).includes('.')) return String(s);
  return String(s).replace(/0+$/, '').replace(/\.$/, '');
}
function fmtCredit(n) {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return '—';
  return trimFixed(v.toFixed(2));
}
function fmtCreditRatio(v, samples, tokens) {
  if (!samples || !tokens) return '—';
  const n = Number(v || 0);
  if (!Number.isFinite(n)) return '—';
  return trimFixed(n.toFixed(4)) + ' / 1M';
}
function fmtModelRate(rate) {
  const s = String(rate || '').trim();
  return s ? 'x' + s : '—';
}

function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}

/* usKpi 用量页的指标卡。比账号池的 .stat 多两样：语义色轨（cls）与副标题（sub，
   放"占比 / 均速率"这类解释性数字）；bar 是卡片内的构成条 HTML，只有需要时才传。 */
function usKpi(v, k, cls, sub, bar) {
  return '<div class="kpi ' + (cls || '') + '">' +
    '<div class="k">' + esc(k) + '</div>' +
    '<div class="v">' + esc(v) + '</div>' +
    (bar || '') +
    (sub ? '<div class="s">' + esc(sub) + '</div>' : '') +
    '</div>';
}

/* usMixBar prompt/completion 占比条。宽度按百分比而不是固定像素：表列宽随窗口变化，
   像素宽度在窄屏会溢出、宽屏又显得没信息。 */
function usMixBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-mix" title="prompt ' + pp.toFixed(1) + '% · completion ' + pc.toFixed(1) + '%">' +
    '<i class="p" style="width:' + pp.toFixed(2) + '%"></i>' +
    '<i class="c" style="width:' + pc.toFixed(2) + '%"></i>' +
    '</span>';
}

/* usPct 占比文案（0 值不显示 "0.0%"，直接 —，避免一行全是零）。 */
function usPct(part, total) {
  const t = Number(total || 0);
  if (!t) return '—';
  return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制延迟/速率两列；列开关显式传入，避免调用方改动后与表头错列。 */
function usRow(name, sub, a, mid, withPerf) {
  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>' + esc(name) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid || '') +
    '<td class="num">' + fmtTok(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span style="color:var(--warn)">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.total_tokens) +
      usMixBar(a.prompt_tokens, a.completion_tokens, a.total_tokens) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}

/* ── 用量明细：三个维度共用一张表 + 页内切换 ──────────────────────────
   账号 / 模型 / 域三张表此前各自占一个 box，页面纵向拉得很长且表头结构几乎一样。
   现在合成一个 box：表头由维度定义生成，行渲染复用 usRow，切换零请求。 */
const US_DIMS = {
  account: { key: 'by_account', title: '账号', withRealm: true, withPerf: true, span: 10 },
  model: { key: 'by_model', title: '模型', withRealm: false, withPerf: false, span: 7 },
  realm: { key: 'by_realm', title: 'realm', withRealm: false, withPerf: false, span: 7 },
};

// usSortRows 按当前排序字段降序（默认合计 Token，最大者最相关）。
function usSortRows(rows, sort) {
  const out = rows.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  const val = a => sort === 'requests' ? num(a.requests)
    : sort === 'errors' ? num(a.errors)
    : sort === 'latency' ? num(a.avg_latency_ms)
    : num(a.total_tokens);
  out.sort((a, b) => val(b) - val(a));
  return out;
}

function usDimHead(dim) {
  const m = US_DIMS[dim] || US_DIMS.account;
  return '<tr><th class="mark" aria-hidden="true"></th><th>' + esc(m.title) + '</th>' +
    (m.withRealm ? '<th>域</th>' : '') +
    '<th class="num">请求</th><th class="num">失败</th>' +
    '<th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计</th>' +
    (m.withPerf ? '<th class="num">均延迟</th><th class="num">均速率</th>' : '') +
    '</tr>';
}

/* usTabsHtml 维度切换按钮（带条数徽标）。整段 innerHTML 重写而不是逐个改 class：
   容器的 click 监听是委托式的，换掉子节点不会丢事件，代码也更短。 */
function usTabsHtml(dims, active, counts) {
  return dims.map(([k, label]) => {
    const n = counts ? counts[k] : null;
    return '<button data-dim="' + k + '"' + (k === active ? ' class="on"' : '') + '>' +
      esc(label) + (n == null ? '' : '<span class="cnt">' + n + '</span>') + '</button>';
  }).join('');
}

const US_DIM_TABS = [['account', '按账号'], ['model', '按模型'], ['realm', '按域']];

function renderUsageDim() {
  const d = usageData || {};
  const m = US_DIMS[usDim] || US_DIMS.account;
  const rows = usSortRows(d[m.key] || [], usSort);
  $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, {
    account: (d.by_account || []).length,
    model: (d.by_model || []).length,
    realm: (d.by_realm || []).length,
  });
  $('usDimHead').innerHTML = usDimHead(usDim);
  $('usDimBody').innerHTML = rows.map(x =>
    usRow(usDim === 'account' ? String(x.key || '').slice(0, 8) : x.key,
      usDim === 'account' ? (x.extra || '') : '',
      x,
      usDim === 'account' ? '<td class="num">' + esc(x.realm || '') + '</td>' : '',
      m.withPerf)
  ).join('') || '<tr><td colspan="' + m.span + '" class="empty">暂无数据</td></tr>';
  $('usDimNote').textContent = rows.length + ' 行 · 请求数含失败尝试';
}

/* 积分扣除：按账号 / 按模型两个维度共用一张表（同上，两个 box 合成一个）。 */
function usCreditHead(dim) {
  return dim === 'model'
    ? '<tr><th class="mark" aria-hidden="true"></th><th>模型</th><th>积分倍率</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>'
    : '<tr><th class="mark" aria-hidden="true"></th><th>账号</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>';
}

/* 缓存命中率（issue #92）：颜色即健康度——≥90% 绿 / 80–90% 黄 / <80% 红，
   样本不足灰。title 带命中/未命中绝对量，供逐项核对。 */
function cacheRateCell(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '<span class="muted">—</span>';
  const pct = h / total * 100;
  const color = pct >= 90 ? 'var(--ok)' : (pct >= 80 ? 'var(--warn)' : 'var(--bad)');
  const txt = trimFixed(pct.toFixed(1)) + '%';
  return '<span style="color:' + color + '" title="命中 ' + fmtTok(h) + ' / 未命中 ' + fmtTok(m) + ' tok">' + txt + '</span>';
}

function renderCreditDim() {
  const d = usageData || {};
  $('usCreditHead').innerHTML = usCreditHead(usCreditDim);
  const empty = '暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。';
  if (usCreditDim === 'model') {
    const models = d.credit_by_model || [];
    $('usCreditBody').innerHTML = models.map(row =>
      '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + esc(row.key || '—') + '</td>' +
        '<td>' + esc(fmtModelRate(row.rate)) + '</td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
      '</tr>'
    ).join('') || '<tr><td colspan="8" class="empty">' + empty + '</td></tr>';
  } else {
    const accounts = d.credit_by_account || [];
    $('usCreditBody').innerHTML = accounts.map(row => {
      const uid = String(row.key || '');
      const account = row.nickname || uid.slice(0, 8) || '—';
      return '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + esc(account) + '<div class="note">' + esc(row.realm || '') + ' · ' + esc(uid.slice(0, 8)) + '</div></td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
        '</tr>';
    }).join('') || '<tr><td colspan="7" class="empty">' + empty + '</td></tr>';
  }
  $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, {
    account: (d.credit_by_account || []).length,
    model: (d.credit_by_model || []).length,
  });
}

function renderUsage(d) {
  usageData = d || {};
  const t = usageData.totals || {};
  const total = Number(t.total_tokens || 0);
  const pt = Number(t.prompt_tokens || 0);
  const ct = Number(t.completion_tokens || 0);
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const okRate = reqs ? (reqs - errs) / reqs * 100 : null;
  // 构成条要的是合法 CSS 宽度，usPct 在无样本时返回 "—"，不能直接拼进 style。
  const pctW = (part) => total ? Math.max(0, Math.min(100, Number(part || 0) / total * 100)).toFixed(2) + '%' : '0%';
  // 六张卡：主指标用强调色，completion 用成功色（与图表里的绿柱呼应），
  // 失败/延迟只在有值时上语义色——全绿全黄的仪表盘等于没有重点。
  $('usStats').innerHTML =
    usKpi(fmtTok(reqs), '请求数', 'c-accent',
      errs ? '其中失败 ' + errs + ' 次' : '全部成功') +
    usKpi(fmtTok(total), '总 token', 'c-accent',
      'prompt ' + usPct(pt, total) + ' · completion ' + usPct(ct, total),
      '<div class="kbar"><i style="width:' + pctW(pt) + ';background:var(--accent)"></i>' +
      '<i style="width:' + pctW(ct) + ';background:var(--ok)"></i></div>') +
    usKpi(fmtTok(pt), 'prompt', 'c-soft', '占比 ' + usPct(pt, total)) +
    usKpi(fmtTok(ct), 'completion', 'c-ok', '占比 ' + usPct(ct, total)) +
    usKpi(String(errs), '失败尝试', errs ? 'c-warn' : 'c-mute',
      okRate == null ? '—' : (errs ? '成功率 ' + okRate.toFixed(1) + '%' : '成功率 100%')) +
    usKpi(fmtMs(t.avg_latency_ms), '平均延迟', 'c-soft',
      t.avg_tokens_per_second ? '吐字 ' + fmtRate(t.avg_tokens_per_second) : '无速率样本');

  // 卡片、明细表与时序图全部按所选窗口统计（切窗口数字随之变化）；
  // 「全部历史」含 90 天前折叠出的日桶。这里标注当前口径与数据起点。
  const winLabel = trangeLabel('usRange');
  // 服务端回显的实际区间优先（自定义区间下它就是权威口径）；滚动窗口没有回显，
  // 用控件自己的标签。
  const rangeEcho = usageData.window_from
    ? String(usageData.window_from).replace('T', ' ').slice(0, 16) +
      (usageData.window_to ? ' → ' + String(usageData.window_to).replace('T', ' ').slice(0, 16) : ' → 现在')
    : '';
  const note = (rangeEcho || winLabel ? (rangeEcho || winLabel) + ' · ' : '') +
    (usageData.buckets || 0) + ' 个分桶' +
    (usageData.since ? ' · 数据自 ' + usageData.since.replace('T', ' ') : '') +
    (usageData.file_bytes ? ' · 文件 ' + (usageData.file_bytes / 1024).toFixed(1) + ' KB' : '');
  $('usNote').textContent = note;
  $('usNote').title = note; // 窄屏单行截断时靠悬停看全

  // 积分扣除的四张卡片与说明。
  $('usCreditStats').innerHTML =
    usKpi(fmtCredit(t.credits), '扣除积分', 'c-accent', '按上游 usage.credit 累计') +
    usKpi(fmtTok(t.credit_tokens), '匹配 Token', 'c-mute', '与积分同时观测到的 Token') +
    usKpi(fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens),
      '平均积分 / 1M Token', 'c-ok', '越低越划算') +
    usKpi(String(t.credit_samples || 0), '有效积分样本', 'c-mute', '缺字段的历史不参与折算') +
    usKpi(cacheRateText(t.cache_hit_tokens, t.cache_miss_tokens), '缓存命中率', 'c-mute',
      '上游前缀缓存命中 / (命中+未命中)；低命中意味着费用数倍放大');
  $('usCreditNote').textContent =
    (usageData.credit_by_account || []).length + ' 个账号 · ' +
    (usageData.credit_by_model || []).length + ' 个模型倍率分组 · 仅统计与积分同时观测到的 Token';

  renderUsageDim();
  renderCreditDim();
  renderUsageChart(usageData.series || []);
}

// 维度切换 / 排序控件。
if ($('usDimTabs')) $('usDimTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usDim = b.dataset.dim;
  renderUsageDim();
});
if ($('usCreditTabs')) $('usCreditTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usCreditDim = b.dataset.dim;
  renderCreditDim();
});
if ($('usSort')) $('usSort').onchange = () => {
  usSort = $('usSort').value;
  renderUsageDim();
};

/* renderUsageChart 画堆叠柱状图。
 *
 * x 轴是**真实时间轴**，不是按序号等距。这一点很重要：数据里存在 1 小时的
 * 间隔，也存在 6~8 小时的断档（没请求的时段不产生桶），等距排布会把 8 小时
 * 画得和 1 小时一样宽，让「什么时候用的」完全失真。
 *
 * 另外不再用 preserveAspectRatio="none"：那会把 viewBox 横向拉伸到容器宽度，
 * 柱子和文字都变形。改为固定比例、按容器宽度自适应高度。
 *
 * viewBox 取 1200×200（原 760×180）：SVG 以 width:100% 渲染，高宽比决定实际
 * 高度——旧比例在 1500px 宽的主区里会撑到 ~355px，只有一两根柱子时整块几乎是
 * 空白。宽 viewBox 把同宽度下的高度压到 ~250px，与下方表格的视觉重量相当。
 *
 * 时间轴用本地时间解析（后端返回的就是本地时区），day 点按当天 00:00 参与定位，
 * 与 hour 点在同一个连续轴上——日桶本来就是他那天所有小时的聚合。
 */

/* parsePointTime 把后端的 t 解析成毫秒时间戳。 */
function parsePointTime(p) {
  // hour: "2026-09-16T13"  day: "2026-09-16"
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

/* fmtTokTimeLabel 时间桶的短标签，与 x 轴刻度同一口径（日桶 MM-DD，小时桶 HH:00）。 */
function fmtTokTimeLabel(p) {
  const d = new Date(p.t);
  return p.scope === 'day'
    ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
    : String(d.getHours()).padStart(2, '0') + ':00';
}

function renderUsageChart(series) {
  const host = $('usChart');

  // 丢掉时间解析不出来的点，而不是让 NaN 传染整张图。
  const pts = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    const pt = Number(p.prompt_tokens || 0);
    const ct = Number(p.completion_tokens || 0);
    pts.push({ t, scope: p.scope, raw: p.t, pt, ct, tt: Number(p.total_tokens || 0) || (pt + ct),
               req: p.requests || 0 });
  }
  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    $('usChartNote').textContent = '—';
    return;
  }

  const W = 1200, H = 200, PL = 58, PR = 14, PT = 18, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB;

  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);

  const max = Math.max(1, ...pts.map(p => p.tt));
  const peak = pts.reduce((a, b) => (b.tt > a.tt ? b : a), pts[0]);
  const avg = pts.reduce((s, p) => s + p.tt, 0) / pts.length;
  $('usChartNote').textContent =
    pts.length + ' 个点 · 峰值 ' + fmtTok(peak.tt) + ' @ ' + fmtTokTimeLabel(peak) +
    ' · 均值 ' + fmtTok(avg);

  // 柱宽取「最小真实间隔」的 70%，并夹在合理区间内——窗口拉到 30 天时柱子会
  // 变细，但不会细到看不见。
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(2, Math.min(30, slot * 0.7));

  // 首尾各让出半个柱宽：否则第一个点和最后一个点的柱子会各有一半跑到绘图区外
  // （末点柱子贴着卡片右边缘被切掉），刻度仍用同一个 xOf，标签与柱子始终对齐。
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
  const yOf = v => PT + ih - ih * (v / max);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" ' +
            'preserveAspectRatio="xMidYMid meet">';

  // 柱体渐变：顶部实、底部略透，堆叠时两段仍能一眼分清（纯色块并排会糊成一片）。
  // 注意 stop-color 必须走 style 而不是 presentation 属性——Blink/WebKit 不解析
  // 属性里的 var()，写成 stop-color="var(--accent)" 会整条渐变失效（柱子全透明）。
  out += '<defs>' +
    '<linearGradient id="usGradP" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--accent);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--accent);stop-opacity:.6"/></linearGradient>' +
    '<linearGradient id="usGradC" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--ok);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--ok);stop-opacity:.6"/></linearGradient>' +
    '</defs>';

  // y 轴网格 + 刻度
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
  }

  // 均值参考线：一眼看出"这根是不是异常高"，比只给刻度省心。
  // 标签放左侧：右侧常被峰值柱占用（峰值柱往往就是最后一根），贴左不会被压住。
  if (avg > 0 && avg < max) {
    const y = yOf(avg);
    out += '<line class="avg" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk-avg" x="' + (PL + 5) + '" y="' + (y - 4).toFixed(1) +
           '" text-anchor="start">均值 ' + fmtTok(avg) + '</text>';
  }

  // 柱子
  const yBase = PT + ih;
  for (const p of pts) {
    const x = xOf(p.t) - bw / 2;
    const hTot = ih * (p.tt / max);
    const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
    const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
    // 每根柱子包一个 <g>，把 <title> 放进去。
    //
    // 为什么必须包裹：SVG 里 <title> 描述的是它的**父元素**。此前 <title> 是
    // <rect> 的兄弟节点（rect 自闭合，无法包含子节点），于是全部平铺在 <svg>
    // 根下 —— 整张图只有一个 tooltip（浏览器取第一个），悬停任何柱子都显示同一
    // 份数据（issue #128）。包进 <g> 后 tooltip 跟随该柱，且堆叠的两段
    //（prompt + completion）共用同一个提示。
    out += '<g><title>' + esc(p.raw) + '  ' + fmtTok(p.pt) + ' prompt / ' +
           fmtTok(p.ct) + ' completion / ' + p.req + ' 次</title>';
    // 圆角只给堆叠顶端（贴轴的底边保持方角，柱子才像"立"在基线上）。
    // 类名用 usbar 而不是 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
    // height 是 rect 的 CSS 几何属性，同名类会把每根柱子压成 3px 高（踩过）。
    if (hP > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hP.toFixed(2) +
      '" fill="url(#usGradP)"' + (hC > 0 ? '' : ' rx="1.5"') + '/>';
    if (hC > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP - hC).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hC.toFixed(2) +
      '" fill="url(#usGradC)" rx="1.5"/>';
    out += '</g>';
  }

  // 峰值标注：柱子够窄时文字压在柱顶，够宽时贴右侧避免和柱体重叠。
  {
    const px = xOf(peak.t);
    const py = yOf(peak.tt);
    const anchor = px > W - PR - 90 ? 'end' : 'middle';
    out += '<text class="tk-peak" x="' + Math.max(PL, Math.min(W - PR, px)).toFixed(1) +
           '" y="' + Math.max(10, py - 5).toFixed(1) + '" text-anchor="' + anchor + '">' +
           '峰值 ' + fmtTok(peak.tt) + '</text>';
  }

  // x 轴基线画在柱子之后，避免压在柱底
  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
         '" y2="' + yBase + '"/>';

  // x 轴刻度：按真实时间等距取 6 个位置，取该位置**最近的实际柱子**做标签，
  // 所以标签永远落在有数据的点上，不会指到空档里。
  const TICKS = Math.min(6, pts.length);
  const usedLabel = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) { best = d; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = pts[bi];
    // 首尾标签靠边对齐，避免被裁掉
    const cx = xOf(p.t);
    const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
           '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' + esc(fmtTokTimeLabel(p)) + '</text>';
  }

  // 跨天时补一条日期分隔线，让「日界」在长窗口里可见
  let prevDay = null;
  for (const p of pts) {
    const d = new Date(p.t).getDate();
    if (prevDay !== null && d !== prevDay) {
      const x = xOf(p.t).toFixed(1);
      out += '<line class="gl" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
             (PT + ih) + '" style="opacity:.45"/>';
    }
    prevDay = d;
  }

  out += '</svg>';
  host.innerHTML = out;
}

function fmtTokTip(v) { return fmtTok(v); }

let usageRateWarmAt = 0;
async function warmUsageModelRates() {
  if (Date.now() - usageRateWarmAt < 10 * 60 * 1000) return;
  try {
    await api('models');
  } catch (e) {
    // 倍率回填是可选增强；失败不阻塞用量统计，10 分钟后再试。
  }
  usageRateWarmAt = Date.now();
}

async function loadUsage() {
  const q = trangeQuery('usRange', true);
  try {
    await warmUsageModelRates();
    const d = await api('usage?' + q.toString());
    renderUsage(d);
  } catch (e) {
    // 失败时三块都要清干净：只改图表会留下上一次窗口的数字，看起来像"刷新成功"。
    usageData = null;
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(e.message) + '</div>';
    $('usChartNote').textContent = '—';
    $('usStats').innerHTML = '';
    $('usCreditStats').innerHTML = '';
    $('usNote').textContent = '—';
    $('usCreditNote').textContent = '—';
    $('usDimNote').textContent = '—';
    $('usDimHead').innerHTML = '';
    $('usCreditHead').innerHTML = '';
    $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, null);
    $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, null);
    $('usDimBody').innerHTML = '<tr><td colspan="10" class="empty">读取用量失败</td></tr>';
    $('usCreditBody').innerHTML = '<tr><td colspan="7" class="empty">读取用量失败</td></tr>';
  }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
// 时间范围控件绑定：任何改动（预设切换 / 自定义起止）都重新拉一次用量。
if ($('usRange')) trangeBind('usRange', loadUsage);

/* ── 官方用量（上游真实调用）─────────────────────────────────────────
   面板自身的 usage 只统计**经过面板 API 网关**的请求；用户用官方客户端直连上游时
   恒为 0。这一块直接拉上游官方逐请求用量，回答「我到底用了多少」。 */
async function loadUsageOfficial() {
  const hours = ($('usOffWindow') && $('usOffWindow').value) || 72;
  const btn = $('btnUsageOfficial');
  if (btn) { btn.disabled = true; btn.textContent = '读取中…'; }
  try {
    const d = await api('usage/official?hours=' + encodeURIComponent(hours));
    renderUsageOfficial(d);
  } catch (e) {
    if ($('usOffChart')) $('usOffChart').innerHTML = '<div class="us-empty">读取官方用量失败：' + esc(e.message) + '</div>';
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = '刷新'; }
  }
}

function renderUsageOfficial(d) {
  const t = d.totals || {};
  $('usOffStats').innerHTML =
    usStat(fmtTok(t.requests), '真实请求数') +
    usStat(fmtCredit(t.credits), '扣除积分') +
    usStat(String(t.active || 0) + ' / ' + String(t.accounts || 0), '有调用账号') +
    usStat(String(t.active_days || 0), '活跃天数');
  const win = ($('usOffWindow') && $('usOffWindow').selectedOptions[0]) ?
    $('usOffWindow').selectedOptions[0].textContent.trim() : '';
  const errs = d.errors || [];
  $('usOffNote').textContent = (win ? win + ' · ' : '') +
    (d.start ? '自 ' + String(d.start).slice(0, 16).replace('T', ' ') : '') +
    (errs.length ? ' · ' + errs.length + ' 个账号读取失败' : '');

  $('usOffAccBody').innerHTML = (d.by_account || []).map(x => {
    const uid = String(x.uid || '');
    return '<tr><td class="mark" aria-hidden="true"></td>' +
      '<td>' + esc(x.nickname || uid.slice(0, 8)) + '<div class="note">' + esc(uid.slice(0, 8)) + '</div></td>' +
      '<td class="num">' + esc(x.realm || '') + (x.truncated ? ' <span class="tag mute">截断</span>' : '') + '</td>' +
      '<td class="num">' + fmtTok(x.requests) + '</td>' +
      '<td class="num">' + fmtCredit(x.credits) + '</td></tr>';
  }).join('') || '<tr><td colspan="5" class="empty">窗口内无调用记录</td></tr>';

  $('usOffModelBody').innerHTML = (d.by_model || []).map(x =>
    '<tr><td class="mark" aria-hidden="true"></td>' +
    '<td>' + esc(x.model || '—') + '</td>' +
    '<td class="num">' + fmtTok(x.requests) + '</td>' +
    '<td class="num">' + fmtCredit(x.credits) + '</td></tr>'
  ).join('') || '<tr><td colspan="4" class="empty">窗口内无调用记录</td></tr>';

  renderOfficialChart(d.series || []);
}

/* 每日请求柱状图（内联 SVG，hover 显示当日请求数与积分）。 */
function renderOfficialChart(series) {
  const host = $('usOffChart');
  if (!host) return;
  if (!series.length) { host.innerHTML = '<div class="us-empty">窗口内无调用记录</div>'; return; }
  const W = 640, H = 150, padL = 40, padR = 10, padT = 12, padB = 24;
  const iw = W - padL - padR, ih = H - padT - padB, n = series.length;
  const maxR = Math.max.apply(null, series.map(s => Number(s.requests) || 0).concat([1]));
  const step = Math.max(1, Math.ceil(n / 14));
  let bars = '', labels = '';
  series.forEach((s, i) => {
    const bw = Math.max(3, iw / n - 4);
    const x = padL + iw * (i + 0.5) / n - bw / 2;
    const h = Math.max(1, Math.round((Number(s.requests) || 0) / maxR * ih));
    const y = padT + ih - h;
    const tip = esc(s.t) + '：' + fmtTok(s.requests) + ' 次 / ' + fmtCredit(s.credits) + ' 积分';
    bars += '<rect x="' + x.toFixed(1) + '" y="' + y.toFixed(1) + '" width="' + bw.toFixed(1) +
      '" height="' + h + '" rx="2" fill="var(--accent)" opacity="0.85"><title>' + tip + '</title></rect>';
    if (i % step === 0 || i === n - 1) {
      labels += '<text class="tk" x="' + (x + bw / 2).toFixed(1) + '" y="' + (H - 7) +
        '" text-anchor="middle">' + esc(String(s.t || '').slice(5)) + '</text>';
    }
  });
  host.innerHTML = '<svg viewBox="0 0 ' + W + ' ' + H + '">' +
    '<line class="gl" x1="' + padL + '" y1="' + padT + '" x2="' + (W - padR) + '" y2="' + padT + '"/>' +
    '<line class="ax" x1="' + padL + '" y1="' + (padT + ih) + '" x2="' + (W - padR) + '" y2="' + (padT + ih) + '"/>' +
    '<text class="tk" x="' + (padL - 5) + '" y="' + (padT + 9) + '" text-anchor="end">' + fmtTok(maxR) + '</text>' +
    bars + labels + '</svg>';
}

if ($('btnUsageOfficial')) $('btnUsageOfficial').onclick = loadUsageOfficial;
if ($('usOffWindow')) $('usOffWindow').onchange = loadUsageOfficial;

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个
   包名一个稳定配色，跨账号对比时同色即同类。 */

const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
                   '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];
const PK_ACCOUNT_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6',
                           '#e2607a', '#20a4a4', '#8fbf3f', '#d4785c',
                           '#7c83db', '#c48a2f', '#b45f8c', '#5aa9e6'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

// pkAccountColorMap 按 UID 稳定分配颜色：排序后分配，账号刷新/重排不会换色。
function pkAccountColorMap(list) {
  const uids = (list || [])
    .filter(a => a && !a.error && a.uid)
    .map(a => String(a.uid))
    .sort();
  const colors = new Map();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
  return colors;
}

/* pkBySource 把包按名称归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    // 分组键用 code + name，而不是只 name：上游给「首登赠送」和普通活动包用了
    // **同一个 PackageName 和同一个 PackageCode**，只按 name 会把两类混成一类，
    // 那正是当初「两个号为何差 1500」看不出来的原因。这里至少把 code 带进键里，
    // 并在卡片上显示最早的发放时间。
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

const PK_DEFAULT_DETAIL_LIMIT = 5;

function pkDetailLimitValue(raw) {
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : PK_DEFAULT_DETAIL_LIMIT;
}

function pkDetailLimit(cfg) {
  return pkDetailLimitValue(cfg && cfg.panel && cfg.panel.package_detail_limit);
}

const PK_DAY_MS = 24 * 3600 * 1000;

function pkExpiryMs(p) {
  const raw = Number(p && p.expires_at);
  if (Number.isFinite(raw) && raw > 0) return raw;
  const text = String((p && p.end_time) || '').trim();
  if (!text) return null;
  let iso = text.includes('T') ? text : text.replace(' ', 'T');
  if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00';
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

// pkDetailCompare 只服务单账号逐包明细：正余额包先按到期时间挑选默认展示项，
// 其余正余额包与已用完包分别折叠；同一到期时间按面额降序。主键跟随视图排序
// 模式（pkSortMode，声明在本区块末尾的绑定块）：end_asc 到期升序、size_desc
// 面额降序（同面额按到期升序）。typeof 守卫：前端 harness 的区域切片求值里
// 没有该全局，回落 end_asc（= 上游原有语义，切片测试的期望序不受影响）。
function pkDetailCompare(a, b) {
  const sizeOf = p => {
    const n = Number(p && p.size);
    return Number.isFinite(n) ? n : 0;
  };
  const mode = (typeof pkSortMode === 'string' && pkSortMode) || 'end_asc';
  if (mode === 'size_desc') {
    const d = sizeOf(b) - sizeOf(a);
    if (d !== 0) return d;
  }
  const ea = pkExpiryMs(a), eb = pkExpiryMs(b);
  if (ea == null && eb != null) return 1;
  if (ea != null && eb == null) return -1;
  if (ea != null && eb != null && ea !== eb) return ea - eb;
  return sizeOf(b) - sizeOf(a);
}

function pkDetailGroups(packs, limit) {
  const active = [], used = [];
  let usedSize = 0, restSize = 0, restRemain = 0;
  for (const p of packs || []) {
    const remain = Number(p && p.remain);
    if (remain > 0) {
      active.push(p);
      continue;
    }
    used.push(p);
    const size = Number(p && p.size);
    if (Number.isFinite(size)) usedSize += size;
  }
  active.sort(pkDetailCompare);
  used.sort(pkDetailCompare);
  const visible = active.slice(0, pkDetailLimitValue(limit));
  const rest = active.slice(visible.length);
  for (const p of rest) {
    const size = Number(p && p.size);
    if (Number.isFinite(size)) restSize += size;
    const remain = Number(p && p.remain);
    if (Number.isFinite(remain)) restRemain += remain;
  }
  return { visible, rest, used, restSize, restRemain, usedSize };
}

function pkCreditOpacity(days) {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

function pkExpiryText(expiresAt) {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}

function pkExpiryDateTime(expiresAt) {
  if (!expiresAt) return '—';
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai', hour12: false,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  });
}

function pkAccountSegments(a, now) {
  let balance = Math.max(0, Number(a.remain || 0));
  const out = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

// pkDayKey 毫秒时间戳 → 北京时间的 YYYY-MM-DD（到期日归并的键）。
function pkDayKey(ms) {
  const parts = new Intl.DateTimeFormat('zh-CN', {
    timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit',
  }).formatToParts(new Date(ms));
  const get = t => (parts.find(p => p.type === t) || {}).value || '';
  return get('year') + '-' + get('month') + '-' + get('day');
}

// summarizeCreditDays 按**到期日**归并（而不是按「剩余天数」）：同一天过期的一批积分
// 合成一行，行序按到期日**从近到远**。按天数分组会把 17/18/19/21/23 天各散一行，
// 看着碎且不知道「哪天要过期多少」——按日期归并后一眼看清。
function summarizeCreditDays(list, now) {
  const buckets = new Map();
  let unavailable = 0;
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) {
      unavailable++;
      continue;
    }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null || segment.expiresAt == null) continue;
      const key = pkDayKey(segment.expiresAt);
      let row = buckets.get(key);
      if (!row) {
        row = { key, expiresAt: segment.expiresAt, credits: 0, segments: [] };
        buckets.set(key, row);
      }
      row.credits += segment.amount;
      row.segments.push(segment);
      if (segment.expiresAt < row.expiresAt) row.expiresAt = segment.expiresAt;
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.expiresAt - b.expiresAt);
  for (const row of rows) {
    row.days = Math.max(0, Math.ceil((row.expiresAt - now) / PK_DAY_MS));
    row.segments.sort((a, b) =>
      (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
      a.accountName.localeCompare(b.accountName) ||
      a.source.localeCompare(b.source));
  }
  return { rows, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
  const summary = summarizeCreditDays(list, now);
  const colors = pkAccountColorMap(list);
  const rows = summary.rows.map(row => {
    const total = row.credits || 1;
    const nodes = row.segments.map(segment => {
      const color = colors.get(segment.uid) || 'var(--accent)';
      const title = segment.source + '\n' + fmtTok(segment.amount) + ' 积分\n到期时间 ' +
        pkExpiryDateTime(segment.expiresAt) + '（' + pkExpiryText(segment.expiresAt) + '）\n' +
        segment.accountName;
      return '<span class="pk-expiry-seg" style="--seg-color:' + color +
        ';opacity:' + pkCreditOpacity(segment.days).toFixed(5) +
        ';flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
        ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
    }).join('');
    const when = row.days <= 0 ? '今天 / 已到期' : row.days + ' 天后';
    const soonCls = row.days <= 7 ? ' class="soon"' : '';
    return '<div class="pk-expiry-row" title="' + esc(pkExpiryDateTime(row.expiresAt)) + ' 到期（' +
      esc(pkExpiryText(row.expiresAt)) + '）">' +
      '<span>' + esc(row.key.slice(5)) + '<em' + soonCls + '>' + esc(when) + '</em></span>' +
      '<div class="pk-expiry-track">' + nodes + '</div>' +
      '<b>' + esc(fmtTok(row.credits)) + '</b></div>';
  }).join('');
  const first = summary.rows[0];
  const soonTxt = first
    ? '最早 ' + first.key.slice(5) + ' 到期 · ' + fmtTok(first.credits) + ' 积分（' +
      (first.days <= 0 ? '已到期' : first.days + ' 天后') + '）'
    : '暂无可汇总积分';
  const foot = soonTxt + ' · ' + summary.accountCount + ' 个账号' +
    (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');
  const legend = (list || []).filter(a =>
    a && !a.error && a.uid && pkAccountSegments(a, now).some(s => s.days != null)
  ).map(a => '<span><i style="background:' + (colors.get(String(a.uid)) || 'var(--accent)') +
    '"></i>' + esc(a.nickname || String(a.uid).slice(0, 8)) + '</span>').join('');
  const hdr = '<div class="pk-expiry-hdr"><span>到期日</span><span style="text-align:center">各账号该日到期的剩余积分</span><b>过期积分</b></div>';
  $('pkExpiry').innerHTML = (rows
    ? hdr + '<div class="pk-expiry-chart">' + rows + '</div>'
    : '<div class="pk-expiry-empty">暂无可汇总积分</div>') +
    (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
    '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

function renderPackages(d, detailLimit) {
  const list = (d.accounts || []);
  const now = Date.now();
  const expiryColors = pkAccountColorMap(list);
  renderExpiryDistribution(list, now);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    return;
  }

  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  names.sort((x, y) => {
    const sz = n => Math.max(...list.map(a => {
      const f = pkBySource(a.packages || []).find(s => s.key === n);
      return f ? f.size : 0;
    }));
    return sz(y) - sz(x);
  });
  const colorOf = n => pkColor(names.indexOf(n));
  // 键 → 展示名，供卡片与明细表共用（同一来源必然同色同名）。
  const labelOf = {};
  for (const a of list) for (const s of pkBySource(a.packages || [])) labelOf[s.key] = s;

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc((a.nickname || a.uid.slice(0, 8))) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const srcs = pkBySource(a.packages || []);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
      colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
    ).join('');
    const legend = srcs.map(s =>
      '<span><i style="background:' + colorOf(s.key) + '"></i>' +
      esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
      (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>'
    ).join('');
    const expiry = pkAccountSegments(a, now);
    const expiryTotal = Math.max(1, expiry.reduce((sum, s) => sum + s.amount, 0));
    const expiryColor = expiryColors.get(String(a.uid)) || 'var(--accent)';
    const expiryBar = expiry.length ? '<div class="expirybar" role="img" aria-label="积分到期分布">' +
      expiry.map(s => {
        const title = s.source + '\n' + fmtTok(s.amount) + ' 积分\n到期时间 ' +
          pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）';
        return '<i style="background:' + expiryColor +
          ';opacity:' + pkCreditOpacity(s.days).toFixed(5) +
          ';flex:' + Math.max(0.008, s.amount / expiryTotal).toFixed(4) +
          ' 1 0" title="' + esc(title) + '"></i>';
      }).join('') + '</div>' : '';
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtTok(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
      ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      expiryBar +
      '<div class="pk-legend">' + legend + '</div>' +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 实时查询上游';

  // 逐包明细：每个账号一个表，排序规则由视图顶部的选择器决定（默认到期近的在前）
  $('pkDetail').innerHTML = list.map(a => {
    if (a.error) return '';
    const groups = pkDetailGroups(a.packages || [], detailLimit);
    const rowOf = (p, rowGroup) => {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                  (p.package_code || '').replace(/^TCACA_/, '');
      return '<tr' + (rowGroup ? ' class="pk-hidden-row pk-' + rowGroup +
        '-row" data-pk-row="' + rowGroup + '" hidden' : '') +
        '><td class="mark" aria-hidden="true"><i style="background:' +
        colorOf(k) + '"></i></td>' +
      '<td>' + esc(p.name || '(未命名)') +
        (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
      '<td class="num">' + fmtTok(p.size) + '</td>' +
      '<td class="num">' + fmtTok(p.remain) + '</td>' +
      '<td class="num">' + fmtTok(p.used) + '</td>' +
      '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
      '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
      '</tr>';
    };
    const groupSummary = (group, label, count, size, remain) =>
      '<tr class="pk-group-summary"><td colspan="7"><button type="button" class="pk-group-toggle"' +
      ' data-pk-group="' + group + '" data-count="' + count + '" data-size="' + size +
      '" data-remain="' + remain + '" aria-expanded="false">' + label + '，展开</button></td></tr>';
    const rows = groups.visible.map(p => rowOf(p, '')).join('');
    const restSummary = groups.rest.length
      ? groupSummary('rest', '其余未用完 ' + groups.rest.length + ' 个包（面额合计 ' +
          fmtTok(groups.restSize) + ' · 剩余 ' + fmtTok(groups.restRemain) + '）',
          groups.rest.length, groups.restSize, groups.restRemain) +
        groups.rest.map(p => rowOf(p, 'rest')).join('')
      : '';
    const usedSummary = groups.used.length
      ? groupSummary('used', '已用完 ' + groups.used.length + ' 个包（面额合计 ' +
          fmtTok(groups.usedSize) + '）', groups.used.length, groups.usedSize, 0) +
        groups.used.map(p => rowOf(p, 'used')).join('')
      : '';
    return '<div class="box"><header><h3>' +
      esc(a.nickname || a.uid.slice(0, 8)) + ' · ' + esc(a.realm || '') +
      '</h3><span class="grow"></span><span class="note">余额 ' + fmtTok(a.remain) +
      ' / 总额 ' + fmtTok(a.size) + ' · 可用 ' + (groups.visible.length + groups.rest.length) + ' 个包' +
      (groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : '') +
      ' · 默认展示最早到期 ' + pkDetailLimitValue(detailLimit) + ' 条（' +
      esc(PK_SORT_LABELS[pkSortMode] || '') + '）</span>' +
      '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + rows + restSummary + usedSummary + '</tbody></table></div></div>';
  }).join('');
}

if ($('pkDetail')) $('pkDetail').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-pk-group]');
  if (!btn) return;
  const body = btn.closest('tbody');
  if (!body) return;
  const group = btn.dataset.pkGroup;
  const expanded = btn.getAttribute('aria-expanded') === 'true';
  body.querySelectorAll('tr[data-pk-row="' + group + '"]').forEach(row => { row.hidden = expanded; });
  const count = btn.dataset.count || '0';
  const size = btn.dataset.size || '0';
  const remain = btn.dataset.remain || '0';
  btn.setAttribute('aria-expanded', String(!expanded));
  if (group === 'rest') {
    btn.textContent = expanded
      ? '其余未用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + ' · 剩余 ' +
        fmtTok(remain) + '），展开'
      : '收起其余未用完 ' + count + ' 个包';
  } else {
    btn.textContent = expanded
      ? '已用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + '），展开'
      : '收起已用完 ' + count + ' 个包';
  }
});

/* ── 逐包明细排序模式 ─────────────────────────────────────────────── */
/* 逐包明细的排序规则（选择持久化在 localStorage，跨会话记住）：
   end_asc   到期升序（默认）——快过期的包排最前，提醒优先消耗；无到期时间的
             包（上游没下发 end_time）没有可比的日期，统一垫底，不掺进日期序里；
   size_desc 面额降序——原来的展示口径，看「钱从哪来」。
   行序统一由 pkDetailCompare 实现（明细折叠分组共用同一比较器，切换模式时
   折叠组内行序同步跟随）。本块整体放在 renderPackages / renderExpiryDistribution
   之后：前端 harness 按区域切片求值（[PK_ACCOUNT_COLORS, renderExpiryDistribution)
   与 [PK_DEFAULT_DETAIL_LIMIT, renderPackages)），顶层 localStorage/$ 语句落进
   切片区会让无 DOM 桩的求值环境 ReferenceError——上游测试的切片边界不动。 */
const LS_PK_SORT = 'pkSortMode';
let pkSortMode = localStorage.getItem(LS_PK_SORT) || 'end_asc';
const PK_SORT_LABELS = { end_asc: '按到期升序 · 近的在前', size_desc: '按面额降序' };
// 排序切换时重排明细需要 detailLimit（上游折叠配置），loadPackages 拉到后缓存。
let lastDetailLimit = PK_DEFAULT_DETAIL_LIMIT;

// 排序规则控件：恢复上次选择并绑定切换。
if ($('pkSort')) {
  $('pkSort').value = pkSortMode;
  if ($('pkSort').value !== pkSortMode) {        // localStorage 里存了废弃值：回落默认
    pkSortMode = 'end_asc';
    localStorage.setItem(LS_PK_SORT, pkSortMode);   // 不用 removeItem：harness 桩无此方法
  }
  $('pkSort').onchange = () => {
    pkSortMode = $('pkSort').value;
    localStorage.setItem(LS_PK_SORT, pkSortMode);
    if (lastPackages) renderPackages(lastPackages, lastDetailLimit);   // 数据在内存，直接重排
  };
}

// loadPackages 按需拉取：有缓存（上次任意入口拉到的 packages 数据）先直接渲染，
// 不再「切进积分构成页就打一次上游」——逐账号实时查询在账号多时既慢又压上游。
// 无缓存（首次进入）才自动查一次；显式刷新（force=true，点「刷新」按钮）始终实时查。
async function loadPackages(force) {
  if (!force && lastPackages) {
    renderPackages(lastPackages, lastDetailLimit);
    const ageMin = Math.floor((Date.now() - lastPackagesAt) / 60000);
    if (ageMin > 0) $('pkNote').textContent = lastPackages.accounts.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，点「刷新」更新';
    return;
  }
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">查询中…</div>';
  try {
    const [d, c] = await Promise.all([
      api('packages'),
      api('config').catch(() => null),
    ]);
    lastPackages = d;                            // 缓存供首页到期卡片与排序切换复用
    lastPackagesAt = Date.now();
    lastDetailLimit = pkDetailLimit(c && c.config);
    renderPackages(d, lastDetailLimit);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
    $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">读取失败：' + esc(e.message) + '</div>';
  }
}

/* ── 积分到期提醒（首页卡片）────────────────────────────────────────── */
/* 积分不是永久的：签到/任务发的裂变包约一个月失效。只看「剩余积分 ÷ 日消耗」
   会系统性偏乐观——用不完的部分到期直接蒸发。这里把「最近要过期的是哪批、
   有多少、到期前每天要至少消耗多少」顶到首页，数据源与「积分构成」共用
   （lastPackages 缓存，EXP_FRESH_MS 内复用，不重复打上游）。 */

// expBatches 把某账号的包聚合成「到期日 → 该日作废积分」升序列表。
// 只统计 remain>0 且有到期时间的包——没余额/长期包到期没有任何影响。
function expBatches(packs) {
  const byDay = new Map();
  for (const p of packs || []) {
    const r = Number(p.remain || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (r <= 0 || !t) continue;
    byDay.set(t, (byDay.get(t) || 0) + r);
  }
  return [...byDay.entries()]
    .map(([date, remain]) => ({ date, remain }))
    .sort((a, b) => (a.date < b.date ? -1 : 1));
}

function expDaysLeft(dateStr, today) {
  return Math.round((new Date(dateStr + 'T00:00:00') - today) / 86400000);
}

function renderExpiry(d) {
  if (!$('expList')) return;
  const list = (d.accounts || []);
  const today = new Date(); today.setHours(0, 0, 0, 0);
  // 按「最近到期」升序排（issue #125）。
  //
  // 后端 /panel/api/packages 是按**余额降序**返回的，恰好把 CN 账号都排在前面、
  // global 排在末尾，看起来像"按域分组"，其实只是余额顺序。这里再排一次，让
  // "最快过期的排最前"这个唯一重要的顺序成立，且不分域。
  //
  // 排序键用最早到期批次的日期（expBatches 已按日期升序，故 [0] 即最早）。
  // 查询失败的账号、以及 7 天内无到期的账号没有可比较的到期时间，统一排在最后，
  // 保持它们原本的相对顺序（稳定排序）。
  const keyed = list.map(a => {
    let key = null;
    if (!a.error) {
      const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
      if (bs.length) key = bs[0].date;
    }
    return { a, key };
  });
  keyed.sort((x, y) => {
    if (x.key === y.key) return 0;
    if (x.key === null) return 1;
    if (y.key === null) return -1;
    return x.key < y.key ? -1 : 1;
  });
  const rows = keyed.map(({ a }) => {
    if (a.error) {
      return '<div class="exp-row"><span class="exp-dot" style="background:var(--ink-3)"></span>' +
        '<span class="exp-nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
        '<span class="exp-main err">查询失败：' + esc(a.error) + '</span></div>';
    }
    const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
    if (!bs.length) {
      return '<div class="exp-row"><span class="exp-dot" style="background:var(--ok)"></span>' +
        '<span class="exp-nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
        '<span class="exp-main">7 天内无到期积分</span></div>';
    }
    const first = bs[0];
    const days = expDaysLeft(first.date, today);
    const daily = Math.ceil(first.remain / Math.max(1, days));
    const week = bs.filter(b => expDaysLeft(b.date, today) <= 7)
      .reduce((s, b) => s + b.remain, 0);
    // 危险度：≤3 天红（不抓紧就真没了）、≤7 天琥珀、更远绿。
    // 上游扣包是 FEFO（按失效时刻升序，实测两号口径一致）：这些快过期批次正是
    // 被消耗得最快的，日均需耗给的是「哪怕单靠这个账号的自然流量也能对齐」的参照。
    const cls = days <= 3 ? 'var(--bad)' : days <= 7 ? 'var(--warn)' : 'var(--ok)';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    const more = bs.length > 4 ? '　等 ' + bs.length + ' 批' : '';
    const rest = bs.slice(1, 4).map(b =>
      '随后 ' + esc(b.date.slice(5)) + ' · ' + fmtTok(b.remain)).join('　') + more;
    return '<div class="exp-row"><span class="exp-dot" style="background:' + cls + '"></span>' +
      '<span class="exp-nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
      '<span class="exp-main">最近到期 <b>' + esc(first.date) + '</b>（' + dayWord +
      '）· 该批 <b>' + fmtTok(first.remain) + '</b> 积分 · 到期前日均需耗 ≥<b>' +
      fmtTok(daily) + '</b>' +
      (week > first.remain ? ' · 7 天内合计 ' + fmtTok(week) : '') +
      (rest ? '<div class="note">' + rest + '</div>' : '') +
      '</span></div>';
  }).join('');
  if (!$('expList')) return;   // 账号池视图已移除：到期卡片 DOM 不存在时静默跳过
  $('expList').innerHTML = rows || '<div class="empty">没有账号</div>';
  // 数据新鲜度透明化：走缓存时标注年龄，免得把旧数据误当实时。
  const ageMin = lastPackages ? Math.floor((Date.now() - lastPackagesAt) / 60000) : 0;
  $('expNote').textContent = (lastPackagesAt && ageMin > 0)
    ? list.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，可点「检查」刷新'
    : list.length + ' 个账号 · 实时查询上游';
  $('expBox').hidden = false;
}

/* 快照版到期提醒：不查上游，直接用 /overview 已有的 credits_earliest_expiry /
   credits_earliest_remaining（由签到与余额后台刷新写入 state.json，默认每 5 分钟
   更新一次）。打开面板即可看到「最近哪批要过期」，成本与首页统计条相同为零。

   与 renderExpiry（逐包明细版）的差异：这里只有**最早一批**的日期与数量，没有
   后续批次、也没法算「7 天内合计」——那是 packages 接口才有的信息。因此卡片上
   保留「检查」按钮作为增强入口，用户想要完整明细再显式触发一次上游查询。 */
function renderExpirySnapshot(d) {
  if (!$('expList')) return;
  const list = (d && d.accounts) || [];
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const keyed = list.map(a => {
    // 后端给的是 RFC3339 带时区；无到期信息（长期包/未刷新）的账号排在最后。
    let key = null, ms = 0;
    if (a.credits_earliest_expiry) {
      const t = new Date(a.credits_earliest_expiry);
      if (!isNaN(t.getTime())) { ms = t.getTime(); key = ms; }
    }
    return { a, key, ms };
  });
  keyed.sort((x, y) => {
    if (x.key === null && y.key === null) return 0;
    if (x.key === null) return 1;
    if (y.key === null) return -1;
    return x.key - y.key;
  });
  const rows = keyed.map(({ a, ms }) => {
    const nm = esc(a.nickname || String(a.uid || '').slice(0, 8));
    if (!ms || !a.credits_earliest_remaining) {
      return '<div class="exp-row"><span class="exp-dot" style="background:var(--ok)"></span>' +
        '<span class="exp-nm">' + nm + '</span>' +
        '<span class="exp-main">无近期到期批次</span></div>';
    }
    const days = Math.max(0, Math.ceil((ms - today.getTime()) / 86400000));
    const remain = Number(a.credits_earliest_remaining || 0);
    const daily = Math.ceil(remain / Math.max(1, days));
    const cls = days <= 3 ? 'var(--bad)' : days <= 7 ? 'var(--warn)' : 'var(--ok)';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    const when = new Date(ms).toISOString().slice(0, 10);
    return '<div class="exp-row"><span class="exp-dot" style="background:' + cls + '"></span>' +
      '<span class="exp-nm">' + nm + '</span>' +
      '<span class="exp-main">最近到期 <b>' + esc(when) + '</b>（' + dayWord +
      '）· 该批 <b>' + fmtTok(remain) + '</b> 积分 · 到期前日均需耗 ≥<b>' +
      fmtTok(daily) + '</b>' +
      '<div class="note">仅最早一批，点「检查」看全部到期批次</div>' +
      '</span></div>';
  }).join('');
  if (!$('expList')) return;   // 账号池视图已移除：到期卡片 DOM 不存在时静默跳过
  $('expList').innerHTML = rows || '<div class="empty">没有账号</div>';
  $('expNote').textContent = list.length + ' 个账号 · 快照（来自本地状态，点「检查」拉实时明细）';
  $('expBox').hidden = false;
}

async function loadExpiry(force) {
  if (!$('expBox')) return;
  if (expFetching) return;
  const fresh = lastPackages && (Date.now() - lastPackagesAt) < EXP_FRESH_MS;
  if (fresh && !force) { renderExpiry(lastPackages); return; }
  expFetching = true;
  $('expBox').hidden = false;
  if (!$('expList').children.length) $('expList').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('expNote').textContent = '查询中…';
  try {
    const d = await api('packages');
    lastPackages = d;                            // 与「积分构成」视图共用同一份缓存
    lastPackagesAt = Date.now();
    renderExpiry(d);
  } catch (e) {
    $('expNote').textContent = '查询失败：' + esc(e.message);
  }
  expFetching = false;
}

if ($('btnExp')) $('btnExp').onclick = () => loadExpiry(true);

// 「刷新」始终实时查（显式 force，不依赖事件对象真值）
if ($('btnPk')) $('btnPk').onclick = () => loadPackages(true);

/* ── Trae / Qoder 积分构成 ──────────────────────────────────────────
   Trae：上游 ide_user_ent_usage 的逐包明细（每月赠送 / 签到奖励，带到期时间）。
   Qoder：上游 me/usage 的套餐内 + 附加包两组 Credits。 */
async function loadPkIdea(quiet) {
  const st = $('pkIdeaState'), box = $('pkIdea');
  if (!st || !box) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; box.innerHTML = ''; }
  try {
    const [td, qd] = await Promise.all([
      api('trae/credits').catch(() => null),
      api('ide/qoder/usage').catch(() => null),
    ]);
    const tarr = (td && td.accounts) || [];
    const parts = [];
    // Trae
    parts.push('<div class="pad"><div class="ck-group">Trae</div></div>');
    if (!tarr.length) {
      parts.push('<div class="state">未找到 Trae 账号。</div>');
    } else {
      parts.push('<div class="tbl-wrap"><table class="acc"><thead><tr><th>账号</th><th>积分包</th><th>额度</th><th>已用</th><th>剩余</th><th>到期</th></tr></thead><tbody>');
      for (const a of tarr) {
        const packs = a.packs || [];
        if (!packs.length) {
          parts.push('<tr><td class="who"><div class="nm">' + esc(a.name || '未命名') + '</div><div class="id">' + esc(a.user_id || '') + '</div></td><td colspan="5"><span style="color:var(--ink-3)">' + (a.has_balance ? '无逐包明细' : '无法获取（凭证过期或接口异常）') + '</span></td></tr>');
          continue;
        }
        let first = true;
        for (const p of packs) {
          const exp = p.end_time ? new Date(p.end_time * 1000).toLocaleDateString('zh-CN') : '—';
          const lim = p.unlimited ? '不限量' : (p.limit || 0).toLocaleString('zh-CN');
          const rem = p.unlimited ? '∞' : (p.remaining || 0).toLocaleString('zh-CN');
          parts.push('<tr>' + (first ? '<td class="who" rowspan="' + packs.length + '"><div class="nm">' + esc(a.name || '未命名') + '</div><div class="id">' + esc(a.user_id || '') + '</div></td>' : '') +
            '<td>' + esc(p.desc || '—') + '</td>' +
            '<td class="num">' + lim + '</td>' +
            '<td class="num">' + (p.used || 0) + '</td>' +
            '<td class="num"><b>' + rem + '</b></td>' +
            '<td class="num">' + exp + '</td></tr>');
          first = false;
        }
      }
      parts.push('</tbody></table></div>');
    }
    // Qoder
    parts.push('<div class="pad"><div class="ck-group">Qoder</div></div>');
    if (!qd || qd.phase !== 'ok') {
      parts.push('<div class="state">' + esc((qd && qd.msg) || '未登录或无法读取 Qoder 额度') + '</div>');
    } else {
      const pkg = qd.user_quota || {}, addon = qd.add_on_quota || {};
      parts.push('<div class="tbl-wrap"><table class="acc"><thead><tr><th>账号</th><th>积分包</th><th>额度</th><th>已用</th><th>剩余</th><th>套餐</th></tr></thead><tbody>' +
        '<tr><td class="who" rowspan="2"><div class="nm">' + esc(qd.name || '当前登录') + '</div><div class="id">' + esc(qd.email || '') + '</div></td>' +
        '<td>套餐内 Credits</td><td class="num">' + (pkg.total || 0) + '</td><td class="num">' + (pkg.used || 0) + '</td><td class="num"><b>' + (pkg.remaining || 0) + '</b></td>' +
        '<td rowspan="2">' + esc(qd.plan_tier_name || qd.plan_tier || '—') + '</td></tr>' +
        '<tr><td>附加（签到）Credits</td><td class="num">' + (addon.total || 0) + '</td><td class="num">' + (addon.used || 0) + '</td><td class="num"><b>' + (addon.remaining || 0) + '</b></td></tr>' +
        '</tbody></table></div>');
    }
    box.innerHTML = parts.join('');
    st.hidden = true;
    if ($('pkIdeaNote')) $('pkIdeaNote').textContent = '';
  } catch (e) {
    st.hidden = false; st.className = 'state err'; st.textContent = e.message;
  }
}
if ($('btnPkIdea')) $('btnPkIdea').onclick = () => loadPkIdea(false);

/* ── IDE 账号（Qoder 切号） ──────────────────────────────────────────
   服务端直接读写本机 Qoder 的 auth.v1.dat（AES-256-GCM + DPAPI 密钥），
   面板只做展示与触发：添加当前账号 / 切换 / 移除。 */
async function loadQoderAccounts(quiet) {
  const st = $('qoderState'), list = $('qoderList');
  if (!st || !list) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const d = await api('ide/qoder/accounts');
    const arr = d.accounts || [];
    const cur = d.current_email || '';
    $('qoderSummary').textContent = cur
      ? '当前：' + cur + (d.current_expires ? '（到期 ' + String(d.current_expires).slice(0, 10) + '）' : '')
      : '当前未登录';
    if (!arr.length) {
      st.hidden = false; st.className = 'state';
      st.textContent = cur
        ? '号池为空。点右上角「添加账号 → Qoder」把当前登录账号存入号池。'
        : '号池为空，且本机 Qoder 未登录。请先用某个账号登录 Qoder，再添加。';
      list.innerHTML = '';
      return;
    }
    const head = '<div class="ahead"><div>账号</div><div>状态</div><div>操作</div></div>';
    list.innerHTML = head + arr.map(a => {
      const status = a.current
        ? '<span class="stask ok"><span class="mark">✓</span>当前登录</span>'
        : (a.expired
          ? '<span class="stask todo"><span class="mark">!</span>凭证过期</span>'
          : '<span class="stask warn"><span class="mark">○</span>可切换</span>');
      const sw = a.current
        ? '<button class="xs" disabled>当前</button>'
        : '<button class="xs primary" data-qoder-switch="' + esc(a.email) + '">切号</button>';
      const renew = a.current ? '<button class="xs" data-qoder-renew="' + esc(a.email) + '">续期</button>' : '';
      const rm = '<button class="xs" data-qoder-remove="' + esc(a.email) + '">移除</button>';
      return '<div class="arow" data-acc="' + esc(a.email) + '">' +
        '<div class="who"><div class="nm">' + esc(a.name || '未命名') + '</div>' +
        '<div class="id" title="' + esc(a.email) + '">' + esc(a.email) + '</div></div>' +
        '<div>' + status + '</div>' +
        '<div class="acts">' + renew + sw + rm + '</div>' +
        '</div>';
    }).join('');
    st.hidden = true;
    list.querySelectorAll('button[data-qoder-switch]').forEach(b => {
      b.onclick = () => doQoderSwitch(b.dataset.qoderSwitch, b);
    });
    list.querySelectorAll('button[data-qoder-remove]').forEach(b => {
      b.onclick = () => doQoderRemove(b.dataset.qoderRemove);
    });
    list.querySelectorAll('button[data-qoder-renew]').forEach(b => {
      b.onclick = () => doQoderRenew(b);
    });
  } catch (e) {
    st.hidden = false; st.className = 'state err'; st.textContent = e.message;
  }
}

async function doQoderRenew(btn) {
  const old = btn ? btn.textContent : '续期';
  if (btn) { btn.disabled = true; btn.textContent = '续期中…'; }
  try {
    const d = await api('ide/qoder/renew', { method: 'POST', body: JSON.stringify({ force: false }) });
    if (d.phase === 'renewed') toast('凭证已续期，到期 ' + (d.expires_at || '').slice(0, 10), 'ok');
    else if (d.phase === 'skipped') toast('凭证仍在有效期内，无需续期', 'ok');
    else toast(d.msg || '续期失败', 'err');
    loadQoderAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = old || '续期'; } }
}

async function doQoderAdd() {
  const btn = $('btnQoderAdd');
  btn.disabled = true; btn.textContent = '添加中…';
  try {
    const d = await api('ide/qoder/add', { method: 'POST', body: '{}' });
    toast((d.added ? '已添加账号：' : '已更新账号：') + d.email, 'ok');
    loadQoderAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '添加当前账号'; }
}

async function doQoderSwitch(email, btn) {
  if (!confirm('切换到 ' + email + '？\n\n会自动关闭 Qoder、写入该账号登录态并重新启动 Qoder。')) return;
  const old = btn.textContent;
  btn.disabled = true; btn.textContent = '切换中…';
  try {
    const d = await api('ide/qoder/switch', {
      method: 'POST',
      body: JSON.stringify({ email, restore_machine_id: false, restart: true }),
    });
    // 完全冻结：不重载列表、不改「当前登录」标记，画面 100% 停在切换前。
    onSwitchDone('qoder', d.email || email, d.restart_note);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = old || '切换'; }
}

async function doQoderRemove(email) {
  if (!confirm('从号池移除 ' + email + '？\n（不会影响 Qoder 当前登录态）')) return;
  try {
    await api('ide/qoder/remove', { method: 'POST', body: JSON.stringify({ email }) });
    toast('已移除 ' + email, 'ok');
    loadQoderAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
}

// doQoderCheckin 签到。email 为空 = 号池全部账号（后端逐个用各自的 token 打接口）。
async function doQoderCheckin(btn, email) {
  const old = btn ? btn.textContent : '签到';
  if (btn) { btn.disabled = true; btn.textContent = '签到中…'; }
  try {
    const body = email ? JSON.stringify({ emails: [email] }) : '{}';
    const d = await api('ide/qoder/checkin', { method: 'POST', body });
    const results = (d && d.results) || [];
    const okc = results.filter(r => r.phase === 'claimed').length;
    const alc = results.filter(r => r.phase === 'already').length;
    // pending = 上游没有下发每日签到活动（不是故障；官方限时活动已结束/未开放时
    // 会一直如此）。单独列出不算失败，避免误读成"签不上"。
    const nop = results.filter(r => r.phase === 'pending').length;
    const bad = results.filter(r => r.phase !== 'claimed' && r.phase !== 'already' && r.phase !== 'pending');
    let text;
    if (results.length > 1 || !email) {
      text = 'Qoder 签到：成功 ' + okc + ' · 今日已领 ' + alc +
        (nop ? ' · 暂无活动 ' + nop : '') + ' · 失败 ' + bad.length;
      if (nop && !okc && !alc && !bad.length) text += '（上游当前没有可领的签到活动；每 30 分钟自动重试，一旦下发即自动领取）';
      if (d && d.credits) text += '，新增 +' + d.credits + ' Credits';
      else if (d && d.credits_already) text += '（今日已入账 ' + d.credits_already + '）';
    } else {
      const r = results[0] || {};
      text = r.phase === 'claimed' ? ('签到成功 +' + (r.credits || 0) + ' Credits')
        : r.phase === 'already' ? '今日已领取'
          : (r.msg || (d && d.msg) || r.phase || '签到失败');
    }
    if (bad.length && bad[0].msg) text += '；' + bad[0].email + '：' + bad[0].msg;
    toast(text, bad.length ? 'err' : (nop && !okc ? 'warn' : 'ok'));
    // 切号冻结期间不自动重绘（画面要保持切换前），只弹结果。
    if (!viewFrozen()) {
      loadCkQoder(true);
      if (view === 'ideaccounts') loadQoderAccounts(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = old || '签到'; } }
}

if ($('btnQoderRefresh')) $('btnQoderRefresh').onclick = () => { thawView(); loadQoderAccounts(false); };
if ($('btnQoderAdd')) $('btnQoderAdd').onclick = doQoderAdd;
if ($('btnCkQoderCheckinAll')) $('btnCkQoderCheckinAll').onclick = () => doQoderCheckin($('btnCkQoderCheckinAll'));
if ($('btnCkQoderSwitched')) $('btnCkQoderSwitched').onclick = () => doQoderSwitchedCheckin($('btnCkQoderSwitched'));

// doQoderSwitchedCheckin 切号逐号签到：逐个切客户端（重启 N 次，约 N×40 秒）。
async function doQoderSwitchedCheckin(btn) {
  if (!confirm('切号逐号签到：会把 Qoder 客户端轮流切到号池每个账号（每号重启一次客户端，全部完成后恢复你原来的登录）。\n\n注意：每台设备每天只有一个号能领到签到积分（服务端规则）。\n\n继续？')) return;
  const old = btn ? btn.textContent : '切号签到';
  if (btn) { btn.disabled = true; btn.textContent = '切号签到中…'; }
  try {
    const d = await api('ide/qoder/checkin_switched', { method: 'POST', body: '{}' });
    toast(d.msg || '切号签到完成', 'ok');
    if (!viewFrozen()) { loadCkQoder(true); loadQkCredits(true); }
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = old || '切号签到'; } }
}

/* ── Trae IDE 账号（切号） ──────────────────────────────────────────
   与「Trae 签到」不同：这里管的是 Trae IDE 自身登录态
   （%APPDATA%\Trae CN\User\globalStorage\storage.json）。 */
async function loadTraeIdeAccounts(quiet) {
  const st = $('traeIdeState'), list = $('traeIdeList');
  if (!st || !list) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const d = await api('ide/trae/accounts');
    const arr = d.accounts || [];
    const cur = d.current_email || d.current_user_id || '';
    $('traeIdeSummary').textContent = cur ? '当前：' + cur : '当前未登录';
    if (!d.available) {
      st.hidden = false; st.className = 'state';
      st.innerHTML = '未检测到 Trae IDE 登录态。<br>数据目录：<code>' + esc(d.data_dir || '') + '</code><br>请先启动 Trae 并登录一个账号，再点「添加当前账号」。';
      list.innerHTML = '';
      return;
    }
    if (!arr.length) {
      st.hidden = false; st.className = 'state';
      st.textContent = cur
        ? '号池为空。Trae 当前已登录 ' + cur + '，点右上角「添加当前账号」存入号池。'
        : '号池为空。请先在 Trae 里登录，再点「添加当前账号」。';
      list.innerHTML = '';
      return;
    }
    const head = '<div class="ahead"><div>账号</div><div>状态</div><div>操作</div></div>';
    list.innerHTML = head + arr.map(a => {
      const id = a.email || a.user_id || '';
      let status, ops;
      if (a.current) {
        // 当前登录优先于其他标记：即便该账号只在签到侧存在，它此刻也是客户端里真正登录的那个。
        status = '<span class="stask ok"><span class="mark">✓</span>当前登录</span>';
        ops = a.checkin_only
          ? '<button class="xs" disabled title="该账号尚未保存 IDE 登录态，请点右上角「添加当前账号」">未入库</button>'
          : '<button class="xs" disabled>当前</button>';
      } else if (a.checkin_only) {
        status = '<span class="stask warn"><span class="mark">·</span>仅签到凭证</span>';
        ops = '';
      } else {
        status = '<span class="stask warn"><span class="mark">○</span>可切换</span>';
        ops = '<button class="xs primary" data-traeide-switch="' + esc(id) + '">切号</button>';
      }
      const rm = a.checkin_only ? '' : '<button class="xs" data-traeide-remove="' + esc(id) + '">移除</button>';
      return '<div class="arow" data-acc="' + esc(id) + '">' +
        '<div class="who"><div class="nm">' + esc(a.name || '未命名') + '</div>' +
        '<div class="id" title="' + esc(a.email || a.user_id || '') + '">' + esc(a.email || a.user_id || '') + '</div></div>' +
        '<div>' + status + '</div>' +
        '<div class="acts">' + ops + rm + '</div>' +
        '</div>';
    }).join('');
    st.hidden = true;
    list.querySelectorAll('button[data-traeide-switch]').forEach(b => {
      b.onclick = () => doTraeIdeSwitch(b.dataset.traeideSwitch, b);
    });
    list.querySelectorAll('button[data-traeide-remove]').forEach(b => {
      b.onclick = () => doTraeIdeRemove(b.dataset.traeideRemove);
    });
  } catch (e) {
    st.hidden = false; st.className = 'state err'; st.textContent = e.message;
  }
}

async function doTraeIdeAdd() {
  const btn = $('btnTraeIdeAdd');
  btn.disabled = true; btn.textContent = '添加中…';
  try {
    const d = await api('ide/trae/add', { method: 'POST', body: '{}' });
    toast((d.added ? '已添加：' : '已更新：') + (d.email || d.user_id), 'ok');
    loadTraeIdeAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '添加当前账号'; }
}

async function doTraeIdeSwitch(id, btn) {
  if (!confirm('切换到 ' + id + '？\n\n会自动关闭 Trae、写入该账号登录态并重新启动 Trae。')) return;
  const old = btn.textContent;
  btn.disabled = true; btn.textContent = '切换中…';
  try {
    const d = await api('ide/trae/switch', {
      method: 'POST',
      body: JSON.stringify({ id, restore_machine_id: false, restart: true }),
    });
    // 完全冻结：画面停在切换前，连「当前登录」标记都不动。
    onSwitchDone('trae', d.email || d.name || id, d.restart_note);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = old || '切换'; }
}

async function doTraeIdeRemove(id) {
  if (!confirm('从号池移除 ' + id + '？\n（不会影响 Trae 当前登录态）')) return;
  try {
    await api('ide/trae/remove', { method: 'POST', body: JSON.stringify({ id }) });
    toast('已移除 ' + id, 'ok');
    loadTraeIdeAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
}

if ($('btnTraeIdeRefresh')) $('btnTraeIdeRefresh').onclick = () => { thawView(); loadTraeIdeAccounts(false); };
if ($('btnTraeIdeAdd')) $('btnTraeIdeAdd').onclick = doTraeIdeAdd;

/* ── WorkBuddy 账号（国内版 / 国际版） ───────────────────────────────
   后端保存每个版本的 refresh token 与账号快照；这里负责版本筛选、登录轮询、
   切换、积分查询和每日签到。新版 WorkBuddy 的真正密文由客户端 daemon 管理，
   所以切换后提示重启/刷新客户端，不把“写入标记”误报成已经替换 daemon 会话。 */
function wbEditionLabel(ed) {
  return ed === 'intl' ? '国际版' : ed === 'intlbuddy' ? '国际备用' : '国内版';
}
function wbUid(a) { return a.uid || ''; }
function wbRowsForEdition(rows, ed) {
  return (rows || []).filter(a => !ed || a.edition === ed);
}
async function loadWorkBuddyAccounts(quiet) {
  const st = $('wbState'), list = $('wbList');
  if (!st || !list) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const d = await api('ide/workbuddy/accounts');
    if (!d.ok) throw new Error(d.error || '读取 WorkBuddy 账号失败');
    const all = d.accounts || [];
    const curs = all.filter(a => a.current);
    $('wbSummary').textContent = curs.length
      ? '当前：' + curs.map(a => wbEditionLabel(a.edition)).join(' + ')
      : '未检测到已登录版本';
    if (!all.length) {
      st.hidden = false; st.className = 'state';
      st.textContent = '号池为空。点上方「添加账号 → WorkBuddy（国内版 / 国际版）」登录添加。';
      list.innerHTML = '';
      return;
    }
    const head = '<div class="ahead"><div>账号</div><div>状态</div><div>操作</div></div>';
    list.innerHTML = head + all.map(a => {
      const uid = wbUid(a);
      // 能否一键切号：必须有 refresh token，且号池已记录该账号的真实登录会话文件。
      const switchable = a.hasRefresh && a.session_ready && !a.expired;
      let status;
      if (a.current) {
        status = '<span class="stask ok"><span class="mark">✓</span>当前登录</span>';
      } else if (a.expired) {
        status = '<span class="stask todo"><span class="mark">!</span>凭据过期</span>';
      } else if (!a.hasRefresh) {
        status = '<span class="stask todo"><span class="mark">!</span>缺 refresh token</span>';
      } else if (!a.session_ready) {
        status = '<span class="stask warn" title="该账号尚未在本机客户端登录过，未记录登录会话，无法一键切号；在客户端登录一次后会自动记录。"><span class="mark">·</span>需登录一次</span>';
      } else {
        status = '<span class="stask warn"><span class="mark">○</span>可切换</span>';
      }
      const sw = a.current
        ? '<button class="xs" disabled>当前</button>'
        : (switchable
          ? '<button class="xs primary" data-wb-switch="' + esc(uid) + '">切号</button>'
          : '<button class="xs" disabled title="需先在客户端登录该账号一次，面板才会记录会话并可切号">切号</button>');
      const rm = '<button class="xs" data-wb-remove="' + esc(uid) + '">移除</button>';
      return '<div class="arow" data-acc="' + esc(uid) + '">' +
        '<div class="who"><div class="nm">' + esc(a.nickname || '未命名') +
        '<span class="tag mute" style="margin-left:6px">' + esc(wbEditionLabel(a.edition)) + '</span></div>' +
        '<div class="id" title="' + esc(uid) + '">' + esc(uid || 'UID 未返回') + '</div></div>' +
        '<div>' + status + '</div>' +
        '<div class="acts">' + sw + rm + '</div>' +
        '</div>';
    }).join('');
    st.hidden = true;
    list.querySelectorAll('button[data-wb-switch]').forEach(b => { b.onclick = () => doWorkBuddySwitch(b.dataset.wbSwitch, b); });
    list.querySelectorAll('button[data-wb-remove]').forEach(b => { b.onclick = () => doWorkBuddyRemove(b.dataset.wbRemove); });
  } catch (e) {
    st.hidden = false; st.className = 'state err'; st.textContent = e.message;
  }
}

async function doWorkBuddyAdd() {
  const btn = $('btnWbAdd'), ed = $('wbEdition') ? $('wbEdition').value : 'cn';
  // 必须在用户点击的同步调用栈内先开空白页，否则异步拿到 authUrl 后再 window.open
  // 常被浏览器当作非用户手势拦截。
  const loginWindow = window.open('about:blank', '_blank');
  if (!loginWindow) { toast('登录窗口被浏览器拦截，请允许此面板打开弹窗后重试', 'err'); return; }
  btn.disabled = true; btn.textContent = '打开登录页…';
  try {
    const start = await api('ide/workbuddy/start', { method: 'POST', body: JSON.stringify({ edition: ed }) });
    if (!start.ok) throw new Error(start.error || 'WorkBuddy 登录初始化失败');
    if (!start.authUrl || !start.state) throw new Error('上游没有返回登录状态');
    loginWindow.opener = null;
    loginWindow.location.href = start.authUrl;
    btn.textContent = '等待授权…';
    const deadline = Date.now() + 5 * 60 * 1000;
    let lastPending = true;
    while (Date.now() < deadline) {
      const d = await api('ide/workbuddy/poll', { method: 'POST', body: JSON.stringify({ edition: ed, state: start.state }) });
      if (!d.ok) throw new Error(d.error || 'WorkBuddy 登录轮询失败');
      if (d.pending) {
        lastPending = true;
        await new Promise(resolve => setTimeout(resolve, 2000));
        continue;
      }
      lastPending = false;
      toast((d.added ? '已添加 WorkBuddy 账号：' : '已更新 WorkBuddy 账号：') + ((d.account && (d.account.nickname || d.account.uid)) || '完成'), 'ok');
      await loadWorkBuddyAccounts(true);
      return;
    }
    if (lastPending) throw new Error('登录等待超时，请完成浏览器授权后重试');
  } catch (e) {
    try { loginWindow.close(); } catch (_) { /* 浏览器窗口可能已由用户关闭 */ }
    toast(e.message, 'err');
  }
  finally { btn.disabled = false; btn.textContent = '浏览器登录并添加'; }
}

// doWorkBuddySwitch：一键切号 —— 结束客户端 → 写入该账号凭据 → 重新启动客户端。
async function doWorkBuddySwitch(uid, btn) {
  if (!confirm('切换到 WorkBuddy 账号 ' + uid + '？\n\n会自动关闭 WorkBuddy、写入该账号凭据并重新启动 WorkBuddy。')) return;
  const old = btn.textContent; btn.disabled = true; btn.textContent = '切换中…';
  try {
    const d = await api('ide/workbuddy/switch', { method: 'POST', body: JSON.stringify({ uid, restart: true }) });
    if (!d.ok) throw new Error(d.error || '切换失败');
    // 完全冻结：不重载列表、不改标记，画面停在切换前。
    onSwitchDone('workbuddy', uid, d.restart_note);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = old || '一键切号'; }
}

async function doWorkBuddyRemove(uid) {
  if (!confirm('从 WorkBuddy 号池移除 ' + uid + '？\n（不会删除客户端当前会话）')) return;
  try {
    await api('ide/workbuddy/remove', { method: 'POST', body: JSON.stringify({ uid }) });
    toast('已移除 ' + uid, 'ok'); loadWorkBuddyAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
}

async function doWorkBuddyUsage(uid, btn) {
  const old = btn.textContent; btn.disabled = true; btn.textContent = '查询中…';
  try {
    const d = await api('ide/workbuddy/usage?uid=' + encodeURIComponent(uid));
    const raw = d.usage || {};
    const text = raw.remaining != null ? ('剩余 ' + raw.remaining) : (raw.credits != null ? ('积分 ' + raw.credits) : JSON.stringify(raw));
    toast(uid + '：' + text, 'ok');
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = old || '积分'; }
}

async function doWorkBuddyCheckin(uid, btn) {
  const old = btn.textContent; btn.disabled = true; btn.textContent = '签到中…';
  try {
    const d = await api('ide/workbuddy/checkin', { method: 'POST', body: JSON.stringify({ uid }) });
    const msg = d.ok ? (d.already ? '今日已签到' : '签到成功')
      : (d.no_activity ? '无签到活动（该版本没有每日签到）' : (d.error || '签到失败'));
    toast(uid + '：' + msg, (d.ok || d.no_activity) ? 'ok' : 'err');
    loadCkWorkBuddy(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = old || '签到'; }
}

if ($('wbEdition')) $('wbEdition').onchange = () => loadWorkBuddyAccounts(false);
if ($('btnWbRefresh')) $('btnWbRefresh').onclick = () => { thawView(); loadWorkBuddyAccounts(false); };
if ($('btnWbAdd')) $('btnWbAdd').onclick = doWorkBuddyAdd;
if ($('btnWbSyncPool')) $('btnWbSyncPool').onclick = async () => {
  const b = $('btnWbSyncPool'); const old = b.textContent; b.disabled = true; b.textContent = '同步中…';
  try {
    const d = await api('ide/workbuddy/sync-pool', { method: 'POST', body: '{}' });
    if (d.ok) toast('已同步主池账号：扫描 ' + (d.scanned || 0) + ' 个，新增 ' + (d.added || 0) + ' 个', 'ok');
    else toast(d.error || '同步失败', 'err');
    loadWorkBuddyAccounts(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = old || '同步账号池账号'; }
};


/* ── 签到池：WorkBuddy / Qoder / Trae 三路签到，集中在顶部一个「一键签到」────
   账号管理在「IDE 账号池」，这里只做签到动作与状态展示。
   已签到的账号按钮置灰并显示 ✓，避免重复点击。 */
let ckPoolLoading = false;
async function loadCheckinPool(quiet) {
  if (ckPoolLoading) return;
  ckPoolLoading = true;
  try {
    await Promise.all([loadCkWorkBuddy(quiet), loadCkQoder(quiet), loadCkMiniMax(quiet), loadCkDoubao(quiet)]);
  } finally { ckPoolLoading = false; }
}

async function loadCkWorkBuddy(quiet) {
  const st = $('ckWbState'), list = $('ckWbList');
  if (!st || !list) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const d = await api('ide/workbuddy/accounts');
    const all = (d && d.ok ? d.accounts : []) || [];
    const done = all.filter(a => a.checked_in_today).length;
    if ($('ckWbSummary')) $('ckWbSummary').textContent = all.length ? ('共 ' + all.length + ' 个 · 已签 ' + done) : '号池为空';
    if (!all.length) {
      st.hidden = false; st.className = 'state';
      st.textContent = 'WorkBuddy 号池为空。';
      list.innerHTML = ''; return;
    }
    const head = '<div class="thead"><div>账号</div><div>版本</div><div>状态</div><div>操作</div></div>';
    list.innerHTML = head + all.map(a => {
      const uid = wbUid(a);
      const doneToday = !!a.checked_in_today;
      const status = doneToday
        ? '<span class="stask ok"><span class="mark">✓</span>今日已签到</span>'
        : (a.expired
          ? '<span class="stask todo"><span class="mark">!</span>凭据过期</span>'
          : (a.hasRefresh ? '<span class="stask warn"><span class="mark">○</span>可签到</span>'
            : '<span class="stask todo"><span class="mark">!</span>缺 refresh token</span>'));
      const btn = doneToday
        ? '<button class="xs" disabled>已签到</button>'
        : '<button class="xs primary" data-ck-wb="' + esc(uid) + '">签到</button>';
      return '<div class="trow">' +
        '<div class="who"><div class="nm">' + esc(a.nickname || '未命名') + '</div>' +
        '<div class="id" title="' + esc(uid) + '">' + esc(uid || 'UID 未返回') + '</div></div>' +
        '<div class="credits">' + esc(wbEditionLabel(a.edition)) + '</div>' +
        '<div>' + status + '</div>' +
        '<div>' + btn + '</div>' +
        '</div>';
    }).join('');
    st.hidden = true;
    list.querySelectorAll('button[data-ck-wb]').forEach(b => { b.onclick = () => doWorkBuddyCheckin(b.dataset.ckWb, b); });
  } catch (e) { st.hidden = false; st.className = 'state err'; st.textContent = e.message; }
}

async function doWbCheckinAll(btn) {
  const old = btn ? btn.textContent : '一键签到';
  if (btn) { btn.disabled = true; btn.textContent = '签到中…'; }
  try {
    const d = await api('ide/workbuddy/accounts');
    const all = (d && d.ok ? d.accounts : []) || [];
    if (!all.length) { toast('WorkBuddy 号池为空', 'err'); return { ok: 0, skip: 0, fail: 0 }; }
    let ok = 0, fail = 0, skip = 0;
    for (const a of all) {
      const uid = wbUid(a);
      if (!uid) continue;
      if (a.checked_in_today) { skip++; continue; }
      try {
        const r = await api('ide/workbuddy/checkin', { method: 'POST', body: JSON.stringify({ uid }) });
        if (r.ok) ok++; else if (r.no_activity) skip++; else fail++;
      } catch (_) { fail++; }
    }
    return { ok, skip, fail };
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = old || '一键签到'; }
  }
}

async function loadCkQoder(quiet) {
  const st = $('ckQoderState'), list = $('ckQoderList');
  if (!st || !list) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const d = await api('ide/qoder/accounts');
    const cur = d.current_email || '';
    const arr = d.accounts || [];
    // 号池为空但本机有登录态：仍然展示当前号（后端会回退成「签当前账号」）。
    const rows = arr.length ? arr : (cur ? [{ email: cur, name: d.current_name || '当前登录', current: true }] : []);
    // 今日签到状态：上游核实结果（后端逐号向 Qoder 活动列表确认）。
    const st2 = await api('ide/qoder/checkin-status').catch(() => null);
    const doneMap = {}, pendMap = {}, quotaMap = {}, msgMap = {};
    ((st2 && st2.results) || []).forEach(r => {
      if (!r || !r.email) return;
      const k = String(r.email).toLowerCase();
      // checked_in_today 由后端按**上游活动列表**判定（真签到口径）。
      // ⚠️ 此前这里读的是 checked_in_local（本地记录，可能是旧版误报），
      //    且顶层 checked_in_today 是「今天整体没有可领的了」的**聚合值**，
      //    被当成当前登录号的状态用 —— 两个号的签到显示因此整个反了。
      doneMap[k] = !!r.checked_in_today;
      pendMap[k] = (r.phase === 'pending' && !r.checked_in_today);
      quotaMap[k] = !!r.device_quota_used;
      msgMap[k] = r.msg || '';
    });
    // 当前登录号：用 current.checked_in_today（**单号**口径），不用顶层聚合值。
    if (cur && st2 && st2.current && typeof st2.current.checked_in_today === 'boolean') {
      doneMap[cur.toLowerCase()] = !!st2.current.checked_in_today;
    }
    const doneCount = rows.filter(a => doneMap[String(a.email || '').toLowerCase()]).length;
    if ($('ckQoderSummary')) {
      $('ckQoderSummary').textContent = rows.length
        ? ('共 ' + rows.length + ' 个 · 已签 ' + doneCount + (arr.length ? '' : '（号池为空，仅当前号）'))
        : '号池为空';
    }
    if (!rows.length) {
      st.hidden = false; st.className = 'state';
      st.textContent = '本机 Qoder 未登录，号池也为空。请先在 Qoder 登录，再到「IDE 账号池」点「添加账号」。';
      list.innerHTML = ''; return;
    }
    const head = '<div class="thead"><div>账号</div><div>积分</div><div>状态</div><div>操作</div></div>';
    list.innerHTML = head + rows.map(a => {
      const email = a.email || '';
      const k = String(email).toLowerCase();
      const isDone = !!doneMap[k];
      const noCamp = !!pendMap[k];
      const quota = !!quotaMap[k];
      const why = msgMap[k] || '';
      const status = isDone
        ? '<span class="stask ok" title="' + esc(why || '上游活动列表确认今天已领取') + '"><span class="mark">✓</span>今日已签到</span>'
        : (noCamp
          ? (quota
            ? '<span class="stask mute2" title="' + esc(why || '同设备当天已有别的账号领走唯一名额') + '"><span class="mark">○</span>名额被同设备其它号占用</span>'
            : '<span class="stask mute2" title="' + esc(why || '上游当前没有下发每日签到活动（限时活动已结束或未开放）；下发后 30 分钟内自动领取') + '"><span class="mark">○</span>暂无签到活动</span>')
          : (a.expired
            ? '<span class="stask todo" title="' + esc(why || '凭证已过期，请重新登录后添加') + '"><span class="mark">!</span>凭证过期</span>'
            : '<span class="stask warn" title="' + esc(why || '上游活动列表里有今天可领的签到活动') + '"><span class="mark">○</span>可签到</span>'));
      return '<div class="trow">' +
        '<div class="who"><div class="nm">' + esc(a.name || '未命名') +
        (a.current ? '<span class="tag mute" style="margin-left:6px">当前登录</span>' : '') + '</div>' +
        '<div class="id" title="' + esc(email) + '">' + esc(email) + '</div></div>' +
        '<div class="credits" data-qk-cred="' + esc(email) + '"><span class="note">查询中…</span></div>' +
        '<div>' + status + '</div>' +
        '<div><button class="xs primary" data-ck-qoder="' + esc(email) + '"' + (isDone ? ' disabled' : '') + '>' +
        (isDone ? '已签到' : '签到') + '</button></div>' +
        '</div>';
    }).join('');
    st.hidden = true;
    list.querySelectorAll('button[data-ck-qoder]').forEach(b => {
      b.onclick = () => doQoderCheckin(b, b.dataset.ckQoder);
    });
    // 每行积分独立查询：各账号用自己的 token 查额度，不需要切号。
    list.querySelectorAll('[data-qk-cred]').forEach(cell => {
      const email = cell.dataset.qkCred;
      api('ide/qoder/usage?email=' + encodeURIComponent(email)).then(u => {
        if (!u || u.phase !== 'ok') {
          cell.innerHTML = '<span class="note">' + esc((u && u.msg) || '无数据') + '</span>';
          return;
        }
        const total = ((u.user_quota && u.user_quota.remaining) || 0) + ((u.add_on_quota && u.add_on_quota.remaining) || 0);
        cell.innerHTML = '<b>' + total.toLocaleString('zh-CN') + '</b>' +
          (u.plan_tier_name ? ' <span class="tag mute">' + esc(u.plan_tier_name) + '</span>' : '');
      }).catch(e => { cell.innerHTML = '<span class="note">' + esc(e.message) + '</span>'; });
    });
  } catch (e) { st.hidden = false; st.className = 'state err'; st.textContent = e.message; }
}

/* ── MiniMax 签到（多账号）────────────────────────────────────────────
   账号池里每个账号各自持有 OAuth token，签到是 token 级操作：
   不需要切号、不需要重启客户端，可以直接对号池逐个领取。 */
async function loadCkMiniMax(quiet) {
  const st = $('ckMmState'), list = $('ckMmList');
  if (!st || !list) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const d = await api('ide/minimax/accounts');
    const arr = d.accounts || [];
    const done = arr.filter(a => a.claimed_today).length;
    if ($('ckMmSummary')) {
      $('ckMmSummary').textContent = arr.length
        ? ('共 ' + arr.length + ' 个 · 已签 ' + done)
        : (d.has_client ? '号池为空（可点「添加当前账号」）' : 'MiniMax Code 未登录');
    }
    if (!arr.length) {
      st.hidden = false; st.className = 'state';
      st.textContent = d.has_client
        ? '号池为空。点右上角「添加当前账号」，把客户端当前登录的账号存进来。'
        : '本机 MiniMax Code 未登录。请先登录，再点「添加当前账号」。';
      list.innerHTML = ''; return;
    }
    const head = '<div class="thead"><div>账号</div><div>积分</div><div>状态</div><div>操作</div></div>';
    list.innerHTML = head + arr.map(a => {
      const id = a.id || '';
      const isDone = !!a.claimed_today;
      const expTxt = a.expires_at_ms
        ? new Date(a.expires_at_ms).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
        : '';
      const status = a.error
        ? '<span class="stask todo" title="' + esc(a.error) + '"><span class="mark">!</span>查询失败</span>'
        : (isDone
          ? '<span class="stask ok"><span class="mark">✓</span>今日已签到</span>'
          : (a.expired
            ? '<span class="stask todo"><span class="mark">!</span>凭证过期</span>'
            : '<span class="stask warn"><span class="mark">○</span>可签到</span>')) +
          (expTxt ? ' <span class="tag mute">' + (a.expired ? '已于 ' : '到期 ') + expTxt + '</span>' : '');
      return '<div class="trow">' +
        '<div class="who"><div class="nm">' + esc(a.user_name || a.label || 'MiniMax 账号') +
        (id && id === (d.current_id || '') ? '<span class="tag mute" style="margin-left:6px">当前</span>' : '') + '</div>' +
        '<div class="id" title="' + esc(a.email || a.user_id || id) + '">' +
        esc(a.email || a.user_id || id) + '</div></div>' +
        '<div class="credits">' + (a.credits == null ? '—' : Number(a.credits).toFixed(0)) + '</div>' +
        '<div>' + status + '</div>' +
        '<div><button class="xs primary" data-mm-ck="' + esc(id) + '"' + (isDone ? ' disabled' : '') + '>' +
        (isDone ? '已签到' : '签到') + '</button>' +
        '<button class="xs" data-mm-renew="' + esc(id) + '" style="margin-left:6px" title="用 refresh_token 换新 token（自动同步回客户端）">续期</button>' +
        '<button class="xs" data-mm-rm="' + esc(id) + '" style="margin-left:6px">移除</button></div>' +
        '</div>';
    }).join('');
    st.hidden = true;
    list.querySelectorAll('button[data-mm-ck]').forEach(b => {
      b.onclick = () => doMmCheckin(b, b.dataset.mmCk);
    });
    list.querySelectorAll('button[data-mm-renew]').forEach(b => {
      b.onclick = () => doMmRenew(b, b.dataset.mmRenew);
    });
    list.querySelectorAll('button[data-mm-rm]').forEach(b => {
      b.onclick = () => doMmRemove(b.dataset.mmRm);
    });
  } catch (e) { st.hidden = false; st.className = 'state err'; st.textContent = e.message; }
}

// doMmRenew 续期单个 MiniMax 账号（或 id 为空 = 全部临期账号）。
async function doMmRenew(btn, id) {
  const old = btn ? btn.textContent : '续期';
  if (btn) { btn.disabled = true; btn.textContent = '续期中…'; }
  try {
    const body = id ? JSON.stringify({ id }) : JSON.stringify({ force: false });
    const d = await api('ide/minimax/renew', { method: 'POST', body });
    const rs = (d && d.results) || [];
    const bad = rs.filter(r => r.phase === 'error');
    if (!bad.length) {
      const r = rs[0] || {};
      toast((r.msg || '凭证已续期') + (r.synced_client ? '（已同步回客户端）' : ''), 'ok');
    } else {
      toast(bad.map(r => (r.label || r.id) + '：' + r.msg).join('；'), 'err');
    }
    loadCkMiniMax(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = old || '续期'; } }
}

// doMmCheckin 签到单个/全部。id 为空 = 号池全部账号。
async function doMmCheckin(btn, id) {
  const old = btn ? btn.textContent : '签到';
  if (btn) { btn.disabled = true; btn.textContent = '签到中…'; }
  try {
    const body = id ? JSON.stringify({ ids: [id] }) : '{}';
    const d = await api('ide/minimax/checkin', { method: 'POST', body });
    const results = d.results || [];
    let text;
    if (results.length > 1 || !id) {
      text = 'MiniMax 签到：成功 ' + (d.claimed_accounts || 0) +
        ' · 今日已领 ' + (d.already_accounts || 0) + ' · 失败 ' + (d.failed_accounts || 0);
      if (d.credits) text += '，新增 +' + d.credits + ' 积分';
    } else {
      const r = results[0] || {};
      text = r.phase === 'claimed' ? ('签到成功 +' + (r.points || 0) + ' 积分')
        : r.phase === 'already' ? '今日已领取'
          : (r.msg || '签到失败');
    }
    toast(text, (d.failed_accounts || 0) > 0 ? 'err' : 'ok');
    if (!viewFrozen()) loadCkMiniMax(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = old || '签到'; } }
}

async function doMmRemove(id) {
  if (!confirm('从 MiniMax 号池移除该账号？\n（不影响 MiniMax Code 当前登录）')) return;
  try {
    await api('ide/minimax/remove', { method: 'POST', body: JSON.stringify({ id }) });
    toast('已移除', 'ok');
    loadCkMiniMax(true);
  } catch (e) { toast(e.message, 'err'); }
}

async function doMmAdd() {
  const btn = $('btnMmAdd');
  const old = btn ? btn.textContent : '添加当前账号';
  if (btn) { btn.disabled = true; btn.textContent = '读取中…'; }
  try {
    const d = await api('ide/minimax/add', { method: 'POST', body: '{}' });
    toast((d.added ? '已添加：' : '已更新：') + (d.label || d.id), 'ok');
    loadCkMiniMax(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = old; } }
}

if ($('btnMmAdd')) $('btnMmAdd').onclick = doMmAdd;
if ($('btnMmRenewAll')) $('btnMmRenewAll').onclick = () => doMmRenew($('btnMmRenewAll'), '');
if ($('btnMmCheckinAll')) $('btnMmCheckinAll').onclick = () => doMmCheckin($('btnMmCheckinAll'));

/* ── 豆包签到（多账号，Cookie 凭据 + CDP 无头浏览器通道）────────────
   豆包签到接口依赖 AGW 设备签名，纯 HTTP 实测被拦；无头浏览器注入 Cookie +
   客户端 UA 打开活动页由页面自身链路完成签到（与 Trae 凭证提取同机制）。
   每号一个会话，约 15-40 秒，全部签到请耐心等。 */
async function loadCkDoubao(quiet) {
  const st = $('ckDbState'), list = $('ckDbList');
  if (!st || !list) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const d = await api('ide/doubao/accounts');
    const arr = (d && d.accounts) || [];
    const done = arr.filter(a => a.checked_in_today).length;
    if ($('ckDbSummary')) {
      $('ckDbSummary').textContent = arr.length
        ? ('共 ' + arr.length + ' 个 · 已签 ' + done)
        : (d.has_client ? '号池为空（本机已装豆包客户端，可一键提取）' : '本机未安装豆包客户端');
    }
    if (!arr.length) {
      st.hidden = false; st.className = 'state';
      st.textContent = d.has_client
        ? '号池为空。点「从客户端提取」把当前登录的豆包账号存进来（客户端会短暂重启）。'
        : '本机未安装豆包客户端。可先安装并登录，再提取；或通过「添加账号 → 豆包」粘贴 Cookie。';
      list.innerHTML = ''; return;
    }
    const head = '<div class="thead"><div>账号</div><div>状态</div><div>操作</div></div>';
    list.innerHTML = head + arr.map(a => {
      const isDone = !!a.checked_in_today;
      const status = isDone
        ? '<span class="stask ok"><span class="mark">✓</span>今日已签到' +
          (a.checked_in_at ? ' <span class="tag mute">' + esc(a.checked_in_at) + '</span>' : '') + '</span>'
        : '<span class="stask warn"><span class="mark">○</span>可签到</span>';
      return '<div class="trow">' +
        '<div class="who"><div class="nm">' + esc(a.label || '豆包账号') +
        (a.source === 'manual' ? '<span class="tag mute" style="margin-left:6px">手动</span>' : '') + '</div>' +
        '<div class="id" title="session 尾号 ' + esc(a.session_tail || '') + ' · ' + esc(a.cookie_count || 0) + ' 个 Cookie">' +
        'session 尾号 ' + esc(a.session_tail || '—') + '</div></div>' +
        '<div>' + status + '</div>' +
        '<div><button class="xs primary" data-db-ck="' + esc(a.id) + '"' + (isDone ? ' disabled' : '') + '>' +
        (isDone ? '已签到' : '签到') + '</button>' +
        '<button class="xs" data-db-rm="' + esc(a.id) + '" style="margin-left:6px">移除</button></div>' +
        '</div>';
    }).join('');
    st.hidden = true;
    list.querySelectorAll('button[data-db-ck]').forEach(b => {
      b.onclick = () => doDoubaoCheckin(b, b.dataset.dbCk);
    });
    list.querySelectorAll('button[data-db-rm]').forEach(b => {
      b.onclick = () => doDoubaoRemove(b.dataset.dbRm);
    });
  } catch (e) { st.hidden = false; st.className = 'state err'; st.textContent = e.message; }
}

async function doDoubaoCheckin(btn, id) {
  const old = btn ? btn.textContent : '签到';
  if (btn) { btn.disabled = true; btn.textContent = '签到中…'; }
  try {
    const body = id ? JSON.stringify({ ids: [id] }) : '{}';
    const d = await api('ide/doubao/checkin', { method: 'POST', body });
    const results = (d && d.results) || [];
    let text;
    if (results.length > 1 || !id) {
      text = '豆包签到：成功 ' + (d.claimed_accounts || 0) +
        ' · 今日已领 ' + (d.already_accounts || 0) + ' · 失败 ' + (d.failed_accounts || 0);
    } else {
      const r = results[0] || {};
      text = r.phase === 'claimed' ? '签到成功'
        : r.phase === 'already' ? '今日已签到'
          : ((r.msg || '签到失败') + (r.sample ? '｜' + r.sample : ''));
    }
    toast(text, (d.failed_accounts || 0) > 0 ? 'err' : 'ok');
    if (!viewFrozen()) loadCkDoubao(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = old || '签到'; } }
}

async function doDoubaoRemove(id) {
  if (!confirm('从豆包签到池移除该账号？')) return;
  try {
    await api('ide/doubao/remove', { method: 'POST', body: JSON.stringify({ id }) });
    toast('已移除', 'ok');
    loadCkDoubao(true);
  } catch (e) { toast(e.message, 'err'); }
}

async function doDoubaoExtract(btn) {
  const old = btn ? btn.textContent : '从客户端提取';
  if (btn) { btn.disabled = true; btn.textContent = '提取中…'; }
  try {
    const d = await api('ide/doubao/add_extract', { method: 'POST', body: '{}' });
    toast(d.msg || '已提取', 'ok');
    loadCkDoubao(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = old || '从客户端提取'; } }
}

if ($('btnDbAdd')) $('btnDbAdd').onclick = () => doDoubaoExtract($('btnDbAdd'));
if ($('btnDbCheckinAll')) $('btnDbCheckinAll').onclick = () => doDoubaoCheckin($('btnDbCheckinAll'), '');

/* 主号池（CodeBuddy / WorkBuddy 国内版账号，即网关 chat 流量的实际来源）逐号签到。
   与 doWbCheckinAll 同构，但走网关自己的号池：overview 取号 → accounts/{uid}/checkin 签。
   两类账号不签：国际版（realm=global，无签到体系）、今日已签（checkin_done）。
   都计入「跳过」，不计入「失败」。 */
async function doMainPoolCheckinAll() {
  const d = await api('overview');
  const all = (d && d.accounts) || [];
  let ok = 0, skip = 0, fail = 0;
  for (const a of all) {
    if (!a.uid) continue;
    if (a.realm === 'global') { skip++; continue; }
    if (a.checkin_done) { skip++; continue; }
    try {
      const r = await api('accounts/' + encodeURIComponent(a.uid) + '/checkin', { method: 'POST' });
      if (r && r.ok) {
        // 上游「今天已签到」也会返回 ok（checkin_done=true），归为已签不算成功。
        if (r.checkin_done) skip++; else ok++;
      } else {
        fail++;
      }
    } catch (_) { fail++; }
  }
  return { ok, skip, fail, total: all.length };
}

/* 总「一键签到」：主号池 → WorkBuddy → Qoder → Trae，全部账号一次签完。
   各平台失败分别计数（不让某个平台的号失效把整条汇总染红）。 */
async function doCheckinAll(btn) {
  const old = btn ? btn.textContent : '一键签到';
  if (btn) { btn.disabled = true; btn.textContent = '签到中…'; }
  const st = $('ckMasterState');
  if (st) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">开始签到 主号池 → WorkBuddy → Qoder → Trae</span>'; }
  const parts = [];
  let failCount = 0;
  try {
    // ① 主号池：网关自己的 CodeBuddy 账号（此前这条链完全没接进「一键签到」）。
    try {
      const mp = await doMainPoolCheckinAll();
      failCount += mp.fail;
      parts.push('主号池 成功 ' + mp.ok + ' · 已签 ' + mp.skip + ' · 失败 ' + mp.fail);
    } catch (e) { failCount++; parts.push('主号池 失败：' + e.message); }

    // ② WorkBuddy IDE 号池。
    const wb = await doWbCheckinAll(null);
    failCount += wb.fail;
    parts.push('WorkBuddy 成功 ' + wb.ok + ' · 已签 ' + wb.skip + ' · 失败 ' + wb.fail);

    try {
      // Qoder 现在是「号池逐号签到」，按 results 汇总（不再有单一 phase）。
      const q = await api('ide/qoder/checkin', { method: 'POST', body: '{}' });
      const qr = (q && q.results) || [];
      const qok = qr.filter(r => r.phase === 'claimed').length;
      const qal = qr.filter(r => r.phase === 'already').length;
      const qnop = qr.filter(r => r.phase === 'pending').length;
      const qf = qr.filter(r => r.phase !== 'claimed' && r.phase !== 'already' && r.phase !== 'pending').length;
      failCount += qf;
      let qtxt = '成功 ' + qok + ' · 已签 ' + qal + (qnop ? ' · 暂无活动 ' + qnop : '') + ' · 失败 ' + qf;
      if (q && q.credits) qtxt += '（+' + q.credits + '）';
      parts.push('Qoder ' + qtxt);
    } catch (e) { failCount++; parts.push('Qoder 失败：' + e.message); }

    try {
      // MiniMax：号池逐账号签到（token 级，无需切号）。
      // 注意：access token 有效期约 1 小时，refresh token 单次有效；客户端没续期时
      // 这里会 401 —— 属于「该号需重新授权」，不是网关故障，如实计数即可。
      const mm = await api('ide/minimax/checkin', { method: 'POST', body: '{}' });
      const mr = (mm && mm.results) || [];
      if (!mr.length) {
        parts.push('MiniMax 无账号');
      } else {
        failCount += (mm.failed_accounts || 0);
        let mtxt = '成功 ' + (mm.claimed_accounts || 0) + ' · 已签 ' + (mm.already_accounts || 0) +
          ' · 失败 ' + (mm.failed_accounts || 0);
        if (mm.credits) mtxt += '（+' + mm.credits + '）';
        parts.push('MiniMax ' + mtxt);
      }
    } catch (e) { failCount++; parts.push('MiniMax 失败：' + e.message); }

    try {
      const t = await api('trae/checkin', { method: 'POST', body: '{}' });
      const arr = (t && t.results) || [];
      const okc = arr.filter(r => r.status === 'claimed').length;
      const alc = arr.filter(r => r.status === 'already').length;
      const fc = arr.filter(r => r.status === 'fail').length;
      failCount += fc;
      parts.push('Trae 成功 ' + okc + ' · 已签 ' + alc + ' · 失败 ' + fc);
    } catch (e) { failCount++; parts.push('Trae 失败：' + e.message); }

    try {
      // 豆包：CDP 无头浏览器通道（每号一个会话，耗时略长）。
      const db = await api('ide/doubao/checkin', { method: 'POST', body: '{}' });
      const dr = (db && db.results) || [];
      if (!dr.length) {
        parts.push('豆包 无账号');
      } else {
        failCount += (db.failed_accounts || 0);
        const dok = dr.filter(r => r.phase === 'claimed').length;
        const dal = dr.filter(r => r.phase === 'already').length;
        parts.push('豆包 成功 ' + dok + ' · 已签 ' + dal + ' · 失败 ' + (db.failed_accounts || 0));
      }
    } catch (e) { failCount++; parts.push('豆包 失败：' + e.message); }

    const text = parts.join('；');
    if (st) { st.className = failCount > 0 ? 'state err' : 'state ok'; st.textContent = '完成：' + text; }
    toast('一键签到完成：' + text, failCount > 0 ? 'err' : 'ok');
    await loadCheckinPool(true);
    loadTraeStatus(true); loadTraeCredits(true); loadQkCredits(true); loadWkCredits(true);
    loadOverview(true);
  } catch (e) {
    if (st) { st.className = 'state err'; st.textContent = e.message; }
    toast(e.message, 'err');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = old || '一键签到'; }
  }
}
if ($('btnCkMaster')) $('btnCkMaster').onclick = () => doCheckinAll($('btnCkMaster'));

/* ── Trae / Qoder 任务 ──────────────────────────────────────────────
   这两个客户端没有独立的业务任务中心（不像 WorkBuddy 有成长任务体系），
   它们在服务端唯一的「任务」就是每日签到领 Credits。这里把它作为任务列出，
   并提供「一键完成」直接执行，不需要再去签到池单独点。 */
async function loadIdeaTasks(quiet) {
  const st = $('ideaTaskState'), list = $('ideaTaskList');
  if (!st || !list) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const [td, qd] = await Promise.all([
      api('trae/accounts').catch(() => null),
      api('ide/qoder/checkin-status').catch(() => null),
    ]);
    const rows = [];
    const tarr = (td && td.accounts) || [];
    for (const a of tarr) {
      rows.push({
        who: a.name || '未命名', sub: a.user_id || '', platform: 'Trae',
        done: !!a.checked_in,
        task: '每日签到（api.trae.cn）',
      });
    }
    // ⚠️ checkin-status 的单号信息在 current 里（顶层没有 email 字段），
    //    此前写 qd.email 永远取不到值 → Qoder 这一行根本不显示。
    //    done 必须用 current 的**单号**口径（顶层 checked_in_today 是聚合值）。
    const qc = (qd && qd.current) || null;
    if (qc && qc.email) {
      rows.push({
        who: qc.name || '当前登录', sub: qc.email, platform: 'Qoder',
        done: !!(qc.checked_in_today || qc.phase === 'claimed'),
        task: '每日签到（100 Credits）',
      });
    }
    if ($('ideaTaskNote')) {
      const done = rows.filter(r => r.done).length;
      $('ideaTaskNote').textContent = rows.length ? ('共 ' + rows.length + ' 项 · 已完成 ' + done) : '';
    }
    if (!rows.length) {
      st.hidden = false; st.className = 'state'; st.textContent = '未找到 Trae / Qoder 账号。'; list.innerHTML = ''; return;
    }
    const head = '<div class="ahead"><div>账号</div><div>任务</div><div>状态</div></div>';
    list.innerHTML = head + rows.map(r =>
      '<div class="arow"><div class="who"><div class="nm">' + esc(r.who) +
      '<span class="tag mute" style="margin-left:6px">' + esc(r.platform) + '</span></div>' +
      '<div class="id">' + esc(r.sub) + '</div></div>' +
      '<div>' + esc(r.task) + '</div>' +
      '<div>' + (r.done
        ? '<span class="stask ok"><span class="mark">✓</span>已完成</span>'
        : '<span class="stask warn"><span class="mark">○</span>待完成</span>') + '</div></div>').join('');
    st.hidden = true;
  } catch (e) { st.hidden = false; st.className = 'state err'; st.textContent = e.message; }
}
async function doIdeaTaskRun(btn) {
  const old = btn ? btn.textContent : '一键完成';
  if (btn) { btn.disabled = true; btn.textContent = '执行中…'; }
  const parts = [];
  try {
    try {
      const t = await api('trae/checkin', { method: 'POST', body: '{}' });
      const ok = t.total_ok || 0, al = t.already || 0, f = t.failed || 0;
      parts.push('Trae 成功 ' + ok + ' · 已签 ' + al + ' · 失败 ' + f);
    } catch (e) { parts.push('Trae 失败：' + e.message); }
    try {
      const q = await api('ide/qoder/checkin', { method: 'POST', body: '{}' });
      parts.push('Qoder ' + (q.phase === 'already' ? '今日已领' : q.phase === 'claimed' ? '签到成功 +' + (q.credits || 0) : (q.msg || q.phase)));
    } catch (e) { parts.push('Qoder 失败：' + e.message); }
    toast('任务完成：' + parts.join('；'), 'ok');
    loadIdeaTasks(true);
  } catch (e) { toast(e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = old || '一键完成'; } }
}
if ($('btnIdeaTaskRun')) $('btnIdeaTaskRun').onclick = () => doIdeaTaskRun($('btnIdeaTaskRun'));

/* ── 积分总览：Trae / Qoder / WorkBuddy 三块 ─────────────────────── */

async function loadQkCredits(quiet) {
  const st = $('qkState'), tb = $('qkBody');
  if (!st || !tb) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; tb.innerHTML = ''; }
  try {
    const d = await api('ide/qoder/accounts');
    const cur = d.current_email || '';
    const arr = d.accounts || [];
    // 号池为空但本机有登录态：仍展示当前号。
    const rows = arr.length ? arr : (cur ? [{ email: cur, name: d.current_name || '当前登录', current: true }] : []);
    if (!rows.length) {
      st.hidden = false; st.className = 'state'; st.textContent = 'Qoder 号池为空，且本机未登录。'; tb.innerHTML = ''; return;
    }
    st.hidden = true;
    tb.innerHTML = rows.map(a => {
      const email = a.email || '';
      return '<tr><td class="who"><div class="nm">' + esc(a.name || '未命名') +
        (a.current ? '<span class="tag mute" style="margin-left:6px">当前</span>' : '') + '</div>' +
        '<div class="id">' + esc(email) + '</div></td>' +
        '<td class="num" data-qk-total="' + esc(email) + '"><span class="note">查询中…</span></td>' +
        '<td class="num"><b>+100</b></td></tr>';
    }).join('');
    // 逐号查额度：各账号用自己的 token，与「当前登录」无关。
    tb.querySelectorAll('[data-qk-total]').forEach(cell => {
      const email = cell.dataset.qkTotal;
      api('ide/qoder/usage?email=' + encodeURIComponent(email)).then(u => {
        if (!u || u.phase !== 'ok') { cell.innerHTML = '<span class="note">' + esc((u && u.msg) || '无数据') + '</span>'; return; }
        const pkg = (u.user_quota && u.user_quota.remaining) || 0;
        const addon = (u.add_on_quota && u.add_on_quota.remaining) || 0;
        cell.innerHTML = '<b>' + (pkg + addon).toLocaleString('zh-CN') + '</b>' +
          (u.plan_tier_name ? ' <span class="tag mute">' + esc(u.plan_tier_name) + '</span>' : '') +
          '<div class="note">套餐 ' + pkg + ' + 附加 ' + addon + '</div>';
      }).catch(e => { cell.innerHTML = '<span class="note">' + esc(e.message) + '</span>'; });
    });
  } catch (e) { st.hidden = false; st.className = 'state err'; st.textContent = e.message; }
}

async function loadWkCredits(quiet) {
  const st = $('wkState'), tb = $('wkBody');
  if (!st || !tb) return;
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; tb.innerHTML = ''; }
  try {
    const d = await api('ide/workbuddy/accounts');
    const all = (d && d.ok ? d.accounts : []) || [];
    if (!all.length) {
      st.hidden = false; st.className = 'state'; st.textContent = 'WorkBuddy 号池为空。'; tb.innerHTML = ''; return;
    }
    st.hidden = true;
    tb.innerHTML = all.map(a => {
      const uid = wbUid(a);
      return '<tr><td class="who"><div class="nm">' + esc(a.nickname || '未命名') +
        '<span class="tag mute" style="margin-left:6px">' + esc(wbEditionLabel(a.edition)) + '</span></div>' +
        '<div class="id">' + esc(uid || '') + '</div></td>' +
        '<td class="num"><span class="note" data-wk-out="' + esc(uid) + '">查询中…</span></td>' +
        '<td class="num"><span style="color:var(--ink-3)">' + (a.checked_in_today ? '今日已签' : '—') + '</span></td></tr>';
    }).join('');
    const query = async (uid, out) => {
      if (out) out.textContent = '查询中…';
      try {
        const r = await api('ide/workbuddy/usage?uid=' + encodeURIComponent(uid));
        if (!out) return;
        if (r.ok && r.usage) {
          if (r.usage.remaining != null) {
            out.innerHTML = '<b>' + r.usage.remaining + '</b>' + (r.usage.total != null ? ' / ' + r.usage.total : '');
            return;
          }
          const found = {};
          (function walk(o) {
            if (!o || typeof o !== 'object') return;
            for (const k in o) {
              const v = o[k];
              if (v && typeof v === 'object') walk(v);
              else if (/credit|balance|remaining|quota|available|used|total/i.test(String(k))) found[k] = v;
            }
          })(r.usage);
          const keys = Object.keys(found).slice(0, 4);
          out.textContent = keys.length ? keys.map(k => k + ':' + found[k]).join(' · ') : '已返回额度数据';
        } else out.textContent = r.error || '无数据';
      } catch (e) { if (out) out.textContent = e.message; }
    };
    // 进页面即自动查询每个账号额度，用户无需手动点。
    all.forEach(a => {
      const uid = wbUid(a);
      if (!uid) return;
      const out = tb.querySelector('[data-wk-out="' + CSS.escape(uid) + '"]');
      query(uid, out);
    });
  } catch (e) { st.hidden = false; st.className = 'state err'; st.textContent = e.message; }
}
