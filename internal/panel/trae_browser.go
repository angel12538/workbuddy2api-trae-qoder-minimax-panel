// trae_browser.go 面板自拉浏览器取 Trae 凭证（替代「授权页 OAuth」路线）。
//
// 为什么换路线：trae.cn 的 /authorization 页在「认证中」阶段要探测本地客户端，
// 而且它**复用浏览器的登录态**——你已登录就直接进授权页（卡在认证中），
// 想加第二个账号又没有干净的会话可切。两条路都堵死。
//
// 改成助手那套思路，但更简单：
//  1. 面板自己拉起系统 Edge/Chrome，带独立 --user-data-dir（**每次一个全新 profile**）
//     → 每个账号都能从零登录，多账号天然可用
//  2. 用户在这个浏览器里登录 trae.cn
//  3. 面板用 CDP 连上去，从 localStorage / sessionStorage / cookie 里捞 JWT
//  4. 拿到就调 GetUserInfo 补昵称，写进 checkin_accounts.json，然后关掉浏览器
//
// 不依赖授权页、不依赖助手、不需要粘贴任何东西。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// traeBrowserPorts 调试端口候选范围（避开 9222，减少与用户自开调试端口的冲突）。
var traeBrowserPorts = func() []int {
	out := make([]int, 0, 100)
	for p := 9333; p <= 9433; p++ {
		out = append(out, p)
	}
	return out
}()

const traeBrowserTTL = 10 * time.Minute

// traeBrowserSession 一次「拉浏览器取凭证」的进行态（同时只允许一个）。
var traeBrowserSession struct {
	mu        sync.Mutex
	port      int
	dir       string
	proc      *exec.Cmd
	startedAt time.Time
	done      bool
	ok        bool
	message   string
}

// findChrome 找系统浏览器（优先 Chrome，回落 Edge）。
func findChrome() (string, error) {
	cands := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	}
	for _, p := range cands {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到 Chrome / Edge，请先安装其中之一")
}

func freePort(ports []int) (int, error) {
	for _, p := range ports {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("9333-9433 无可用调试端口")
}

// traeBrowserBaseDir 浏览器 profile 的根目录：config.json 所在目录下的 data/。
// 取不到就回落系统临时目录（功能仍可用，只是重启后 profile 不保留——对「每次全新
// 会话」的语义来说反而更干净）。
//
// ⚠️ **必须返回绝对路径**：ConfigPath 常是相对路径（命令行传的 "config.json"），
// 而 Chrome 是以**自身安装目录**为 cwd 去解析 --user-data-dir 的相对路径的——
// 实测把 190MB+ 的 profile 写进了 `C:\Program Files\Google\Chrome\Application\data\`，
// 我们自己的目录反而空空如也，自动续期自然读不到登录态。
func traeBrowserBaseDir(p *Panel) string {
	if d := strings.TrimSpace(p.cfg.ConfigPath); d != "" {
		dir := filepath.Dir(d)
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		return filepath.Join(dir, "data")
	}
	return filepath.Join(os.TempDir(), "wb2api")
}

// traeBrowserStart 拉起浏览器（新 profile）并打开 trae.cn。
func (p *Panel) traeBrowserStart(w http.ResponseWriter, r *http.Request) {
	traeBrowserSession.mu.Lock()
	defer traeBrowserSession.mu.Unlock()
	if traeBrowserSession.proc != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "浏览器已在运行，请在打开的窗口里登录 Trae"})
		return
	}
	chrome, err := findChrome()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// 每次全新 profile：保证从零登录，多账号可反复添加。
	// 目录落在 config.json 同级的 data/ 下（与 state_file 同域），便于用户自行清理。
	// 登录成功后会被 traeArchiveProfile 归档成 acc-<UserID>，供日后自动续期复用。
	dir := filepath.Join(traeBrowserBaseDir(p), "trae-browser", "pending-"+time.Now().Format("20060102-150405"))
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs // Chrome 按自身安装目录解析相对路径，必须给绝对路径（见 traeBrowserBaseDir 注释）
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "创建浏览器 profile 目录失败: "+err.Error())
		return
	}
	cmd := exec.Command(chrome,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+dir,
		"--no-first-run", "--no-default-browser-check",
		"https://www.trae.cn/",
	)
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusBadGateway, "启动浏览器失败: "+err.Error())
		return
	}
	traeBrowserSession.port = port
	traeBrowserSession.dir = dir
	traeBrowserSession.proc = cmd
	traeBrowserSession.startedAt = time.Now()
	traeBrowserSession.done = false
	traeBrowserSession.ok = false
	traeBrowserSession.message = ""
	log.Printf("panel: 已拉起浏览器取 Trae 凭证（调试端口 %d，profile %s）", port, dir)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已打开浏览器，请登录 Trae 账号（新窗口是干净会话，可登录任意账号）"})
}

