// device_login.go MiniMax 浏览器登录（OAuth 2.0 Device Authorization Grant，
// RFC 8628）+ S256 PKCE。
//
// 为什么需要它：MiniMax 号池此前只有两条加号路径 ——「捕获本机客户端登录态」
// 与「手工粘贴 access/refresh token」。前者要求账号已经在本机 MiniMax Code
// 里登录过，后者要求用户去别处抠 token。两者都做不到「在面板里点一下、
// 浏览器里登录完就进池」。设备码授权补上了这条路径。
//
// 实测契约（2026-10-24，account.minimax.cn 与 account.minimax.io 同款）：
//
//	POST {origin}/oauth2/device/code
//	  form: client_id=mcode-public & scope=agent.default & audience=agent-backend
//	        & code_challenge=base64url(sha256(verifier)) & code_challenge_method=S256
//	  → 200 { device_code, user_code, verification_uri, verification_uri_complete,
//	        interval, expires_in }
//	    实测 expires_in=300（授权窗口只有 5 分钟）、interval=3。
//
//	POST {origin}/oauth2/token
//	  form: client_id=mcode-public & device_code=…
//	        & grant_type=urn:ietf:params:oauth:grant-type:device_code & code_verifier=…
//	  → 200 { access_token, refresh_token, token_type, expires_in, scope }
//	  → 400 { error: "authorization_pending" | "slow_down" | "expired_token" | "access_denied" }
//
// 两个必须记住的坑（都是实测踩出来的）：
//
//  1. **必须带 S256 PKCE**。缺 code_challenge 时上游直接
//     400 {"error":"invalid_request","error_description":"valid S256 PKCE is required"}。
//  2. **轮询不能快于 interval**。快了就返回
//     400 {"error":"slow_down"}；按 RFC 8628 收到 slow_down 要把间隔加 5 秒，
//     否则会一直 slow_down，永远换不到 token。
package minimax

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
	"os"
	"strings"
	"time"
)

const (
	// accountOriginCN 国内版账号域。
	accountOriginCN = "https://account.minimax.cn"
	// accountOriginGlobal 国际版账号域。
	accountOriginGlobal = "https://account.minimax.io"

	// agentBaseCN / agentBaseGlobal 模型与签到网关的基址（同一 host，
	// 不同 path 前缀：模型走 /mavis/api/v1，签到走 /minimax-cloud/api/v1）。
	agentBaseCN     = "https://agent.minimax.cn/mavis/api/v1"
	agentBaseGlobal = "https://agent.minimax.io/mavis/api/v1"

	// deviceCodePath 设备码申请端点。
	deviceCodePath = "/oauth2/device/code"
	// deviceTokenPath 设备码换 token 端点（与续期同一个 OAuth 服务器）。
	deviceTokenPath = "/oauth2/token"

	// deviceGrantType RFC 8628 的设备码授权类型。
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// deviceHTTPTimeout 设备码相关请求的单次超时。
	deviceHTTPTimeout = 30 * time.Second
)

// RegionMiniMax 区域标识：cn / global。
const (
	RegionCN     = "cn"
	RegionGlobal = "global"
)

// NormalizeRegion 把用户输入的域标识规整成 cn / global（默认 cn）。
func NormalizeRegion(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "global", "en", "intl", "international", "oversea", "globaledition":
		return RegionGlobal
	default:
		return RegionCN
	}
}

// AccountOrigin 按区域返回账号域（OAuth 服务器）。
// 可用环境变量 MINIMAX_ACCOUNT_ORIGIN_CN / _GLOBAL 覆盖（自建/测试网关）。
func AccountOrigin(region string) string {
	if NormalizeRegion(region) == RegionGlobal {
		if v := strings.TrimSpace(os.Getenv("MINIMAX_ACCOUNT_ORIGIN_GLOBAL")); v != "" {
			return strings.TrimRight(v, "/")
		}
		return accountOriginGlobal
	}
	if v := strings.TrimSpace(os.Getenv("MINIMAX_ACCOUNT_ORIGIN_CN")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return accountOriginCN
}

// AgentBase 按区域返回模型网关基址。
func AgentBase(region string) string {
	if NormalizeRegion(region) == RegionGlobal {
		return agentBaseGlobal
	}
	return agentBaseCN
}

// deviceOrigin 设备码流程使用的账号域。
//
// 若设置了 MINIMAX_OAUTH_URL（续期端点），设备码端点必须与它同源 —— 否则
// 拿到的 refresh_token 在续期端点根本认不出来，账号一小时后就死。
func deviceOrigin(region string) string {
	if v := strings.TrimSpace(os.Getenv("MINIMAX_OAUTH_URL")); v != "" {
		base := strings.TrimRight(v, "/")
		base = strings.TrimSuffix(base, deviceTokenPath)
		if base != "" {
			return base
		}
	}
	return AccountOrigin(region)
}

// newPKCE 生成一对 RFC 7636 的 code_verifier / code_challenge（S256）。
func newPKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 48)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("生成 PKCE 随机数失败: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// DeviceSession 进行中的设备码授权会话。
type DeviceSession struct {
	// State 面板侧会话号（我们自己的随机串，不是上游 device_code）。
	State string
	// Region 区域（cn / global）。
	Region string
	// Origin OAuth 服务器账号域。
	Origin string
	// DeviceCode 上游下发的设备码。
	DeviceCode string
	// UserCode 用户在授权页输入的验证码（如 DPC5-6W4S）。
	UserCode string
	// VerifyURI 授权页地址。
	VerifyURI string
	// VerifyURICComplete 带验证码的授权页地址（可直接点开）。
	VerifyURICComplete string
	// Interval 轮询间隔（秒）；收到 slow_down 会自动加大。
	Interval int
	// ExpiresAt 授权窗口截止时间（上游 expires_in，实测 300 秒）。
	ExpiresAt time.Time
	// CodeVerifier PKCE 明文（换 token 时要带上，不能丢）。
	CodeVerifier string
	// Created 会话创建时刻（用于回收过期会话）。
	Created time.Time
}

// DeviceAuthResult 授权成功后拿到的凭证。
type DeviceAuthResult struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    int64
	Scope        string
	ExpiresAt    time.Time
}

