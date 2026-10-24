// qoder_web.go Qoder「网页登录并签到」——每个号一个独立浏览器 profile。
//
// 为什么走网页而不是桌面客户端（2026-10-24 修正）：
// 官方活动页写明「Claim 100 Credits Daily，**只能在 Qoder 桌面客户端领取**」，
// 但**网页控制台（qoder.com/account/usage）同样能领**——它的成长页
// （openapi.qoder.sh/growth-page/activity-iframe）就是同一个领取入口。
// 之前面板写成「Qoder 的可用状态只存在于桌面客户端」，这是**错的**，已更正。
//
// 关键收益：**每个号一份独立浏览器 profile = 一份独立的网页会话**。
// 服务端把每日额度按"人"发（实测 failureCode=SAME_PERSON_ALREADY_CLAIMED），
// 而网页端的会话身份与浏览器 profile 绑定 —— 于是每个号都能领自己那份，
// 不再互相抢同一个名额。
//
// 流程：
//  1. 每号一个 profile 目录 data/qoder-web/<账号哈希>/，启动浏览器打开
//     qoder.com/account/usage，你在这个窗口里登录一次（会话留在 profile 里，
//     以后不用再登）；
//  2. 面板用**浏览器级** CDP 抓全部 Cookie，用同一个 CookieJar 复现网页端
//     自己的请求：GET /sash/api/v1/me/campaigns → 有可领的 CLAIM_BENEFIT 就领；
//  3. 会话 Cookie 一并存进号池账号，之后每天后台直接用（过期了就用同一 profile
//     再起一次浏览器抓新的）。
package panel

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// qoderWebOrigin Qoder 网页控制台。
const qoderWebOrigin = "https://qoder.com"

// qoderWebAPI Qoder 开放接口（成长页就是它）。
const qoderWebAPI = "https://openapi.qoder.sh"

const qoderWebTTL = 30 * time.Minute

// qoderWebSession 一次「网页登录 + 领取」的进行态（按账号分别记）。
var qoderWebSessions = struct {
	mu  sync.Mutex
	by  map[string]*qoderWebState
}{by: map[string]*qoderWebState{}}

type qoderWebState struct {
	email     string
	port      int
	dir       string
	proc      *exec.Cmd
	startedAt time.Time
	done      bool
	ok        bool
	message   string
}

// qoderWebProfileDir 每号一个浏览器 profile（会话持久化 → 登录一次就够）。
func qoderWebProfileDir(p *Panel, email string) string {
	base := filepath.Join(traeBrowserBaseDir(p), "qoder-web")
	dir := filepath.Join(base, shortHash(email))
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// shortHash 取邮箱的短哈希（做目录名，避免邮箱里的 @ 和 . ）。
func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	h := hex.EncodeToString(sum[:])
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// qoderWebStart 拉起浏览器（该账号的独立 profile）并打开 Qoder 网页控制台。
func (p *Panel) qoderWebStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	email := strings.TrimSpace(req.Email)
	if email == "" {
		writeErr(w, http.StatusBadRequest, "email_required")
		return
	}

	qoderWebSessions.mu.Lock()
	st := qoderWebSessions.by[email]
	if st != nil && st.proc != nil && !st.done {
		qoderWebSessions.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": true, "message": "浏览器已在运行，请在窗口里登录这个 Qoder 账号"})
		return
	}
	chrome, err := findChrome()
	if err != nil {
		qoderWebSessions.mu.Unlock()
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		qoderWebSessions.mu.Unlock()
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	dir := qoderWebProfileDir(p, email)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		qoderWebSessions.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, "创建浏览器 profile 目录失败: "+err.Error())
		return
	}
	cmd := exec.Command(chrome,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+dir,
		"--no-first-run", "--no-default-browser-check",
		qoderWebOrigin+"/account/usage",
	)
	if err := cmd.Start(); err != nil {
		qoderWebSessions.mu.Unlock()
		writeErr(w, http.StatusBadGateway, "启动浏览器失败: "+err.Error())
		return
	}
	qoderWebSessions.by[email] = &qoderWebState{
		email: email, port: port, dir: dir, proc: cmd, startedAt: time.Now(),
	}
	qoderWebSessions.mu.Unlock()
	log.Printf("panel: Qoder 网页登录：%s（调试端口 %d，profile %s）", email, port, dir)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "running": true,
		"message": "已打开浏览器（这个账号的独立会话），请登录要添加的 Qoder 账号 —— 登录一次即可，以后每天由面板自动签到",
	})
}