// traeBrowserStop 关掉浏览器并清理会话态。
func (p *Panel) traeBrowserStop(w http.ResponseWriter, r *http.Request) {
	traeBrowserSession.mu.Lock()
	defer traeBrowserSession.mu.Unlock()
	traeStopBrowserLocked()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// traeCloseViaCDP 通过 CDP 的 Browser.close 优雅关闭浏览器（含全部子进程）。
//
// 为什么不能只 Kill 主进程：Chrome 是**多进程**架构，`Process.Kill()` 只杀启动器，
// renderer/gpu/network 等子进程会变成孤儿继续占着 --user-data-dir，导致后续
// `os.Rename` 归档 profile 报 "Access is denied"（实测踩过）。优雅关闭还能让
// Chrome 把 profile 完整落盘，下次启动不弹「未正常关闭」提示。
// 失败无妨——调用方随后会兜底强杀。
func traeCloseViaCDP(port int) {
	if port <= 0 {
		return
	}
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v struct {
		WSURL string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(raw, &v) != nil || v.WSURL == "" {
		return
	}
	ws, err := wsDial(v.WSURL, 3*time.Second)
	if err != nil {
		return
	}
	defer ws.Close()
	_ = ws.writeText([]byte(`{"id":1,"method":"Browser.close"}`))
}

// traeWaitExit 等进程退出，超时则强杀。
func traeWaitExit(proc *os.Process, timeout time.Duration) {
	if proc == nil {
		return
	}
	done := make(chan struct{})
	go func() { _, _ = proc.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = proc.Kill()
	}
}

func traeStopBrowserLocked() {
	port := traeBrowserSession.port
	proc := traeBrowserSession.proc
	if port > 0 {
		traeCloseViaCDP(port) // 先优雅关：Chrome 会把 profile 落盘、子进程一并退出
	}
	if proc != nil && proc.Process != nil {
		traeWaitExit(proc.Process, 4*time.Second)
	}
	traeBrowserSession.proc = nil
	traeBrowserSession.port = 0
	traeBrowserSession.dir = ""
}

// traeBrowserPoll 轮询浏览器里的登录态；拿到 JWT 就落盘账号。
func (p *Panel) traeBrowserPoll(w http.ResponseWriter, r *http.Request) {
	traeBrowserSession.mu.Lock()
	if traeBrowserSession.proc == nil {
		traeBrowserSession.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "浏览器未在运行"})
		return
	}
	if !traeBrowserSession.done && time.Since(traeBrowserSession.startedAt) > traeBrowserTTL {
		traeStopBrowserLocked()
		traeBrowserSession.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "expired": true, "message": "等待登录超时，请重新开始"})
		return
	}
	if traeBrowserSession.done {
		res := map[string]any{"done": true, "ok": traeBrowserSession.ok, "message": traeBrowserSession.message}
		traeBrowserSession.mu.Unlock()
		writeJSON(w, http.StatusOK, res)
		return
	}
	port := traeBrowserSession.port
	dir := traeBrowserSession.dir
	traeBrowserSession.mu.Unlock()

	jwt, err := traeBrowserExtract(port)
	if err != nil || jwt == "" {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待在浏览器中完成 Trae 登录…"})
		return
	}
	userID := upstream.TraeUserIDFromJWT(jwt)
	msg, err := p.traeSaveFromJWT(jwt)
	traeBrowserSession.mu.Lock()
	traeBrowserSession.done = true
	traeBrowserSession.ok = err == nil
	if err != nil {
		traeBrowserSession.message = err.Error()
	} else {
		traeBrowserSession.message = msg
	}
	traeStopBrowserLocked()
	traeBrowserSession.mu.Unlock()
	// 关掉浏览器**之后**再归档 profile：Windows 下进程未退出时目录被占用，rename 会失败。
	if err == nil {
		traeArchiveProfile(p, dir, userID)
	}
	if err != nil {
		log.Printf("panel: 浏览器取 Trae 凭证后保存失败: %v", err)
	} else {
		log.Printf("panel: 浏览器取 Trae 凭证成功：%s", msg)
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "ok": err == nil, "message": traeBrowserSession.message})
}

