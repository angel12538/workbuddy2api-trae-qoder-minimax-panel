package panel

// The browser performs TRAE's normal interactive authorization. Only the returned
// loopback URL is pasted here; no browser or desktop client runs on this server.
import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type traeManualSession struct {
	created  time.Time
	callback string
	busy     bool
	// Keep a successfully exchanged, rotated token in memory until validation and
	// saving finish. A transient status/file error can then be retried safely.
	pending *upstream.TraeAccount
	receipt [32]byte
}

type traeManualServices struct {
	exchange func(string) (string, string, error)
	status   func(upstream.TraeAccount) (bool, float64, string, error)
}

func (p *Panel) traeManualStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		writeErr(w, 500, "无法创建登录会话，请重试")
		return
	}
	state := hex.EncodeToString(entropy[:])
	port := 49152 + int(binary.BigEndian.Uint16(entropy[:2]))%16384
	callback := fmt.Sprintf("http://127.0.0.1:%d/authorize", port)
	p.traeManualMu.Lock()
	if p.traeManualLogins == nil {
		p.traeManualLogins = make(map[string]*traeManualSession)
	}
	for key, session := range p.traeManualLogins {
		if !session.busy && time.Since(session.created) > traeLoginTTL {
			delete(p.traeManualLogins, key)
		}
	}
	if len(p.traeManualLogins) >= 8 {
		p.traeManualMu.Unlock()
		writeErr(w, 429, "进行中的登录过多，请等待已有登录过期后重试")
		return
	}
	p.traeManualLogins[state] = &traeManualSession{created: time.Now(), callback: callback}
	p.traeManualMu.Unlock()
	q := url.Values{
		"client_id": {traeOAuthClientID}, "auth_from": {"trae"},
		"login_channel": {"native_ide"}, "auth_type": {"local"},
		"login_version": {"1"}, "redirect": {"0"},
		"auth_callback_url": {callback}, "login_trace_id": {state},
		"machine_id": {state},
	}
	authURL := traeAuthURLBase + "?" + q.Encode()
	writeJSON(w, 200, map[string]any{"ok": true, "state": state,
		"url": authURL, "expires_in": int(traeLoginTTL.Seconds())})
}

// Never request the submitted URL: it is parsed locally, and all credential
// exchange/validation goes to the fixed TRAE endpoints already used by the app.
func parseTraeManualReturn(raw, state, callback string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Fragment != "" || u.Scheme != "http" ||
		u.Scheme+"://"+u.Host+u.Path != callback || u.RawPath != "" {
		return "", fmt.Errorf("请粘贴本次授权后以 http://127.0.0.1: 开头的完整返回地址")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("返回地址格式不完整，请重新复制")
	}
	for _, key := range []string{"loginTraceID", "refreshToken", "data", "scope", "error_code"} {
		if len(q[key]) > 1 {
			return "", fmt.Errorf("返回地址含重复参数，请重新复制")
		}
	}
	if q.Get("loginTraceID") != state {
		return "", fmt.Errorf("此返回地址不属于本次登录，请使用当前页面的登录链接")
	}
	if q.Get("scope") != "trae" {
		return "", fmt.Errorf("返回地址不是 TRAE 账号授权结果")
	}
	if code := q.Get("error_code"); code != "" && code != "0" {
		return "", fmt.Errorf("官方授权未成功，请重新登录")
	}
	refresh := strings.TrimSpace(q.Get("refreshToken"))
	if refresh == "" {
		return "", fmt.Errorf("返回地址缺少续期凭证，请复制登录授权后的地址，而不是登录页链接")
	}
	if data := q.Get("data"); data != "" && data != refresh {
		return "", fmt.Errorf("返回地址中的凭证参数不一致，请重新复制")
	}
	if len(refresh) > 8192 || strings.ContainsAny(refresh, "\r\n\x00") {
		return "", fmt.Errorf("返回地址中的凭证格式异常")
	}
	return refresh, nil
}