// qoderWebPoll 轮询网页登录态；登录好了就把今天的每日额度领掉并存进号池。
func (p *Panel) qoderWebPoll(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	qoderWebSessions.mu.Lock()
	st := qoderWebSessions.by[email]
	qoderWebSessions.mu.Unlock()
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "还没开始，请点「网页登录并添加」"})
		return
	}
	qoderWebSessions.mu.Lock()
	if st.done {
		res := map[string]any{"done": true, "ok": st.ok, "message": st.message}
		qoderWebSessions.mu.Unlock()
		writeJSON(w, http.StatusOK, res)
		return
	}
	if time.Since(st.startedAt) > qoderWebTTL {
		st.done = true
		st.ok = false
		st.message = "等待登录超时，请重新开始"
		qoderWebSessions.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "expired": true, "message": st.message})
		return
	}
	port := st.port
	dir := st.dir
	qoderWebSessions.mu.Unlock()

	all, err := cdpAllCookies(port)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待浏览器就绪…"})
		return
	}
	jar, cookies := qoderCookieJar(all)
	if len(cookies) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待在浏览器中完成 Qoder 登录…"})
		return
	}

	// 用网页会话复现网页端自己的请求：先查活动列表。
	status, body, err := qoderWebGet(jar, "/sash/api/v1/me/campaigns")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待浏览器就绪…"})
		return
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		qoderWebSessions.mu.Lock()
		st.done = true
		st.ok = false
		st.message = "这个浏览器会话拿不到 openapi 的授权（HTTP " + fmt.Sprint(status) + "）。请在窗口里确认登录的是 Qoder 账号后重试。"
		qoderWebSessions.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": true, "ok": false, "message": st.message})
		return
	}
	if status != http.StatusOK {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": fmt.Sprintf("查询活动失败 HTTP %d", status)})
		return
	}

	var camps struct {
		Campaigns []struct {
			CampaignID  string `json:"campaignId"`
			ActionType  string `json:"actionType"`
			ClaimStatus string `json:"claimStatus"`
			Benefit     struct {
				Amount float64 `json:"amount"`
			} `json:"benefit"`
		} `json:"campaigns"`
	}
	if err := json.Unmarshal(body, &camps); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "活动列表解析失败"})
		return
	}

	claimID := ""
	amount := 0.0
	already := false
	for _, c := range camps.Campaigns {
		if c.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		if c.ClaimStatus == "CLAIMABLE" {
			claimID = c.CampaignID
			amount = c.Benefit.Amount
			break
		}
		if c.ClaimStatus == "CLAIMED" {
			already = true
			amount = c.Benefit.Amount
		}
	}

	msg := ""
	ok := false
	switch {
	case claimID != "":
		st2, b2, cerr := qoderWebPost(jar, "/sash/api/v1/me/campaigns/"+claimID+"/claim")
		if cerr == nil && st2 == http.StatusOK {
			var r struct {
				Status      string `json:"status"`
				FailureCode string `json:"failureCode"`
			}
			_ = json.Unmarshal(b2, &r)
			if r.Status == "CLAIMED" || r.Status == "SUCCESS" {
				ok = true
				msg = fmt.Sprintf("签到成功 +%.0f Credits（网页会话领取）", amount)
			} else {
				msg = "领取被服务端拒绝：" + r.FailureCode
			}
		} else {
			msg = fmt.Sprintf("领取失败 HTTP %d", st2)
		}
	case already:
		ok = true
		msg = fmt.Sprintf("今天已领取（+%.0f Credits）", amount)
	default:
		msg = "这个号今天没有可领的每日额度（可能已被同一人另一账号领走）"
	}

	// 会话存进该账号的 profile 目录（cookies.json）：之后每天后台直接用，
	// 过期了再用同一 profile 起一次浏览器抓新的。
	if ok {
		if err := saveQoderWebCookies(dir, all); err != nil {
			log.Printf("panel: Qoder 网页会话落盘失败 %s: %v", email, err)
		}
	}
	qoderWebSessions.mu.Lock()
	st.done = true
	st.ok = ok
	st.message = msg
	qoderWebSessions.mu.Unlock()
	log.Printf("panel: Qoder 网页登录 %s -> %s", email, msg)
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "ok": ok, "message": msg, "email": email})
}

// saveQoderWebCookies 把网页会话 Cookie 落到该账号的 profile 目录（0600）。
func saveQoderWebCookies(dir string, all []cdpCookie) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var keep []cdpCookie
	for _, c := range all {
		host := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if !strings.Contains(host, "qoder") || c.Name == "" || c.Value == "" {
			continue
		}
		keep = append(keep, c)
	}
	buf, err := json.MarshalIndent(keep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "cookies.json"), buf, 0o600)
}

// loadQoderWebCookies 读回该账号的网页会话 Cookie。
func loadQoderWebCookies(dir string) []cdpCookie {
	raw, err := os.ReadFile(filepath.Join(dir, "cookies.json"))
	if err != nil {
		return nil
	}
	var out []cdpCookie
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// qoderCookieJar 把抓到的 Cookie 装进 jar（按域规则），并返回登录态域名列表。
func qoderCookieJar(all []cdpCookie) (*cookiejar.Jar, []string) {	jar, _ := cookiejar.New(nil)
	var doms []string
	seen := map[string]bool{}
	for _, c := range all {
		if c.Name == "" || c.Value == "" {
			continue
		}
		host := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if !strings.Contains(host, "qoder") {
			continue
		}
		if !seen[host] {
			seen[host] = true
			doms = append(doms, host)
		}
		u := &url.URL{Scheme: "https", Host: host, Path: c.Path}
		if u.Path == "" {
			u.Path = "/"
		}
		jar.SetCookies(u, []*http.Cookie{{
			Name: c.Name, Value: c.Value, Path: u.Path, Domain: c.Domain,
			HttpOnly: c.HTTPOnly, Secure: c.Secure,
		}})
	}
	return jar, doms
}

// qoderWebGet / qoderWebPost 用网页会话调 openapi（CookieJar 自动按域挑 cookie）。
func qoderWebGet(jar *cookiejar.Jar, path string) (int, []byte, error) {
	return qoderWebDo(jar, http.MethodGet, path, nil)
}

func qoderWebPost(jar *cookiejar.Jar, path string) (int, []byte, error) {
	return qoderWebDo(jar, http.MethodPost, path, []byte("{}"))
}

func qoderWebDo(jar *cookiejar.Jar, method, path string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var rdr io.Reader
	if len(body) > 0 {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, qoderWebAPI+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}
