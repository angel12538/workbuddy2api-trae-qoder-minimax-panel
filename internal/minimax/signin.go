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

// RefreshToken 结果。UsedRefreshToken 记录本次续期用的 RT：
// 写回客户端 auth.json 时靠它定位要更新的那条记录（绝不能碰别的记录）。
type Refreshed struct {
	AccessToken      string
	RefreshToken     string
	UsedRefreshToken string
	ExpiresAt        time.Time
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.oauthTokenURL(), strings.NewReader(form.Encode()))
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
	out := &Refreshed{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, UsedRefreshToken: refreshToken}
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

// ---------------------------------------------------------------------------
// 凭证续期（v2）
//
// MiniMax 的 access token 只有 1 小时，refresh token **每次刷新都会轮换**（旧的
// 立即作废）。这套轮换语义带来两类故障，必须分开处理：
//
//  1. 号池独占账号（没登录在客户端里）：号池自己持有 RT 链，定期刷新即可长期
//     有效 —— 刷新后把新 RT 写回号池，链条不断。
//  2. 与客户端相同的账号：客户端也在用同一根 RT 链，两边共享。客户端刷一次，
//     号池里存的 RT 就作废一次（这就是「token 一小时就过期」的真相：号池副本
//     没人续，而续期用的 RT 又被客户端轮换掉了）。反过来，如果号池先刷，客户端
//     存的 RT 也会作废 —— 所以号池续期成功后**必须把新 token 同步回客户端的
//     auth.json**，两边永远共享最新的 RT，谁先刷都不掉链。
//
// 判定「是不是客户端账号」必须用服务端 user_id（RT 本身不能证明身份），且只有
// 确认是同一个 user 才允许用客户端的 RT 代刷 —— 否则会把 A 账号刷成 B 账号的
// token，还会作废客户端的 RT（等于把用户从客户端里踢下线）。
// ---------------------------------------------------------------------------

// isInvalidGrant 判断是否为 refresh token 已作废（被轮换/过期）。
func isInvalidGrant(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid_grant")
}

// WriteClientCredentials 把凭证写回客户端 auth.json（按 refreshToken 匹配记录；
// 匹配不到不写 —— 绝不覆盖不明身份的记录）。返回是否写入。
func WriteClientCredentials(nw *Refreshed) bool {
	if nw == nil || nw.RefreshToken == "" {
		return false
	}
	written := false
	for _, p := range AuthCandidates() {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// ⚠️ 必须用通用 JSON 而不是固定结构体：客户端 auth.json 的记录里有
		// schemaVersion / scopes / generation / loginEpoch / audience 等校验字段，
		// 固定结构体写回会把它们全部剥掉 —— 客户端随后报
		// 「CredentialRecordCorruptError: stored OAuth credential record is invalid」
		// （2026-10-24 实测踩坑）。这里只改三个 token 字段，其余原样保留。
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		records, _ := doc["records"].(map[string]any)
		if records == nil {
			continue
		}
		hit := false
		for k, rv := range records {
			rec, ok := rv.(map[string]any)
			if !ok {
				continue
			}
			// 只更新「我们刚用它的 RT 完成续期」的那条记录；其它记录不碰。
			if rt, _ := rec["refreshToken"].(string); rt != nw.UsedRefreshToken {
				continue
			}
			rec["accessToken"] = nw.AccessToken
			rec["refreshToken"] = nw.RefreshToken
			rec["expiresAtMs"] = nw.ExpiresAt.UnixMilli()
			records[k] = rec
			hit = true
		}
		if !hit {
			continue
		}
		buf, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			continue
		}
		if err := atomicWriteFile(p, buf); err == nil {
			written = true
		}
	}
	return written
}

// atomicWriteFile 原子写文件（临时文件 + rename，客户端 auth.json 与号池共用）。
func atomicWriteFile(p string, data []byte) error {
	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, ".mm-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, p)
}

// adoptClientCredentials 把客户端当前登录态并入号池账号（身份经 user_id 校验）。
func adoptClientCredentials(ctx context.Context, c *Client, acc *Account, cc Credentials, clientUID string) bool {
	if clientUID == "" || acc.UserID == "" || acc.UserID != clientUID {
		return false
	}
	acc.AccessToken, acc.RefreshToken = cc.AccessToken, cc.RefreshToken
	if !cc.ExpiresAt.IsZero() {
		acc.ExpiresAtMs = cc.ExpiresAt.UnixMilli()
	}
	acc.SavedAt = time.Now().Format(time.RFC3339)
	saveAccount(*acc)
	return true
}

// saveAccount 把账号快照写回号池（按 ID 匹配；找不到则跳过）。
func saveAccount(acc Account) {
	if pool, err := LoadPool(); err == nil {
		for _, a := range pool.Accounts {
			if a != nil && a.ID == acc.ID {
				*a = acc
			}
		}
		_ = SavePool(pool)
	}
}

// RenewResult 单账号续期结果。
type RenewResult struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Phase    string `json:"phase"` // renewed / adopted / skipped / error
	Msg      string `json:"msg"`
	SyncedClient bool `json:"synced_client,omitempty"` // 新 token 已同步回客户端 auth.json
}

