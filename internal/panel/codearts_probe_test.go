package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// 诊断探针（默认不跑）：用号池里的真实 Cookie 打开 CodeArts 控制台，
// 把页面结构、导航项、含「领取/签到/福利/积分」的元素全 dump 出来，
// 用来定位「每日领取」入口的真实位置。
//
// 运行：
//
//	CODEARTS_POOL_FILE=D:\WorkBuddy\data\ide_accounts\codearts_accounts.json \
//	  go test ./internal/panel/ -run TestCodeArtsProbe -v -timeout 300s
func TestCodeArtsProbe(t *testing.T) {
	if os.Getenv("CODEARTS_PROBE") == "" {
		t.Skip("设 CODEARTS_PROBE=1 才跑")
	}
	pool, err := codearts.LoadPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		t.Fatalf("号池为空: %v", err)
	}
	acc := pool.Accounts[0]
	t.Logf("账号 %s cookies=%d", acc.Label, len(acc.Cookies))

	chrome, err := findChrome()
	if err != nil {
		t.Fatal(err)
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "ca-probe-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := launchChromeHeadless(chrome, port, dir); err != nil {
		t.Fatal(err)
	}

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
		t.Fatalf("写 Cookie 失败: %v", err)
	}
	for _, u := range []string{
		codeartsConsoleURL,
		"https://devcloud.cn-north-4.huaweicloud.com/developer/?locale=zh-cn",
	} {
		if err := caNavigate(ws, u); err != nil {
			t.Logf("导航 %s 失败: %v", u, err)
			continue
		}
		time.Sleep(7 * time.Second)
		t.Logf("=== 页面 %s", caPageURL(port, "huaweicloud"))
		raw, err := caEval(ws, probeDumpJS)
		if err != nil {
			t.Logf("取页面失败: %v", err)
			continue
		}
		var d struct {
			Text  string   `json:"text"`
			Items []string `json:"items"`
			Links []string `json:"links"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Logf("解析失败: %v raw=%s", err, string(raw))
			continue
		}
		t.Logf("--- 正文(前2500) ---\n%s", d.Text)
		t.Logf("--- 可点项(前120) ---\n%v", d.Items)
		t.Logf("--- 链接(前120) ---\n%v", d.Links)
	}
}

// probeDumpJS 把页面结构 dump 出来。
const probeDumpJS = `(() => {
  const norm = e => ((e.innerText || e.textContent || '')).replace(/\s+/g,' ').trim();
  const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const text = ((document.body && document.body.innerText) || '').replace(/\s+/g,' ').slice(0, 2500);
  const items = [], links = [], seen = new Set();
  for (const e of document.querySelectorAll('a,button,div,span,li,p,[role=button],[role=menuitem]')) {
    const t = norm(e);
    if (!t || t.length > 40) continue;
    if (!vis(e)) continue;
    if (seen.has(t)) continue;
    seen.add(t);
    items.push(t);
    if (items.length > 400) break;
  }
  for (const a of document.querySelectorAll('a[href]')) {
    const h = a.getAttribute('href');
    if (h && !links.includes(h)) links.push(h);
    if (links.length > 200) break;
  }
  return JSON.stringify({ text, items: items.slice(0,400), links: links.slice(0,200) });
})()`

func launchChromeHeadless(chrome string, port int, dir string) error {
	cmd := exec.Command(chrome,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+dir, "--no-first-run", "--no-default-browser-check",
		"--headless=new", "--disable-gpu", "--window-size=1600,1000", "about:blank")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
