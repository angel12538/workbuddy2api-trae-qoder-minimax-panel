package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// codearts_cdp.go CodeArts 通道用的 CDP 小工具（打开标签页 / 导航 / 写 Cookie）。
// 复用 wsclient + doubaoCDPCall（同一套 CDP 调用与解包逻辑）。

// caOpenTab 新建一个标签页并返回它的 ws 连接。
func caOpenTab(port int, target string) (*wsConn, error) {
	// 新版 Chrome 要求 PUT；老版接受 GET，先试 PUT。
	u := fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", port, url.QueryEscape(target))
	cl := &http.Client{Timeout: 6 * time.Second}
	var body []byte
	ok := false
	for _, m := range []string{http.MethodPut, http.MethodGet} {
		req, err := http.NewRequest(m, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := cl.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			body, ok = b, true
			break
		}
	}
	if !ok {
		// 退回：找已有的 page 目标
		list, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
		if err != nil {
			return nil, fmt.Errorf("无法创建标签页: %w", err)
		}
		defer list.Body.Close()
		var targets []struct {
			Type string `json:"type"`
			WS   string `json:"webSocketDebuggerUrl"`
		}
		raw, _ := io.ReadAll(io.LimitReader(list.Body, 1<<20))
		if json.Unmarshal(raw, &targets) != nil {
			return nil, fmt.Errorf("浏览器没有可用的标签页")
		}
		for _, t := range targets {
			if t.Type == "page" && t.WS != "" {
				return wsDial(t.WS, 6*time.Second)
			}
		}
		return nil, fmt.Errorf("浏览器没有可用的标签页")
	}
	var t struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.WS == "" {
		return nil, fmt.Errorf("标签页没有调试地址")
	}
	return wsDial(t.WS, 6*time.Second)
}

// caNavigate 导航到指定地址。
func caNavigate(ws *wsConn, target string) error {
	_, err := doubaoCDPCall(ws, 2, "Page.navigate", map[string]any{"url": target}, 15*time.Second)
	return err
}

// caEval 在页面里执行 JS（返回页面 JSON 字符串）。
func caEval(ws *wsConn, expr string) (json.RawMessage, error) {
	return doubaoCDPCall(ws, 3, "Runtime.evaluate", map[string]any{
		"expression": expr, "awaitPromise": true, "returnByValue": true,
	}, 45*time.Second)
}

// caSetCookies 把号池里的 Cookie 写进浏览器（HttpOnly 也能写）。
func caSetCookies(port int, cookies []codearts.Cookie) error {
	if len(cookies) == 0 {
		return fmt.Errorf("该账号没有 Cookie")
	}
	cl := &http.Client{Timeout: 6 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(raw, &v) != nil || v.WS == "" {
		return fmt.Errorf("浏览器调试端口没返回 webSocketDebuggerUrl")
	}
	ws, err := wsDial(v.WS, 6*time.Second)
	if err != nil {
		return err
	}
	defer ws.Close()

	list := make([]map[string]any, 0, len(cookies))
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		d := c.Domain
		if d == "" {
			d = ".huaweicloud.com"
		}
		if !strings.HasPrefix(d, ".") && !strings.HasPrefix(d, "console.") && !strings.HasPrefix(d, "codearts.") {
			d = "." + d
		}
		item := map[string]any{
			"name": c.Name, "value": c.Value, "domain": d, "path": c.Path,
			"secure": true,
		}
		if item["path"] == "" {
			item["path"] = "/"
		}
		list = append(list, item)
	}
	buf, _ := json.Marshal(map[string]any{
		"id": 11, "method": "Storage.setCookies", "params": map[string]any{"cookies": list},
	})
	if err := ws.writeText(buf); err != nil {
		return err
	}
	// 读回确认（忽略非本次请求的消息）
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := ws.readMessage(10 * time.Second)
		if err != nil {
			return err
		}
		var r struct {
			ID    int             `json:"id"`
			Error *struct{ Message string } `json:"error"`
		}
		if json.Unmarshal(msg, &r) != nil || r.ID != 11 {
			continue
		}
		if r.Error != nil {
			return fmt.Errorf("%s", r.Error.Message)
		}
		return nil
	}
	return fmt.Errorf("写入 Cookie 超时")
}