func (p *Panel) traeManualImport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		State       string `json:"state"`
		CallbackURL string `json:"callback_url"`
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误或返回地址过长")
		return
	}
	message, status, err := p.finishTraeManual(body.State, body.CallbackURL, traeManualServices{
		exchange: upstream.TraeExchangeToken, status: upstream.TraeStatus,
	})
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": message})
}

func (p *Panel) finishTraeManual(state, raw string, services traeManualServices) (string, int, error) {
	p.traeManualMu.Lock()
	session := p.traeManualLogins[state]
	if session == nil || time.Since(session.created) > traeLoginTTL {
		if session != nil && !session.busy {
			delete(p.traeManualLogins, state)
		}
		p.traeManualMu.Unlock()
		return "", 400, fmt.Errorf("登录已过期或已完成，请重新获取登录链接")
	}
	if session.busy {
		p.traeManualMu.Unlock()
		return "", 409, fmt.Errorf("正在验证此账号，请稍候")
	}
	refresh, err := parseTraeManualReturn(raw, state, session.callback)
	if err != nil {
		p.traeManualMu.Unlock()
		return "", 400, err
	}
	receipt := sha256.Sum256([]byte(refresh))
	if session.pending != nil && session.receipt != receipt {
		p.traeManualMu.Unlock()
		return "", 400, fmt.Errorf("此会话已收到另一份授权结果，请重新获取登录链接")
	}
	session.busy = true
	p.traeManualMu.Unlock()
	defer func() { p.traeManualMu.Lock(); session.busy = false; p.traeManualMu.Unlock() }()

	// Share the refresh lock so import cannot race a scheduled token rotation.
	traeRefreshMu.Lock()
	defer traeRefreshMu.Unlock()
	if session.pending == nil {
		jwt, rotated, err := services.exchange(refresh)
		if err != nil {
			var detail *upstream.TraeExchangeError
			if errors.As(err, &detail) {
				return "", 502, fmt.Errorf("TRAE 凭证换取失败：%s；未写入账号文件", detail.Error())
			}
			return "", 502, fmt.Errorf("TRAE 凭证换取失败，请稍后重试；未写入账号文件")
		}
		if !strings.HasPrefix(jwt, "Cloud-IDE-JWT ") {
			jwt = "Cloud-IDE-JWT " + jwt
		}
		uid := upstream.TraeUserIDFromJWT(jwt)
		if uid == "" {
			return "", 502, fmt.Errorf("TRAE 未返回有效的账号标识；未写入账号文件")
		}
		if rotated != "" {
			refresh = rotated
		}
		nameID := uid
		if len(nameID) > 8 {
			nameID = nameID[:8]
		}
		session.pending = &upstream.TraeAccount{Name: "账号_" + nameID, UserID: uid, JWT: jwt, RefreshToken: refresh}
		session.receipt = receipt
	}
	account := *session.pending
	if _, _, _, err := services.status(account); err != nil {
		return "", 502, fmt.Errorf("TRAE 接口校验未通过，未写入账号文件；可稍后用同一返回地址重试")
	}
	accounts, err := upstream.LoadTraeAccounts(p.cfg.TraeDataDir)
	if err != nil {
		return "", 500, fmt.Errorf("无法读取现有账号文件；未覆盖文件，请检查配置与权限")
	}
	for _, existing := range accounts {
		if existing.UserID == account.UserID && existing.Name != "" {
			account.Name = existing.Name
		}
	}
	added, err := upstream.SaveTraeAccount(p.cfg.TraeDataDir, account)
	if err != nil {
		return "", 500, fmt.Errorf("账号保存失败，请检查数据目录权限后用同一返回地址重试")
	}
	p.traeManualMu.Lock()
	delete(p.traeManualLogins, state)
	session.pending = nil
	p.traeManualMu.Unlock()
	verb := "更新"
	if added {
		verb = "新增"
	}
	return verb + "账号 " + account.Name + "，已验证并保存，支持容器自动续期", 200, nil
}
