package ideauth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// WorkBuddy 认证协议（从 app.asar 逆向还原，见 WorkBuddy 认证协议）
//
//  1. POST {endpoint}/v2/plugin/auth/state?platform=workbuddy
//     → {"code":0,"data":{"state":"<uuid>","authUrl":"https://<host>/login?platform=workbuddy&state=<uuid>"}}
//  2. 系统浏览器打开 authUrl（追加 version / loginSessionId）
//  3. GET  {endpoint}/v2/plugin/auth/token?state={state}
//     → pending 码 11217（login ing...）→ 成功后返回 {accessToken,refreshToken,expiresAt,domain}
//  4. GET  {endpoint}/v2/plugin/login/account?state={state}   Authorization: Bearer <accessToken>
//  5. GET  {endpoint}/v2/plugin/account                      Authorization: Bearer <accessToken>
//  6. POST {endpoint}/v2/plugin/login/enterprise[/{entId}]   X-Refresh-Token / X-Enterprise-Id / X-Tenant-Id
//  7. POST {endpoint}/v2/plugin/auth/token/refresh           X-Refresh-Token / X-Auth-Refresh-Source: plugin
// ---------------------------------------------------------------------------

const (
	// WorkBuddyLoginPollPendingToken 是 token 轮询的“仍在登录”返回码。
	WorkBuddyLoginPollPendingToken = 11217
	// WorkBuddyLoginPollPendingAccount 是 account 轮询的“仍在登录”返回码。
	WorkBuddyLoginPollPendingAccount = 12151

	workBuddyPrefixPath = "/plugin"

	// workBuddyPollInterval 轮询间隔。
	workBuddyPollInterval = 2 * time.Second
	// workBuddyLoginTimeout 登录整体超时。
	workBuddyLoginTimeout = 5 * time.Minute
)

// WorkBuddyEdition 表示 WorkBuddy 的版本（决定 endpoint 与认证类型）。
type WorkBuddyEdition string

const (
	// WorkBuddyCN 国内版：www.workbuddy.cn，认证类型 account-scoped-external-link。
	WorkBuddyCN WorkBuddyEdition = "cn"
	// WorkBuddyIntl 国际版（copilot）：copilot.tencent.com，cli-external-link。
	WorkBuddyIntl WorkBuddyEdition = "intl"
	// WorkBuddyIntlBuddy 国际版（tencentbuddy）：wb.tencentbuddy.com。
	WorkBuddyIntlBuddy WorkBuddyEdition = "intlbuddy"
)

// WorkBuddyEndpoints 各版本对应的服务端点。
var WorkBuddyEndpoints = map[WorkBuddyEdition]string{
	WorkBuddyCN:        "https://www.workbuddy.cn",
	WorkBuddyIntl:      "https://copilot.tencent.com",
	WorkBuddyIntlBuddy: "https://wb.tencentbuddy.com",
}

// WorkBuddyEndpointFor 返回指定版本的端点（未知版本回退国内版）。
func WorkBuddyEndpointFor(e WorkBuddyEdition) string {
	if v, ok := WorkBuddyEndpoints[e]; ok {
		return v
	}
	return WorkBuddyEndpoints[WorkBuddyCN]
}

// ---------------------------------------------------------------------------
// 数据类型
// ---------------------------------------------------------------------------

// WorkBuddyAuth 是 auth/token 返回的认证对象。
type WorkBuddyAuth struct {
	AccessToken     string `json:"accessToken"`
	RefreshToken    string `json:"refreshToken"`
	TokenType       string `json:"tokenType,omitempty"`
	ExpiresIn       int64  `json:"expiresIn,omitempty"`
	ExpiresAt       int64  `json:"expiresAt,omitempty"`
	RefreshExpiresIn int64 `json:"refreshExpiresIn,omitempty"`
	RefreshExpiresAt int64 `json:"refreshExpiresAt,omitempty"`
	Domain          string `json:"domain,omitempty"`
	LastRefreshTime int64  `json:"lastRefreshTime,omitempty"`
}