// traeBrowserExtract 连上浏览器 CDP，从页面存储里捞一个 JWT。
//
// 扫三处：localStorage / sessionStorage / document.cookie —— 站点把 Cloud-IDE 令牌
// 放哪一处随版本变化，全扫一遍比赌某一处稳。命中后**必须在页面里校验一次可用性**，
// 因为存储里可能残留已过期的旧令牌。
func traeBrowserExtract(port int) (string, error) {
	targets, err := traeBrowserTargets(port)
	if err != nil {
		return "", err
	}
	for _, t := range targets {
		if !strings.Contains(t.URL, "trae.cn") {
			continue
		}
		ws, err := wsDial(t.WSURL, 5*time.Second)
		if err != nil {
			continue
		}
		jwt, err := traeEvalJWT(ws)
		ws.Close()
		if err == nil && jwt != "" {
			return jwt, nil
		}
	}
	return "", nil
}

type traeTarget struct {
	URL   string `json:"url"`
	WSURL string `json:"webSocketDebuggerUrl"`
}

func traeBrowserTargets(port int) ([]traeTarget, error) {
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out []traeTarget
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// traeJWTScanJS 在页面里扫 JWT。返回 JSON 数组（key + 候选值）。
const traeJWTScanJS = `(() => {
  const out = [];
  const isJWT = s => typeof s === 'string' && /^eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\./.test(s);
  const grab = (src, k, v) => {
    if (!v) return;
    if (isJWT(v)) { out.push({src, k, v}); return; }
    const m = String(v).match(/eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}/);
    if (m) out.push({src, k, v: m[0]});
  };
  try { for (let i = 0; i < localStorage.length; i++) { const k = localStorage.key(i); grab('local', k, localStorage.getItem(k)); } } catch (e) {}
  try { for (let i = 0; i < sessionStorage.length; i++) { const k = sessionStorage.key(i); grab('session', k, sessionStorage.getItem(k)); } } catch (e) {}
  try { (document.cookie || '').split(';').forEach(c => { const i = c.indexOf('='); if (i > 0) grab('cookie', c.slice(0, i).trim(), c.slice(i + 1).trim()); }); } catch (e) {}
  return JSON.stringify(out);
})()`

// traeEvalJWT 连一个页面 target，执行扫描脚本并挑出一个可用 JWT。
func traeEvalJWT(ws *wsConn) (string, error) {
	req := map[string]any{
		"id": 1, "method": "Runtime.evaluate",
		"params": map[string]any{"expression": traeJWTScanJS, "returnByValue": true, "awaitPromise": true},
	}
	raw, _ := json.Marshal(req)
	if err := ws.writeText(raw); err != nil {
		return "", err
	}
	// 页面里可能有别的 CDP 事件先到，循环直到拿到 id=1 的响应。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := ws.readMessage(8 * time.Second)
		if err != nil {
			return "", err
		}
		var resp struct {
			ID     int `json:"id"`
			Result struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			} `json:"result"`
		}
		if err := json.Unmarshal(msg, &resp); err != nil || resp.ID != 1 {
			continue
		}
		var cands []struct{ Src, K, V string }
		if err := json.Unmarshal([]byte(resp.Result.Result.Value), &cands); err != nil {
			return "", nil
		}
		for _, c := range cands {
			if jwt := traePickUsableJWT(c.V); jwt != "" {
				return jwt, nil
			}
		}
		return "", nil
	}
	return "", fmt.Errorf("CDP 求值超时")
}

// traePickUsableJWT 判断候选是不是「能用的 Trae access token」：
// 必须能解析出 user id，且 exp 未过期。
func traePickUsableJWT(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	jwt := v
	if !strings.HasPrefix(jwt, "Cloud-IDE-JWT ") {
		jwt = "Cloud-IDE-JWT " + jwt
	}
	if upstream.TraeUserIDFromJWT(jwt) == "" {
		return ""
	}
	if exp, ok := upstream.JWTExpiry(jwt); ok && time.Now().After(exp) {
		return "" // 存储里的过期残留，别写进账号文件
	}
	return jwt
}

