package minimax

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MiniMax Cloud 的签到与积分接口（逆向自客户端 signin transport）：
//
//	GET  {cloud}/signin/status?timezone_id=Asia/Shanghai
//	GET  {cloud}/signin/config
//	POST {cloud}/signin/claim?timezone_id=Asia/Shanghai   body {}
//	GET  {cloud}/credit/details
//
// 鉴权与模型接口相同：Authorization: Bearer <accessToken>。
// 关键点：签到是**账号级**的，只需要该账号的 token，不需要切号、不需要客户端在线，
// 所以可以按号池逐个账号领（多号一起领积分）。

const (
	cloudSuffix = "/minimax-cloud/api/v1"
	// 签到结算时区，客户端取 Intl.DateTimeFormat().resolvedOptions().timeZone。
	// 可用 MINIMAX_TZ 覆盖（如 Asia/Tokyo）。
	defaultTZ = "Asia/Shanghai"
	// 领取结果枚举（客户端 SigninClaimResult）。
	claimResultClaimed = 1
	claimResultAlready = 2
)

// CloudBase 由模型 baseURL 推导（同一 host，不同 path 前缀）。
func (c *Client) CloudBase() string {
	base := c.BaseURL
	if i := strings.Index(base, "/mavis/api/"); i >= 0 {
		return base[:i] + cloudSuffix
	}
	return strings.TrimRight(base, "/") + cloudSuffix
}

func signinTZ() string {
	if v := strings.TrimSpace(os.Getenv("MINIMAX_TZ")); v != "" {
		return v
	}
	return defaultTZ
}