// WorkBuddyAccount 是账号对象。
type WorkBuddyAccount struct {
	UID            string `json:"uid"`
	Nickname       string `json:"nickname,omitempty"`
	Type           string `json:"type,omitempty"` // personal / enterprise
	EnterpriseID   string `json:"enterpriseId,omitempty"`
	EnterpriseName string `json:"enterpriseName,omitempty"`
	Avatar         string `json:"avatar,omitempty"`
	LastLogin      bool   `json:"lastLogin,omitempty"`
}

// WorkBuddySnapshot 是持久化的账号快照（号池元素）。
type WorkBuddySnapshot struct {
	Edition      string           `json:"edition"`
	Endpoint     string           `json:"endpoint"`
	Auth         *WorkBuddyAuth   `json:"auth"`
	Account      *WorkBuddyAccount `json:"account"`
	Accounts     []*WorkBuddyAccount `json:"accounts,omitempty"` // 企业下的全部账号
	SavedAt      string           `json:"savedAt"`
	// AuthFilePath / AuthFileRaw 保存 WorkBuddy 客户端真实登录会话文件的路径与原文
	// （%LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\<id>.info，明文 JSON，
	// token 字段用 keyblob envelope 加密但跨账号可解）。切号时整份写回即完成真实换号。
	AuthFilePath string `json:"authFilePath,omitempty"`
	AuthFileRaw  string `json:"authFileRaw,omitempty"`
}

// WorkBuddyAccountView 是面板展示用的账号视图。
type WorkBuddyAccountView struct {
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	Type           string `json:"type"`
	EnterpriseID   string `json:"enterpriseId"`
	EnterpriseName string `json:"enterpriseName"`
	Edition        string `json:"edition"`
	Endpoint       string `json:"endpoint"`
	SavedAt        string `json:"savedAt"`
	Expired        bool   `json:"expired"`
	Current        bool   `json:"current"`
	HasRefresh     bool   `json:"hasRefresh"`
	// SessionReady 表示号池里已保存该账号的「真实登录会话文件」原文。
	// 只有曾在本机客户端登录过的账号才会被记录；未记录则该账号无法一键切号
	// （WorkBuddy 会话用运行时密钥加密，无法离线伪造）。
	SessionReady bool `json:"session_ready"`
	// CheckedInToday 面板观察到该账号今天已完成签到（用于把签到按钮置灰）。
	CheckedInToday bool `json:"checked_in_today"`
	// Credits / HasCredits 上一次查询到的剩余额度（未查询时为 0/false）。
	Credits        float64 `json:"credits,omitempty"`
	HasCredits     bool    `json:"has_credits,omitempty"`
	CreditsTotal   float64 `json:"credits_total,omitempty"`
}

// ---------------------------------------------------------------------------
// HTTP 基础
// ---------------------------------------------------------------------------

func wbHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// wbNoAuthHeaders 构造“匿名”请求头（auth/state 与 auth/token 用）。
func wbNoAuthHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-No-Authorization", "true")
	h.Set("X-No-User-Id", "true")
	h.Set("X-No-Enterprise-Id", "true")
	h.Set("X-No-Department-Info", "true")
	return h
}

// wbAuthHeaders 构造带 Bearer 的请求头。
func wbAuthHeaders(auth *WorkBuddyAuth, account *WorkBuddyAccount) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if auth != nil {
		if auth.AccessToken != "" {
			h.Set("Authorization", "Bearer "+auth.AccessToken)
		}
		if auth.RefreshToken != "" {
			h.Set("X-Refresh-Token", auth.RefreshToken)
		}
		if auth.Domain != "" {
			h.Set("X-Domain", auth.Domain)
		}
	}
	if account != nil {
		h.Set("X-No-User-Id", "true")
		h.Set("X-No-Enterprise-Id", "true")
		h.Set("X-No-Department-Info", "true")
	}
	return h
}

type wbResp struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func wbDo(method, url string, headers http.Header, body []byte) (*wbResp, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := wbHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var r wbResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("响应解析失败(HTTP %d): %s", resp.StatusCode, truncateStr(string(raw), 200))
	}
	return &r, nil
}

