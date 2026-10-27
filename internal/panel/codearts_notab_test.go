package panel

import (
	"os"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// 回归：登录窗口的轮询**不能新建标签页**。
//
// 症状（用户实测）：面板弹出登录窗口后，窗口一直刷新、标签页越开越多。
// 真因：轮询函数用 caOpenTab 每次都新建一个 chat/home 标签页，4 秒一轮。
// 登录窗口是用户正在操作的那一个，不能动它 —— 只能**读**已有页面。
//
// 运行：
//
//	$env:CODEARTS_PROBE='1'
//	go test ./internal/panel/ -run TestCodeArtsProbeNoNewTab -v -timeout 300s
func TestCodeArtsProbeNoNewTab(t *testing.T) {
	if os.Getenv("CODEARTS_PROBE") == "" {
		t.Skip("设 CODEARTS_PROBE=1 才跑")
	}
	_ = codearts.LoadPool // 保持 import（号池可能为空，测试只关心标签页行为）

	chrome, err := findChrome()
	if err != nil {
		t.Fatal(err)
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "ca-notab-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cleanup, err := launchChromeHeadless(chrome, port, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ws, err := caOpenTab(port, codeartsConsoleURL)
	for i := 0; i < 30 && err != nil; i++ {
		time.Sleep(500 * time.Millisecond)
		ws, err = caOpenTab(port, codeartsConsoleURL)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = ws
	time.Sleep(4 * time.Second)

	before, err := caListPages(port)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("没有页面")
	}
	t.Logf("初始页面数: %d", len(before))

	for i := 0; i < 5; i++ {
		_, _, _ = codeartsLoginProbe(port)
	}

	after, err := caListPages(port)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("轮询 5 次后页面数: %d", len(after))
	if len(after) != len(before) {
		t.Fatalf("轮询不许新建标签页：%d → %d", len(before), len(after))
	}
}
