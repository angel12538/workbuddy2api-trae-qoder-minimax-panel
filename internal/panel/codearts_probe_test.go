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
//	$env:CODEARTS_PROBE='1'
//	$env:CODEARTS_POOL_FILE='D:\WorkBuddy\data\ide_accounts\codearts_accounts.json'
//	go test ./internal/panel/ -run TestCodeArtsProbe -v -timeout 400s
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
		t.Fatalf("写 Cookie 失败: %v", err)
	}
	if err := caNavigate(ws, codeartsConsoleURL); err != nil {
		t.Logf("导航失败: %v", err)
	}
	time.Sleep(8 * time.Second)

	t.Logf("=== 页面 %s", caPageURL(port, "huaweicloud"))
	dumpProbe(t, ws, "初始页")

	// 关键：用**真实鼠标**点「活动福利」，把打开后的面板内容 dump 出来
	//（未登录也能看到面板结构 —— 入口长什么样一目了然）。
	for _, kw := range []string{"活动福利", "限时福利", "福利中心"} {
		ok, tx, cerr := caClickByText(ws, kw, 16)
		t.Logf("点击 %q -> ok=%v text=%q err=%v", kw, ok, tx, cerr)
		if !ok {
			continue
		}
		time.Sleep(4 * time.Second)
		dumpProbe(t, ws, "点开 "+tx+" 后")
		break
	}
}

// dumpProbe 把当前页面的结构打出来。
func dumpProbe(t *testing.T, ws *wsConn, tag string) {
	t.Helper()
	raw, err := caEval(ws, probeDumpJS)
	if err != nil {
		t.Logf("[%s] 取页面失败: %v", tag, err)
		return
	}
	var d struct {
		Text  string   `json:"text"`
		Items []string `json:"items"`
		Links []string `json:"links"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Logf("[%s] 解析失败: %v raw=%s", tag, err, string(raw))
		return
	}
	t.Logf("[%s] 正文(前2500) ---\n%s", tag, d.Text)
	t.Logf("[%s] 可点项(前150) ---\n%v", tag, d.Items)
	if len(d.Links) > 0 {
		t.Logf("[%s] 链接(前100) ---\n%v", tag, d.Links)
	}
}

// probeDumpJS 把页面结构 dump 出来。
const probeDumpJS = `(() => {
  const norm = e => ((e.innerText || e.textContent || '')).replace(/\s+/g, ' ').trim();
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

// launchChromeHeadless 起一个无头浏览器，返回清理函数。
//
// ⚠️ 必须把清理函数用 defer 掉：go test 结束时子进程会被**遗留**（孤儿
// Chrome），一个个占着 9333-9341 调试端口，还吃内存 —— 实测漏了 97 个
// 进程，最后把网关自己挤挂了。
func launchChromeHeadless(chrome string, port int, dir string) (func(), error) {
	cmd := exec.Command(chrome,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+dir, "--no-first-run", "--no-default-browser-check",
		"--headless=new", "--disable-gpu", "--window-size=1600,1000", "about:blank")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	cleanup := func() {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(8 * time.Second):
		}
	}
	return cleanup, nil
}