// expiringSoon 判断账号是否将在 d 内过期（已过期也算）。
func (a *Account) expiringSoon(d time.Duration) bool {
	if a.ExpiresAtMs <= 0 {
		return true // 没记到期时间：当作临期，逼一次续期
	}
	return time.Now().Add(d).After(time.UnixMilli(a.ExpiresAtMs))
}

// clientIdentity 客户端当前登录的 user_id（一次 profile 查询；失败返回空）。
func (c *Client) clientIdentity(ctx context.Context) (Credentials, string) {
	cc, err := LoadCredentials()
	if err != nil {
		return Credentials{}, ""
	}
	if p, err := c.FetchProfile(ctx, cc); err == nil && p != nil {
		return cc, p.UserID
	}
	return cc, ""
}

// RenewAccount 续期单个号池账号（安全语义见本文件顶部注释）。
// force=false 时，距过期还有 30 分钟以上的账号直接跳过。
func (c *Client) RenewAccount(ctx context.Context, id string, force bool) RenewResult {
	pool, err := LoadPool()
	if err != nil {
		return RenewResult{ID: id, Phase: "error", Msg: "读取号池失败: " + err.Error()}
	}
	var acc *Account
	for _, a := range pool.Accounts {
		if a != nil && a.ID == id {
			acc = a
			break
		}
	}
	if acc == nil {
		return RenewResult{ID: id, Phase: "error", Msg: "号池里没有该账号"}
	}
	return c.renewAccount(ctx, pool, acc, force)
}

func (c *Client) renewAccount(ctx context.Context, pool *PoolFile, acc *Account, force bool) RenewResult {
	res := RenewResult{ID: acc.ID, Label: acc.Label}
	if !force && !acc.expiringSoon(30*time.Minute) {
		res.Phase = "skipped"
		res.Msg = "凭证仍在有效期内（>30 分钟），无需续期"
		return res
	}
	// 1) 用号池自己的 RT 续期（号池独占账号的主路径；客户端账号也走这条 ——
	//    客户端已刷过时这里会 invalid_grant，落到底下的兜底分支）。
	//    续期端点按账号区域选：国际号必须打 account.minimax.io，用 CN 端点刷
	//    必然 invalid_grant（token 的签发域与校验域一致）。
	rc := c.ForRegion(acc.Region)
	if acc.RefreshToken != "" {
		if nw, err := rc.Refresh(ctx, acc.RefreshToken); err == nil {
			acc.AccessToken, acc.RefreshToken = nw.AccessToken, nw.RefreshToken
			if !nw.ExpiresAt.IsZero() {
				acc.ExpiresAtMs = nw.ExpiresAt.UnixMilli()
			}
			acc.SavedAt = time.Now().Format(time.RFC3339)
			saveAccount(*acc)
			// 同一个账号也登录在客户端里：新 RT 必须同步回客户端，否则客户端
			// 下次刷新必挂（refresh token 已被我们轮换掉）。宁多写不少写。
			res.Phase, res.Msg = "renewed", "凭证已续期"
			if nw.UsedRefreshToken != "" && WriteClientCredentials(nw) {
				res.SyncedClient = true
				res.Msg += "（已同步回客户端）"
			}
			return res
		}
	}
	// 2) 号池 RT 失效：若该账号就是客户端当前登录的账号， adopt 客户端的最新
	//    登录态（客户端自己刷新过）。身份必须经 user_id 校验。
	cc, clientUID := c.clientIdentity(ctx)
	if clientUID != "" && acc.UserID == clientUID {
		if adoptClientCredentials(ctx, c, acc, cc, clientUID) {
			// 客户端的 AT 也过期时替它续一把（写回两边）：客户端在运行时无所谓，
			// 不在运行时号池就是它唯一的续期者。
			if credsOf(acc).Expired() && cc.RefreshToken != "" {
				if nw, err := rc.Refresh(ctx, cc.RefreshToken); err == nil {
					acc.AccessToken, acc.RefreshToken = nw.AccessToken, nw.RefreshToken
					if !nw.ExpiresAt.IsZero() {
						acc.ExpiresAtMs = nw.ExpiresAt.UnixMilli()
					}
					acc.SavedAt = time.Now().Format(time.RFC3339)
					saveAccount(*acc)
					if WriteClientCredentials(nw) {
						return RenewResult{ID: acc.ID, Label: acc.Label, Phase: "renewed",
							Msg: "已接管客户端登录态并续期（同步回客户端）", SyncedClient: true}
					}
				}
			}
			return RenewResult{ID: acc.ID, Label: acc.Label, Phase: "adopted",
				Msg: "已从客户端同步最新凭证"}
		}
	}
	res.Phase = "error"
	res.Msg = "refresh_token 已失效（可能被客户端轮换或已过期）；请在 MiniMax Code 重新登录该账号后点「添加当前账号」"
	return res
}