// WorkBuddyAppVersion 返回本机安装的 WorkBuddy 版本号（读不到返回空串）。
// 依次尝试 <配置目录>/last-launch.json 与 <配置目录>/app/last-launch.json。
func WorkBuddyAppVersion() string {
	for _, p := range []string{
		filepath.Join(WorkBuddyConfigDir(), "last-launch.json"),
		filepath.Join(WorkBuddyAppDataDir(), "last-launch.json"),
	} {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var v struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		if v.Version != "" {
			return v.Version
		}
	}
	return ""
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------------------------------------------------------------------------
// 认证流程
// ---------------------------------------------------------------------------

// WorkBuddyStartLogin 请求一次登录会话，返回 state 与 authUrl。
func WorkBuddyStartLogin(edition WorkBuddyEdition) (state, authURL string, err error) {
	ep := WorkBuddyEndpointFor(edition)
	url := fmt.Sprintf("%s/v2%s/auth/state?platform=workbuddy", ep, workBuddyPrefixPath)
	r, err := wbDo(http.MethodPost, url, wbNoAuthHeaders(), []byte("{}"))
	if err != nil {
		return "", "", err
	}
	if r.Code != 0 {
		return "", "", fmt.Errorf("auth/state 失败: code=%d msg=%s", r.Code, r.Msg)
	}
	var d struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return "", "", fmt.Errorf("auth/state 数据解析失败: %w", err)
	}
	if d.State == "" {
		return "", "", fmt.Errorf("auth/state 未返回 state")
	}
	// 追加 version / loginSessionId（与客户端行为一致）
	au := d.AuthURL
	sep := "?"
	if strings.Contains(au, "?") {
		sep = "&"
	}
	au += sep + "version=" + WorkBuddyAppVersion()

	return d.State, au, nil
}

// WorkBuddyPollToken 轮询一次 token，未就绪返回 (nil,true,nil)。
func WorkBuddyPollToken(edition WorkBuddyEdition, state string) (*WorkBuddyAuth, bool, error) {
	ep := WorkBuddyEndpointFor(edition)
	url := fmt.Sprintf("%s/v2%s/auth/token?state=%s", ep, workBuddyPrefixPath, state)
	r, err := wbDo(http.MethodGet, url, wbNoAuthHeaders(), nil)
	if err != nil {
		return nil, false, err
	}
	if r.Code == WorkBuddyLoginPollPendingToken {
		return nil, true, nil
	}
	if r.Code != 0 {
		return nil, false, fmt.Errorf("auth/token 失败: code=%d msg=%s", r.Code, r.Msg)
	}
	var a WorkBuddyAuth
	if err := json.Unmarshal(r.Data, &a); err != nil {
		return nil, false, fmt.Errorf("auth/token 数据解析失败: %w", err)
	}
	if a.AccessToken == "" {
		return nil, true, nil
	}
	a.LastRefreshTime = time.Now().UnixMilli()
	fillExpires(&a)
	return &a, false, nil
}

func fillExpires(a *WorkBuddyAuth) {
	now := time.Now().UnixMilli()
	if a.ExpiresAt == 0 && a.ExpiresIn > 0 {
		a.ExpiresAt = now + a.ExpiresIn*1000
	}
	if a.RefreshExpiresAt == 0 && a.RefreshExpiresIn > 0 {
		a.RefreshExpiresAt = now + a.RefreshExpiresIn*1000
	}
}

// WorkBuddyFetchAccount 用 token 拉取当前账号。
func WorkBuddyFetchAccount(edition WorkBuddyEdition, auth *WorkBuddyAuth) (*WorkBuddyAccount, error) {
	ep := WorkBuddyEndpointFor(edition)
	url := fmt.Sprintf("%s/v2%s/account", ep, workBuddyPrefixPath)
	r, err := wbDo(http.MethodGet, url, wbAuthHeaders(auth, nil), nil)
	if err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("account 失败: code=%d msg=%s", r.Code, r.Msg)
	}
	var a WorkBuddyAccount
	if err := json.Unmarshal(r.Data, &a); err != nil {
		return nil, fmt.Errorf("account 数据解析失败: %w", err)
	}
	return &a, nil
}

