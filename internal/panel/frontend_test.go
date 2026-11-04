package panel

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（v1.11.3/1.11.4 两连炸后补的运行时闸门）：
// node + DOM 桩执行 app.js（含按 hash 落到各视图的 go() 顶层调用），抓 TDZ/
// ReferenceError 类运行时错误——Go 侧 frontend_test 不执行 JS，语法层检查对此全盲。
// 无 node 的环境跳过（CI/精简机不受影响）；harness 与 app.js 同判（app.js 顶层
// start() 的 setInterval 会让 node 事件循环不退出，故成功路径显式 exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS smoke skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#taskscenter' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#packages"} {
		cmd := exec.Command(node, hf.Name(), "app.js")
		cmd.Dir = "." // 测试工作目录 = internal/panel
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app.js 顶层求值 %s 崩溃: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js smoke %s 未通过:\n%s", hash, out)
		}
	}
}

// 积分扣除维度的格式必须稳定，且缺样本/缺匹配 Token 时不能伪造比例。
func TestAppJSCreditDimensionFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
	const start = src.indexOf('function trimFixed');
const end = src.indexOf('function usStat');
if (start < 0 || end < 0) throw new Error('credit helpers not found');
const ctx = { Number, String, RegExp };
vm.createContext(ctx);
vm.runInContext(
  src.slice(start, end) +
  '\nthis.fmtCredit=fmtCredit; this.fmtCreditRatio=fmtCreditRatio; this.fmtModelRate=fmtModelRate;',
  ctx
);
process.stdout.write(JSON.stringify({
  credit: ctx.fmtCredit(1.25),
  zero: ctx.fmtCredit(0),
  hundred: ctx.fmtCredit(100),
  ratio: ctx.fmtCreditRatio(12.5, 2, 400),
  noSamples: ctx.fmtCreditRatio(12.5, 0, 400),
  noTokens: ctx.fmtCreditRatio(12.5, 2, 0),
  rate: ctx.fmtModelRate('0.5'),
  noRate: ctx.fmtModelRate(''),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"credit":"1.25","zero":"0","hundred":"100","ratio":"12.5 / 1M","noSamples":"—","noTokens":"—","rate":"x0.5","noRate":"—"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit formatting=%s want %s", out, want)
	}
}

// 模型限流时间必须同时支持上游 reset_at、网关 until 和无重置时间三种形态。
func TestAppJSRateLimitMeta(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; rate limit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function dur(');
const end = src.indexOf('function rateLimitRowsHtml');
if (start < 0 || end < 0) throw new Error('rate limit helpers not found');
const ctx = { Date, Number, String, Math };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.rateLimitMeta=rateLimitMeta;', ctx);
const now = new Date(2026, 8, 28, 14, 0, 0).getTime();
const reset = new Date(2026, 8, 28, 16, 0, 0).getTime();
const until = new Date(2026, 8, 28, 15, 0, 0).getTime();
const rate = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit', reset_at: new Date(reset).toISOString(), until: new Date(until).toISOString() }, now);
const unavailable = ctx.rateLimitMeta({ model: 'missing', kind: 'model_unavailable', until: new Date(until).toISOString() }, now);
const unknown = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit' }, now);
process.stdout.write(JSON.stringify({
  rate: rate.detail,
  unavailable: unavailable.detail,
  unknown: unknown.detail,
}));`
	f, err := os.CreateTemp(t.TempDir(), "rate-limit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("rate-limit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"rate":"预计 2026-09-28 16:00 解封（剩余 2时00分） · 网关最快 1时00分 后重试","unavailable":"预计 1时00分 后重试","unknown":"预计解封时间未知"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("rate-limit formatting=%s want %s", out, want)
	}
}

// 请求记录行必须紧凑、可读，并带上调用来源（IP / UA）；来源缺失时以 — 兜底。
func TestAppJSRequestLogFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request log formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const reqStart = src.indexOf('function requestLogText');
const reqEnd = src.indexOf('function fmtBytes');
if ([escStart, escEnd, fmtStart, fmtEnd, reqStart, reqEnd].some(v => v < 0)) throw new Error('request log helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(reqStart, reqEnd) +
  '\nthis.requestLogText=requestLogText;',
  ctx
);
const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const good = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12, request_id: 'req-1', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0' };
const noSource = { ...good, request_id: 'req-3', client_ip: '', user_agent: '' };
const cached = { ...good, request_id: 'req-2', cache_hit_tokens: 2257, cache_miss_tokens: 43 };
process.stdout.write(JSON.stringify({
  good: ctx.requestLogText(good),
  noSource: ctx.requestLogText(noSource),
  cached: ctx.requestLogText(cached),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-log-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request log formatting node test failed: %v\n%s", err, out)
	}
	text := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-1"
	noSource := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | — | — | 1.25s | 2.3k tok | 0.12 credit | req-3"
	cached := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | 命中 98.1% | req-2"
	want := `{"good":` + strconv.Quote(text) + `,"noSource":` + strconv.Quote(noSource) + `,"cached":` + strconv.Quote(cached) + `}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request log formatting=%s want %s", out, want)
	}
}

// 请求记录筛选：IP / UA / 模型 / 账号 / 请求 ID 的包含匹配（空格分词 AND）+ 结果精确匹配。
func TestAppJSRequestMatch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function reqMatch');
const end = src.indexOf('function reqOutcomeTag');
if (start < 0 || end < 0) throw new Error('reqMatch not found');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.reqMatch=reqMatch;', ctx);
const base = { outcome: 'success', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0', model: 'cn:glm-5.3', account: '示例(uid8)', request_id: 'req-1' };
const other = { outcome: 'http_error', client_ip: '198.51.100.4', user_agent: 'Mozilla/5.0 Chrome/120', model: 'global:hy3', account: '甲(uid9)', request_id: 'req-2' };
const rows = [base, other];
const pick = f => rows.filter(e => ctx.reqMatch(e, f)).map(e => e.request_id);
process.stdout.write(JSON.stringify({
  all: pick({ q: '', outcome: '' }),
  byIP: pick({ q: '203.0.113', outcome: '' }),
  byUA: pick({ q: 'chrome/120', outcome: '' }),
  byModel: pick({ q: 'glm', outcome: '' }),
  multiKw: pick({ q: 'glm success', outcome: '' }),
  multiMiss: pick({ q: 'glm chrome', outcome: '' }),
  byOutcome: pick({ q: '', outcome: 'http_error' }),
  combined: pick({ q: '198.51', outcome: 'http_error' }),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request filter node test failed: %v\n%s", err, out)
	}
	// q 对 outcome 不参与匹配（outcome 有独立下拉），multiKw 里的 success 命中不了任何字段。
	const want = `{"all":["req-1","req-2"],"byIP":["req-1"],"byUA":["req-2"],"byModel":["req-1"],"multiKw":[],"multiMiss":[],"byOutcome":["req-2"],"combined":["req-2"]}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request filter=%s want %s", out, want)
	}
}

// 模型按条件查询：域 / 能力 / 档位 / 价格 / 关键词，以及倍率、上下文、输出排序。
func TestAppJSModelFilter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function mdRateValue');
const end = src.indexOf('function mdRowHtml');
if (start < 0 || end < 0) throw new Error('model filter helpers not found');
const ctx = { Number, String, Array, Object, isFinite, parseFloat };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.mdMatch=mdMatch; this.mdSortList=mdSortList; this.mdRateValue=mdRateValue;', ctx);
const models = [
  { id: 'cn:glm-5.2', name: 'GLM-5.2', vendor: 'Zhipu', tags: ['视觉'], supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: true, supported_efforts: ['high', 'xhigh'], default_effort: 'high', is_default: false, credits: '0.79', promo_factor: 0.5, promo_credits: '0.40', promo_label: '夜间折扣', context_length: 1000000, max_output_tokens: 131000 },
  { id: 'cn:hy3', name: 'Hy3', supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: false, supported_efforts: ['low', 'high'], default_effort: 'high', is_default: false, credits: '0', promo_factor: 0, promo_credits: '0', promo_label: '限时免费', context_length: 192000, max_output_tokens: 64000 },
  { id: 'global:hy3', name: 'Hy3 Global', supports_tool_call: false, supports_images: false, supports_reasoning: false, supported_efforts: [], is_default: false, credits: '0.11', context_length: 1000000, max_output_tokens: 393000 },
  { id: 'cn:auto', name: 'Auto', supports_tool_call: true, supports_images: true, supports_reasoning: true, is_default: true, credits: null, context_length: 256000, max_output_tokens: 32000 },
];
const ids = list => list.map(m => m.id);
const filter = f => ids(ctx.mdSortList(models.filter(m => ctx.mdMatch(m, f)), f));
process.stdout.write(JSON.stringify({
  all: ids(models),
  realm: filter({ realm: 'cn' }),
  tool: filter({ cap: 'tool' }),
  vision: filter({ cap: 'vision' }),
  reasoning: filter({ cap: 'reasoning' }),
  isDefault: filter({ cap: 'default' }),
  effortOff: filter({ effort: 'off' }),
  effortLow: filter({ effort: 'low' }),
  free: filter({ promo: 'free' }),
  promo: filter({ promo: 'promo' }),
  discount: filter({ promo: 'discount' }),
  q: filter({ q: 'glm zhipu' }),
  qMiss: filter({ q: 'glm nosuch' }),
  sortRate: filter({ sort: 'rate' }),
  sortContext: filter({ sort: 'context' }),
  sortOutput: filter({ sort: 'output' }),
  sortName: filter({ sort: 'name' }),
  rateFree: ctx.mdRateValue(models[1]),
  rateMissing: ctx.mdRateValue(models[3]),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model filter node test failed: %v\n%s", err, out)
	}
	const want = `{"all":["cn:glm-5.2","cn:hy3","global:hy3","cn:auto"],` +
		`"realm":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"tool":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"vision":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"reasoning":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"isDefault":["cn:auto"],` +
		`"effortOff":["cn:glm-5.2"],` +
		`"effortLow":["cn:hy3"],` +
		`"free":["cn:hy3"],` +
		`"promo":["cn:glm-5.2","cn:hy3"],` +
		`"discount":["cn:glm-5.2"],` +
		`"q":["cn:glm-5.2"],` +
		`"qMiss":[],` +
		`"sortRate":["cn:hy3","global:hy3","cn:glm-5.2","cn:auto"],` +
		`"sortContext":["cn:glm-5.2","global:hy3","cn:auto","cn:hy3"],` +
		`"sortOutput":["global:hy3","cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"sortName":["cn:auto","cn:glm-5.2","cn:hy3","global:hy3"],` +
		`"rateFree":0,"rateMissing":null}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("model filter=%s\nwant %s", out, want)
	}
}

// 用量时序图的柱体类名不得叫 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
// height 是 rect 的 CSS 几何属性——同名类会把每根柱子压成 3px 高，图看起来"没数据"。
// 这个坑只能在浏览器里看出来，所以在这里钉住类名。
func TestAppJSUsageChartBarClass(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; usage chart test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function parsePointTime');
const end = src.indexOf('function fmtTokTip');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
if ([start, end, escStart, escEnd, fmtStart, fmtEnd].some(v => v < 0)) throw new Error('usage chart helpers not found');
const host = { innerHTML: '', textContent: '' };
const ctx = {
  Date, Number, String, Math, RegExp, isNaN, Set, Array, Object, Infinity,
  document: { getElementById: () => host },
  $: () => host,
};
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(start, end) +
  '\nthis.renderUsageChart=renderUsageChart;', ctx);
const series = [
  { t: '2026-09-30T09', scope: 'hour', prompt_tokens: 35, completion_tokens: 16, total_tokens: 51, requests: 1 },
  { t: '2026-09-30T11', scope: 'hour', prompt_tokens: 978324, completion_tokens: 20621, total_tokens: 998945, requests: 39 },
  { t: '2026-09-30T13', scope: 'hour', prompt_tokens: 27400952, completion_tokens: 104913, total_tokens: 27505865, requests: 200 },
];
ctx.renderUsageChart(series);
const svg = host.innerHTML;
process.stdout.write(JSON.stringify({
  hasUsbar: svg.includes('class="usbar"'),
  hasBareBar: /class="bar"/.test(svg),
  hasGradient: svg.includes('usGradP') && svg.includes('usGradC'),
  barCount: (svg.match(/class="usbar"/g) || []).length,
  hasPeak: svg.includes('峰值'),
  hasAvg: svg.includes('均值'),
  emptyState: (function () { ctx.renderUsageChart([]); return host.innerHTML.includes('us-empty'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "usage-chart-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("usage chart node test failed: %v\n%s", err, out)
	}
	const want = `{"hasUsbar":true,"hasBareBar":false,"hasGradient":true,"barCount":6,"hasPeak":true,"hasAvg":true,"emptyState":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("usage chart=%s\nwant %s", out, want)
	}
}

// 时间范围控件：预设 → 查询参数的映射。要点：
//   - 「今天」必须发浏览器本地时区的 00:00（服务端时区未必一致），且不带 to；
//   - 滚动预设 rolling=true 发 hours（服务端整点对齐），rolling=false 折算成 from；
//   - 「全部历史」两者都不发；「自定义」发用户挑的 from/to。
func TestAppJSTimeRangeQuery(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; time range test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const TRANGE_PRESETS');
const end = src.indexOf('function rateLimitMeta');
if (start < 0 || end < 0 || end < start) throw new Error('trange helpers not found');
const host = { innerHTML: '' };
const ctx = {
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams,
  document: { getElementById: () => host },
  $: () => host,
  esc: s => String(s == null ? '' : s),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.trangeState=trangeState; this.trangeQuery=trangeQuery; this.trangeLabel=trangeLabel; this.trangeMidnight=trangeMidnight;', ctx);
const q = (preset, rolling) => {
  ctx.trangeState('t').preset = preset;
  return ctx.trangeQuery('t', rolling).toString();
};
const secOf = d => String(Math.floor(d.getTime() / 1000));
const approx = (qs, wantSec) => {
  const m = /(?:^|&)from=(\d+)/.exec(qs);
  return m && Math.abs(Number(m[1]) - wantSec) < 120;
};
const now = Date.now();
const todayQ = q('today', true);
process.stdout.write(JSON.stringify({
  todayIsMidnight: todayQ === 'from=' + secOf(ctx.trangeMidnight()),
  todayNoTo: !/to=/.test(todayQ),
  rolling24: q('24', true),
  rolling72: q('72', true),
  rolling0: q('0', true),
  log0: q('0', false),
  log24From: approx(q('24', false), Math.floor((now - 24 * 3600e3) / 1000)),
  log24HasHours: /hours=/.test(q('24', false)),
  log7dFrom: approx(q('168', false), Math.floor((now - 168 * 3600e3) / 1000)),
  custom: (function () {
    const st = ctx.trangeState('t');
    st.preset = 'custom';
    st.from = new Date(2026, 8, 30, 9, 0, 0);
    st.to = new Date(2026, 8, 30, 18, 30, 0);
    return ctx.trangeQuery('t', true).toString();
  })(),
  labelCustom: ctx.trangeLabel('t'),
  labelToday: (function () { ctx.trangeState('t').preset = 'today'; return ctx.trangeLabel('t'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "trange-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cmd := exec.Command(node, f.Name(), "app.js")
	// 两侧都固定用 UTC：node 子进程的 TZ 显式设成 UTC（Windows 上 node 认 TZ、
	// Go 不认，两边不统一时「本地时刻 → epoch」的断言会随时区假失败），Go 侧的
	// 期望值同样按 UTC 算。测的是「自定义区间 → from/to epoch」的映射本身，
	// 「今天 = 本地零点」由 todayIsMidnight 在 node 内部自洽验证。
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("time range node test failed: %v\n%s", err, out)
	}
	local := func(h, m int) string {
		return strconv.FormatInt(time.Date(2026, 9, 30, h, m, 0, 0, time.UTC).Unix(), 10)
	}
	want := `{"todayIsMidnight":true,"todayNoTo":true,` +
		// rolling0 必须是 "hours=0"（显式全部历史）。此前期望值是空串——那恰好把
		// issue #121 的错误行为固化成了断言：空 query 会被后端的 72 小时缺省接管，
		// 于是「全部历史」显示成「近 3 天」。
		// log0 仍为空：请求记录端点没有缺省窗口，不传 from/to 就是全部历史。
		`"rolling24":"hours=24","rolling72":"hours=72","rolling0":"hours=0","log0":"",` +
		`"log24From":true,"log24HasHours":false,"log7dFrom":true,` +
		`"custom":"from=` + local(9, 0) + `&to=` + local(18, 30) + `",` +
		`"labelCustom":"9-30 09:00 → 9-30 18:30","labelToday":"今天"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("time range=%s\nwant %s", out, want)
	}
}

// 配置表单与 CFG_MAP 必须一一对应，且面板声称"可在线改"的热生效键必须真的
// 出现在表单里。
//
// 为什么需要：`logging.request_client_info` 曾经在表单里存在过，后来在某次改动中
// 被连带删掉，而 Go 侧的配置键、livecfg 热生效通路、README 的描述都还在——面板
// 少了一个开关而 Go 测试全绿，只有人肉点开配置页才会发现。这里把"表单字段 ↔
// CFG_MAP"与"关键热改键必须在表单里"两条都钉住。
func TestConfigFormMatchesCFGMap(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)

	// CFG_MAP 块（下面两条检查共用）。
	mapBlock := js[strings.Index(js, "const CFG_MAP = {"):]
	mapBlock = mapBlock[:strings.Index(mapBlock, "\n};")]
	// 不能按行首匹配：CFG_MAP 里多个键写在同一行（`a: [...], b: [...]`），只有行首
	// 那个带换行缩进。按「前面是行首或分隔符」判定才不漏。
	inMap := func(name string) bool {
		return regexp.MustCompile(`(?:^|[\s,{])` + regexp.QuoteMeta(name) + `:\s*\[`).MatchString(mapBlock)
	}

	// 1) 表单里的每个 name 都要有 CFG_MAP 条目（否则收集/回填都拿不到它）。
	form := html[strings.Index(html, `<form id="cfgForm">`):]
	form = form[:strings.Index(form, "</form>")]
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`name="([a-z_0-9]+)"`).FindAllStringSubmatch(form, -1) {
		names[m[1]] = true
	}
	if len(names) == 0 {
		t.Fatal("未从配置表单解析出任何 name 字段")
	}
	for n := range names {
		if !inMap(n) {
			t.Errorf("表单字段 %q 在 CFG_MAP 里没有条目（保存时会被静默丢弃）", n)
		}
	}

	// 2) CFG_MAP 里的每个键都要在表单里有控件（否则回填/保存是空转）。
	for _, m := range regexp.MustCompile(`(?:^|[\s,{])([a-z_0-9]+):\s*\[`).FindAllStringSubmatch(mapBlock, -1) {
		if !names[m[1]] {
			t.Errorf("CFG_MAP 键 %q 在配置表单里没有对应控件", m[1])
		}
	}

	// 3) 明确断言这一个键：后端有配置项、README 说面板可改，UI 不能少。
	if !strings.Contains(js, "request_client_info: ['logging', 'request_client_info']") {
		t.Error("CFG_MAP 缺 request_client_info 条目")
	}
	if !names["request_client_info"] {
		t.Error("配置表单缺「记录调用来源」开关（logging.request_client_info）")
	}
}

// 同到期时间按面额降序；其余未用完包与零/负余额包分别聚合。
func TestAppJSDetailGroups(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; detail groups test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_DEFAULT_DETAIL_LIMIT');
const end = src.indexOf('function renderPackages');
if (start < 0 || end < 0) throw new Error('detail group functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.pkDetailGroups = pkDetailGroups; this.pkDetailLimit = pkDetailLimit;', ctx);
const input = [
  { id: 'small-late', size: 100, remain: 1, expires_at: 400 },
  { id: 'zero-early-b', size: 200, remain: 0, expires_at: 200 },
  { id: 'small-early', size: 100, remain: 2, expires_at: 200 },
  { id: 'large-unknown', size: 300, remain: 3, end_time: '' },
  { id: 'zero-early-a', size: 200, remain: -1, expires_at: 200 },
  { id: 'small-unknown', size: 100, remain: 1, end_time: '' },
  { id: 'large-early', size: 300, remain: 4, expires_at: 200 },
  { id: 'zero-late', size: 300, remain: 0, expires_at: 300 },
];
const before = input.map(p => p.id).join(',');
const out = ctx.pkDetailGroups(input, 2);
process.stdout.write(JSON.stringify({
  visible: out.visible.map(p => p.id),
  rest: out.rest.map(p => p.id),
  used: out.used.map(p => p.id),
  restSize: out.restSize,
  restRemain: out.restRemain,
  usedSize: out.usedSize,
  defaultLimit: ctx.pkDetailLimit({}),
  configuredLimit: ctx.pkDetailLimit({ panel: { package_detail_limit: 7 } }),
  unchanged: input.map(p => p.id).join(',') === before,
}));`
	f, err := os.CreateTemp(t.TempDir(), "detail-groups-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("detail groups node test failed: %v\n%s", err, out)
	}
	const want = `{"visible":["large-early","small-early"],"rest":["small-late","large-unknown","small-unknown"],"used":["zero-early-b","zero-early-a","zero-late"],"restSize":500,"restRemain":5,"usedSize":700,"defaultLimit":5,"configuredLimit":7,"unchanged":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("detail groups=%s want %s", out, want)
	}
}

// 精确剩余天数聚合、账号内按总余额钳制、无到期批次不进入图表。
func TestAppJSExpirySummary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry summary test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_ACCOUNT_COLORS');
const end = src.indexOf('function renderExpiryDistribution');
if (start < 0 || end < 0) throw new Error('expiry summary functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.summarizeCreditDays = summarizeCreditDays; this.pkAccountColorMap = pkAccountColorMap;', ctx);
const day = 86400000, now = 100000;
const out = ctx.summarizeCreditDays([
  { uid: 'a', remain: 100, packages: [
    { name: 'soon-a', remain: 30, expires_at: now + day },
    { name: 'later', remain: 70, expires_at: now + 7 * day },
  ] },
  { uid: 'b', remain: 55, packages: [
    { name: 'soon-b', remain: 20, expires_at: now + day },
    { name: 'unknown', remain: 5, end_time: '' },
  ] },
  { uid: 'err', error: 'offline' },
], now);
process.stdout.write(JSON.stringify({
  rows: out.rows.map(row => ({ days: row.days, credits: row.credits })),
  accountCount: out.accountCount,
  unavailable: out.unavailable,
  colorA: ctx.pkAccountColorMap([{ uid: 'b' }, { uid: 'a' }]).get('a'),
  colorB: ctx.pkAccountColorMap([{ uid: 'a' }, { uid: 'b' }]).get('b'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry summary node test failed: %v\n%s", err, out)
	}
	const want = `{"rows":[{"days":1,"credits":50},{"days":7,"credits":70}],"accountCount":3,"unavailable":1,"colorA":"#4f8cff","colorB":"#25b08b"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry summary=%s want %s", out, want)
	}
}

// TestAppJSCollectConfigClearable 钉住 collectConfig 的空串语义。
//
// 覆盖型字段（user_agent / prompt_file）空串必须照发：漏发会让面板显示"已保存"
// 而 config.json 里的值没变（issue #102 附带发现 2）。
//
// 同时钉住反面：其余文本字段空串仍然不下发。这条同样重要——若哪天为了修上面那个
// 问题改成"所有空串都发"，表单里任何一个没填的框都会变成"请清空"，静默抹掉配置。
func TestAppJSCollectConfigClearable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; collectConfig test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const CFG_MAP');
const end = src.indexOf('/* Go 时长字段即时校验');
if (start < 0 || end < 0 || end < start) throw new Error('collectConfig region not found');
const mk = v => ({ type: 'text', value: v });
const cfgForm = { elements: {
  listen: mk(''),
  api_key: mk('secret'),
  user_agent: mk(''),
  prompt_file: mk(''),
  checkin_hours: mk(''),
}};
const ctx = {
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams, Set,
  document: { getElementById: id => (id === 'cfgForm' ? cfgForm : null) },
  $: id => (id === 'cfgForm' ? cfgForm : null),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.collectConfig = collectConfig;', ctx);
const out = ctx.collectConfig();
const has = (o, k) => Object.prototype.hasOwnProperty.call(o || {}, k);
process.stdout.write(JSON.stringify([
  has(out.upstream, 'user_agent'), (out.upstream || {}).user_agent,
  has(out.prompt, 'file'), (out.prompt || {}).file,
  has(out, 'listen'),
  has(out.schedule, 'checkin_hours'),
  out.api_key
]));`
	f, err := os.CreateTemp(t.TempDir(), "cfgc-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("collectConfig node test failed: %v\n%s", err, out)
	}
	// [user_agent 已发, 其值, prompt.file 已发, 其值, listen 未发, checkin_hours 未发, api_key]
	const want = `[true,"",true,"",false,false,"secret"]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("collectConfig=%s want %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSExpirySortedByExpiry 到期提醒按「最近到期」升序，且不分域（issue #125）。
//
// 后端 /panel/api/packages 是按**余额降序**返回的，恰好把 CN 账号都排在前面、
// global 排在末尾，看上去像"按域分组"，实际只是余额顺序。本用例刻意按这个形态构造
// 输入（余额高的到期最晚、global 余额最低），若前端不重排就会保持该顺序而失败。
//
// 日期按「今天 +N 天」生成而不是写死：写死的话过了那天用例就会自己失效。
func TestAppJSExpirySortedByExpiry(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry sort test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function expBatches');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry region not found');
const sink = { innerHTML: '', textContent: '', hidden: true };
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: () => sink,
  Date, Math, Number, String, Map, Array, Object, isNaN,
  lastPackages: null, lastPackagesAt: 0,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpiry = renderExpiry;', ctx);
const iso = n => {
  const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + n);
  const p = x => String(x).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
};
const pack = (days, amount) => ({ end_time: iso(days) + ' 00:00:00', remain: amount });
// 模拟后端顺序：余额降序 → CN 高余额在前且到期最晚，global 最少且最快到期。
const d = { accounts: [
  { uid: 'a', nickname: 'cn-late',  realm: 'cn',     packages: [pack(16, 900)] },
  { uid: 'b', nickname: 'cn-mid',   realm: 'cn',     packages: [pack(12, 700)] },
  { uid: 'c', nickname: 'gl-soon',  realm: 'global', packages: [pack(2, 500)] },
  { uid: 'd', nickname: 'no-expiry', realm: 'cn',    packages: [pack(-3, 100)] },
  { uid: 'e', nickname: 'broken',   realm: 'cn',     error: 'offline' },
] };
ctx.renderExpiry(d);
const names = [...sink.innerHTML.matchAll(/<span class="exp-nm">([^<]*)<\/span>/g)].map(m => m[1]);
process.stdout.write(JSON.stringify(names));`
	f, err := os.CreateTemp(t.TempDir(), "expsort-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry sort node test failed: %v\n%s", err, out)
	}
	// 最快到期的排最前（跨域）；无 7 天内到期的与查询失败的排最后。
	const want = `["gl-soon","cn-mid","cn-late","no-expiry","broken"]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry order=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSExpirySnapshotRenders 到期卡片用 overview 快照即可渲染（issue #137）。
//
// 此前的死锁：卡片在 index.html 上写死 hidden，而唯一解除隐藏的语句在
// renderExpiry() 末尾——该函数只有点「检查」才被调用。按钮本身就在隐藏卡片里，
// 用户看不见也点不到，于是"积分到期提醒"这个功能等于不存在。
//
// 修复引入 renderExpirySnapshot()：吃 /overview 响应里的 credits_earliest_expiry /
// credits_earliest_remaining（state.json 现成数据，零上游请求），并显式设
// expBox.hidden = false。本用例钉住两件事：
//  1. 只给 overview 形态的输入（没有 packages 字段）也能出内容；
//  2. 渲染后 expBox.hidden 必须为 false——把这句删掉（还原 bug）测试就必须 FAIL。
func TestAppJSExpirySnapshotRenders(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry snapshot test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderExpirySnapshot');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry snapshot region not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', hidden: true });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpirySnapshot = renderExpirySnapshot;', ctx);
const day = n => {
  const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + n);
  return d.toISOString();
};
// overview 形态：没有 packages 数组，只有到期快照字段。
const d = { accounts: [
  { uid: 'aaaaaaaa-1', nickname: '秋',     credits: 4099,
    credits_earliest_expiry: day(2),  credits_earliest_remaining: 499 },
  { uid: 'bbbbbbbb-2', nickname: '秋千易', credits: 3243,
    credits_earliest_expiry: day(30), credits_earliest_remaining: 43 },
  { uid: 'cccccccc-3', nickname: '长期包', credits: 900,
    credits_earliest_expiry: null,    credits_earliest_remaining: 0 },
] };
ctx.renderExpirySnapshot(d);
const html = sinks['expList'] ? sinks['expList'].innerHTML : '';
const names = [...html.matchAll(/<span class="exp-nm">([^<]*)<\/span>/g)].map(m => m[1]);
// 2 天到期的应排最前（红），30 天其次，无到期信息的排最后。
const wantNames = JSON.stringify(['秋', '秋千易', '长期包']);
const gotNames = JSON.stringify(names);
process.stdout.write(JSON.stringify({
  visible: sinks['expBox'] ? sinks['expBox'].hidden === false : false,
  rows: names.length,
  order: gotNames === wantNames,
  got: gotNames,
  want: wantNames,
}));`
	f, err := os.CreateTemp(t.TempDir(), "expsnap-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry snapshot node test failed: %v\n%s", err, out)
	}
	const want = `{"visible":true,"rows":3,"order":true,"got":"[\"秋\",\"秋千易\",\"长期包\"]","want":"[\"秋\",\"秋千易\",\"长期包\"]"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry snapshot=%s\nwant %s（visible 必须为 true，否则卡片仍隐藏）",
			strings.TrimSpace(string(out)), want)
	}
}

// TestPanelIndexExpiryBoxNotHidden 到期卡片在 HTML 骨架里不得带 hidden 属性（issue #137）。
//
// 与上一个用例互补：app.js 侧有渲染兜底，HTML 侧若写死 hidden，
// 用户在首次接口返回前（以及 JS 加载失败时）看到的依然是空白。这个断言就是
// 那个曾经卡住整个功能的属性——把它加回去测试必须 FAIL。
//
// 说明：积分页改版后，到期卡片骨架 id 由 expBox/expList 换成 pkExpiry
// （见「WorkBuddy 积分到期分布」卡片），断言随之指向新 id；语义不变。
func TestPanelIndexExpiryBoxNotHidden(t *testing.T) {
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	// 定位到期卡片那一行，只检查它自己，避免误伤其他合法的 hidden 元素。
	idx := strings.Index(string(b), `id="pkExpiry"`)
	if idx < 0 {
		t.Fatal(`index.html missing id="pkExpiry"`)
	}
	lineStart := strings.LastIndex(string(b[:idx]), "\n") + 1
	lineEnd := strings.Index(string(b[idx:]), "\n")
	if lineEnd < 0 {
		lineEnd = len(b) - idx
	}
	line := string(b[lineStart : idx+lineEnd])
	if strings.Contains(line, "hidden") {
		t.Fatalf(`pkExpiry 元素不得带 hidden（issue #137 死锁根因），实际行：%s`, strings.TrimSpace(line))
	}
}

// TestAppJSChartTooltipInsideBar 图表 tooltip 必须挂在每根柱子内部（issue #128）。
//
// <title> 在 SVG 里描述的是**父元素**。此前它被平铺在 <svg> 根下（<rect> 是自闭合的，
// 无法包含子节点），于是整张图共用一个 tooltip —— 浏览器取第一个 —— 悬停任何柱子都
// 显示同一份数据。这类问题不报错、不影响渲染，只靠肉眼看很容易漏。
//
// 断言结构：<g> 数量 == 数据点数，且每个 <g> 紧跟一个 <title>；同时确认没有
// 游离在 <g> 之外的 <title>。
func TestAppJSChartTooltipInsideBar(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; chart tooltip test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function parsePointTime');
const chartStart = src.indexOf('function renderUsageChart');
if (start < 0 || chartStart < 0) throw new Error('chart functions not found');
let end = src.indexOf('\nfunction ', chartStart + 10);
if (end < 0) end = src.length;
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '' });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN, Infinity, isFinite, Set,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderUsageChart = renderUsageChart;', ctx);
const series = [];
for (let h = 0; h < 5; h++) {
  const d = new Date(); d.setHours(d.getHours() - (4 - h), 0, 0, 0);
  const iso = d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' +
              String(d.getDate()).padStart(2, '0') + 'T' + String(d.getHours()).padStart(2, '0');
  series.push({ t: iso, scope: 'hour', prompt_tokens: (h + 1) * 100,
                completion_tokens: (h + 1) * 10, total_tokens: (h + 1) * 110, requests: h + 1 });
}
ctx.renderUsageChart(series);
const svg = sinks['usChart'] ? sinks['usChart'].innerHTML : '';
const groups = svg.match(/<g><title>/g) || [];
const titles = svg.match(/<title>[^<]*<\/title>/g) || [];
// 游离的 <title>：前面不是 <g>（即仍平铺在根下）
const loose = (svg.match(/(?:<rect[^>]*\/>|<\/g>)<title>/g) || []).length;
process.stdout.write(JSON.stringify({
  points: series.length, groups: groups.length, titles: titles.length, loose: loose,
}));`
	f, err := os.CreateTemp(t.TempDir(), "charttip-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("chart tooltip node test failed: %v\n%s", err, out)
	}
	const want = `{"points":5,"groups":5,"titles":5,"loose":0}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("chart tooltip structure=%s\nwant %s（groups 应等于数据点数，loose 应为 0）",
			strings.TrimSpace(string(out)), want)
	}
}

/* ── C-UI-REPAIR@1（T10）前端回归 ───────────────────────────────────────
   断言策略：只钉稳定的 data-* 锚点与契约字段消费，不校验整段 HTML。
   需要跑真实 JS 行为的用例用 node + vm 抽取单段函数（与既有 expiry/chart
   用例同一方式），无 node 环境自动跳过。 */

// runNodeScript 把脚本写入临时文件并以 app.js 为参数执行，返回 stdout。
func runNodeScript(t *testing.T, name, script string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipped")
	}
	f, err := os.CreateTemp(t.TempDir(), name+"-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("%s node test failed: %v\n%s", name, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestFrontendRepairContractAnchors 页面源码必须消费 C-UI-REPAIR@1 的稳定锚点：
// CodeArts 三按钮 + 票据字段、Qoder 设备/出口与历史、任务中心错误横幅。
// 同时钉住被明令禁止的无路径承诺文案。
func TestFrontendRepairContractAnchors(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	src, page := string(js), string(html)

	must := []string{
		// CodeArts 契约字段（C-CA-API@1）
		"ticket_deadline", "ticket_source", "session_expires_at", "last_ok_at",
		"renew_next_at", "renew_result", "renew_fail_msg", "probe_state", "probe_msg",
		"can_auto_renew", "needs_manual", "expires_known_count", "session_cookie_count",
		// CodeArts 操作入口（data-* 供测试）
		"data-ca-probe", "data-ca-renew", "data-ca-keepalive",
		"ide/codearts/probe", "ide/codearts/renew", "ide/codearts/keepalive",
		// 文案：会话级 / 估算 / 未观测
		"票据为会话级", "（估算）", "票据寿命未观测（可点补采）",
		// Qoder（C-QD-API@1 / C-QD-LEDGER@1）
		"data-qd-egress", "data-qd-history",
		"ide/qoder/egress", "ide/qoder/claim_history",
		"quota_group", "quota_taken_by", "rotation_rank", "rotation_order",
		"同一人已领", "blocked_accounts", "claim_history_days",
		"blocked", "login_required",
		// 任务中心（C-WB-SCAN@1 / C-WB-AUTO@1）
		"scan_ok", "scan_state", "upstream_total", "incomplete_total",
		"renderScanErrors", "partial", "unverified", "verified",
		"progress_before", "progress_after",
	}
	for _, s := range must {
		if !strings.Contains(src, s) {
			t.Errorf("app.js 缺少 C-UI-REPAIR@1 锚点/字段：%q", s)
		}
	}
	// 禁止文案：没有任何代码路径支撑“重新登录会采集”。
	for _, banned := range []string{"下次重新登录会采集", "重新登录会采集"} {
		if strings.Contains(src, banned) || strings.Contains(page, banned) {
			t.Errorf("页面仍含无路径承诺文案 %q", banned)
		}
	}
	// 必须存在“空 deadline + 空 source”的唯一降级文案。
	if !strings.Contains(src, "票据寿命未观测") {
		t.Error("缺少票据寿命未观测的降级文案")
	}

	// index.html 的骨架锚点
	for _, s := range []string{
		`id="tcScanErr"`, `class="scan-error"`,
		`id="ckQoderBuckets"`, `id="taskAutoResult"`,
		`id="qdEgressVeil"`, `id="qdEgressMode"`, `id="qdEgressProxy"`, `id="qdEgressProfile"`,
		`id="qdHistVeil"`,
	} {
		if !strings.Contains(page, s) {
			t.Errorf("index.html 缺少锚点：%q", s)
		}
	}
}

// TestFrontendCodeArtsTicketLines 票据寿命文案：空字段只能是「未观测」，
// 会话级不得报成不可续期，估算必须标注。
func TestFrontendCodeArtsTicketLines(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const CA_TICKET_SOURCE');
const end = src.indexOf('const CA_ACTIONS');
if (start < 0 || end < 0 || end < start) throw new Error('caTicketLines region not found');
const ctx = {
  parseAPITime: v => { const ms = Date.parse(String(v || '')); return Number.isFinite(ms) ? ms : 0; },
  fmtLocalDateTime: ms => { const d = new Date(ms); const p = n => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes()); },
  String, Number, Date, Object, Array,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.caTicketLines = caTicketLines;', ctx);
const empty = ctx.caTicketLines({});
const session = ctx.caTicketLines({ ticket_source: 'session', session_expires_at: '2026-11-02T21:30:00+08:00' });
const cookie = ctx.caTicketLines({
  ticket_deadline: '2026-11-05T08:00:00+08:00', ticket_source: 'cookie',
  last_ok_at: '2026-11-02T14:30:00+08:00', renew_next_at: '2026-11-03T10:00:00+08:00',
  renew_result: 'failed', renew_fail_msg: '票据换不回会话',
  probe_state: 'failed', probe_msg: '网络错误',
  expires_known_count: 37, session_cookie_count: 14, can_auto_renew: false,
});
const manual = ctx.caTicketLines({ needs_manual: true, probe_state: 'login_dead', probe_msg: '会话已失效', can_auto_renew: false });
process.stdout.write(JSON.stringify({ empty: empty, session: session, cookie: cookie, manual: manual }));`
	out := runNodeScript(t, "ca-ticket", script)
	var got struct {
		Empty   []string `json:"empty"`
		Session []string `json:"session"`
		Cookie  []string `json:"cookie"`
		Manual  []string `json:"manual"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	join := func(lines []string) string { return strings.Join(lines, "\n") }
	if len(got.Empty) != 1 || got.Empty[0] != "票据寿命未观测（可点补采）" {
		t.Fatalf("空字段文案=%v，want 仅「票据寿命未观测（可点补采）」", got.Empty)
	}
	if s := join(got.Session); !strings.Contains(s, "票据为会话级") || !strings.Contains(s, "（估算）") {
		t.Fatalf("会话级文案缺关键句：%s", s)
	}
	c := join(got.Cookie)
	for _, want := range []string{"票据到期：", "上次成功", "下次续期检查", "续期失败原因：票据换不回会话",
		"探测状态：探测失败（网络错误）", "已观测过期 37 条 · 会话级 14 条", "当前不可自动续期"} {
		if !strings.Contains(c, want) {
			t.Errorf("cookie 文案缺 %q：\n%s", want, c)
		}
	}
	if m := join(got.Manual); !strings.Contains(m, "会话已失效") {
		t.Errorf("needs_manual 文案缺原因：%s", m)
	}
}

// TestFrontendQoderBuckets 七相分桶：blocked 必须单列且显示 quota_taken_by，
// credits 不得把被阻塞的号显示成已入账。
func TestFrontendQoderBuckets(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const QD_PHASE_ORDER');
const end = src.indexOf('function renderQoderBuckets');
if (start < 0 || end < 0 || end < start) throw new Error('qoder buckets region not found');
const ctx = {
  esc: s => String(s == null ? '' : s),
  Map, Array, String, Number, Object, console,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.qdBucketHTML = qdBucketHTML; this.qdCounts = qdCounts; this.qdPhaseText = qdPhaseText;', ctx);
const d = { results: [
  { email: 'a@x', phase: 'claimed', credits: 100, quota_taken_by: 'a@x', strategy: 'shared_person', msg: '领取成功' },
  { email: 'b@y', phase: 'blocked', credits: 0, benefit_amount: 100, quota_taken_by: 'a@x', strategy: 'shared_person', msg: '服务端判定同人' },
  { email: 'c@z', phase: 'pending', credits: 0 },
  { email: 'd@z', phase: 'failed', credits: 0, msg: 'HTTP 500' },
  { email: 'e@z', phase: 'login_required', credits: 0 },
  { email: 'f@z', phase: 'error', credits: 0 },
  { email: 'g@z', phase: 'already', credits: 100 },
] };
const html = ctx.qdBucketHTML(d);
const c = ctx.qdCounts(d.results);
process.stdout.write(JSON.stringify({
  html: html,
  counts: c,
  hasSeven: ['claimed','already','blocked','pending','failed','login_required','error']
    .every(p => html.indexOf('data-qd-bucket="' + p + '"') >= 0),
  blockedText: html.indexOf('同一人已领（a@x）') >= 0,
  blockedZero: html.indexOf('data-qd-result="b@y"') >= 0 && html.indexOf('+0 Credits（活动面值 100，未入账）') >= 0,
  sumOk: c.claimed === 1 && c.already === 1 && c.blocked === 1 && c.pending === 1 &&
         c.failed === 1 && c.login_required === 1 && c.error === 1,
}));`
	out := runNodeScript(t, "qoder-buckets", script)
	var got struct {
		HasSeven    bool           `json:"hasSeven"`
		BlockedText bool           `json:"blockedText"`
		BlockedZero bool           `json:"blockedZero"`
		SumOk       bool           `json:"sumOk"`
		Counts      map[string]int `json:"counts"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if !got.HasSeven {
		t.Errorf("七相分桶未全部渲染：%s", out)
	}
	if !got.BlockedText {
		t.Errorf("blocked 未显示「同一人已领（quota_taken_by）」：%s", out)
	}
	if !got.BlockedZero {
		t.Errorf("blocked 的 credits 必须为 0 且面值标注未入账：%s", out)
	}
	if !got.SumOk {
		t.Errorf("七相计数不等于实际尝试数：%v", got.Counts)
	}
}

// TestFrontendTaskCenterScanError 错误态禁止渲染“没有待办任务 🎉”。
func TestFrontendTaskCenterScanError(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function scanErrors');
const end = src.indexOf('function updateProgress');
if (start < 0 || end < 0 || end < start) throw new Error('scan render region not found');
const sinks = {};
const mk = id => (sinks[id] = {
  innerHTML: '', textContent: '', hidden: false, style: {},
  querySelector: sel => (sinks[id + sel] || (sinks[id + sel] = { textContent: '' })),
});
const ctx = {
  $: id => sinks[id] || mk(id),
  esc: s => String(s == null ? '' : s),
  fmtUnlock: () => '',
  GROWTH_TITLES: {},
  Object, Number, String, Set, Array, Map,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderScanResult = renderScanResult;', ctx);
ctx.renderScanResult({ scan_ok: false, errors: [{ uid: 'u-401', scope: 'growth', message: 'HTTP 401: invalid token' }], accounts: [] });
const fail = {
  banner: sinks.tcScanErr.hidden === false,
  bannerText: sinks.tcScanErr.innerHTML,
  emptyDisplay: sinks.tcEmpty.style.display,
  list: sinks.qcList.innerHTML,
  summary: sinks.qcSummary.textContent,
};
ctx.renderScanResult({ scan_ok: true, errors: [], accounts: [
  { uid: 'u-empty', nickname: '空号', scan_state: 'empty', upstream_total: 0, incomplete_total: 0 },
] });
const ok = {
  bannerHidden: sinks.tcScanErr.hidden === true,
  emptyDisplay: sinks.tcEmpty.style.display,
  emptyTitle: sinks['tcEmpty.t'] ? sinks['tcEmpty.t'].textContent : '',
};
process.stdout.write(JSON.stringify({ fail: fail, ok: ok }));`
	out := runNodeScript(t, "scan-error", script)
	var got struct {
		Fail struct {
			Banner       bool   `json:"banner"`
			BannerText   string `json:"bannerText"`
			EmptyDisplay string `json:"emptyDisplay"`
			List         string `json:"list"`
			Summary      string `json:"summary"`
		} `json:"fail"`
		Ok struct {
			BannerHidden bool   `json:"bannerHidden"`
			EmptyDisplay string `json:"emptyDisplay"`
			EmptyTitle   string `json:"emptyTitle"`
		} `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if !got.Fail.Banner {
		t.Error("scan_ok=false 时未显示 .scan-error 横幅")
	}
	for _, want := range []string{"u-401", "growth", "HTTP 401"} {
		if !strings.Contains(got.Fail.BannerText, want) {
			t.Errorf("错误横幅缺 %q：%s", want, got.Fail.BannerText)
		}
	}
	if got.Fail.EmptyDisplay != "none" {
		t.Errorf("错误态不得渲染空态（tcEmpty.display=%q）", got.Fail.EmptyDisplay)
	}
	if got.Fail.List != "" {
		t.Errorf("错误态列表应为空：%q", got.Fail.List)
	}
	if !got.Ok.BannerHidden {
		t.Error("scan_ok=true 时错误横幅应隐藏")
	}
	if got.Ok.EmptyDisplay == "none" {
		t.Error("合法空结果应允许渲染空态（scan_ok=true 且无错误）")
	}
}

// TestFrontendAutoSummaryStatuses 一键完成汇总：done 只在 verified 时计入成功，
// partial/unverified/manual/locked/skipped/failed 必须分别报告。
func TestFrontendAutoSummaryStatuses(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const AUTO_STATUS_TEXT');
const end = src.indexOf("$('btnTaskAutoAll').onclick");
if (start < 0 || end < 0 || end < start) throw new Error('auto summary region not found');
const ctx = { String, Number, Object, Array };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.autoSummaryText = autoSummaryText; this.autoCounts = autoCounts; this.normalizeAutoStatus = normalizeAutoStatus;', ctx);
const r = { results: [
  { task_code: 't1', status: 'done', verified: true },
  { task_code: 't2', status: 'done', verified: false },
  { task_code: 't3', status: 'partial', verified: false },
  { task_code: 't4', status: 'unverified', verified: false },
  { task_code: 't5', status: 'manual' },
  { task_code: 't6', status: 'locked' },
  { task_code: 't7', status: 'skipped' },
  { task_code: 't8', status: 'failed' },
  { task_code: 'ctl', status: 'done', kind: 'control' },
] };
const text = ctx.autoSummaryText(r, 12);
const c = ctx.autoCounts(r);
process.stdout.write(JSON.stringify({ text: text, counts: c, t2: ctx.normalizeAutoStatus({ status: 'done', verified: false }) }));`
	out := runNodeScript(t, "auto-summary", script)
	var got struct {
		Text   string         `json:"text"`
		Counts map[string]int `json:"counts"`
		T2     string         `json:"t2"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Counts["done"] != 1 || got.Counts["unverified"] != 2 {
		t.Errorf("done 必须只计 verified=true 的 1 项, unverified=2，实际 %v", got.Counts)
	}
	if got.Counts["total"] != 8 {
		t.Errorf("summary.total 应排除 control 条目=8，实际 %d", got.Counts["total"])
	}
	if got.T2 != "unverified" {
		t.Errorf("done+verified=false 必须降级为 unverified，实际 %q", got.T2)
	}
	for _, want := range []string{"完成 1 项（已验证）", "部分完成 1 项", "未证实 2 项",
		"需手动 1 项", "未解锁 1 项", "跳过 1 项", "失败 1 项"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("汇总缺 %q：%s", want, got.Text)
		}
	}
	if strings.Contains(got.Text, "完成 2 项") {
		t.Errorf("未证实的 done 被计入成功：%s", got.Text)
	}
}

// TestFrontendQueueRowVerifiedProgress 队列行必须显示 verified 与前后进度，
// 且 done+verified=false 不得显示成完成。
func TestFrontendQueueRowVerifiedProgress(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const ST_WORDS');
const end = src.indexOf('function renderQueue');
if (start < 0 || end < 0 || end < start) throw new Error('qrow region not found');
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtUnlock: () => '',
  GROWTH_TITLES: {},
  String, Object, Array,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.qrowHTML = qrowHTML;', ctx);
const partial = ctx.qrowHTML({ status: 'partial', code: 'chat_5', progress_before: '3/5', progress_after: '4/5', verified: false, action: 'auto' });
const fakeDone = ctx.qrowHTML({ status: 'done', code: 'chat_6', verified: false });
const realDone = ctx.qrowHTML({ status: 'done', code: 'chat_7', verified: true });
process.stdout.write(JSON.stringify({ partial: partial, fakeDone: fakeDone, realDone: realDone }));`
	out := runNodeScript(t, "queue-row", script)
	var got struct {
		Partial  string `json:"partial"`
		FakeDone string `json:"fakeDone"`
		RealDone string `json:"realDone"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	for _, want := range []string{"部分完成", "3/5 → 4/5", "未证实", "可自动执行"} {
		if !strings.Contains(got.Partial, want) {
			t.Errorf("partial 行缺 %q：%s", want, got.Partial)
		}
	}
	if !strings.Contains(got.FakeDone, "未证实") {
		t.Errorf("done+verified=false 必须显示未证实：%s", got.FakeDone)
	}
	if strings.Contains(got.RealDone, "未证实") {
		t.Errorf("verified=true 的 done 不得显示未证实：%s", got.RealDone)
	}
	if !strings.Contains(got.RealDone, "完成") {
		t.Errorf("verified=true 的 done 应显示完成：%s", got.RealDone)
	}
}

// TestFrontendCodeArtsRowCondensed CodeArts 卡片默认只显示一行摘要：
// 正常态给「自动维护中」且不出现排障按钮，只有需人工时才给重登/重置入口，
// 技术字段（slot_dir / person_key）只在展开详情里出现。
//
// 断言的都是**可见文案**与稳定锚点，不校验整段 HTML 的排版；数据 shape 直接
// 用 GET ide/codearts/accounts 的字段名（后端契约），前端不得自造同义字段。
func TestFrontendCodeArtsRowCondensed(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function caStatusPhrase');
const end = src.indexOf('async function doCaAction');
if (start < 0 || end < 0 || end < start) throw new Error('caStatusPhrase region not found');
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtNum: n => String(n),
  caTime: iso => String(iso || ''),
  CA_RENEW_RESULT: { needs_login: '需人工登录', failed: '续期失败' },
  CA_PROBE_STATE: { login_dead: '会话已失效' },
  UI: {
    act: (attr, val, label) => '<button class="xs primary" ' + attr + '="' + val + '">' + label + '</button>',
    sub: (attr, val, label) => '<button class="xs" ' + attr + '="' + val + '">' + label + '</button>',
    off: label => '<button class="xs" disabled>' + label + '</button>',
    badge: (kind, mark, text) => '<span class="stask ' + kind + '">' + text + '</span>',
  },
  Number, String, console,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.caStatusPhrase = caStatusPhrase; this.caSummaryHtml = caSummaryHtml;' +
  'this.caDetailHtml = caDetailHtml; this.caRowHtml = caRowHtml;', ctx);

const normal = { id: 'a1', label: '秋', can_auto_renew: true, checked_in_today: true,
  today_gain: 100, ticket_source: 'cookie' };
const manual = { id: 'a2', label: '乙', needs_manual: true, probe_state: 'login_dead',
  probe_msg: '会话已失效', can_auto_renew: false };
const sessionAcc = { id: 'a3', ticket_source: 'session', can_auto_renew: false };
const failedAcc = { id: 'a4', renew_result: 'failed', can_auto_renew: false };
const fresh = { id: 'a5' };
const banned = ['补采票据寿命', '验证会话', '立即续期'];
const normalSum = ctx.caSummaryHtml(normal);
const manualSum = ctx.caSummaryHtml(manual);
const detail = ctx.caDetailHtml(normal, {
  slot_dir: 'D:/x/codearts-slots/a1', person_key: 'person:abc',
  slot_id: 'a1', device_guid: 'guid-1', client_source: 'browser',
});
process.stdout.write(JSON.stringify({
  phraseNormal: ctx.caStatusPhrase(normal),
  phraseManual: ctx.caStatusPhrase(manual),
  phraseSession: ctx.caStatusPhrase(sessionAcc),
  phraseFailed: ctx.caStatusPhrase(failedAcc),
  phraseFresh: ctx.caStatusPhrase(fresh),
  normalHasBanned: banned.some(x => normalSum.indexOf(x) >= 0),
  manualHasBanned: banned.some(x => manualSum.indexOf(x) >= 0),
  manualHasRelogin: manualSum.indexOf('data-ca-relogin') >= 0,
  manualHasForget: manualSum.indexOf('重置身份') >= 0,
  normalHasAuto: normalSum.indexOf('自动维护中') >= 0,
  normalHasToday: normalSum.indexOf('今日 +100') >= 0,
  normalHasName: normalSum.indexOf('秋') >= 0,
  detailHasSlotDir: detail.indexOf('slot_dir') >= 0 && detail.indexOf('D:/x/codearts-slots/a1') >= 0,
  detailHasPersonKey: detail.indexOf('person_key') >= 0 && detail.indexOf('person:abc') >= 0,
  detailFolded: ctx.caRowHtml(normal).indexOf('data-ca-detail="a1" hidden') >= 0,
}));`
	out := runNodeScript(t, "ca-condensed", script)
	var got struct {
		PhraseNormal     string `json:"phraseNormal"`
		PhraseManual     string `json:"phraseManual"`
		PhraseSession    string `json:"phraseSession"`
		PhraseFailed     string `json:"phraseFailed"`
		PhraseFresh      string `json:"phraseFresh"`
		NormalHasBanned  bool   `json:"normalHasBanned"`
		ManualHasBanned  bool   `json:"manualHasBanned"`
		ManualHasRelogin bool   `json:"manualHasRelogin"`
		ManualHasForget  bool   `json:"manualHasForget"`
		NormalHasAuto    bool   `json:"normalHasAuto"`
		NormalHasToday   bool   `json:"normalHasToday"`
		NormalHasName    bool   `json:"normalHasName"`
		DetailHasSlotDir bool   `json:"detailHasSlotDir"`
		DetailHasPerson  bool   `json:"detailHasPersonKey"`
		DetailFolded     bool   `json:"detailFolded"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	want := map[string]string{
		"正常态":  "自动维护中",
		"人工态":  "需重新登录",
		"会话级":  "会话票据（自动续期）",
		"续期失败": "续期失败（自动重试）",
		"空字段":  "待维护",
	}
	gotPhrases := map[string]string{
		"正常态": got.PhraseNormal, "人工态": got.PhraseManual,
		"会话级": got.PhraseSession, "续期失败": got.PhraseFailed, "空字段": got.PhraseFresh,
	}
	for name, w := range want {
		if gotPhrases[name] != w {
			t.Errorf("%s状态短语=%q，want %q", name, gotPhrases[name], w)
		}
	}
	if got.NormalHasBanned {
		t.Error("正常态默认行不得出现补采/验证/续期按钮文案")
	}
	if got.ManualHasBanned {
		t.Error("需人工的默认行也不得出现补采/验证/续期按钮文案（它们属于展开详情）")
	}
	if !got.ManualHasRelogin || !got.ManualHasForget {
		t.Errorf("needs_manual 必须给「重新登录」主按钮与「重置身份」文字链（relogin=%v forget=%v）",
			got.ManualHasRelogin, got.ManualHasForget)
	}
	if !got.NormalHasAuto {
		t.Error("正常态必须显示「自动维护中」")
	}
	if !got.NormalHasToday || !got.NormalHasName {
		t.Errorf("默认行必须是 `昵称 · 状态 · 今日积分` 单行（name=%v today=%v）",
			got.NormalHasName, got.NormalHasToday)
	}
	if !got.DetailHasSlotDir || !got.DetailHasPerson {
		t.Errorf("展开详情必须显示 slot_dir / person_key（slot_dir=%v person_key=%v）",
			got.DetailHasSlotDir, got.DetailHasPerson)
	}
	if !got.DetailFolded {
		t.Error("详情区默认必须是折叠（hidden）状态，不能把技术字段铺在默认行")
	}
}