// traeSaveFromJWT 由 JWT 补齐昵称并落盘账号。
func (p *Panel) traeSaveFromJWT(jwt string) (string, error) {
	userID := upstream.TraeUserIDFromJWT(jwt)
	if userID == "" {
		return "", fmt.Errorf("无法从 JWT 中解析 user_id")
	}
	name := ""
	if _, uname, err := traeGetUserInfo(jwt); err == nil {
		name = uname
	}
	if name == "" {
		s := userID
		if len(s) > 8 {
			s = s[:8]
		}
		name = "账号_" + s
	}
	added, err := upstream.SaveTraeAccount(p.cfg.TraeDataDir, upstream.TraeAccount{
		Name: name, UserID: userID, JWT: jwt,
	})
	if err != nil {
		return "", fmt.Errorf("写入 Trae 账号文件失败: %w", err)
	}
	verb := "更新"
	if added {
		verb = "新增"
	}
	return fmt.Sprintf("%s账号 %s（UserID %s）", verb, name, userID), nil
}

// ── 持久 profile 与自动续期 ───────────────────────────────────────────────
//
// 「添加账号」用的是一个**临时** profile（pending-<时间戳>，保证从零登录、可加多账号）；
// 登录成功后把它**归档**成 acc-<UserID>，此后自动续期就用这个目录重开浏览器——
// 因为里面留着 trae.cn 的长效登录态，站点会自己换新 JWT，不必再让人登录。

// traeProfileDirFor 某账号绑定的持久浏览器 profile 目录。
//
// 按 user_id 确定性命名（**不写进 checkin_accounts.json**）：既不侵入助手共用的
// 文件格式，也让「加账号」与「自动续期」两边天然指向同一个目录。
func traeProfileDirFor(p *Panel, userID string) string {
	return filepath.Join(traeBrowserBaseDir(p), "trae-browser", "acc-"+userID)
}

// traeArchiveProfile 把临时登录 profile 归档为 acc-<userID>。
// 失败不影响本次添加，只是该账号以后不能走「浏览器续期」通道（仍可走 refresh_token）。
func traeArchiveProfile(p *Panel, dir, userID string) {
	if dir == "" || userID == "" {
		return
	}
	dst := traeProfileDirFor(p, userID)
	if filepath.Clean(dir) == filepath.Clean(dst) {
		return
	}
	if _, err := os.Stat(dst); err == nil {
		_ = os.RemoveAll(dst) // 同账号重新登录：覆盖旧会话
	}
	// 重试：浏览器子进程刚退出时目录可能仍被短暂占用（Windows 的删除语义滞后）。
	var rerr error
	for i := 0; i < 8; i++ {
		if rerr = os.Rename(dir, dst); rerr == nil {
			log.Printf("panel: Trae 浏览器 profile 已归档 %s → %s", filepath.Base(dir), filepath.Base(dst))
			return
		}
		time.Sleep(700 * time.Millisecond)
	}
	log.Printf("panel: Trae 浏览器 profile 归档失败（仅影响后续自动续期，不影响账号添加）: %v", rerr)
}

// traeBrowserRefresh 用账号绑定的持久 profile 拉起**无头**浏览器打开 trae.cn，
// 让站点用长效登录态自动换新 JWT，再从 localStorage 读回。
//
// 全程无窗口（--headless=new），用户无感；读到即关进程。
func traeBrowserRefresh(dir string) (string, error) {
	// 兜底转绝对路径：Chrome 按自身安装目录解析相对 --user-data-dir（见 traeBrowserBaseDir 注释）。
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	chrome, err := findChrome()
	if err != nil {
		return "", err
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(chrome,
		"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+dir,
		"https://www.trae.cn/",
	)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("启动无头浏览器失败: %w", err)
	}
	defer func() {
		traeCloseViaCDP(port) // 优雅关闭：子进程一并退出，profile 落盘干净
		if cmd.Process != nil {
			traeWaitExit(cmd.Process, 4*time.Second)
		}
	}()
	// 轮询等站点把新 token 写进存储：登录态有效时通常 3–8 秒内就有。
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if jwt, err := traeBrowserExtract(port); err == nil && jwt != "" {
			return jwt, nil
		}
	}
	return "", fmt.Errorf("30 秒内未读到有效凭证（该浏览器会话的登录态可能已失效，需重新登录）")
}
