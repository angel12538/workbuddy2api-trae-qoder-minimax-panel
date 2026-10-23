package panel

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 切号「不掉上下文」回归测试。
//
// 背景：切号只应更换后端登录态，面板画面必须 100% 停在切换前
// （列表顺序、滚动位置、额度数字、连「当前登录」徽章都不许变）。
// 这里锁死两件事：
//   1) 结构：切号函数里不得再出现 load*Accounts 之类的整表重载；
//      自动刷新必须有统一闸门；页面必须有冻结提示条。
//   2) 行为：用 Node 抽出真实函数源码，跑一遍冻结 → 解冻的状态机。

// readAppJS 读 app.js 原文。
func readAppJS(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	return string(raw)
}

// grabJSFunc 按名字从源码里抠出一个函数定义（花括号配平）。
func grabJSFunc(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "function "+name+"(")
	if start < 0 {
		t.Fatalf("app.js 里找不到函数 %s", name)
	}
	depth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("函数 %s 花括号不配平", name)
	return ""
}

// TestAppJSSwitchDoesNotReload 切号后不得整表重载或改写列表。
func TestAppJSSwitchDoesNotReload(t *testing.T) {
	src := readAppJS(t)

	for _, fn := range []string{"doQoderSwitch", "doTraeIdeSwitch", "doWorkBuddySwitch"} {
		body := grabJSFunc(t, src, fn)
		for _, bad := range []string{"loadQoderAccounts(", "loadTraeIdeAccounts(", "loadWorkBuddyAccounts("} {
			if strings.Contains(body, bad) {
				t.Errorf("%s 里仍然调用了 %s：切号会重绘列表，画面会跳变", fn, bad)
			}
		}
		if !strings.Contains(body, "onSwitchDone(") {
			t.Errorf("%s 没有走 onSwitchDone：冻结/提示条不会生效", fn)
		}
	}
}

// TestAppJSAutoRefreshGate 所有自动刷新入口必须有冻结闸门。
func TestAppJSAutoRefreshGate(t *testing.T) {
	src := readAppJS(t)

	rv := grabJSFunc(t, src, "refreshVisible")
	if !strings.Contains(rv, "viewFrozen()") {
		t.Error("refreshVisible 缺少冻结闸门：切号后全局数字仍会被自动刷新成新号的")
	}

	if !strings.Contains(src, "if (autoRefreshBlocked()) return;") {
		t.Error("任务队列轮询缺少冻结闸门")
	}

	// 冻结期内进入 IDE 账号页不得自动重取
	if !strings.Contains(src, "if (v === 'ideaccounts' && !viewFrozen())") {
		t.Error("go() 进 IDE 账号页时没有跳过自动加载：冻结画面会被重绘")
	}

	// 冻结必须由切号触发，且解冻必须能重载
	for _, fn := range []string{"freezeView", "thawView", "onSwitchDone", "reloadCurrentView"} {
		if !strings.Contains(src, "function "+fn+"(") {
			t.Errorf("缺少函数 %s", fn)
		}
	}
	if !strings.Contains(src, "restoreViewState") || !strings.Contains(src, "saveViewState") {
		t.Error("缺少视图位置持久化（切号/刷新后会回到页面顶部）")
	}
}

// TestIndexHTMLHasFreezeBar 冻结提示条必须存在（否则用户不知道怎么刷新）。
func TestIndexHTMLHasFreezeBar(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	if !strings.Contains(html, `id="freezeBar"`) {
		t.Error("index.html 缺少 #freezeBar 容器")
	}
	if !strings.Contains(html, ".freeze-bar") {
		t.Error("index.html 缺少 .freeze-bar 样式")
	}
}

// TestAppJSFreezeStateMachine 用真实源码跑冻结/解冻状态机。
func TestAppJSFreezeStateMachine(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; 跳过状态机测试")
	}

	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');

function grab(name) {
  const i = src.indexOf('function ' + name + '(');
  if (i < 0) throw new Error('missing ' + name);
  let d = 0;
  for (let j = src.indexOf('{', i); j < src.length; j++) {
    if (src[j] === '{') d++;
    else if (src[j] === '}') { d--; if (!d) return src.slice(i, j + 1); }
  }
  throw new Error('unbalanced ' + name);
}

let calls = [];
const spy = n => (...a) => { calls.push(n); };
const bar = { hidden: true, innerHTML: '', onclick: null };
const list = { scrollTop: 42, innerHTML: 'ROWS' };
const els = { freezeBar: bar, qoderList: list, traeIdeList: list, wbList: list, accBody: list };

