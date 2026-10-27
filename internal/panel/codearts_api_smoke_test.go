package panel

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// 冒烟测试（默认不跑）：在真实浏览器里跑一遍 caWelfareListJS，验证
// ① JS 语法没问题；② 未登录时能走到「会话失效」分支而不是报错。
//
// 运行：
//
//	$env:CODEARTS_PROBE='1'
//	$env:CODEARTS_POOL_FILE='D:\WorkBuddy\data\ide_accounts\codearts_accounts.json'
//	go test ./internal/panel/ -run TestCodeArtsAPISmoke -v -timeout 300s
func TestCodeArtsAPISmoke(t *testing.T) {
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
	dir, err := os.MkdirTemp("", "ca-api-*")
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

	if err := caSetCookies(port, acc.Cookies); err != nil {
		t.Logf("写 Cookie 失败: %v", err)
	}
	if err := caNavigate(ws, codeartsConsoleURL); err != nil {
		t.Logf("导航失败: %v", err)
	}
	time.Sleep(8 * time.Second)

	raw, err := caEval(ws, caWelfareListJS)
	if err != nil {
		t.Fatalf("caWelfareListJS 执行失败（JS 语法有问题？）: %v", err)
	}
	t.Logf("caWelfareListJS 返回: %s", string(raw))

	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		t.Fatalf("返回不是合法 JSON: %s", string(raw))
	}
	if v["ok"] != true {
		t.Logf("（预期：会话已过期，走 err 分支）err=%v snippet=%v", v["err"], v["snippet"])
	}

	// 领取脚本的 JS 也要能解析（用假 campaignId 跑一次，看是否报 JS 语法错）
	raw2, err2 := caEval(ws, caWelfareClaimJSFor("3"))
	if err2 != nil {
		t.Fatalf("caWelfareClaimJS 执行失败: %v", err2)
	}
	t.Logf("caWelfareClaimJS 返回: %s", string(raw2))
}