// WorkBuddyPollAccount 轮询 login/account 直到拿到账号。
func WorkBuddyPollAccount(edition WorkBuddyEdition, state string, auth *WorkBuddyAuth) (*WorkBuddyAccount, error) {
	ep := WorkBuddyEndpointFor(edition)
	deadline := time.Now().Add(workBuddyLoginTimeout)
	for time.Now().Before(deadline) {
		url := fmt.Sprintf("%s/v2%s/login/account?state=%s", ep, workBuddyPrefixPath, state)
		r, err := wbDo(http.MethodGet, url, wbAuthHeaders(auth, &WorkBuddyAccount{}), nil)
		if err == nil {
			if r.Code == WorkBuddyLoginPollPendingAccount {
				time.Sleep(workBuddyPollInterval)
				continue
			}
			if r.Code == 0 && len(r.Data) > 0 {
				var a WorkBuddyAccount
				if err := json.Unmarshal(r.Data, &a); err == nil && a.UID != "" {
					return &a, nil
				}
			}
		}
		time.Sleep(workBuddyPollInterval)
	}
	return nil, fmt.Errorf("等待账号信息超时")
}

// WorkBuddyLogin 执行完整登录：取 state → 打开浏览器 → 轮询 token → 拿账号。
// openBrowser 为 true 时会尝试调用系统默认浏览器打开授权页。
func WorkBuddyLogin(edition WorkBuddyEdition, openBrowser bool) (*WorkBuddySnapshot, error) {
	state, authURL, err := WorkBuddyStartLogin(edition)
	if err != nil {
		return nil, err
	}
	if openBrowser {
		_ = OpenURL(authURL)
	}
	auth, err := WorkBuddyWaitToken(edition, state)
	if err != nil {
		return nil, err
	}
	acc, err := WorkBuddyPollAccount(edition, state, auth)
	if err != nil {
		return nil, err
	}
	return &WorkBuddySnapshot{
		Edition:  string(edition),
		Endpoint: WorkBuddyEndpointFor(edition),
		Auth:     auth,
		Account:  acc,
		SavedAt:  time.Now().Format(time.RFC3339),
	}, nil
}

// WorkBuddyWaitToken 轮询 token 直到就绪或超时。
func WorkBuddyWaitToken(edition WorkBuddyEdition, state string) (*WorkBuddyAuth, error) {
	deadline := time.Now().Add(workBuddyLoginTimeout)
	for time.Now().Before(deadline) {
		a, pending, err := WorkBuddyPollToken(edition, state)
		if err != nil {
			// 网络抖动不致命，继续轮询
			time.Sleep(workBuddyPollInterval)
			continue
		}
		if a != nil {
			return a, nil
		}
		if !pending {
			time.Sleep(workBuddyPollInterval)
			continue
		}
		time.Sleep(workBuddyPollInterval)
	}
	return nil, fmt.Errorf("等待登录 token 超时")
}

// WorkBuddyRefreshToken 刷新 accessToken。
func WorkBuddyRefreshToken(s *WorkBuddySnapshot) (*WorkBuddyAuth, error) {
	if s == nil || s.Auth == nil || s.Auth.RefreshToken == "" {
		return nil, fmt.Errorf("缺少 refreshToken")
	}
	ep := s.Endpoint
	if ep == "" {
		ep = WorkBuddyEndpointFor(WorkBuddyEdition(s.Edition))
	}
	url := fmt.Sprintf("%s/v2%s/auth/token/refresh", ep, workBuddyPrefixPath)
	h := wbAuthHeaders(s.Auth, nil)
	h.Set("X-Auth-Refresh-Source", "plugin")
	r, err := wbDo(http.MethodPost, url, h, []byte("{}"))
	if err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("refresh 失败: code=%d msg=%s", r.Code, r.Msg)
	}
	var a WorkBuddyAuth
	if err := json.Unmarshal(r.Data, &a); err != nil {
		return nil, fmt.Errorf("refresh 数据解析失败: %w", err)
	}
	if a.AccessToken == "" {
		return nil, fmt.Errorf("refresh 未返回 accessToken")
	}
	a.LastRefreshTime = time.Now().UnixMilli()
	fillExpires(&a)
	return &a, nil
}

