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
	"bytes"
	"sort"
	"sync"
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

// caPageInfo 一个已打开的标签页。
type caPageInfo struct {
	URL string
	WS  string
}

// caListPages 列出浏览器里**已存在**的页面（不新建）。
func caListPages(port int) ([]caPageInfo, error) {
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
	if err := json.Unmarshal(raw, &targets); err != nil {
		return nil, err
	}
	out := make([]caPageInfo, 0, len(targets))
	for _, t := range targets {
		if t.Type != "page" || t.WS == "" {
			continue
		}
		out = append(out, caPageInfo{URL: t.URL, WS: t.WS})
	}
	return out, nil
}

// caPickPage 选一个**已存在**的页面（绝不新建标签页）。
//
// 优先级：① URL 含 /chat 的控制台页；② URL 含 huaweicloud 的任意页；
// ③ 第一个 page。
//
// ⚠️ 这里必须复用已有页面：登录窗口是用户正在操作的页面，轮询时如果每次都
// 新开一个标签，用户那边看到的现象就是「窗口一直刷新」（而且会开出一堆
// 标签页）。实测踩过：4 秒一次轮询 → 无限刷新。
func caPickPage(port int) (*wsConn, string, error) {
	pages, err := caListPages(port)
	if err != nil {
		return nil, "", err
	}
	var chat, hw, any *caPageInfo
	for i := range pages {
		p := &pages[i]
		switch {
		case strings.Contains(p.URL, "/chat"):
			if chat == nil {
				chat = p
			}
		case strings.Contains(p.URL, "huaweicloud"):
			if hw == nil {
				hw = p
			}
		default:
			if any == nil {
				any = p
			}
		}
	}
	pick := chat
	if pick == nil {
		pick = hw
	}
	if pick == nil {
		pick = any
	}
	if pick == nil {
		return nil, "", fmt.Errorf("浏览器里没有可用的页面")
	}
	ws, err := wsDial(pick.WS, 6*time.Second)
	if err != nil {
		return nil, "", err
	}
	return ws, pick.URL, nil
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

// caClickExactText 用**真实鼠标**点一个**文案完全相等**的元素（可多个候选，
// 按候选顺序命中第一个）。
//
// 为什么要有精确版：caClickByText 是「包含」匹配，maxLen=6 时「每日领取」
// 也会被「领取」命中 —— 领任务奖励时会把刚点过的签到按钮再点一遍。
func caClickExactText(ws *wsConn, labels []string) (bool, string, error) {
	expr := fmt.Sprintf(`(() => {
      const labels = %s;
      const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
      const norm = e => ((e.innerText || e.textContent || '')).replace(/\s+/g, ' ').trim();
      const els = [...document.querySelectorAll('a,button,div,span,li,p,[role=button]')];
      for (const lab of labels) {
        const cands = els.filter(e => norm(e) === lab && vis(e) && !e.disabled);
        if (cands.length) {
          const e = cands[0];
          e.scrollIntoView({ block: 'center', inline: 'center' });
          const r = e.getBoundingClientRect();
          return JSON.stringify({ found: true, text: lab,
            x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) });
        }
      }
      return JSON.stringify({ found: false });
    })()`, jsonList(labels))
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
	if err := doubaoCDPClick(ws, 62, loc.X, loc.Y); err != nil {
		return false, loc.Text, err
	}
	return true, loc.Text, nil
}

// jsonList 把字符串数组渲染成 JS 字面量。
func jsonList(items []string) string {
	buf, _ := json.Marshal(items)
	return string(buf)
}

// caCollectHints 收集页面上所有含「领取/签到/积分/登录」的短文案，
// 用于诊断「入口长什么样」（面板改版时靠它定位真实按钮文案）。
func caCollectHints(ws *wsConn) []string {
	raw, err := caEval(ws, `(() => {
      const norm = e => ((e.innerText || e.textContent || '')).replace(/\s+/g, ' ').trim();
      const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
      const out = [];
      for (const e of document.querySelectorAll('a,button,div,span,li,p')) {
        const t = norm(e);
        if (!t || t.length > 18 || !vis(e)) continue;
        if (/(领取|签到|积分|登录)/.test(t) && !out.includes(t)) out.push(t);
        if (out.length >= 40) break;
      }
      return JSON.stringify(out);
    })()`)
	if err != nil {
		return nil
	}
	var out []string
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
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

// caDiscoverPorts 扫描 9333-9433，找出真正在跑 CodeArts 页面的调试端口。
//
// 为什么需要：面板的登录会话存在内存里，服务一重启就丢；此时浏览器窗口还开着，
// 但 poll 只会说"未启动"，用户以为扫描坏了。这里直接按端口探活，把窗口找回来。
func caDiscoverPorts() []int {
	var mu sync.Mutex
	var hits []int
	var wg sync.WaitGroup
	for p := 9333; p <= 9433; p++ {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			cl := &http.Client{Timeout: 400 * time.Millisecond}
			resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
			if err != nil {
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if bytes.Contains(raw, []byte("devcloud")) && bytes.Contains(raw, []byte("chat")) {
				mu.Lock()
				hits = append(hits, port)
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	sort.Ints(hits)
	return hits
}

// caGracefulClose 让 Chrome 自己退出（CDP Browser.close），确保 Cookie 落盘。
//
// 为什么要这一步：直接 Process.Kill 时 Chrome 来不及把 Cookie 写进
// Network/Cookies（有写缓存），归档出来的 profile 就是"空壳"——
// 下次打开页面仍显示「登录华为云」，签到便报"登录已过期"。
func caGracefulClose(port int) bool {
	if port <= 0 {
		return false
	}
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(raw, &v) != nil || v.WS == "" {
		return false
	}
	ws, err := wsDial(v.WS, 4*time.Second)
	if err != nil {
		return false
	}
	defer ws.Close()
	buf, _ := json.Marshal(map[string]any{"id": 1, "method": "Browser.close"})
	if err := ws.writeText(buf); err != nil {
		return false
	}
	time.Sleep(800 * time.Millisecond) // 给落盘留时间
	return true
}
