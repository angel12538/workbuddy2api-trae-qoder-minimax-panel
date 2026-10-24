// web_cookies.go 浏览器级 Cookie 提取（CDP）。
//
// 为什么必须用**浏览器级**（/json/version → webSocketDebuggerUrl → Storage.getCookies）：
//
//  1. 页面级 Network.getAllCookies 在部分 Chrome 版本里要先 Network.enable 才返回
//     内容，否则是空数组 —— 2026-10-24 实测：豆包浏览器登录后一直"添加不了"。
//  2. 页面级只拿得到**当前页所属域**的 Cookie；而登录常常发生在弹窗/新标签页
//     （域名是 passport 之类），真正的 sessionid 落在别的域上 → 漏掉登录态。
//
// 浏览器级的 Storage.getCookies 一次返回**所有域**的 Cookie，登录弹窗在哪都不怕。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// cdpCookie 一条 Cookie（CDP Storage.getCookies 的形状）。
type cdpCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool   `json:"httpOnly"`
	Secure   bool   `json:"secure"`
}

// cdpAllCookies 取浏览器里**所有域**的 Cookie。
func cdpAllCookies(port int) ([]cdpCookie, error) {
	if port <= 0 {
		return nil, fmt.Errorf("调试端口无效")
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return nil, fmt.Errorf("连不上浏览器调试端口 %d: %w", port, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v struct {
		WSURL string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(raw, &v) != nil || v.WSURL == "" {
		return nil, fmt.Errorf("浏览器调试端口没返回 webSocketDebuggerUrl")
	}
	ws, err := wsDial(v.WSURL, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("CDP 连接失败: %w", err)
	}
	defer ws.Close()

	req := map[string]any{"id": 1, "method": "Storage.getCookies", "params": map[string]any{}}
	buf, _ := json.Marshal(req)
	if err := ws.writeText(buf); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := ws.readMessage(10 * time.Second)
		if err != nil {
			return nil, err
		}
		var resp struct {
			ID     int `json:"id"`
			Result struct {
				Cookies []cdpCookie `json:"cookies"`
			} `json:"result"`
		}
		if json.Unmarshal(msg, &resp) != nil || resp.ID != 1 {
			continue
		}
		return resp.Result.Cookies, nil
	}
	return nil, fmt.Errorf("CDP 取 Cookie 超时")
}

// cookiesForDomains 从浏览器里挑出属于这些域（后缀匹配）的 Cookie，合成 name→value。
func cookiesForDomains(cookies []cdpCookie, domains ...string) map[string]string {
	out := map[string]string{}
	for _, c := range cookies {
		if c.Name == "" || c.Value == "" {
			continue
		}
		host := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		for _, d := range domains {
			d = strings.TrimPrefix(strings.ToLower(d), ".")
			if d == "" {
				continue
			}
			if host == d || strings.HasSuffix(host, "."+d) {
				out[c.Name] = c.Value
				break
			}
		}
	}
	return out
}

// cookieDomainsOf 列出这些 Cookie 都在哪些域上（诊断用：告诉用户登录态到底落在哪）。
func cookieDomainsOf(cookies []cdpCookie, names ...string) []string {
	want := map[string]bool{}
	for _, n := range names {
		want[strings.ToLower(n)] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range cookies {
		if want[strings.ToLower(c.Name)] {
			d := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}
