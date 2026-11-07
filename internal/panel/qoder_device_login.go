package panel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

type qoderDeviceRegion struct{ website, api, clientID, redirect string }

var qoderDeviceRegions = map[string]qoderDeviceRegion{
	"cn":   {"https://qoder.com.cn", "https://openapi.qoder.com.cn", "1c5e33e1-364d-4ce6-b02c-acaa81274a5c", "qoder-work-cn://"},
	"intl": {"https://qoder.com", "https://openapi.qoder.sh", "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb", ""},
}

const qoderDeviceTTL = 10 * time.Minute

type qoderDeviceSession struct {
	mu                       sync.Mutex
	created                  time.Time
	region                   qoderDeviceRegion
	realm                    string
	nonce, verifier, machine string
	// Keep an issued grant in memory until a failed identity lookup/save can be retried.
	grant map[string]any
	done  bool
	email string
}

var qoderDeviceSessions = struct {
	sync.Mutex
	by map[string]*qoderDeviceSession
}{by: make(map[string]*qoderDeviceSession)}

func (p *Panel) loginCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"desktop_login": runtime.GOOS == "windows"})
}

func (p *Panel) qoderDeviceStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		Region string `json:"region"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	region, ok := qoderDeviceRegions[req.Region]
	if !ok {
		writeErr(w, http.StatusBadRequest, "请选择 Qoder 国内版或国际版")
		return
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		writeErr(w, 500, "无法生成授权会话")
		return
	}
	verifier := base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256([]byte(verifier))
	nonce := uuid.NewString()
	if req.Region == "intl" {
		nonce = strings.ReplaceAll(nonce, "-", "")
	}
	machine := uuid.NewString()
	state := uuid.NewString()
	q := url.Values{"challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "challenge_method": {"S256"},
		"nonce": {nonce}, "client_id": {region.clientID}, "machine_id": {machine}}
	if region.redirect != "" {
		q.Set("redirect_uri", region.redirect)
	}
	qoderDeviceSessions.Lock()
	for k, s := range qoderDeviceSessions.by {
		if time.Since(s.created) > qoderDeviceTTL {
			delete(qoderDeviceSessions.by, k)
		}
	}
	if len(qoderDeviceSessions.by) >= 32 {
		qoderDeviceSessions.Unlock()
		writeErr(w, http.StatusTooManyRequests, "登录会话过多，请稍后重试")
		return
	}
	qoderDeviceSessions.by[state] = &qoderDeviceSession{created: time.Now(), region: region, realm: req.Region, nonce: nonce, verifier: verifier, machine: machine}
	qoderDeviceSessions.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "state": state, "auth_url": region.website + "/device/selectAccounts?" + q.Encode(), "expires_in": 600})
}

// Never return upstream bodies, URLs or transport errors: they can contain credentials.
func qoderDeviceGet(ctx context.Context, client *http.Client, address, token string) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("请求创建失败")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "QoderWork")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("连接 Qoder 失败，请检查容器网络后重试")
	}
	defer resp.Body.Close()
	var data map[string]any
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("Qoder 响应格式异常")
	}
	if nested, ok := data["data"].(map[string]any); ok {
		data = nested
	}
	return resp.StatusCode, data, nil
}

func qdString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

var qdIntegerID = regexp.MustCompile(`^[0-9]+$`)

func qdUserID(m map[string]any, keys ...string) string {
	if len(keys) == 0 {
		keys = []string{"id", "user_id", "userId", "uid"}
	}
	for _, key := range keys {
		if value := qdString(m, key); value != "" {
			return value
		}
		if value, ok := m[key].(json.Number); ok && qdIntegerID.MatchString(value.String()) {
			return value.String()
		}
	}
	return ""
}

func qdExpiry(m map[string]any, now time.Time) string {
	if s := qdString(m, "expires_at", "expiresAt"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	ms, _ := m["expires_in"].(float64)
	if number, ok := m["expires_in"].(json.Number); ok {
		ms, _ = number.Float64()
	}
	if ms > 0 && ms < float64(365*24*time.Hour/time.Millisecond) {
		return now.Add(time.Duration(ms) * time.Millisecond).Format(time.RFC3339)
	}
	return now.Add(30 * 24 * time.Hour).Format(time.RFC3339)
}

func (p *Panel) qoderDevicePoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	qoderDeviceSessions.Lock()
	s := qoderDeviceSessions.by[r.URL.Query().Get("state")]
	qoderDeviceSessions.Unlock()
	if s == nil {
		writeErr(w, 400, "登录会话不存在，请重新获取登录链接")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.created) > qoderDeviceTTL {
		s.grant = nil
		writeJSON(w, 200, map[string]any{"done": true, "ok": false, "message": "登录链接已过期，请重新获取"})
		return
	}
	if s.done {
		writeJSON(w, 200, map[string]any{"done": true, "ok": true, "email": s.email, "message": "账号已添加"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if s.grant == nil {
		q := url.Values{"nonce": {s.nonce}, "verifier": {s.verifier}, "challenge_method": {"S256"}}
		status, data, err := qoderDeviceGet(ctx, client, s.region.api+"/api/v1/deviceToken/poll?"+q.Encode(), "")
		if status == 202 || status == 404 {
			writeJSON(w, 200, map[string]any{"done": false, "message": "等待在 Qoder 网页完成授权…"})
			return
		}
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		if status != 200 {
			writeErr(w, 502, fmt.Sprintf("Qoder 授权接口返回 HTTP %d，请重新获取登录链接", status))
			return
		}
		if qdString(data, "token", "device_token", "access_token", "accessToken") == "" {
			writeJSON(w, 200, map[string]any{"done": false, "message": "等待在 Qoder 网页完成授权…"})
			return
		}
		s.grant = data
	}
	token := qdString(s.grant, "token", "device_token", "access_token", "accessToken")
	status, user, err := qoderDeviceGet(ctx, client, s.region.api+"/api/v1/userinfo", token)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if status != 200 {
		writeErr(w, 502, fmt.Sprintf("Qoder 账号验证返回 HTTP %d，未保存账号", status))
		return
	}
	email := qdString(user, "email")
	uid, grantUID := qdUserID(user), qdUserID(s.grant, "user_id", "userId")
	if uid != "" && grantUID != "" && uid != grantUID {
		writeErr(w, 502, "Qoder 返回的账号身份不一致，未保存账号；请重新授权")
		return
	}
	if uid == "" {
		uid = grantUID
	}
	if uid == "" {
		writeErr(w, 502, "Qoder 未返回可验证的用户 ID，未保存账号；请重新授权")
		return
	}
	auth := &ideauth.QoderAuth{SchemaVersion: 1, Realm: s.realm, Token: token, RefreshToken: qdString(s.grant, "refresh_token", "refreshToken"),
		ExpiresAt: qdExpiry(s.grant, time.Now()), RefreshTokenExpiresAt: qdString(s.grant, "refresh_token_expires_at"),
		User: ideauth.QoderUser{ID: uid, Email: email, Name: qdString(user, "name", "nickname"), AvatarURL: qdString(user, "avatar_url", "avatarUrl", "avatar")}}
	if err := ideauth.ImportQoderDeviceAuth(auth, s.machine); err != nil {
		writeErr(w, 500, "账号保存失败，请重试；已有账号未改动")
		return
	}
	apiCacheClear()
	s.done, s.email, s.grant = true, auth.AccountKey, nil
	s.verifier, s.nonce = "", ""
	label := auth.User.Name
	if label == "" {
		label = "UID " + uid
	}
	writeJSON(w, 200, map[string]any{"done": true, "ok": true, "email": auth.AccountKey, "user_id": uid, "message": "已添加 Qoder 账号：" + label})
}