// RenewAll 批量续期号池账号（缺省 = 临期账号；force = 全部）。串行执行，
// 单号失败不影响其它号。
func (c *Client) RenewAll(ctx context.Context, force bool) []RenewResult {
	pool, err := LoadPool()
	if err != nil {
		return []RenewResult{{Phase: "error", Msg: "读取号池失败: " + err.Error()}}
	}
	out := make([]RenewResult, 0, len(pool.Accounts))
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		out = append(out, c.renewAccount(ctx, pool, a, force))
	}
	return out
}

// AddManual 手动添加账号（粘贴 access_token / refresh_token）—— 默认国内区。
// 身份从服务端补齐（user_id 做去重键），避免同一账号重复入库。
func (c *Client) AddManual(ctx context.Context, access, refresh string, expiresAtMs int64) (*Account, bool, error) {
	return c.AddManualRegion(ctx, access, refresh, expiresAtMs, RegionCN)
}

// AddManualRegion 手动添加账号并指定区域（cn / global）。
//
// 区域决定两件事：资料/签到/续期打哪套域（国际号必须走 agent.minimax.io 与
// account.minimax.io），以及号池里记的 region 字段。
func (c *Client) AddManualRegion(ctx context.Context, access, refresh string, expiresAtMs int64, region string) (*Account, bool, error) {
	access = strings.TrimSpace(access)
	if access == "" {
		return nil, false, fmt.Errorf("access_token 不能为空")
	}
	region = NormalizeRegion(region)
	cred := Credentials{AccessToken: access, RefreshToken: strings.TrimSpace(refresh)}
	if expiresAtMs <= 0 {
		expiresAtMs = time.Now().Add(time.Hour).UnixMilli() // 未提供则按 1 小时估
	}
	cred.ExpiresAt = time.UnixMilli(expiresAtMs)
	pool, err := LoadPool()
	if err != nil {
		return nil, false, err
	}
	id := Fingerprint(access)
	var prof *Profile
	if c != nil {
		if p, perr := c.ForRegion(region).FetchProfile(ctx, cred); perr == nil && p != nil && p.UserID != "" {
			prof = p
			id = "u" + p.UserID
		}
	}
	added := true
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		same := a.ID == id || (prof != nil && prof.UserID != "" && a.UserID == prof.UserID)
		if !same {
			continue
		}
		a.ID = id
		a.AccessToken, a.RefreshToken, a.ExpiresAtMs = cred.AccessToken, cred.RefreshToken, expiresAtMs
		a.SavedAt = time.Now().Format(time.RFC3339)
		a.Region = region
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
		Region: region,
		AccessToken: cred.AccessToken, RefreshToken: cred.RefreshToken,
		ExpiresAtMs: expiresAtMs, SavedAt: time.Now().Format(time.RFC3339),
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
	// Region 区域（cn / global）。决定续期与签到打哪套域 —— 国际号必须走
	// account.minimax.io / agent.minimax.io，用 CN 端点刷必然 invalid_grant。
	Region       string `json:"region,omitempty"`
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
	// 区域从 auth.json 路径推断（~/.minimax/auth/prod/{cn,global}/…）。
	region := RegionFromAuthPath(cred.Source)
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
		if region != "" {
			a.Region = region
		}
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
		Region: region,
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
		// 按账号区域选客户端：国际号的签到要打 agent.minimax.io。
		rc := client.ForRegion(a.Region)
		if rc == nil {
			rc = client
		}
		// access token 只有 1 小时：过期就先续，再签；401 再补一次续期重试。
		if credsOf(a).Expired() {
			refreshAccount(ctx, rc, a)
		}
		cred := credsOf(a)
		claim, err := rc.ClaimSignin(ctx, cred)
		if err != nil && is401(err) && refreshAccount(ctx, rc, a) {
			cred = credsOf(a)
			claim, err = rc.ClaimSignin(ctx, cred)
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
	Region       string  `json:"region,omitempty"`
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
		// 按账号区域选客户端：国际号的签到/积分要打 agent.minimax.io。
		rc := client.ForRegion(a.Region)
		if rc == nil {
			rc = client
		}
		if credsOf(a).Expired() {
			refreshAccount(ctx, rc, a)
		}
		cred := credsOf(a)
		signin, err := rc.QuerySignin(ctx, cred)
		if err != nil && is401(err) && refreshAccount(ctx, rc, a) {
			cred = credsOf(a)
			signin, err = rc.QuerySignin(ctx, cred)
		}
		// 补齐真实昵称/邮箱（号池只存 token，没有身份信息），并写回池文件。
		rc.fillProfile(ctx, a, cred)
		st := AccountStatus{
			ID: a.ID, Label: a.Label, UserID: a.UserID, UserName: a.UserName, Email: a.Email,
			Region: NormalizeRegion(a.Region),
			AvatarURL: a.AvatarURL, Expired: cred.Expired(), ExpiresAtMs: a.ExpiresAtMs,
		}
		if err == nil && signin.Today != nil {
			st.ClaimedToday = signin.Claimed
			st.Points = signin.Today.Points
		} else if err != nil {
			st.Error = err.Error()
		}
		credits, cerr := rc.QueryCredits(ctx, cred)
		if cerr != nil && is401(cerr) && refreshAccount(ctx, rc, a) {
			cred = credsOf(a)
			credits, cerr = rc.QueryCredits(ctx, cred)
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