const store = {};
const sandbox = {
  console, JSON, Date, Math, Object, Array, String, Number,
  $: id => els[id] || null,
  esc: s => String(s),
  toast: m => calls.push('toast:' + m),
  view: 'ideaccounts',
  TITLES: { checkin: 1, usage: 1, packages: 1, taskscenter: 1, ideaccounts: 1, models: 1, config: 1, logs: 1 },
  sessionStorage: { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); } },
  window: { scrollY: 123, scrollTo: (x, y) => calls.push('scrollTo:' + y), addEventListener() {} },
  requestAnimationFrame: cb => cb(),
  setTimeout: fn => fn(),
  loadOverview: spy('loadOverview'),
  loadQoderAccounts: spy('loadQoderAccounts'),
  loadTraeIdeAccounts: spy('loadTraeIdeAccounts'),
  loadWorkBuddyAccounts: spy('loadWorkBuddyAccounts'),
  loadLogs: spy('loadLogs'), reattachQueueView: spy('reattachQueueView'),
  loadIdeaTasks: spy('loadIdeaTasks'), loadTraeStatus: spy('loadTraeStatus'),
  loadTraeCredits: spy('loadTraeCredits'), loadCheckinPool: spy('loadCheckinPool'),
  loadQkCredits: spy('loadQkCredits'), loadWkCredits: spy('loadWkCredits'),
  loadUsage: spy('loadUsage'), loadUsageOfficial: spy('loadUsageOfficial'),
  loadPackages: spy('loadPackages'), loadPkIdea: spy('loadPkIdea'),
  loadModels: spy('loadModels'), loadModelQueue: spy('loadModelQueue'),
  go: spy('go'),
};
sandbox.globalThis = sandbox;
vm.createContext(sandbox);

const parts = [];
const constLine = re => { const m = src.match(re); if (!m) throw new Error('missing const ' + re); return m[0]; };
parts.push(constLine(/const VIEW_STATE_KEY = '[^']*';/));
parts.push(constLine(/const FREEZE = \{[^}]*\};/));
['viewFrozen','autoRefreshBlocked','freezeView','thawView','reloadCurrentView','showFreezeBar','hideFreezeBar','onSwitchDone','saveViewState','readViewState','restoreScrollTo','applyListScroll'].forEach(n => parts.push(grab(n)));

vm.runInContext(parts.join('\n'), sandbox, { filename: 'freeze.js' });

const out = {};
const fail = msg => { out.error = msg; console.log(JSON.stringify(out)); process.exit(1); };

if (sandbox.viewFrozen() !== false) fail('初始不该是冻结态');
if (sandbox.autoRefreshBlocked() !== false) fail('初始闸门应放行');

const before = calls.length;
sandbox.freezeView('qoder', 'b@x.com');
out.afterFreeze = {
  frozen: sandbox.viewFrozen(),
  blocked: sandbox.autoRefreshBlocked(),
  barHidden: bar.hidden,
  barHasTarget: bar.innerHTML.indexOf('b@x.com') >= 0,
  loadCallsDuringFreeze: calls.length - before,
  listIntact: list.innerHTML === 'ROWS' && list.scrollTop === 42,
  stateSaved: !!(store['wb2a.viewstate.v1'] && JSON.parse(store['wb2a.viewstate.v1']).view === 'ideaccounts'),
};
if (!out.afterFreeze.frozen || !out.afterFreeze.blocked) fail('freezeView 未生效');
if (out.afterFreeze.barHidden) fail('冻结后提示条应可见');
if (!out.afterFreeze.barHasTarget) fail('提示条未显示目标账号');
if (out.afterFreeze.loadCallsDuringFreeze !== 0) fail('冻结过程不应触发任何数据加载');
if (!out.afterFreeze.listIntact) fail('冻结过程改动了列表 DOM');

calls = [];
const was = sandbox.thawView({ reload: true });
out.afterThaw = {
  was,
  frozen: sandbox.viewFrozen(),
  barHidden: bar.hidden,
  reloads: calls.filter(c => c !== 'toast:' && !c.startsWith('scrollTo:')),
  scrolledTo: (calls.find(c => c.startsWith('scrollTo:')) || '').replace('scrollTo:', ''),
};
if (out.afterThaw.was !== true) fail('thawView 应返回 true');
if (out.afterThaw.frozen) fail('解冻后仍是冻结态');
if (!out.afterThaw.barHidden) fail('解冻后提示条应隐藏');
if (!out.afterThaw.reloads.includes('loadQoderAccounts')) fail('解冻后未重载 IDE 账号列表');
if (out.afterThaw.scrolledTo !== '123') fail('解冻后未恢复滚动位置');

calls = [];
sandbox.onSwitchDone('trae', 'c@y.com', '已重启客户端');
out.afterSwitchDone = { frozen: sandbox.viewFrozen(), toast: calls.filter(c => c.startsWith('toast:'))[0] || '' };
if (!out.afterSwitchDone.frozen) fail('onSwitchDone 未冻结');
if (out.afterSwitchDone.toast.indexOf('c@y.com') < 0) fail('切号完成提示缺少目标账号');

console.log(JSON.stringify(out));
process.exit(0);
`

	hf, err := os.CreateTemp(t.TempDir(), "freeze-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()

	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, hf.Name(), path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("冻结状态机测试失败: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte(`"afterThaw"`)) {
		t.Fatalf("状态机输出不完整:\n%s", out)
	}
	t.Logf("状态机结果: %s", out)
}
