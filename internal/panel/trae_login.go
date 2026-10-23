// trae_login.go 面板内嵌的 **Trae** 账号添加（OAuth 授权码 + 本地回调）。
//
// 与 login.go 的 WorkBuddy 设备码流程是**两套完全独立的体系**，别混：
//
//	WorkBuddy：copilot.tencent.com，设备码，面板轮询 auth/token，落 auths/workbuddy-<uid>.json
//	Trae     ：www.trae.cn，授权码 + 本地回调，落 <Trae 数据目录>/data/checkin_accounts.json
//
// 口径对齐 Trae Work 助手 src-tauri/src/commands/oauth.rs（常量与请求体逐字一致）。
//
// ⚠️ 关键：Trae 官网授权页在「认证中，正在验证身份」阶段会**探测本地
// 127.0.0.1:17388 是否有客户端在监听**，确认在线后才完成授权并回调
// http://127.0.0.1:17388/authorize?refreshToken=..&accessToken=..&userId=..
// 所以面板必须在该端口起一个回调服务器：探测请求答 200、回调请求接住凭证。
// 少了它，官网会一直卡在「认证中」（助手侧踩过这个坑）。
// 探测是从 https 跨源发起的，**响应必须带 CORS 头**，否则浏览器拦截，官网同样认为不在线。
package panel

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Trae OAuth 常量（与 Trae Work 助手逐字一致，改一处必须两边同改）。
const (
	traeOAuthClientID     = "en1oxy7wnw8j9n"
	traeOAuthClientSecret = "-"
	traeOAuthAppID        = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	traeOAuthCallbackPort = 17388
	traeAuthURLBase       = "https://www.trae.cn/authorization"
	traeAPIBase           = "https://api.trae.com.cn/cloudide/api/v3/trae"
)

// traeLoginTTL 一次 Trae 授权登录的最长有效期（与 WorkBuddy 侧 loginTTL 同量级）。
const traeLoginTTL = 15 * time.Minute

// traeLoginHTTP 授权流程专用 client：短超时、无 cookie。
var traeLoginHTTP = &http.Client{Timeout: 30 * time.Second}

// traeOAuthState 面板同时只保留一次 Trae 授权登录（回调端口是固定的，
// 并发两次登录本身就会抢端口，串行更符合直觉）。
var traeOAuthState struct {
	mu        sync.Mutex
	state     string
	createdAt time.Time
	done      bool
	ok        bool         // done 时：是否成功
	message   string
	ln        net.Listener // 非 nil = 回调服务器已在监听
	srv       *http.Server
}

// randomHex 生成 n 个十六进制字符（OAuth state / machine_id 用）。
func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		// 极端情况下退回时间戳：state 只需唯一，不需要密码学强度。
		return fmt.Sprintf("%0*x", n, time.Now().UnixNano())[:n]
	}
	return hex.EncodeToString(b)[:n]
}

// randomDigits 生成 n 位数字串（device_id 用，与助手侧同形）。
func randomDigits(n int) string {
	out := make([]byte, n)
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range out {
		out[i] = '0' + b[i]%10
	}
	return string(out)
}

// traeAuthURL 拼授权页 URL（参数顺序与助手一致；redirect_uri 必须 URL 编码）。
func traeAuthURL(state string) string {
	q := url.Values{}
	q.Set("client_id", traeOAuthClientID)
	q.Set("client_secret", traeOAuthClientSecret)
	q.Set("app_id", traeOAuthAppID)
	q.Set("auth_callback_url", fmt.Sprintf("http://127.0.0.1:%d/authorize", traeOAuthCallbackPort))
	q.Set("state", state)
	q.Set("machine_id", randomHex(32))
	q.Set("device_id", randomDigits(15))
	q.Set("response_type", "code")
	return traeAuthURLBase + "?" + q.Encode()
}

// ensureTraeCallbackServer 确保 127.0.0.1:17388 上的回调服务器已启动（幂等）。
// 处理器闭包捕获 Panel，以便落盘时用到与签到读取**同一份** Trae 数据目录配置。
func ensureTraeCallbackServer(p *Panel) error {
	traeOAuthState.mu.Lock()
	defer traeOAuthState.mu.Unlock()
	if traeOAuthState.ln != nil {
		return nil
	}
	addr := fmt.Sprintf("127.0.0.1:%d", traeOAuthCallbackPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("本地回调端口 %d 绑定失败：%v（可能被 Trae 客户端或其它程序占用）", traeOAuthCallbackPort, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.RawQuery
		if !traeQueryHasToken(query) {
			// 官网的在线探测：200 表示「客户端在线」。打日志便于定位「页面到底有没有探测」。
			log.Printf("panel: Trae 回调端口收到 /authorize 探测 %s（UA=%s）", r.Method, shortUA(r.Header.Get("User-Agent")))
			traeWithCORS(w, "ok")
			return
		}
		log.Printf("panel: Trae 回调端口收到授权回调，开始处理")
		msg, err := p.traeFinishLogin(r.URL.Query())
		traeOAuthState.mu.Lock()
		traeOAuthState.done = true
		traeOAuthState.ok = err == nil
		if err != nil {
			traeOAuthState.message = err.Error()
		} else {
			traeOAuthState.message = msg
		}
		traeOAuthState.mu.Unlock()
		if err != nil {
			log.Printf("panel: Trae 授权回调处理失败: %v", err)
			traeWithCORS(w, "登录失败："+err.Error()+"\n请回到面板重试。")
			return
		}
		log.Printf("panel: Trae 账号已添加 %s", msg)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, traeSuccessHTML)
	})
	// 官网可能探测其它路径，一律答 200 保持「在线」。
	mux.HandleFunc("/", traeProbeHandler)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	traeOAuthState.ln = ln
	traeOAuthState.srv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("panel: Trae 回调服务器退出: %v", err)
		}
	}()
	log.Printf("panel: Trae 回调服务器已监听 %s", addr)
	return nil
}

