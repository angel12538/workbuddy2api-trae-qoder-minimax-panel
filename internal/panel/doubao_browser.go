// doubao_browser.go 豆包「浏览器登录」：面板自拉一个受控浏览器窗口打开豆包网页版，
// 用户在浏览器里登录，面板用 CDP 把该会话的 Cookie 捞出来直接写进豆包号池。
//
// 为什么这条路成立：豆包签到用的凭据就是**网页 Cookie**（sessionid / sessionid_ss /
// ttwid / msToken …），而「从客户端提取」拿到的也是同一批 Cookie。所以浏览器登录
// ＝ 直接拿到签到凭据，不需要客户端、不需要手工粘贴。
//
// 与 Trae 浏览器登录同一套机制（自拉浏览器 + 独立 profile + CDP 捞凭据），但：
//   - Trae 捞的是页面存储里的 JWT；豆包捞的是 **Cookie**；
//   - 豆包的 sessionid 是 **HttpOnly**，document.cookie 读不到，必须用 CDP 的
//     Network.getAllCookies（它会把 HttpOnly 一并返回）。
//
// 每次都用全新 profile：多账号可反复添加，且不会复用上一个号的登录态。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/doubao"
)

// doubaoBrowserPorts 调试端口候选（与 Trae 共用同一段区间，端口由 freePort 实际占用）。
var doubaoBrowserPorts = traeBrowserPorts

const doubaoBrowserTTL = 15 * time.Minute

// doubaoBrowserSession 一次「拉浏览器取豆包 Cookie」的进行态（同时只允许一个）。
var doubaoBrowserSession struct {
	mu        sync.Mutex
	port      int
	dir       string
	proc      *exec.Cmd
	startedAt time.Time
	done      bool
	ok        bool
	message   string
}

// doubaoBrowserStart 拉起浏览器（新 profile）并打开豆包网页版。
func (p *Panel) doubaoBrowserStart(w http.ResponseWriter, r *http.Request) {
	doubaoBrowserSession.mu.Lock()
	defer doubaoBrowserSession.mu.Unlock()
	if doubaoBrowserSession.proc != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "running": true,
			"message": "浏览器已在运行，请在打开的窗口里登录豆包账号",
		})
		return
	}
	chrome, err := findChrome()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	port, err := freePort(doubaoBrowserPorts)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	dir := filepath.Join(traeBrowserBaseDir(p), "doubao-browser", "pending-"+time.Now().Format("20060102-150405"))
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs // Chrome 按自身安装目录解析相对路径，必须给绝对路径
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "创建浏览器 profile 目录失败: "+err.Error())
		return
	}
	cmd := exec.Command(chrome,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+dir,
		"--no-first-run", "--no-default-browser-check",
		"https://www.doubao.com/",
	)
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusBadGateway, "启动浏览器失败: "+err.Error())
		return
	}
	doubaoBrowserSession.port = port
	doubaoBrowserSession.dir = dir
	doubaoBrowserSession.proc = cmd
	doubaoBrowserSession.startedAt = time.Now()
	doubaoBrowserSession.done = false
	doubaoBrowserSession.ok = false
	doubaoBrowserSession.message = ""
	log.Printf("panel: 已拉起浏览器取豆包 Cookie（调试端口 %d，profile %s）", port, dir)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "running": true,
		"message": "已打开浏览器，请登录豆包账号（新窗口是干净会话，可登录任意账号）",
	})
}

// doubaoBrowserStop 关掉浏览器并清理会话态。
func (p *Panel) doubaoBrowserStop(w http.ResponseWriter, r *http.Request) {
	doubaoBrowserSession.mu.Lock()
	defer doubaoBrowserSession.mu.Unlock()
	doubaoStopBrowserLocked()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func doubaoStopBrowserLocked() {
	port := doubaoBrowserSession.port
	proc := doubaoBrowserSession.proc
	if port > 0 {
		traeCloseViaCDP(port)
	}
	if proc != nil && proc.Process != nil {
		traeWaitExit(proc.Process, 4*time.Second)
	}
	doubaoBrowserSession.proc = nil
	doubaoBrowserSession.port = 0
	doubaoBrowserSession.dir = ""
}

// doubaoBrowserPoll 轮询浏览器里的登录态；拿到 sessionid 就落盘账号。
func (p *Panel) doubaoBrowserPoll(w http.ResponseWriter, r *http.Request) {
	doubaoBrowserSession.mu.Lock()
	if doubaoBrowserSession.proc == nil {
		doubaoBrowserSession.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "浏览器未在运行"})
		return
	}
	if !doubaoBrowserSession.done && time.Since(doubaoBrowserSession.startedAt) > doubaoBrowserTTL {
		doubaoStopBrowserLocked()
		doubaoBrowserSession.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "expired": true, "message": "等待登录超时，请重新开始"})
		return
	}
	if doubaoBrowserSession.done {
		res := map[string]any{"done": true, "ok": doubaoBrowserSession.ok, "message": doubaoBrowserSession.message}
		doubaoBrowserSession.mu.Unlock()
		writeJSON(w, http.StatusOK, res)
		return
	}
	port := doubaoBrowserSession.port
	doubaoBrowserSession.mu.Unlock()

	cookies, err := doubaoBrowserExtractCookies(port)
	if err != nil || len(cookies) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待在浏览器中完成豆包登录…"})
		return
	}
	acc, added, err := doubao.Upsert(cookies, "browser")
	doubaoBrowserSession.mu.Lock()
	doubaoBrowserSession.done = true
	doubaoBrowserSession.ok = err == nil
	if err != nil {
		doubaoBrowserSession.message = err.Error()
	} else {
		verb := "已添加"
		if !added {
			verb = "已更新"
		}
		doubaoBrowserSession.message = verb + "：" + acc.Label
	}
	doubaoStopBrowserLocked()
	doubaoBrowserSession.mu.Unlock()
	if err != nil {
		log.Printf("panel: 浏览器取豆包 Cookie 后保存失败: %v", err)
	} else {
		log.Printf("panel: 浏览器取豆包 Cookie 成功：%s", doubaoBrowserSession.message)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true, "ok": err == nil, "message": doubaoBrowserSession.message,
	})
}

// doubaoBrowserExtractCookies 连上浏览器 CDP，取 doubao.com 域下的 Cookie。
//
// 必须用 Network.getAllCookies：sessionid 是 HttpOnly，页面里的 document.cookie
// 看不到它。取不到 sessionid / sessionid_ss 就说明还没登录。
func doubaoBrowserExtractCookies(port int) (map[string]string, error) {
	targets, err := traeBrowserTargets(port)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		if !strings.Contains(t.URL, "doubao.com") {
			continue
		}
		ws, err := wsDial(t.WSURL, 5*time.Second)
		if err != nil {
			continue
		}
		raw, err := doubaoCDPCall(ws, 9001, "Network.getAllCookies", map[string]any{}, 8*time.Second)
		ws.Close()
		if err != nil {
			continue
		}
		var resp struct {
			Result struct {
				Cookies []struct {
					Name   string `json:"name"`
					Value  string `json:"value"`
					Domain string `json:"domain"`
				} `json:"cookies"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &resp) != nil {
			continue
		}
		out := map[string]string{}
		for _, c := range resp.Result.Cookies {
			if !strings.Contains(c.Domain, "doubao.com") {
				continue
			}
			if c.Name == "" || c.Value == "" {
				continue
			}
			out[c.Name] = c.Value
		}
		if out["sessionid"] == "" && out["sessionid_ss"] == "" {
			continue // 还没登录（或拿到的不是豆包登录态）
		}
		return out, nil
	}
	return nil, nil
}
