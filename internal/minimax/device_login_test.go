package minimax

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeOAuth 假的 OAuth 服务器：按路径分派，可指定响应。
type fakeOAuth struct {
	codeForm url.Values
	tokForm  url.Values
	tokBody  string
	tokCode  int
}

func (f *fakeOAuth) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("解析表单失败: %v", err)
		}
		switch r.URL.Path {
		case deviceCodePath:
			f.codeForm = r.PostForm
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"device_code":"DC-123","user_code":"AAAA-BBBB",` +
				`"verification_uri":"https://account.example/oauth-authorize",` +
				`"verification_uri_complete":"https://account.example/oauth-authorize?user_code=AAAA-BBBB",` +
				`"interval":3,"expires_in":300}`))
		case deviceTokenPath:
			f.tokForm = r.PostForm
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.tokCode)
			_, _ = w.Write([]byte(f.tokBody))
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestStartDeviceAuth 表单契约：client_id / scope / audience / PKCE 四件套。
func TestStartDeviceAuth(t *testing.T) {
	fake := &fakeOAuth{}
	srv := fake.server(t)
	t.Setenv("MINIMAX_ACCOUNT_ORIGIN_CN", srv.URL)
	t.Setenv("MINIMAX_OAUTH_URL", "") // 别让续期端点覆盖账号域推导

	sess, err := StartDeviceAuth(context.Background(), srv.Client(), RegionCN)
	if err != nil {
		t.Fatalf("StartDeviceAuth 失败: %v", err)
	}
	if sess.DeviceCode != "DC-123" || sess.UserCode != "AAAA-BBBB" {
		t.Fatalf("设备码/验证码解析错误: %+v", sess)
	}
	if sess.Interval != 3 {
		t.Errorf("interval = %d, 期望 3", sess.Interval)
	}
	if !strings.Contains(sess.VerifyURICComplete, "user_code=AAAA-BBBB") {
		t.Errorf("verification_uri_complete 未带验证码: %q", sess.VerifyURICComplete)
	}
	if sess.CodeVerifier == "" {
		t.Fatal("code_verifier 为空，换 token 时会失败")
	}

	f := fake.codeForm
	if got := f.Get("client_id"); got != "mcode-public" {
		t.Errorf("client_id = %q, 期望 mcode-public", got)
	}
	if got := f.Get("scope"); got != "agent.default" {
		t.Errorf("scope = %q, 期望 agent.default", got)
	}
	if got := f.Get("audience"); got != "agent-backend" {
		t.Errorf("audience = %q, 期望 agent-backend", got)
	}
	if got := f.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, 期望 S256（上游缺这个会 400）", got)
	}
	// code_challenge 必须等于 base64url(sha256(verifier))。
	sum := sha256.Sum256([]byte(sess.CodeVerifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := f.Get("code_challenge"); got != want {
		t.Errorf("code_challenge 与 verifier 不匹配:\n got=%s\nwant=%s", got, want)
	}
}

// TestPollDeviceAuthPending 未授权 → pending（前端继续轮询）。
func TestPollDeviceAuthPending(t *testing.T) {
	fake := &fakeOAuth{tokCode: http.StatusBadRequest,
		tokBody: `{"error":"authorization_pending","error_description":"the user has not finished authorizing yet"}`}
	srv := fake.server(t)
	t.Setenv("MINIMAX_ACCOUNT_ORIGIN_CN", srv.URL)

	sess := &DeviceSession{Origin: srv.URL, DeviceCode: "DC-123", CodeVerifier: "v", Interval: 3}
	state, tok, err := PollDeviceAuth(context.Background(), srv.Client(), sess)
	if state != DevicePending || tok != nil || err != nil {
		t.Fatalf("pending 分支错误: state=%s tok=%v err=%v", state, tok, err)
	}
	if got := fake.tokForm.Get("grant_type"); got != deviceGrantType {
		t.Errorf("grant_type = %q, 期望 %q", got, deviceGrantType)
	}
	if got := fake.tokForm.Get("code_verifier"); got != "v" {
		t.Errorf("code_verifier = %q, 期望 v", got)
	}
}

// TestPollDeviceAuthSlowDown 轮询过快 → slow_down，间隔要加大（RFC 8628 §3.5）。
func TestPollDeviceAuthSlowDown(t *testing.T) {
	fake := &fakeOAuth{tokCode: http.StatusBadRequest,
		tokBody: `{"error":"slow_down","error_description":"polling faster than the interval that was issued"}`}
	srv := fake.server(t)
	t.Setenv("MINIMAX_ACCOUNT_ORIGIN_CN", srv.URL)

	sess := &DeviceSession{Origin: srv.URL, DeviceCode: "DC-123", CodeVerifier: "v", Interval: 3}
	state, tok, err := PollDeviceAuth(context.Background(), srv.Client(), sess)
	if state != DevicePending || tok != nil || err != nil {
		t.Fatalf("slow_down 分支错误: state=%s tok=%v err=%v", state, tok, err)
	}
	if sess.Interval != 8 {
		t.Errorf("interval = %d, 期望 8（3 + 5）", sess.Interval)
	}
}

// TestPollDeviceAuthSuccess 授权成功 → 拿到 token 并算出到期时间。
func TestPollDeviceAuthSuccess(t *testing.T) {
	fake := &fakeOAuth{tokCode: http.StatusOK,
		tokBody: `{"access_token":"AT","refresh_token":"RT","token_type":"Bearer","expires_in":3600,"scope":"agent.default"}`}
	srv := fake.server(t)
	t.Setenv("MINIMAX_ACCOUNT_ORIGIN_CN", srv.URL)

	sess := &DeviceSession{Origin: srv.URL, DeviceCode: "DC-123", CodeVerifier: "v", Interval: 3}
	state, tok, err := PollDeviceAuth(context.Background(), srv.Client(), sess)
	if err != nil {
		t.Fatalf("成功分支报错: %v", err)
	}
	if state != DeviceOK {
		t.Fatalf("state = %s, 期望 ok", state)
	}
	if tok.AccessToken != "AT" || tok.RefreshToken != "RT" {
		t.Fatalf("令牌解析错误: %+v", tok)
	}
	if tok.ExpiresIn != 3600 {
		t.Errorf("expires_in = %d, 期望 3600", tok.ExpiresIn)
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("ExpiresAt 未计算")
	}
}

// TestPollDeviceAuthDenied 拒绝授权 → denied。
func TestPollDeviceAuthDenied(t *testing.T) {
	fake := &fakeOAuth{tokCode: http.StatusBadRequest,
		tokBody: `{"error":"access_denied","error_description":"user denied"}`}
	srv := fake.server(t)
	t.Setenv("MINIMAX_ACCOUNT_ORIGIN_CN", srv.URL)

	sess := &DeviceSession{Origin: srv.URL, DeviceCode: "DC-123", CodeVerifier: "v", Interval: 3}
	state, _, err := PollDeviceAuth(context.Background(), srv.Client(), sess)
	if state != DeviceDenied {
		t.Fatalf("state = %s, 期望 denied", state)
	}
	if err == nil || !strings.Contains(err.Error(), "拒绝") {
		t.Errorf("错误信息应说明被拒绝，实际: %v", err)
	}
}

// TestPollDeviceAuthExpired 会话过期 → expired。
func TestPollDeviceAuthExpired(t *testing.T) {
	fake := &fakeOAuth{}
	srv := fake.server(t)
	t.Setenv("MINIMAX_ACCOUNT_ORIGIN_CN", srv.URL)

	sess := &DeviceSession{Origin: srv.URL, DeviceCode: "DC-123", CodeVerifier: "v", Interval: 3,
		ExpiresAt: time.Now().Add(-time.Minute)}
	state, _, err := PollDeviceAuth(context.Background(), srv.Client(), sess)
	if state != DeviceExpired {
		t.Fatalf("state = %s, 期望 expired", state)
	}
	if err == nil {
		t.Error("过期应带错误信息")
	}
}

// TestRegionMapping 区域 → 域的映射。
func TestRegionMapping(t *testing.T) {
	cases := map[string]struct{ origin, agent string }{
		"cn":     {"https://account.minimax.cn", "https://agent.minimax.cn/mavis/api/v1"},
		"global": {"https://account.minimax.io", "https://agent.minimax.io/mavis/api/v1"},
		"":       {"https://account.minimax.cn", "https://agent.minimax.cn/mavis/api/v1"},
		"en":     {"https://account.minimax.io", "https://agent.minimax.io/mavis/api/v1"},
	}
	for in, want := range cases {
		if got := AccountOrigin(in); got != want.origin {
			t.Errorf("AccountOrigin(%q) = %q, 期望 %q", in, got, want.origin)
		}
		if got := AgentBase(in); got != want.agent {
			t.Errorf("AgentBase(%q) = %q, 期望 %q", in, got, want.agent)
		}
	}
}

// TestForRegion 区域客户端的基址切换。
func TestForRegion(t *testing.T) {
	c := New()
	g := c.ForRegion(RegionGlobal)
	if g == c {
		t.Fatal("全球区应返回新客户端")
	}
	if g.BaseURL != agentBaseGlobal {
		t.Errorf("全球区 BaseURL = %q, 期望 %q", g.BaseURL, agentBaseGlobal)
	}
	if c.BaseURL != agentBaseCN {
		t.Errorf("原客户端 BaseURL 被改动了: %q", c.BaseURL)
	}
	if same := c.ForRegion(RegionCN); same != c {
		t.Error("同区域应复用原客户端")
	}
}

// TestOAuthTokenURLPerRegion 国际号的续期端点必须是国际域。
func TestOAuthTokenURLPerRegion(t *testing.T) {
	t.Setenv("MINIMAX_OAUTH_URL", "")
	g := New().ForRegion(RegionGlobal)
	if got := g.oauthTokenURL(); got != accountOriginGlobal+deviceTokenPath {
		t.Errorf("国际版续期端点 = %q, 期望 %q", got, accountOriginGlobal+deviceTokenPath)
	}
	cn := New()
	if got := cn.oauthTokenURL(); got != defaultOAuthURL {
		t.Errorf("国内版续期端点 = %q, 期望 %q", got, defaultOAuthURL)
	}
}