// 设备码轮询结果状态。
const (
	// DevicePending 还没授权完（前端继续轮询）。
	DevicePending = "pending"
	// DeviceOK 授权成功。
	DeviceOK = "ok"
	// DeviceDenied 用户拒绝授权。
	DeviceDenied = "denied"
	// DeviceExpired 设备码过期（授权窗口只有 5 分钟，超时就得重来）。
	DeviceExpired = "expired"
)

// devicePost 发一次表单请求（OAuth 端点都是 x-www-form-urlencoded，不是 JSON）。
func devicePost(ctx context.Context, c *http.Client, fullURL string, form url.Values) ([]byte, int, error) {
	if c == nil {
		c = &http.Client{Timeout: deviceHTTPTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// StartDeviceAuth 发起一次设备码授权：拿验证码 + 授权链接。
//
// 只申请设备码，不会消耗任何账号；真正的授权动作发生在用户的浏览器里。
func StartDeviceAuth(ctx context.Context, c *http.Client, region string) (*DeviceSession, error) {
	verifier, challenge, err := newPKCE()
	if err != nil {
		return nil, err
	}
	origin := deviceOrigin(region)
	form := url.Values{}
	form.Set("client_id", oauthClientID())
	form.Set("scope", oauthScope())
	form.Set("audience", oauthAudience())
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")

	raw, status, err := devicePost(ctx, c, origin+deviceCodePath, form)
	if err != nil {
		return nil, fmt.Errorf("申请设备码失败（HTTP %d）: %w", status, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("申请设备码失败 HTTP %d: %s", status, truncate(string(raw), 200))
	}
	var r struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("设备码响应解析失败: %w", err)
	}
	if r.DeviceCode == "" {
		return nil, fmt.Errorf("设备码响应缺少 device_code: %s", truncate(string(raw), 200))
	}
	if r.Interval <= 0 {
		r.Interval = 5
	}
	if r.ExpiresIn <= 0 {
		r.ExpiresIn = 300
	}
	return &DeviceSession{
		Region:            NormalizeRegion(region),
		Origin:            origin,
		DeviceCode:        r.DeviceCode,
		UserCode:          r.UserCode,
		VerifyURI:         r.VerificationURI,
		VerifyURICComplete: r.VerificationURIComplete,
		Interval:          r.Interval,
		ExpiresAt:         time.Now().Add(time.Duration(r.ExpiresIn) * time.Second),
		CodeVerifier:      verifier,
		Created:           time.Now(),
	}, nil
}

// PollDeviceAuth 轮询一次授权结果。
//
// 返回状态：pending（继续等）/ ok（拿到凭证）/ denied / expired。
// pending 时 err 为 nil；ok 时 err 为 nil 且 tok 非空。
func PollDeviceAuth(ctx context.Context, c *http.Client, sess *DeviceSession) (string, *DeviceAuthResult, error) {
	if sess == nil {
		return DevicePending, nil, fmt.Errorf("授权会话不存在")
	}
	if !sess.ExpiresAt.IsZero() && time.Now().After(sess.ExpiresAt) {
		return DeviceExpired, nil, fmt.Errorf("授权码已过期（授权窗口约 %d 秒），请重新发起登录", int(time.Until(sess.ExpiresAt).Seconds())+300)
	}
	form := url.Values{}
	form.Set("client_id", oauthClientID())
	form.Set("device_code", sess.DeviceCode)
	form.Set("grant_type", deviceGrantType)
	form.Set("code_verifier", sess.CodeVerifier)

	raw, status, err := devicePost(ctx, c, sess.Origin+deviceTokenPath, form)
	if err != nil {
		return DevicePending, nil, fmt.Errorf("轮询失败: %w", err)
	}
	if status == http.StatusOK {
		var r struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			TokenType    string `json:"token_type"`
			ExpiresIn    int64  `json:"expires_in"`
			Scope        string `json:"scope"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return DevicePending, nil, fmt.Errorf("令牌响应解析失败: %w", err)
		}
		if r.AccessToken == "" {
			return DevicePending, nil, fmt.Errorf("令牌响应缺少 access_token: %s", truncate(string(raw), 200))
		}
		out := &DeviceAuthResult{
			AccessToken:  r.AccessToken,
			RefreshToken: r.RefreshToken,
			TokenType:    r.TokenType,
			ExpiresIn:    r.ExpiresIn,
			Scope:        r.Scope,
		}
		if r.ExpiresIn > 0 {
			out.ExpiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
		}
		return DeviceOK, out, nil
	}

	// OAuth 错误体：{"error":"authorization_pending","error_description":"…"}
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &e)
	switch e.Error {
	case "authorization_pending":
		return DevicePending, nil, nil
	case "slow_down":
		// RFC 8628 §3.5：收到 slow_down 就把间隔加 5 秒再继续。
		sess.Interval += 5
		return DevicePending, nil, nil
	case "expired_token":
		return DeviceExpired, nil, fmt.Errorf("授权码已过期：%s", e.Description)
	case "access_denied":
		return DeviceDenied, nil, fmt.Errorf("授权被拒绝：%s", e.Description)
	default:
		msg := e.Description
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		return DevicePending, nil, fmt.Errorf("授权失败：%s", msg)
	}
}