// WorkBuddySwitchEnterprise 切换到指定企业账号（enterpriseId 为空表示切回个人版）。
func WorkBuddySwitchEnterprise(s *WorkBuddySnapshot, enterpriseID string) (*WorkBuddySnapshot, error) {
	if s == nil || s.Auth == nil {
		return nil, fmt.Errorf("账号快照不完整")
	}
	ep := s.Endpoint
	if ep == "" {
		ep = WorkBuddyEndpointFor(WorkBuddyEdition(s.Edition))
	}
	path := fmt.Sprintf("%s/v2%s/login/enterprise", ep, workBuddyPrefixPath)
	if enterpriseID != "" {
		path += "/" + enterpriseID
	}
	h := wbAuthHeaders(s.Auth, nil)
	if enterpriseID != "" {
		h.Set("X-Enterprise-Id", enterpriseID)
		h.Set("X-Tenant-Id", enterpriseID)
	}
	r, err := wbDo(http.MethodPost, path, h, []byte("{}"))
	if err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("切换账号失败: code=%d msg=%s", r.Code, r.Msg)
	}
	var a WorkBuddyAuth
	if err := json.Unmarshal(r.Data, &a); err != nil {
		return nil, fmt.Errorf("切换账号数据解析失败: %w", err)
	}
	if a.AccessToken == "" {
		return nil, fmt.Errorf("切换账号未返回 accessToken")
	}
	if a.Domain == "" {
		a.Domain = s.Auth.Domain
	}
	a.LastRefreshTime = time.Now().UnixMilli()
	fillExpires(&a)

	ns := *s
	ns.Auth = &a
	if acc, err := WorkBuddyFetchAccount(WorkBuddyEdition(ns.Edition), &a); err == nil {
		ns.Account = acc
	}
	ns.SavedAt = time.Now().Format(time.RFC3339)
	return &ns, nil
}

// ---------------------------------------------------------------------------
// 账号信息（积分 / 签到）
// ---------------------------------------------------------------------------

// WorkBuddyUsage 是积分余额信息（结构按需保留原始 JSON）。
type WorkBuddyUsage struct {
	Raw json.RawMessage `json:"-"`
}

// WorkBuddyGetUsage 拉取账号积分余额。
func WorkBuddyGetUsage(s *WorkBuddySnapshot) (json.RawMessage, error) {
	if s == nil || s.Auth == nil {
		return nil, fmt.Errorf("账号快照不完整")
	}
	ep := s.Endpoint
	if ep == "" {
		ep = WorkBuddyEndpointFor(WorkBuddyEdition(s.Edition))
	}
	url := fmt.Sprintf("%s/v2%s/account/usage", ep, workBuddyPrefixPath)
	r, err := wbDo(http.MethodGet, url, wbAuthHeaders(s.Auth, s.Account), nil)
	if err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("usage 失败: code=%d msg=%s", r.Code, r.Msg)
	}
	return r.Data, nil
}

// WorkBuddyGetCheckinStatus 查询每日签到状态。
func WorkBuddyGetCheckinStatus(s *WorkBuddySnapshot) (json.RawMessage, error) {
	return wbCheckinCall(s, "/checkin/status", nil)
}

// WorkBuddyClaimDailyCheckin 执行每日签到。
func WorkBuddyClaimDailyCheckin(s *WorkBuddySnapshot) (json.RawMessage, error) {
	return wbCheckinCall(s, "/checkin/claim", []byte("{}"))
}

func wbCheckinCall(s *WorkBuddySnapshot, suffix string, body []byte) (json.RawMessage, error) {
	if s == nil || s.Auth == nil {
		return nil, fmt.Errorf("账号快照不完整")
	}
	ep := s.Endpoint
	if ep == "" {
		ep = WorkBuddyEndpointFor(WorkBuddyEdition(s.Edition))
	}
	url := fmt.Sprintf("%s/v2%s%s", ep, workBuddyPrefixPath, suffix)
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	r, err := wbDo(method, url, wbAuthHeaders(s.Auth, s.Account), body)
	if err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("签到接口失败: code=%d msg=%s", r.Code, r.Msg)
	}
	return r.Data, nil
}

