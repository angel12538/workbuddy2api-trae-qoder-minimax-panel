/* 账号表排序（issue #111）：自然序账号名、额度两种口径、取不到值沉底、三态循环。
 *
 * 为什么测：这三条都是「看起来对、实际错」的高发区——
 *   1. Z Z2 与 Z Z10 用词典序会排反；
 *   2. 额度拉取失败的账号若被当成 0，会挤在「额度用尽」中间，用户以为它还有量；
 *   3. 排序方向反转时「沉底」不能跟着翻（否则失败账号跑到最上面）。
 */
const fs = require('fs');
const path = require('path');
const ROOT = process.env.QD_DASH_ROOT || path.join(__dirname, '..');
const html = fs.readFileSync(path.join(ROOT, 'dashboard.html'), 'utf8');
let PASS = 0, FAIL = 0;
function check(name, cond, extra){
  if(cond){ PASS++; console.log('  ok   ' + name); }
  else { FAIL++; console.log('  FAIL ' + name + (extra !== undefined ? '  ' + JSON.stringify(extra) : '')); }
}
function slice(from, to){
  const a = html.indexOf(from), b = html.indexOf(to);
  return (a >= 0 && b > a) ? html.slice(a, b) : '';
}

const SORT = slice('// ---- 账号表排序（issue #111）', 'function renderAccounts(){');
const THEAD_HEAD = 'box.innerHTML = ';
const THEAD = slice(THEAD_HEAD + "'<div class=\"table-wrap\"><table><thead><tr>'", "+ '<div class=\"legend\">");
check('锚点：排序函数区已抽到', SORT.length > 1200, SORT.length);
check('锚点：表头区已抽到', THEAD.length > 300, THEAD.length);
check('排序函数区不直接摸 DOM（除 renderAccounts 调用）',
  !/getElementById|innerHTML/.test(SORT));

const store = {};
const localStorage = {
  getItem: k => (k in store ? store[k] : null),
  setItem: (k, v) => { store[k] = String(v); },
};
let RENDER_CALLS = 0;
const USAGE = {};
const api = new Function('localStorage', 'USAGE_BY_ACCOUNT', 'renderAccounts',
  SORT + '; return { sortAccounts, acctSortValue, sortAccountList, sortTh, state: () => ACCT_SORT };')(
  localStorage, USAGE, () => { RENDER_CALLS++; });

const A = (uid, name, extra) => Object.assign({ uid: uid, nickname: name, realm: 'intl',
  enabled: true, source: 'oauth', expiresIn: '30d' }, extra || {});

api.sortAccounts('name');
let rows = ['Z Z10', 'Z Z2', 'Z Z1', 'Z Z20'].map((n, i) => A('u' + i, n));
let sorted = api.sortAccountList(rows).map(r => r.nickname);
check('账号列按自然序（Z Z2 在 Z Z10 之前）',
  JSON.stringify(sorted) === JSON.stringify(['Z Z1', 'Z Z2', 'Z Z10', 'Z Z20']), sorted);

rows = [A('a', '  Bob'), A('b', 'alice'), A('c', ' Cathy ')];
sorted = api.sortAccountList(rows).map(r => r.nickname);
check('账号列忽略大小写与首尾空白',
  JSON.stringify(sorted) === JSON.stringify(['alice', '  Bob', ' Cathy ']), sorted);

rows = [A('uid-zzz', ''), A('uid-aaa', '')];
sorted = api.sortAccountList(rows).map(r => r.uid);
check('昵称缺失时退回 UID 排序',
  JSON.stringify(sorted) === JSON.stringify(['uid-aaa', 'uid-zzz']), sorted);

api.sortAccounts('credits');
const creditRows = [
  A('r1', 'r1', { credits: { remain: 100, used: 0, size: 700 } }),
  A('r2', 'r2', { credits: { remain: 684, used: 16, size: 700 } }),
  A('r3', 'r3', { creditsDegraded: { reason: 'envelopes HTTP 403 code 112' } }),
  A('r4', 'r4', { credits: { remain: 0, used: 200, size: 200 } }),
];
sorted = api.sortAccountList(creditRows).map(r => r.uid);
check('剩余额度升序：0 → 100 → 684，失败的沉底',
  JSON.stringify(sorted) === JSON.stringify(['r4', 'r1', 'r2', 'r3']), sorted);

