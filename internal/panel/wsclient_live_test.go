package panel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestWSClientAgainstLiveChrome 对着**真实运行中的 Chrome** 验证 wsConn：
// 走 CDP Runtime.evaluate 求值一次。Chrome 没在跑就 Skip（不污染常规测试）。
//
// 这个测试存在的意义：wsConn 是手写的 WebSocket 实现（握手/掩码/分片），
// 出错时上层只会表现为「一直等待登录」，不报错——必须有一条能直接验证它的用例。
func TestWSClientAgainstLiveChrome(t *testing.T) {
	port := 0
	for p := 9333; p <= 9340; p++ {
		targets, err := traeBrowserTargets(p)
		if err != nil {
			continue
		}
		for _, tg := range targets {
			if strings.Contains(tg.URL, "trae.cn") && tg.WSURL != "" {
				port = p
				break
			}
		}
		if port != 0 {
			break
		}
	}
	if port == 0 {
		t.Skip("没有运行中的 trae.cn 调试浏览器，跳过")
	}
	targets, _ := traeBrowserTargets(port)
	var wsURL string
	for _, tg := range targets {
		if strings.Contains(tg.URL, "trae.cn") {
			wsURL = tg.WSURL
		}
	}
	ws, err := wsDial(wsURL, 5*time.Second)
	if err != nil {
		t.Fatalf("wsDial 失败: %v", err)
	}
	defer ws.Close()

	req, _ := json.Marshal(map[string]any{
		"id": 1, "method": "Runtime.evaluate",
		"params": map[string]any{"expression": "1+1", "returnByValue": true},
	})
	if err := ws.writeText(req); err != nil {
		t.Fatalf("writeText 失败: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := ws.readMessage(10 * time.Second)
		if err != nil {
			t.Fatalf("readMessage 失败: %v", err)
		}
		var resp struct {
			ID     int `json:"id"`
			Result struct {
				Result struct {
					Value float64 `json:"value"`
				} `json:"result"`
			} `json:"result"`
		}
		if json.Unmarshal(msg, &resp) == nil && resp.ID == 1 {
			if resp.Result.Result.Value != 2 {
				t.Fatalf("求值结果应为 2，got %v", resp.Result.Result.Value)
			}
			t.Logf("✓ CDP 求值成功（端口 %d）", port)
			return
		}
	}
	t.Fatal("CDP 求值超时")
}
