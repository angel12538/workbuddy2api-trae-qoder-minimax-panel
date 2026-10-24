package ideauth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// 回归：号池里多个账号被服务端判定为「同一个人」时的签到口径。
//
// 实测（2026-10-24，真实上游）：每日 100 Credits 按**人**给，同一个人的多个账号
// 共用一条活动实例。第一个账号领过之后，另一个账号的 campaigns 列表里就看不到
// 那条活动了；用第一个账号的实例 id 去领，服务端回：
//
//	{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED"}
//
// 面板要做的事：**一键把池里每个号都打一遍**（用户要求，不切号），并且把服务端
// 的真实判定如实显示成「今日已领取（同一人额度已由另一账号领取）」，而不是让这个
// 号一直显示"可签到"让人以为没签上。
func TestQoderCheckinPoolSamePersonProbe(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	if sys := os.Getenv("SystemRoot"); sys != "" {
		t.Setenv("QODER_UMID_EXE", filepath.Join(sys, "System32", "cmd.exe"))
	}

	var mu sync.Mutex
	var order []string // 账号请求顺序（用于验证 priority）

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == qoderPathCampaigns:
			mu.Lock()
			order = append(order, tok)
			mu.Unlock()
			if tok == "tok-a" {
				// A 号今天有可领的每日活动。
				_, _ = w.Write([]byte(`{"campaigns":[{"campaignId":"c1","name":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}}]}`))
				return
			}
			// B 号列表里看不到今天的活动（同一人已被领走）。
			_, _ = w.Write([]byte(`{"campaigns":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/sash/api/v1/me/campaigns/c1/claim":
			if tok == "tok-a" {
				_, _ = w.Write([]byte(`{"status":"CLAIMED","benefit":{"amount":100}}`))
				return
			}
			// B 号去领 A 的实例 → 服务端判定同一人已领。
			_, _ = w.Write([]byte(`{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED","benefit":{"amount":100}}`))
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
		t.Fatal(err)
	}

	out, err := QoderCheckinPool(nil)
	if err != nil {
		t.Fatalf("QoderCheckinPool: %v", err)
	}
	if out.Claimed != 1 || out.Already != 1 || out.Failed != 0 {
		t.Fatalf("应为一个号真领到、另一个号被判定同一人已领（0 失败），实际 %+v", out)
	}
	if out.Credits != 100 {
		t.Errorf("新增积分应为 100（只有 A 号真领到），实际 %v", out.Credits)
	}
	byEmail := map[string]QoderCheckinResult{}
	for _, r := range out.Results {
		byEmail[r.Email] = r
	}
	a := byEmail["a@test.com"]
	if a.Phase != "claimed" || !a.CheckedInToday {
		t.Errorf("A 号应真领到：phase=%s checked=%v", a.Phase, a.CheckedInToday)
	}
	b := byEmail["b@test.com"]
	if b.Phase != "already" || !b.CheckedInToday {
		t.Errorf("B 号应判为已签到（今日已领取），实际 phase=%s checked=%v", b.Phase, b.CheckedInToday)
	}
	if !strings.Contains(b.Msg, "SAME_PERSON_ALREADY_CLAIMED") {
		t.Errorf("B 号的说明应带上服务端判定码，实际 %q", b.Msg)
	}
	if !strings.Contains(b.Msg, "另一账号") {
		t.Errorf("B 号的说明应讲清是同一人另一账号领的，实际 %q", b.Msg)
	}

	// priority：B 号优先打（用户想「今天这 100 给谁就点谁」）
	mu.Lock()
	order = nil
	mu.Unlock()
	if _, err := QoderCheckinPoolOrdered(nil, "b@test.com"); err != nil {
		t.Fatalf("priority run: %v", err)
	}
	mu.Lock()
	first := ""
	if len(order) > 0 {
		first = order[0]
	}
	mu.Unlock()
	if first != "tok-b" {
		t.Errorf("priority 应让指定账号先打，实际首个请求 %q（顺序 %v）", first, order)
	}
}