// traeWithCORS 给响应加 CORS 头。官网从 https 跨源 fetch 探测本地端口，
// 没有这组头浏览器会拦掉响应，官网就会认为「客户端不在线」而卡在认证中。
func traeWithCORS(w http.ResponseWriter, body string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, body)
}

// traeQueryHasToken 区分「授权回调」与「在线探测」。
func traeQueryHasToken(query string) bool {
	for _, k := range []string{"refreshToken", "refresh_token", "accessToken", "access_token", "code="} {
		if strings.Contains(query, k) {
			return true
		}
	}
	return false
}

// traeProbeHandler 在线探测应答（无凭证参数）。
//
// 打日志是**刻意**的：Trae 授权页卡住时，唯一能区分「页面根本没探测」还是
// 「探测了但应答不合它意」的证据就是这条。节流到每 2 秒一条，避免页面轮询刷屏。
func traeProbeHandler(w http.ResponseWriter, r *http.Request) {
	traeProbeLog.mu.Lock()
	if time.Since(traeProbeLog.at) > 2*time.Second {
		traeProbeLog.at = time.Now()
		traeProbeLog.mu.Unlock()
		log.Printf("panel: Trae 回调端口收到探测 %s %s（UA=%s）", r.Method, r.URL.RequestURI(),
			shortUA(r.Header.Get("User-Agent")))
	} else {
		traeProbeLog.mu.Unlock()
	}
	traeWithCORS(w, "ok")
}

var traeProbeLog struct {
	mu sync.Mutex
	at time.Time
}

func shortUA(ua string) string {
	if len(ua) > 60 {
		return ua[:60] + "…"
	}
	return ua
}

// traeLoginURL 站点自己的登录跳转 URL：未登录时 /authorization 会被前端跳到
// /login?redirect_url=<authorization>。**直接给用户这个 URL**，让他一步落到登录页，
// 登录完成后再自动跳回授权页——少一次「先看到加载中再被跳走」的困惑。
func traeLoginURL(state string) string {
	return "https://www.trae.cn/login?redirect_url=" + url.QueryEscape(traeAuthURL(state))
}

const traeSuccessHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>登录成功</title>
<style>body{font-family:system-ui,sans-serif;background:#f8fafc;color:#0f172a;
display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{text-align:center;padding:40px 48px;background:#fff;border-radius:12px;
box-shadow:0 4px 16px rgba(0,0,0,.08)}.icon{font-size:40px}
h1{font-size:18px;margin:12px 0 8px}p{font-size:14px;color:#64748b;margin:0}</style></head>
<body><div class="card"><div class="icon">&#9989;</div>
<h1>Trae 账号添加成功</h1><p>凭证已捕获，请回到面板查看。</p></div></body></html>`

// traeFinishLogin 解析回调 → 换 token（缺 accessToken 时）→ 取用户信息 → 落盘账号。
// 返回一句可读的结果描述（写进面板与日志）。
func (p *Panel) traeFinishLogin(q url.Values) (string, error) {
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(q.Get(k)); v != "" {
				return v
			}
		}
		return ""
	}
	refreshToken := pick("refreshToken", "refresh_token")
	accessToken := pick("accessToken", "access_token")
	userID := pick("userId", "user_id", "UserID")
	userName := pick("userName", "user_name", "nickname")

	if refreshToken == "" && accessToken == "" {
		return "", fmt.Errorf("回调缺少 refreshToken / accessToken")
	}
	// 回调没直接给 accessToken 时，用 refresh_token 换一次（助手同口径）。
	if accessToken == "" {
		at, newRT, err := traeExchangeToken(refreshToken)
		if err != nil {
			return "", err
		}
		accessToken = at
		if newRT != "" {
			refreshToken = newRT // 换回的新 refresh_token 更“新鲜”，优先落盘
		}
	}
	// 规范化：面板与助手都按 "Cloud-IDE-JWT <token>" 存。
	jwt := accessToken
	if !strings.HasPrefix(jwt, "Cloud-IDE-JWT ") {
		jwt = "Cloud-IDE-JWT " + jwt
	}
	if userID == "" {
		userID = upstream.TraeUserIDFromJWT(jwt)
	}
	if userID == "" {
		return "", fmt.Errorf("无法从回调或 JWT 中取得 user_id")
	}
	name := userName
	if name == "" {
		if uid, uname, err := traeGetUserInfo(jwt); err == nil {
			if uname != "" {
				name = uname
			} else if uid != "" {
				name = uid
			}
		}
	}
	if name == "" {
		s := userID
		if len(s) > 8 {
			s = s[:8]
		}
		name = "账号_" + s
	}
	added, err := upstream.SaveTraeAccount(p.cfg.TraeDataDir, upstream.TraeAccount{
		Name: name, UserID: userID, JWT: jwt, RefreshToken: refreshToken,
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

// traeExchangeToken POST oauth/ExchangeToken：用 refresh_token 换 access_token。
func traeExchangeToken(refreshToken string) (accessToken, newRefreshToken string, err error) {
	body, _ := json.Marshal(map[string]string{
		"ClientID":     traeOAuthClientID,
		"RefreshToken": refreshToken,
		"ClientSecret": traeOAuthClientSecret,
		"UserID":       "",
	})
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AccessToken  string `json:"access_token"`
			Token        string `json:"token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := traePostJSON(traeAPIBase+"/oauth/ExchangeToken", "", body, &out); err != nil {
		return "", "", fmt.Errorf("ExchangeToken 请求失败: %w", err)
	}
	if out.Code != 0 {
		return "", "", fmt.Errorf("ExchangeToken 失败 (code=%d): %s", out.Code, out.Message)
	}
	at := out.Data.AccessToken
	if at == "" {
		at = out.Data.Token
	}
	if at == "" {
		return "", "", fmt.Errorf("ExchangeToken 响应缺少 access_token")
	}
	return at, out.Data.RefreshToken, nil
}

// traeGetUserInfo POST GetUserInfo：取 user_id / name（失败不致命，调用方回落）。
func traeGetUserInfo(jwt string) (userID, name string, err error) {
	var out struct {
		Data struct {
			UserID   string `json:"user_id"`
			UserIDUp string `json:"UserID"`
			UserIDC  string `json:"userId"`
			Name     string `json:"name"`
			UserName string `json:"user_name"`
			Nickname string `json:"nickname"`
		} `json:"data"`
	}
	if err := traePostJSON(traeAPIBase+"/GetUserInfo", jwt, []byte("{}"), &out); err != nil {
		return "", "", err
	}
	d := out.Data
	userID = firstNonEmpty(d.UserID, d.UserIDUp, d.UserIDC)
	name = firstNonEmpty(d.Name, d.UserName, d.Nickname)
	return userID, name, nil
}

// traePostJSON 发一次 JSON 请求并解析。bearer 非空时按 Cloud-IDE-JWT 形态带鉴权头。
func traePostJSON(fullURL, bearer string, body []byte, out any) error {
	req, err := http.NewRequest(http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	resp, err := traeLoginHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("upstream %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// traeLoginStart 发起 Trae 授权登录：起回调服务器 + 返回授权 URL。
func (p *Panel) traeLoginStart(w http.ResponseWriter, r *http.Request) {
	if err := ensureTraeCallbackServer(p); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	state := randomHex(32)
	traeOAuthState.mu.Lock()
	traeOAuthState.state = state
	traeOAuthState.createdAt = time.Now()
	traeOAuthState.done = false
	traeOAuthState.ok = false
	traeOAuthState.message = ""
	traeOAuthState.mu.Unlock()
	log.Printf("panel: 发起 Trae 授权登录（state=%s...，回调端口 %d）", state[:8], traeOAuthCallbackPort)
	// 直接给「登录跳转 URL」：未登录时会落到 trae.cn 登录页，登录完自动跳回授权页。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "url": traeLoginURL(state), "state": state,
		"auth_url": traeAuthURL(state),
	})
}

// traeLoginPoll 轮询 Trae 授权结果（前端每 2s 一次）。
func (p *Panel) traeLoginPoll(w http.ResponseWriter, r *http.Request) {
	traeOAuthState.mu.Lock()
	defer traeOAuthState.mu.Unlock()
	if traeOAuthState.state == "" {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "尚未发起登录"})
		return
	}
	if !traeOAuthState.done && time.Since(traeOAuthState.createdAt) > traeLoginTTL {
		traeOAuthState.state = ""
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "expired": true, "message": "登录超时，请重新发起"})
		return
	}
	if !traeOAuthState.done {
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true, "ok": traeOAuthState.ok, "message": traeOAuthState.message,
	})
}