// ---------------------------------------------------------------------------
// WorkBuddy 本地数据目录（用于备份/恢复账号态）
// ---------------------------------------------------------------------------

// WorkBuddyConfigDir 返回 WorkBuddy 的配置目录（~/.workbuddy）。
func WorkBuddyConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".workbuddy")
}

// WorkBuddyAppDataDir 返回 WorkBuddy Electron 数据目录（配置目录下的 app）。
func WorkBuddyAppDataDir() string {
	return filepath.Join(WorkBuddyConfigDir(), "app")
}

// WorkBuddyAuthFilePath 返回 WorkBuddy 认证文件路径（新版格式）。
// 路径规则：<sharedDataPath>/auth/<authenticationId>.info
func WorkBuddyAuthFilePath() string {
	return filepath.Join(WorkBuddyConfigDir(), "auth", "workbuddy-desktop.info")
}

// WorkBuddyLegacyStateDB 返回旧版 WorkBuddy（IDE 内核）会话数据库路径。
func WorkBuddyLegacyStateDB() string {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		home, _ := os.UserHomeDir()
		appdata = filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(appdata, "WorkBuddy", "User", "globalStorage", "state.vscdb")
}

// WorkBuddyAccountSnapshotPath 返回账号快照文件（记录当前 uid / 类型）。
func WorkBuddyAccountSnapshotPath() string {
	return filepath.Join(WorkBuddyConfigDir(), "storage", "skeleton", "account-snapshot.json")
}

// WorkBuddyEditionConfigDirs 返回各版本 WorkBuddy 客户端可能的配置目录。
// 国内版：~/.workbuddy；国际版（WorkBuddy AI）：~/.workbuddy-ai。
// 每个版本各自维护一份 account-snapshot.json，因此判断“当前登录”必须逐目录扫描。
func WorkBuddyEditionConfigDirs() map[WorkBuddyEdition]string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return map[WorkBuddyEdition]string{
		WorkBuddyCN:   filepath.Join(home, ".workbuddy"),
		WorkBuddyIntl: filepath.Join(home, ".workbuddy-ai"),
	}
}

// workBuddyUIDFromDir 读取某配置目录的当前登录 uid（无则空）。
func workBuddyUIDFromDir(dir string) string {
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, "storage", "skeleton", "account-snapshot.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var v struct {
		Primary struct {
			UID string `json:"uid"`
		} `json:"primary"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return strings.TrimSpace(v.Primary.UID)
}

// WorkBuddyCurrentUIDsByEdition 返回各版本当前登录的 uid（值为空表示该版本未登录）。
func WorkBuddyCurrentUIDsByEdition() map[WorkBuddyEdition]string {
	out := map[WorkBuddyEdition]string{}
	for ed, dir := range WorkBuddyEditionConfigDirs() {
		out[ed] = workBuddyUIDFromDir(dir)
	}
	return out
}

// WorkBuddyCurrentUIDs 返回所有版本当前登录 uid 的集合（可能同时登录国内 + 国际）。
func WorkBuddyCurrentUIDs() []string {
	var out []string
	seen := map[string]bool{}
	for _, uid := range WorkBuddyCurrentUIDsByEdition() {
		if uid != "" && !seen[uid] {
			seen[uid] = true
			out = append(out, uid)
		}
	}
	return out
}

// WorkBuddyCurrentUID 读取当前登录 uid（未登录返回空）。
// 兼容旧调用：优先国内版目录，其次任一已登录版本。
func WorkBuddyCurrentUID() (string, error) {
	if uid := workBuddyUIDFromDir(WorkBuddyConfigDir()); uid != "" {
		return uid, nil
	}
	for _, uid := range WorkBuddyCurrentUIDs() {
		return uid, nil
	}
	return "", nil
}