// cloudDo 用指定登录态发一次 minimax-cloud 请求。
func (c *Client) cloudDo(ctx context.Context, cred Credentials, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	u := c.CloudBase() + path
	if !strings.Contains(path, "timezone_id=") {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		u += sep + "timezone_id=" + url.QueryEscape(signinTZ())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// baseResp 是所有 minimax-cloud 响应的公共头。
type baseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

func checkBase(raw []byte) error {
	var r struct {
		BaseResp baseResp `json:"base_resp"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return fmt.Errorf("响应不是合法 JSON")
	}
	if r.BaseResp.StatusCode != 0 {
		return fmt.Errorf("上游错误 %d: %s", r.BaseResp.StatusCode, r.BaseResp.StatusMsg)
	}
	return nil
}

// SigninDay 签到面板中的一天。
type SigninDay struct {
	DayNo  int     `json:"day_no"`
	Points float64 `json:"points"`
	Status int     `json:"status"` // 3=已领；1=未领
	Today  bool    `json:"is_today"`
}

// SigninStatus 签到面板。
type SigninStatus struct {
	Days    []SigninDay `json:"days"`
	Today   *SigninDay  `json:"today,omitempty"`
	Claimed bool        `json:"claimed_today"`
}

// QuerySignin 查询某账号的签到面板。
func (c *Client) QuerySignin(ctx context.Context, cred Credentials) (*SigninStatus, error) {
	status, raw, err := c.cloudDo(ctx, cred, http.MethodGet, "/signin/status", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("签到底座 HTTP %d: %s", status, truncate(string(raw), 160))
	}
	if err := checkBase(raw); err != nil {
		return nil, err
	}
	var r struct {
		Data struct {
			Days []SigninDay `json:"days"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	out := &SigninStatus{Days: r.Data.Days}
	for i := range out.Days {
		if out.Days[i].Today {
			d := out.Days[i]
			out.Today = &d
			out.Claimed = d.Status == 3
		}
	}
	return out, nil
}

// SigninClaimResult 领取结果。
type SigninClaimResult struct {
	Result   int     `json:"result"` // 1=领取成功 2=今日已领
	DayNo    int     `json:"day_no"`
	Points   float64 `json:"points"`
	ExpireAt int64   `json:"expire_at_ms"`
	Phase    string  `json:"phase"` // claimed / already / error
	Msg      string  `json:"msg"`
}

// ClaimSignin 领取今日积分（幂等：已领返回 already）。
func (c *Client) ClaimSignin(ctx context.Context, cred Credentials) (*SigninClaimResult, error) {
	status, raw, err := c.cloudDo(ctx, cred, http.MethodPost, "/signin/claim", []byte("{}"))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("签到 HTTP %d: %s", status, truncate(string(raw), 160))
	}
	if err := checkBase(raw); err != nil {
		return nil, err
	}
	var r struct {
		Data struct {
			ClaimResult int     `json:"claim_result"`
			DayNo       int     `json:"day_no"`
			Points      float64 `json:"points"`
			ExpireAtMs  int64   `json:"expire_at_ms"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	out := &SigninClaimResult{
		Result: r.Data.ClaimResult, DayNo: r.Data.DayNo,
		Points: r.Data.Points, ExpireAt: r.Data.ExpireAtMs,
	}
	switch out.Result {
	case claimResultClaimed:
		out.Phase, out.Msg = "claimed", fmt.Sprintf("签到成功 +%.0f 积分", out.Points)
	case claimResultAlready:
		out.Phase, out.Msg = "already", "今日已领取"
	default:
		out.Phase, out.Msg = "error", fmt.Sprintf("未知领取结果 %d", out.Result)
	}
	return out, nil
}

// CreditSummary 积分概览。
type CreditSummary struct {
	Remaining float64 `json:"remaining"`
	Granted   float64 `json:"granted"`
	ExpireAt  int64   `json:"expire_at_ms"`
}

// QueryCredits 查询某账号的积分明细（汇总剩余/累计/最近到期）。
func (c *Client) QueryCredits(ctx context.Context, cred Credentials) (*CreditSummary, error) {
	status, raw, err := c.cloudDo(ctx, cred, http.MethodGet, "/credit/details", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("积分接口 HTTP %d", status)
	}
	if err := checkBase(raw); err != nil {
		return nil, err
	}
	var r struct {
		Details []struct {
			Granted   string `json:"granted_amount"`
			Remaining string `json:"remaining_amount"`
			ExpireAt  int64  `json:"expire_at_ms"`
		} `json:"details"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	out := &CreditSummary{}
	for _, d := range r.Details {
		out.Remaining += parseFloatOr0(d.Remaining)
		out.Granted += parseFloatOr0(d.Granted)
		if out.ExpireAt == 0 || (d.ExpireAt > 0 && d.ExpireAt < out.ExpireAt) {
			out.ExpireAt = d.ExpireAt
		}
	}
	return out, nil
}

func parseFloatOr0(s string) float64 {
	var f float64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%f", &f)
	if err != nil {
		return 0
	}
	return f
}

// ---------------------------------------------------------------------------
// OAuth 续期
//
// MiniMax Code 的 access token 只有 1 小时（expires_in=3600），客户端自己会续；
// 但号池里的「非当前登录号」没人替它续，所以必须自己刷：
//
//	POST https://account.minimax.cn/oauth2/token
//	form: grant_type=refresh_token&refresh_token=...&client_id=mcode-public
//	→ {access_token, refresh_token, expires_in, token_type, scope}
//
// 注意：表单编码（application/x-www-form-urlencoded），不是 JSON；
// 且 client_id 必填，缺了会 400 invalid_request。
// ---------------------------------------------------------------------------

const (
	defaultOAuthURL = "https://account.minimax.cn/oauth2/token"
	defaultClientID = "mcode-public"
	// defaultScope / defaultAudience 是 OAuth 续期必须带的两个参数。
	//
	// 实测契约（MiniMax Code 客户端自身用的同一套 grant）：
	//   POST https://account.minimax.cn/oauth2/token
	//   grant_type=refresh_token & refresh_token=<mmort_…> & client_id=mcode-public
	//   & scope=agent.default & audience=agent-backend
	//   -> { access_token, refresh_token, token_type, expires_in, scope }
	//
	// 此前只发前三个参数，缺 scope/audience —— 上游对不完整的续期请求直接拒，
	// 于是号池里的 access token 一过期就只能 401，号也就永远签不上。
	// 两个参数都能用环境变量覆盖（国际版 / 自建网关）。
	defaultScope    = "agent.default"
	defaultAudience = "agent-backend"
)

func oauthURL() string {
	if v := strings.TrimSpace(os.Getenv("MINIMAX_OAUTH_URL")); v != "" {
		return v
	}
	return defaultOAuthURL
}

func oauthClientID() string {
	if v := strings.TrimSpace(os.Getenv("MINIMAX_CLIENT_ID")); v != "" {
		return v
	}
	return defaultClientID
}

func oauthScope() string {
	if v := strings.TrimSpace(os.Getenv("MINIMAX_OAUTH_SCOPE")); v != "" {
		return v
	}
	return defaultScope
}

func oauthAudience() string {
	if v := strings.TrimSpace(os.Getenv("MINIMAX_OAUTH_AUDIENCE")); v != "" {
		return v
	}
	return defaultAudience
}

// RefreshToken 结果。
type Refreshed struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// Refresh 用 refresh_token 换新 token。
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Refreshed, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("没有 refresh_token")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", oauthClientID())
	// scope / audience 必填（见 defaultScope 注释的实测契约）；缺了上游直接拒。
	form.Set("scope", oauthScope())
	form.Set("audience", oauthAudience())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("续期失败 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("续期响应解析失败: %w", err)
	}
	if r.AccessToken == "" {
		return nil, fmt.Errorf("续期响应没有 access_token")
	}
	out := &Refreshed{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken}
	if out.RefreshToken == "" {
		out.RefreshToken = refreshToken // 服务端没轮换则沿用
	}
	if r.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	return out, nil
}

// refreshAccount 刷新号池账号并写回（尽力而为：写盘失败不影响本次使用）。
func refreshAccount(ctx context.Context, c *Client, acc *Account) bool {
	if acc == nil || acc.RefreshToken == "" {
		return false
	}
	nw, err := c.Refresh(ctx, acc.RefreshToken)
	if err != nil {
		return false
	}
	acc.AccessToken = nw.AccessToken
	acc.RefreshToken = nw.RefreshToken
	if !nw.ExpiresAt.IsZero() {
		acc.ExpiresAtMs = nw.ExpiresAt.UnixMilli()
	}
	if pool, err := LoadPool(); err == nil {
		for _, a := range pool.Accounts {
			if a != nil && a.ID == acc.ID {
				a.AccessToken, a.RefreshToken, a.ExpiresAtMs = acc.AccessToken, acc.RefreshToken, acc.ExpiresAtMs
			}
		}
		_ = SavePool(pool)
	}
	return true
}

// credsOf 由账号快照构造凭证。
func credsOf(a *Account) Credentials {
	cred := Credentials{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken}
	if a.ExpiresAtMs > 0 {
		cred.ExpiresAt = time.UnixMilli(a.ExpiresAtMs)
	}
	return cred
}

// is401 判断错误是否来自鉴权失败（需要续期后重试）。
func is401(err error) bool {
	return err != nil && strings.Contains(err.Error(), "401")
}

// ---------------------------------------------------------------------------
// 多账号池：一个账号 = 一份 auth.json 快照（OAuth token 对）。
// 与 Qoder/WorkBuddy 的池子同思路，但这里的「账号」没有邮箱等身份信息，
// 所以用 token 指纹做 id，用加入顺序做显示名。
// ---------------------------------------------------------------------------

// Account 号池中的一个 MiniMax 账号。
type Account struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	UserID       string `json:"user_id,omitempty"`
	UserName     string `json:"user_name,omitempty"`
	Email        string `json:"email,omitempty"`
	AvatarURL    string `json:"avatar_url,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAtMs  int64  `json:"expires_at_ms"`
	SavedAt      string `json:"saved_at"`
}

// PoolFile 号池文件。
type PoolFile struct {
	Accounts []*Account `json:"accounts"`
}

// PoolPath 号池文件路径：MINIMAX_POOL_FILE 优先，否则 ./data/minimax_accounts.json。
func PoolPath() string {
	if v := strings.TrimSpace(os.Getenv("MINIMAX_POOL_FILE")); v != "" {
		return v
	}
	return filepath.Join("data", "minimax_accounts.json")
}

// Fingerprint 由 token 派生稳定短 id（同一 token 永远同一 id）。
func Fingerprint(token string) string {
	sum := sha1.Sum([]byte(token))
	return hex.EncodeToString(sum[:6])
}

// LoadPool 读取号池（文件不存在 → 空池）。
func LoadPool() (*PoolFile, error) {
	raw, err := os.ReadFile(PoolPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &PoolFile{}, nil
		}
		return nil, err
	}
	var f PoolFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("号池文件损坏: %w", err)
	}
	return &f, nil
}

// SavePool 写回号池（0600，含 token）。
func SavePool(f *PoolFile) error {
	if f == nil {
		return nil
	}
	p := PoolPath()
	if dir := filepath.Dir(p); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	buf, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, buf, 0o600)
}

// CaptureCurrent 把当前客户端登录态存入号池（已存在则更新 token）。
//
// 身份用 **服务端 user_id**（/matrix/api/v1/user/profile）而不是 token 指纹：
// access token 每小时轮换，用指纹做 id 会导致「每续期一次就多出一条重复账号」。
// 拿不到资料时才退回指纹。
func CaptureCurrent(ctx context.Context, c *Client) (*Account, bool, error) {
	cred, err := LoadCredentials()
	if err != nil {
		return nil, false, err
	}
	pool, err := LoadPool()
	if err != nil {
		return nil, false, err
	}
	id := Fingerprint(cred.AccessToken)
	var prof *Profile
	if c != nil {
		if p, perr := c.FetchProfile(ctx, cred); perr == nil && p != nil && p.UserID != "" {
			prof = p
			id = "u" + p.UserID
		}
	}
	exp := int64(0)
	if !cred.ExpiresAt.IsZero() {
		exp = cred.ExpiresAt.UnixMilli()
	}
	added := true
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		// 同一个人：id 相同，或 user_id 相同（兼容历史用指纹命名的条目）。
		same := a.ID == id || (prof != nil && prof.UserID != "" && a.UserID == prof.UserID)
		if !same {
			continue
		}
		a.ID = id
		a.AccessToken, a.RefreshToken, a.ExpiresAtMs = cred.AccessToken, cred.RefreshToken, exp
		a.SavedAt = time.Now().Format(time.RFC3339)
		if prof != nil {
			a.UserID, a.UserName, a.Email, a.AvatarURL = prof.UserID, prof.UserName, prof.Email, prof.Avatar
			if prof.UserName != "" {
				a.Label = prof.UserName
			}
		}
		dedupeByUser(pool)
		added = false
		if err := SavePool(pool); err != nil {
			return nil, false, err
		}
		return a, false, nil
	}
	acc := &Account{
		ID: id, Label: fmt.Sprintf("MiniMax 账号 %d", len(pool.Accounts)+1),
		AccessToken: cred.AccessToken, RefreshToken: cred.RefreshToken,
		ExpiresAtMs: exp, SavedAt: time.Now().Format(time.RFC3339),
	}
	if prof != nil {
		acc.UserID, acc.UserName, acc.Email, acc.AvatarURL = prof.UserID, prof.UserName, prof.Email, prof.Avatar
		if prof.UserName != "" {
			acc.Label = prof.UserName
		}
	}
	pool.Accounts = append(pool.Accounts, acc)
	if err := SavePool(pool); err != nil {
		return nil, false, err
	}
	return acc, added, nil
}

// RemoveAccount 从号池移除一个账号。
func RemoveAccount(id string) error {
	pool, err := LoadPool()
	if err != nil {
		return err
	}
	out := pool.Accounts[:0]
	for _, a := range pool.Accounts {
		if a != nil && a.ID != id {
			out = append(out, a)
		}
	}
	pool.Accounts = out
	return SavePool(pool)
}

// AccountSignin 单账号签到结果。
type AccountSignin struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Phase    string  `json:"phase"` // claimed / already / error
	Points   float64 `json:"points"`
	Msg      string  `json:"msg"`
	Expired  bool    `json:"expired"`
	DayNo    int     `json:"day_no"`
}

// SigninAccounts 对号池账号逐个签到（ids 为空 = 全部）。
// 每个账号用自己的 token，互不影响；不需要切号，也不需要客户端在线。
func SigninAccounts(ctx context.Context, client *Client, ids []string) ([]AccountSignin, error) {
	pool, err := LoadPool()
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	out := make([]AccountSignin, 0, len(pool.Accounts))
	for _, a := range pool.Accounts {
		if a == nil || a.AccessToken == "" {
			continue
		}
		if len(want) > 0 && !want[a.ID] {
			continue
		}
		// access token 只有 1 小时：过期就先续，再签；401 再补一次续期重试。
		if credsOf(a).Expired() {
			refreshAccount(ctx, client, a)
		}
		cred := credsOf(a)
		claim, err := client.ClaimSignin(ctx, cred)
		if err != nil && is401(err) && refreshAccount(ctx, client, a) {
			cred = credsOf(a)
			claim, err = client.ClaimSignin(ctx, cred)
		}
		res := AccountSignin{ID: a.ID, Label: a.Label, Expired: cred.Expired()}
		if err != nil {
			res.Phase, res.Msg = "error", friendlyMinimaxErr(err)
		} else {
			res.Phase, res.Msg, res.Points, res.DayNo = claim.Phase, claim.Msg, claim.Points, claim.DayNo
		}
		out = append(out, res)
	}
	return out, nil
}

// AccountStatus 号池账号的面板状态（签到 + 积分）。
type AccountStatus struct {
	ID           string  `json:"id"`
	Label        string  `json:"label"`
	UserID       string  `json:"user_id,omitempty"`
	UserName     string  `json:"user_name,omitempty"`
	Email        string  `json:"email,omitempty"`
	AvatarURL    string  `json:"avatar_url,omitempty"`
	ClaimedToday bool    `json:"claimed_today"`
	Points       float64 `json:"points_today"`
	Credits      float64 `json:"credits"`
	Expired      bool    `json:"expired"`
	ExpiresAtMs  int64   `json:"expires_at_ms"`
	Error        string  `json:"error,omitempty"`
}

// StatusOfAccounts 查询号池各账号的签到状态与积分（只读，不打领取接口）。
func StatusOfAccounts(ctx context.Context, client *Client) ([]AccountStatus, error) {
	pool, err := LoadPool()
	if err != nil {
		return nil, err
	}
	// 历史遗留：早期版本用 token 指纹做 id，续期后会重复。这里顺手收拢。
	if before := len(pool.Accounts); before > 0 {
		dedupeByUser(pool)
		if len(pool.Accounts) != before {
			_ = SavePool(pool)
		}
	}
	out := make([]AccountStatus, 0, len(pool.Accounts))
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		if credsOf(a).Expired() {
			refreshAccount(ctx, client, a)
		}
		cred := credsOf(a)
		signin, err := client.QuerySignin(ctx, cred)
		if err != nil && is401(err) && refreshAccount(ctx, client, a) {
			cred = credsOf(a)
			signin, err = client.QuerySignin(ctx, cred)
		}
		// 补齐真实昵称/邮箱（号池只存 token，没有身份信息），并写回池文件。
		client.fillProfile(ctx, a, cred)
		st := AccountStatus{
			ID: a.ID, Label: a.Label, UserID: a.UserID, UserName: a.UserName, Email: a.Email,
			AvatarURL: a.AvatarURL, Expired: cred.Expired(), ExpiresAtMs: a.ExpiresAtMs,
		}
		if err == nil && signin.Today != nil {
			st.ClaimedToday = signin.Claimed
			st.Points = signin.Today.Points
		} else if err != nil {
			st.Error = err.Error()
		}
		credits, cerr := client.QueryCredits(ctx, cred)
		if cerr != nil && is401(cerr) && refreshAccount(ctx, client, a) {
			cred = credsOf(a)
			credits, cerr = client.QueryCredits(ctx, cred)
		}
		if cerr == nil && credits != nil {
			st.Credits = credits.Remaining
		}
		out = append(out, st)
	}
	return out, nil
}

// friendlyMinimaxErr 把 401/invalid_grant 翻译成用户能照做的提示。
func friendlyMinimaxErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "401") || strings.Contains(msg, "invalid_grant") || strings.Contains(msg, "invalid access token") {
		return "凭证已失效：请在 MiniMax Code 重新登录该账号，再点「添加当前账号」刷新它"
	}
	return msg
}

// Profile 账号公开资料（/matrix/api/v1/user/profile）。
type Profile struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	Email    string `json:"email"`
	Avatar   string `json:"avatar_url"`
}

// FetchProfile 用该账号的 token 取资料：号池里没有邮箱/昵称时用它补齐显示名。
func (c *Client) FetchProfile(ctx context.Context, cred Credentials) (*Profile, error) {
	base := c.BaseURL
	if i := strings.Index(base, "/mavis/api/"); i >= 0 {
		base = base[:i]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/matrix/api/v1/user/profile", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("资料接口 HTTP %d", resp.StatusCode)
	}
	var out Profile
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// fillProfile 给账号补齐昵称/邮箱/头像，并把 label 换成真实昵称。
// 只在缺失时查，避免每次刷面板都打一遍上游。
func (c *Client) fillProfile(ctx context.Context, a *Account, cred Credentials) {
	if a == nil || a.UserName != "" {
		return
	}
	p, err := c.FetchProfile(ctx, cred)
	if err != nil || p == nil {
		return
	}
	a.UserID, a.UserName, a.Email, a.AvatarURL = p.UserID, p.UserName, p.Email, p.Avatar
	if p.UserName != "" {
		a.Label = p.UserName
	} else if p.UserID != "" {
		a.Label = "MiniMax " + tail(p.UserID, 6)
	}
	if pool, err := LoadPool(); err == nil {
		for _, x := range pool.Accounts {
			if x != nil && x.ID == a.ID {
				x.UserID, x.UserName, x.Email, x.AvatarURL, x.Label = a.UserID, a.UserName, a.Email, a.AvatarURL, a.Label
			}
		}
		_ = SavePool(pool)
	}
}

// tail 返回字符串末尾 n 个字符。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// dedupeByUser 同一 user_id 只保留一条（保留 SavedAt 最新、token 未过期的）。
func dedupeByUser(pool *PoolFile) {
	if pool == nil {
		return
	}
	best := map[string]*Account{}
	out := pool.Accounts[:0]
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		key := a.UserID
		if key == "" {
			out = append(out, a)
			continue
		}
		prev, ok := best[key]
		if !ok {
			best[key] = a
			out = append(out, a)
			continue
		}
		// 已有同 user：保留保存时间更晚 / token 更新鲜的那条。
		if preferAccount(a, prev) {
			for i, x := range out {
				if x == prev {
					out[i] = a
					break
				}
			}
			best[key] = a
		}
	}
	pool.Accounts = out
}

func preferAccount(a, b *Account) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	if a.ExpiresAtMs != b.ExpiresAtMs {
		return a.ExpiresAtMs > b.ExpiresAtMs
	}
	return a.SavedAt > b.SavedAt
}