api.sortAccounts('credits');
sorted = api.sortAccountList(creditRows).map(r => r.uid);
check('降序时失败账号仍然沉底（不跟着翻到最上面）',
  JSON.stringify(sorted) === JSON.stringify(['r2', 'r1', 'r4', 'r3']), sorted);

api.sortAccounts('credits', { shiftKey: true });
sorted = api.sortAccountList(creditRows).map(r => r.uid);
check('Shift+点击按已用额度排序',
  JSON.stringify(sorted) === JSON.stringify(['r2', 'r4', 'r1', 'r3']), sorted);
check('shift 排序键记为 used', api.state().key === 'used', api.state());

const usageRows = [A('x1', 'x1'), A('x2', 'x2'), A('x3', 'x3')];
USAGE.x1 = { requests: 9, total_tokens: 100, cached_tokens: 5 };
USAGE.x2 = { requests: 2, total_tokens: 5000, cached_tokens: 0 };
USAGE.x3 = { requests: 5, total_tokens: 300, cached_tokens: 42 };
api.sortAccounts('requests');
check('请求数升序',
  JSON.stringify(api.sortAccountList(usageRows).map(r => r.uid))
  === JSON.stringify(['x2', 'x3', 'x1']));
api.sortAccounts('tokens');
check('总 token 升序',
  JSON.stringify(api.sortAccountList(usageRows).map(r => r.uid))
  === JSON.stringify(['x1', 'x3', 'x2']));
api.sortAccounts('cached');
check('缓存升序',
  JSON.stringify(api.sortAccountList(usageRows).map(r => r.uid))
  === JSON.stringify(['x2', 'x1', 'x3']));

const stateRows = [
  A('s1', 's1'),
  A('s2', 's2', { enabled: false }),
  A('s3', 's3', { inCooldown: true, cooldownFor: 30 }),
  A('s4', 's4', { expiresIn: 'expired' }),
  A('s5', 's5', { lastError: 'upstream 500' }),
];
api.sortAccounts('state');
check('状态按可用程度排序',
  JSON.stringify(api.sortAccountList(stateRows).map(r => r.uid))
  === JSON.stringify(['s1', 's5', 's3', 's2', 's4']),
  api.sortAccountList(stateRows).map(r => r.uid));

api.sortAccounts('name');
check('第一次点击：升序', api.state().key === 'name' && api.state().dir === 1);
api.sortAccounts('name');
check('第二次点击：降序', api.state().key === 'name' && api.state().dir === -1);
api.sortAccounts('name');
check('第三次点击：回默认（不再排序）', api.state().key === '' && api.state().dir === 1);
const before = api.sortAccountList(rows).map(r => r.uid);
check('默认顺序 = 输入顺序（后端原序）',
  JSON.stringify(before) === JSON.stringify(rows.map(r => r.uid)), before);

api.sortAccounts('tokens');
check('排序状态写进 localStorage',
  JSON.parse(store['qd-acct-sort'] || '{}').key === 'tokens', store['qd-acct-sort']);
check('每次点击都会重渲染', RENDER_CALLS >= 7, RENDER_CALLS);

check('表头 7 列可排序（版本/账号/状态/积分/请求/token/缓存）',
  (THEAD.match(/sortTh\(/g) || []).length === 7, (THEAD.match(/sortTh\(/g) || []).length);
check('护栏与操作列不可排序（不是数值，排序无意义）',
  THEAD.includes('护栏') && THEAD.includes('操作'));
check('积分列提示 Shift+点击', /Shift\+点击/.test(THEAD));

api.sortAccounts('credits');
const th = api.sortTh('credits', '积分', 'width:120px');
check('当前排序列带 ▲ 与 sorted 类', th.includes('▲') && th.includes('sorted'), th);
api.sortAccounts('credits');
check('降序显示 ▼', api.sortTh('credits', '积分', '').includes('▼'));
api.sortAccounts('credits');
check('默认态无箭头', !/[▲▼]/.test(api.sortTh('credits', '积分', '')));
check('表头 onclick 带 event（供 Shift 判定）',
  api.sortTh('credits', '积分', '').includes(", event)"));

console.log('');
console.log('SUMMARY: TOTAL ' + (PASS + FAIL) + ' checks, ' + PASS + ' passed, ' + FAIL + ' failed');
console.log('RESULT: ' + (FAIL ? 'RED' : 'GREEN') + ' (exit ' + (FAIL ? 1 : 0) + ')');
process.exit(FAIL ? 1 : 0);