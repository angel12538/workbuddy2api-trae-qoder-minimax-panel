package ideauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// Qoder 号池多账号签到回归测试。
//
// 背景（真实 bug）：面板号池里有两个 Qoder 号，但「签到池」只能签当前登录那一个——
// 因为旧实现只读磁盘上的 auth.v1.dat（当前登录态），签到接口也不接受账号参数。
// 修复后：按号池逐个账号用各自的 token 打 Campaign 接口，设备指纹只取一次共用。
//
// 本测试用假上游（httptest）覆盖：
//   1) 两个账号都能领取（各自 Authorization 头正确、互不串号）；
//   2) 指定 emails 时只签指定账号；
//   3) 401 时用各自 refresh_token 续期，并把轮换后的 token 写回号池（不写客户端登录态）。
func TestQoderCheckinPoolMultiAccount(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	// 隔离号池目录：PoolDir 由 IDEAUTH_POOL_DIR 控制，签到日志也落在同一目录。
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	// 避免测试真的去跑 Qoder 的 runtime-info.exe（可能耗 40s）：指向 cmd.exe 让它快速失败。
	if sys := os.Getenv("SystemRoot"); sys != "" {
		t.Setenv("QODER_UMID_EXE", filepath.Join(sys, "System32", "cmd.exe"))
	}

	var mu sync.Mutex
	seenAuth := map[string]int{}   // token -> 请求次数
	claimed := map[string]bool{}   // token -> 今日是否已领（模拟上游状态）
	refreshCalled := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok := r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == qoderPathCampaigns:
			if tok == "Bearer expired-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			mu.Lock()
			seenAuth[tok]++
			done := claimed[tok]
			mu.Unlock()
			status := "CLAIMABLE"
			if done {
				status = "CLAIMED"
			}
			_, _ = w.Write([]byte(`{"campaigns":[{"campaignId":"c1","name":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"` + status + `","benefit":{"amount":100}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/sash/api/v1/me/campaigns/c1/claim":
			mu.Lock()
			claimed[tok] = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"CLAIMED","benefit":{"amount":100}}`))
		case r.Method == http.MethodPost && r.URL.Path == qoderPathDeviceRefresh:
			mu.Lock()
			refreshCalled++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"device_token":"refreshed-token","refreshToken":"refreshed-refresh"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	pool := &QoderPoolFile{Accounts: []*QoderSnapshot{
		{Email: "a@test.com", UserID: "ua", Name: "A", Auth: &QoderAuth{Token: "tok-a", RefreshToken: "rt-a", User: QoderUser{Email: "a@test.com", ID: "ua", Name: "A"}}},
		{Email: "b@test.com", UserID: "ub", Name: "B", Auth: &QoderAuth{Token: "tok-b", RefreshToken: "rt-b", User: QoderUser{Email: "b@test.com", ID: "ub", Name: "B"}}},
	}}
	if err := SaveQoderPool(pool); err != nil {
		t.Fatalf("save pool: %v", err)
	}

	out, err := QoderCheckinPool(nil)
	if err != nil {
		t.Fatalf("QoderCheckinPool: %v", err)
	}
	if len(out.Results) != 2 || out.Claimed != 2 || out.Failed != 0 {
		t.Fatalf("两个号都应签到成功，得到 %+v", out)
	}
	if out.Credits != 200 {
		t.Fatalf("合计积分应为 200，得到 %v", out.Credits)
	}
	// 再跑一次：今日已领，本次新增必须为 0（不能把已领额度算成「又赚了」）
	again, err := QoderCheckinPool(nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again.Claimed != 0 || again.Already != 2 || again.Credits != 0 || again.AlreadyCredits != 200 {
		t.Fatalf("重复签到口径错误（应为本次 0 / 已领 200）: %+v", again)
	}
	mu.Lock()
	gotA, gotB := seenAuth["Bearer tok-a"], seenAuth["Bearer tok-b"]
	mu.Unlock()
	if gotA == 0 || gotB == 0 {
		t.Fatalf("每个账号都必须用各自的 token 打上游，实际 A=%d B=%d（%v）", gotA, gotB, seenAuth)
	}
	for _, r := range out.Results {
		if r.Phase != "claimed" || !r.CheckedInToday {
			t.Errorf("%s 状态异常: phase=%s checked=%v", r.Email, r.Phase, r.CheckedInToday)
		}
	}

	// 指定账号：只签 b
	mu.Lock()
	seenAuth = map[string]int{}
	mu.Unlock()
	out2, err := QoderCheckinPool([]string{"B@TEST.COM"})
	if err != nil {
		t.Fatalf("filtered: %v", err)
	}
	if len(out2.Results) != 1 || out2.Results[0].Email != "b@test.com" {
		t.Fatalf("按邮箱过滤失败（应大小写不敏感）: %+v", out2.Results)
	}
	mu.Lock()
	onlyB := seenAuth["Bearer tok-b"] > 0 && seenAuth["Bearer tok-a"] == 0
	mu.Unlock()
	if !onlyB {
		t.Fatalf("过滤后不该请求其他账号: %v", seenAuth)
	}

	// 401 续期：第三个号 token 已过期
	pool.Accounts = append(pool.Accounts, &QoderSnapshot{
		Email: "c@test.com", UserID: "uc", Name: "C",
		Auth: &QoderAuth{Token: "expired-token", RefreshToken: "rt-c", User: QoderUser{Email: "c@test.com", ID: "uc", Name: "C"}},
	})
	if err := SaveQoderPool(pool); err != nil {
		t.Fatalf("save pool2: %v", err)
	}
	out3, err := QoderCheckinPool([]string{"c@test.com"})
	if err != nil {
		t.Fatalf("refresh case: %v", err)
	}
	if len(out3.Results) != 1 || out3.Results[0].Phase != "claimed" || !out3.Results[0].Refreshed {
		t.Fatalf("过期 token 应续期后签到成功: %+v", out3.Results)
	}
	mu.Lock()
	rc := refreshCalled
	mu.Unlock()
	if rc == 0 {
		t.Fatal("401 后应调用 deviceToken/refresh")
	}
	// 轮换后的 token 必须写回号池
	reloaded, err := LoadQoderPool()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range reloaded.Accounts {
		if s.Email == "c@test.com" {
			found = true
			if s.Auth.Token != "refreshed-token" || s.Auth.RefreshToken != "refreshed-refresh" {
				t.Fatalf("续期后的 token 未写回号池: token=%q refresh=%q", s.Auth.Token, s.Auth.RefreshToken)
			}
		}
	}
	if !found {
		t.Fatal("号池里找不到续期账号")
	}

	// 状态列表：三个号都应标记今日已签
	statuses := QoderPoolStatuses()
	if len(statuses) != 3 {
		t.Fatalf("状态列表应有 3 个账号，得到 %d", len(statuses))
	}
	for _, st := range statuses {
		if st["checked_in_today"] != true {
			t.Errorf("%v 应标记今日已签到", st["email"])
		}
	}

	// 号池 JSON 必须仍是可解析结构（防止字段名漂移）
	raw, err := os.ReadFile(filepath.Join(PoolDir(), "qoder_accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var chk map[string]any
	if json.Unmarshal(raw, &chk) != nil {
		t.Fatal("号池文件不是合法 JSON")
	}
}
