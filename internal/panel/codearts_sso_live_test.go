package panel

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// TestCodeArtsSilentSSO 验一件事：**网页会话死后，能不能不靠人工扫码换回新会话**。
//
// 为什么必须自己验一遍：仓库里"会话过期只能重新登录"的结论，实测的只是
// 「把过期 Cookie 注进新浏览器再打开 /chat/home」这一条路（10-26 22:15 抓的
// Cookie 到 10-27 09:00 才测，早已超过实测约 7 小时的会话寿命）。而号池里存着
// **持久的 CAS/SSO 票据**（SSOTGC / SSOJTC / CASTGCLOW=TGT / hwid_cas_sid /
// cbc-sid），它们的寿命比应用会话长得多。会话死时网关会 JS 跳到
// `auth.huaweicloud.com/authui/login?service=…` —— 这条链如果凭 TGT 能静默发
// 回新会话，"每天自动续期"就成立；如果要出登录页/验证码，就只能定期人工扫码。
//
// 跑法（会用真实号池 Cookie 起一个无头浏览器，只读，不改号池）：
//
//	$env:CODEARTS_SSO_PROBE='1'
//	$env:CODEARTS_POOL_FILE='D:\WorkBuddy\data\ide_accounts\codearts_accounts.json'
//	go test ./internal/panel/ -run TestCodeArtsSilentSSO -v -timeout 400s
func TestCodeArtsSilentSSO(t *testing.T) {
	if os.Getenv("CODEARTS_SSO_PROBE") == "" {
		t.Skip("设 CODEARTS_SSO_PROBE=1 才跑")
	}
	pool, err := codearts.LoadPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		t.Fatalf("号池为空: %v", err)
	}
	acc := pool.Accounts[0]
	if id := strings.TrimSpace(os.Getenv("CODEARTS_SSO_ID")); id != "" {
		for _, a := range pool.Accounts {
			if a != nil && a.ID == id {
				acc = a
			}
		}
	}
	t.Logf("账号 %s(%s) cookies=%d saved_at=%s", acc.ID, acc.Label, len(acc.Cookies), acc.SavedAt)

	// 先记下 Cookie 快照之外的关键票据在不在，否则"静默续期失败"无从归因。
	have := map[string]bool{}
	for _, c := range acc.Cookies {
		have[c.Name] = true
	}
	for _, n := range []string{"SSOTGC", "SSOJTC", "CASTGCLOW", "hwid_cas_sid", "cbc-sid", "devclouddevuibjtcftk"} {
		t.Logf("  票据 %s: %v", n, have[n])
	}

	chrome, err := findChrome()
	if err != nil {
		t.Fatal(err)
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "ca-sso-*")
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
	for i := 0; i < 30 && ws == nil; i++ {
		time.Sleep(500 * time.Millisecond)
		ws, err = caOpenTab(port, "about:blank")
	}
	if ws == nil {
		t.Fatalf("连不上浏览器: %v", err)
	}
	defer ws.Close()

	if err := caSetCookies(port, acc.Cookies); err != nil {
		t.Fatalf("写 Cookie 失败: %v", err)
	}
	// 会话死时网关给的那个跳转入口：service 指回控制台首页。
	loginURL := "https://auth.huaweicloud.com/authui/login?service=" +
		url.QueryEscape(codearts.APIBase+"/chat/home")
	t.Logf("走静默 SSO: %s", loginURL)
	if err := caNavigate(ws, loginURL); err != nil {
		t.Logf("导航失败: %v", err)
	}

	// 跟着 JS/302 链走，最多 90 秒，记录访问过的每一跳。
	// ⚠️ 判"回到控制台"必须看 **host**：service= 参数里就带着 devcloud 字样，
	// 拿整串 URL 做子串匹配会把"停在登录页"误判成"续期成功"（第一版就栽在这）。
	var trail []string
	final := ""
	for i := 0; i < 45; i++ {
		time.Sleep(2 * time.Second)
		u := caPageURL(port, "")
		if u == "" || strings.Contains(u, "about:blank") {
			continue
		}
		if u != final {
			trail = append(trail, u)
			final = u
		}
		if strings.Contains(hostOf(u), "devcloud") {
			break
		}
		// 停在登录页超过 20 秒：SPA 的静默检查（IDP 侧 iframe/跨域）不会再发生了。
		if i >= 10 && !strings.Contains(hostOf(u), "devcloud") {
			break
		}
	}
	t.Logf("跳转轨迹(%d 跳):", len(trail))
	for _, u := range trail {
		t.Logf("   %s", u)
	}
	t.Logf("最终页面: %s", final)

	devHost := strings.Contains(hostOf(final), "devcloud")
	txt := pageTextForProbe(ws)
	if !devHost {
		t.Logf("停在 %s，正文(前800): %s", final, txt)
		t.Errorf("结论：静默 SSO **不成立** —— 带着号池里的 CAS/SSO 票据（SSOTGC/SSOJTC/CASTGCLOW/hwid_cas_sid/cbc-sid 全在）" +
			"走 authui 链，最终仍停在人工登录页，没有静默换回会话")
		return
	}

	raw, err := caEval(ws, `(async () => {
      const r = await fetch('/chat/snap-manager/v1/package/overview', {headers:{'Accept':'application/json, text/plain, */*','language':'zh-cn','x-language':'zh-cn','x-codearts-doer-scenario-type':'web','x-requested-with':'XMLHttpRequest'}});
      const t = await r.text();
      let json = false, remain = null;
      try { const j = JSON.parse(t); json = true; remain = (((j||{}).all_credit_package)||{}).package_credit_remain ?? null; } catch (e) {}
      return JSON.stringify({status:r.status, len:t.length, json, remain, head:t.slice(0,200)});
    })()`)
	if err != nil {
		t.Fatalf("页内 fetch 失败: %v", err)
	}
	var got struct {
		Status int     `json:"status"`
		Len    int     `json:"len"`
		JSONOk bool    `json:"json"`
		Remain float64 `json:"remain"`
		Head   string  `json:"head"`
	}
	if json.Unmarshal(raw, &got) != nil {
		t.Fatalf("解析失败: %s", string(raw))
	}
	t.Logf("overview -> HTTP %d, len=%d, 是JSON=%v, 余额=%v, head=%s", got.Status, got.Len, got.JSONOk, got.Remain, got.Head)
	if got.JSONOk {
		t.Logf("结论：静默 SSO **成立** —— 凭号池里的 CAS/SSO 票据可以不扫码换回新会话（余额 %.0f）", got.Remain)
		// 把新下发的 Cookie 打出来（证明服务端确实重新发了会话）。
		if cs, cerr := cdpAllCookies(port); cerr == nil {
			names := make([]string, 0, len(cs))
			for _, c := range cs {
				names = append(names, c.Name+"@"+c.Domain)
			}
			t.Logf("续期后共 %d 条 Cookie: %v", len(cs), names)
		} else {
			t.Logf("取续期后 Cookie 失败: %v", cerr)
		}
		return
	}
	t.Errorf("结论：页面虽回到 devcloud，但接口仍不是 JSON（HTTP %d, head=%s）—— 静默续期没成", got.Status, got.Head)
}

// pageTextForProbe 取当前页正文（判断是不是人工登录页）。
func pageTextForProbe(ws *wsConn) string {
	raw, err := caEval(ws, `((document.body && document.body.innerText)||'').replace(/\s+/g,' ').slice(0,1200)`)
	if err != nil {
		return "取正文失败: " + err.Error()
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}