// caEvalExistingPage 在**已存在的**页面里求值（不新开标签），用于救回
// 服务重启后丢失句柄的登录窗口。返回页面文本。
func caEvalExistingPage(port int, urlHint string) (string, error) {
	cl := &http.Client{Timeout: 6 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var targets []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
		WS   string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(raw, &targets); err != nil {
		return "", err
	}
	var pick string
	for _, t := range targets {
		if t.Type != "page" || t.WS == "" {
			continue
		}
		if urlHint == "" || strings.Contains(t.URL, urlHint) {
			pick = t.WS
			break
		}
	}
	if pick == "" {
		return "", fmt.Errorf("没有匹配的页面（url 需包含 %q）", urlHint)
	}
	ws, err := wsDial(pick, 6*time.Second)
	if err != nil {
		return "", err
	}
	defer ws.Close()
	raw2, err := doubaoCDPCall(ws, 31, "Runtime.evaluate", map[string]any{
		"expression":    `(() => ((document.body && document.body.innerText) || '').replace(/\s+/g,' ').slice(0, 4000))()`,
		"returnByValue": true,
	}, 20*time.Second)
	if err != nil {
		return "", err
	}
	var s string
	if json.Unmarshal(raw2, &s) == nil {
		return s, nil
	}
	return string(raw2), nil
}

// caPageURL 返回匹配页面的当前 URL（用于判断是否已跳出登录页）。
func caPageURL(port int, urlHint string) string {
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var targets []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if json.Unmarshal(raw, &targets) != nil {
		return ""
	}
	for _, t := range targets {
		if t.Type == "page" && (urlHint == "" || strings.Contains(t.URL, urlHint)) {
			return t.URL
		}
	}
	return ""
}

// caEvalOnPage 在已存在的页面里执行 JS（awaitPromise + returnByValue）。
func caEvalOnPage(port int, urlHint, expr string) (json.RawMessage, error) {
	cl := &http.Client{Timeout: 6 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var targets []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
		WS   string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(raw, &targets) != nil {
		return nil, fmt.Errorf("无法解析标签页列表")
	}
	var pick string
	for _, t := range targets {
		if t.Type == "page" && t.WS != "" && strings.Contains(t.URL, urlHint) {
			pick = t.WS
			break
		}
	}
	if pick == "" {
		return nil, fmt.Errorf("没找到包含 %q 的页面", urlHint)
	}
	ws, err := wsDial(pick, 6*time.Second)
	if err != nil {
		return nil, err
	}
	defer ws.Close()
	return doubaoCDPCall(ws, 41, "Runtime.evaluate", map[string]any{
		"expression": expr, "awaitPromise": true, "returnByValue": true,
	}, 60*time.Second)
}

// caConnectPage 连上已存在的页面（按 URL 片段），返回 ws。
func caConnectPage(port int, urlHint string) (*wsConn, error) {
	cl := &http.Client{Timeout: 6 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var targets []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
		WS   string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(raw, &targets) != nil {
		return nil, fmt.Errorf("无法解析标签页列表")
	}
	for _, t := range targets {
		if t.Type == "page" && t.WS != "" && strings.Contains(t.URL, urlHint) {
			return wsDial(t.WS, 6*time.Second)
		}
	}
	return nil, fmt.Errorf("没找到包含 %q 的页面", urlHint)
}

// caClickByText 用**真实鼠标事件**点一个有指定文案的元素。
//
// 为什么不用 e.click()：CodeArts 控制台是 React 应用，合成 click 常常不触发
// 菜单展开（实测「活动福利」点了没反应）；豆包那边同样结论，用 CDP Input 域
// 派发鼠标事件才有效。
func caClickByText(ws *wsConn, kw string, maxLen int) (bool, string, error) {
	expr := fmt.Sprintf(`(() => {
      const kw = %q, maxLen = %d;
      const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
      const norm = e => ((e.innerText || e.textContent || '')).replace(/\s+/g, ' ').trim();
      const els = [...document.querySelectorAll('a,button,div,span,li,p')];
      const cands = els.filter(e => { const t = norm(e); return t.includes(kw) && t.length <= maxLen && vis(e); });
      cands.sort((a, b) => norm(a).length - norm(b).length);
      const e = cands[0];
      if (!e) return JSON.stringify({ found: false });
      e.scrollIntoView({ block: 'center', inline: 'center' });
      const r = e.getBoundingClientRect();
      return JSON.stringify({ found: true, text: norm(e),
        x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) });
    })()`, kw, maxLen)
	raw, err := caEval(ws, expr)
	if err != nil {
		return false, "", err
	}
	var loc struct {
		Found bool    `json:"found"`
		Text  string  `json:"text"`
		X     float64 `json:"x"`
		Y     float64 `json:"y"`
	}
	if json.Unmarshal(raw, &loc) != nil || !loc.Found {
		return false, "", nil
	}
	if err := doubaoCDPClick(ws, 61, loc.X, loc.Y); err != nil {
		return false, loc.Text, err
	}
	return true, loc.Text, nil
}
