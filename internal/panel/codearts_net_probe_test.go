package panel

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// 诊断探针（默认不跑）：在 CodeArts 控制台页面里注入 fetch/XHR 钩子，
// 抓「活动福利」面板打开时页面自己发起的接口请求 —— 用来定位
// 「每日领取」背后的真实接口（能不能不走点按钮、直接调接口）。
//
// 运行：
//
//	$env:CODEARTS_PROBE='1'
//	$env:CODEARTS_POOL_FILE='D:\WorkBuddy\data\ide_accounts\codearts_accounts.json'
//	go test ./internal/panel/ -run TestCodeArtsNetProbe -v -timeout 400s
func TestCodeArtsNetProbe(t *testing.T) {
	if os.Getenv("CODEARTS_PROBE") == "" {
		t.Skip("设 CODEARTS_PROBE=1 才跑")
	}
	pool, err := codearts.LoadPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		t.Fatalf("号池为空: %v", err)
	}
	acc := pool.Accounts[0]

	chrome, err := findChrome()
	if err != nil {
		t.Fatal(err)
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "ca-net-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cleanup, err := launchChromeHeadless(chrome, port, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	var ws *wsConn
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		ws, err = caOpenTab(port, codeartsConsoleURL)
		if err == nil {
			break
		}
	}
	if ws == nil {
		t.Fatal("连不上浏览器")
	}
	defer ws.Close()

	// 注入钩子（Page.enable 必须先开，否则 addScriptToEvaluateOnNewDocument
	// 可能不生效 —— 实测加 Page.enable 之前钩子根本没装上，hooked=false）
	if _, err := doubaoCDPCall(ws, 500, "Page.enable", map[string]any{}, 10*time.Second); err != nil {
		t.Logf("Page.enable 失败: %v", err)
	}
	if _, err := doubaoCDPCall(ws, 501, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": caNetLogJS,
	}, 10*time.Second); err != nil {
		t.Logf("注入钩子失败: %v", err)
	}
	if err := caSetCookies(port, acc.Cookies); err != nil {
		t.Fatalf("写 Cookie 失败: %v", err)
	}
	if err := caNavigate(ws, codeartsConsoleURL); err != nil {
		t.Logf("导航失败: %v", err)
	}
	time.Sleep(9 * time.Second)

	// 保险：点击前直接在当前文档里再打一次补丁（覆盖 addScript 没生效的情况）。
	// 关心的请求本来就发生在「点开活动福利」那一刻，所以来得及。
	if _, err := caEval(ws, caNetLogJS); err != nil {
		t.Logf("直接打补丁失败: %v", err)
	}

	// 点开「活动福利」
	for _, kw := range []string{"活动福利", "限时福利", "福利中心"} {
		if ok, tx, _ := caClickByText(ws, kw, 16); ok {
			t.Logf("点开 %q", tx)
			break
		}
	}
	time.Sleep(6 * time.Second)

	// 把所有框架里的记录都捞出来（含 iframe —— 福利面板的数据请求在 iframe 里）
	raw3, err3 := caEval(ws, `(() => {
      const all = [];
      const push = (w) => { try { if (w && Array.isArray(w.__wb2api)) all.push(...w.__wb2api); } catch (e) {} };
      push(window);
      try { for (const f of document.querySelectorAll('iframe')) push(f.contentWindow); } catch (e) {}
      return JSON.stringify(all);
    })()`)
	if err3 != nil {
		t.Fatalf("读钩子失败: %v", err3)
	}
	var rows []caNetRow
	if err := json.Unmarshal(raw3, &rows); err != nil {
		t.Fatalf("解析失败: %v raw=%s", err, string(raw3))
	}
	t.Logf("抓到 %d 条请求", len(rows))

	// 唯一 URL 汇总
	seen := map[string]int{}
	order := []string{}
	for _, r := range rows {
		if _, ok := seen[r.URL]; !ok {
			order = append(order, r.URL)
		}
		seen[r.URL]++
	}
	t.Logf("唯一 URL %d 个:", len(order))
	for _, u := range order {
		t.Logf("   x%-3d %s", seen[u], u)
	}

	// 福利/领取相关的响应体（完整）
	kw := []string{"welfare", "reward", "claim", "checkin", "check_in", "sign", "point", "credit",
		"task", "gift", "benefit", "bonus", "activity", "campaign", "operation", "config.json"}
	for _, r := range rows {
		lu := strings.ToLower(r.URL)
		hit := false
		for _, k := range kw {
			if strings.Contains(lu, k) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		t.Logf("=== 命中 [%d] %s", r.Status, r.URL)
		t.Logf("    body: %s", r.Body)
	}
}

// caNetRow 钩子记录的一条请求。
type caNetRow struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// caNetLogJS 页面内 fetch/XHR 钩子：记录所有请求的 URL / 状态 / 响应体。
const caNetLogJS = `(() => {
  if (window.__wb2api) return;
  window.__wb2api = [];
  const rec = (url, status, body) => {
    const row = { url: String(url).slice(0, 300), status: status, body: String(body || '').slice(0, 4000) };
    try { window.__wb2api.push(row); } catch (e) {}
    // iframe 里的请求镜像到父/顶层（同源才镜像得到）。
    try { if (parent && parent !== window) { (parent.__wb2api = parent.__wb2api || []).push(row); } } catch (e) {}
    try { if (top && top !== window && top !== parent) { (top.__wb2api = top.__wb2api || []).push(row); } } catch (e) {}
  };
  try {
    const of = window.fetch;
    if (of) window.fetch = async function (...args) {
      const res = await of.apply(this, args);
      try {
        const url = String((args[0] && args[0].url) || args[0] || '');
        const c = res.clone(); c.text().then(t => rec(url, res.status, t)).catch(() => {});
      } catch (e) {}
      return res;
    };
  } catch (e) {}
  try {
    const oo = XMLHttpRequest.prototype.open, os = XMLHttpRequest.prototype.send;
    XMLHttpRequest.prototype.open = function (m, u) { this.__u = u; return oo.apply(this, arguments); };
    XMLHttpRequest.prototype.send = function () {
      this.addEventListener('load', () => {
        try {
          let b = '';
          try { b = this.responseText; } catch (e) { try { b = JSON.stringify(this.response); } catch (e2) {} }
          rec(this.__u, this.status, b);
        } catch (e) {}
      });
      return os.apply(this, arguments);
    };
  } catch (e) {}
})()`
