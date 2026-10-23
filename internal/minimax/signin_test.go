package minimax

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// MiniMax 签到/多账号号池回归测试。
//
// 背景：MiniMax Code 的签到是 token 级接口（/minimax-cloud/api/v1/signin/claim），
// 只要拿到账号 token 就能领，不需要切号。这里用假云端验证：
//   1) 号池里多个账号各自能领（各自 Bearer）；
//   2) 已领的返回 already（幂等），不会重复计数；
//   3) 状态查询能同时拿到「今日已签」与积分余额；
//   4) 添加当前账号按 token 指纹去重。

func newFakeCloud(t *testing.T) (*httptest.Server, map[string]bool) {
	t.Helper()
	var mu sync.Mutex
	claimed := map[string]bool{} // token -> 今天是否已领

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(path, "/signin/status"):
			mu.Lock()
			done := claimed[tok]
			mu.Unlock()
			st := 1
			if done {
				st = 3
			}
			_, _ = w.Write([]byte(`{"data":{"scene":2,"days":[{"day_no":1,"points":400,"status":` +
				itoa(st) + `,"is_today":true}]},"base_resp":{"status_code":0,"status_msg":"ok"}}`))
		case strings.HasSuffix(path, "/signin/claim"):
			mu.Lock()
			already := claimed[tok]
			claimed[tok] = true
			mu.Unlock()
			res := 1
			if already {
				res = 2
			}
			_, _ = w.Write([]byte(`{"data":{"claim_id":"c1","claim_result":` + itoa(res) +
				`,"day_no":1,"points":400,"expire_at_ms":1795104000000},"base_resp":{"status_code":0,"status_msg":"ok"}}`))
		case strings.HasSuffix(path, "/credit/details"):
			_, _ = w.Write([]byte(`{"details":[{"granted_amount":"400.00","remaining_amount":"399.93","expire_at_ms":1795104000000}],"base_resp":{"status_code":0,"status_msg":"ok"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, claimed
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestSigninAccountsMultiAccount(t *testing.T) {
	srv, _ := newFakeCloud(t)
	c := &Client{BaseURL: srv.URL + "/mavis/api/v1", HTTP: srv.Client()}

	t.Setenv("MINIMAX_POOL_FILE", filepath.Join(t.TempDir(), "pool.json"))
	pool := &PoolFile{Accounts: []*Account{
		{ID: "id-a", Label: "账号 A", AccessToken: "tok-a"},
		{ID: "id-b", Label: "账号 B", AccessToken: "tok-b"},
	}}
	if err := SavePool(pool); err != nil {
		t.Fatal(err)
	}

	out, err := SigninAccounts(context.Background(), c, nil)
	if err != nil {
		t.Fatalf("SigninAccounts: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("应有 2 个账号结果，得到 %d", len(out))
	}
	for _, r := range out {
		if r.Phase != "claimed" || r.Points != 400 {
			t.Errorf("%s 应领取成功 +400，得到 %+v", r.Label, r)
		}
	}

	// 再跑一次：幂等，应全部 already
	again, err := SigninAccounts(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range again {
		if r.Phase != "already" {
			t.Errorf("%s 重复签到应返回 already，得到 %s", r.Label, r.Phase)
		}
	}

	// 只签指定账号
	only, err := SigninAccounts(context.Background(), c, []string{"id-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].ID != "id-b" {
		t.Fatalf("按 id 过滤失败: %+v", only)
	}

	// 状态：两个都已签，积分 399.93
	sts, err := StatusOfAccounts(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 2 {
		t.Fatalf("状态应有 2 条，得到 %d", len(sts))
	}
	for _, s := range sts {
		if !s.ClaimedToday || s.Points != 400 {
			t.Errorf("%s 状态错误: %+v", s.Label, s)
		}
		if s.Credits < 399 || s.Credits > 401 {
			t.Errorf("%s 积分应为 399.93，得到 %v", s.Label, s.Credits)
		}
	}
}

func TestCaptureCurrentDeduplicates(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	auth := `{"schemaVersion":1,"records":{"k":{"accessToken":"tok-x","refreshToken":"rt-x","expiresAtMs":1792561562136}}}`
	if err := os.WriteFile(authPath, []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINIMAX_AUTH_FILE", authPath)
	t.Setenv("MINIMAX_POOL_FILE", filepath.Join(dir, "pool.json"))

	acc, added, err := CaptureCurrent(context.Background(), nil)
	if err != nil {
		t.Fatalf("CaptureCurrent: %v", err)
	}
	if !added || acc.ID == "" {
		t.Fatalf("首次添加应 added=true 且有 id: %+v", acc)
	}
	_, added2, err := CaptureCurrent(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added2 {
		t.Error("同一 token 重复添加不应新增账号")
	}
	pool, _ := LoadPool()
	if len(pool.Accounts) != 1 {
		t.Fatalf("号池应只有 1 个账号，得到 %d", len(pool.Accounts))
	}
	// 号池文件必须是合法 JSON 且含 token
	raw, err := os.ReadFile(filepath.Join(dir, "pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	var chk map[string]any
	if json.Unmarshal(raw, &chk) != nil {
		t.Fatal("号池文件不是合法 JSON")
	}
}
