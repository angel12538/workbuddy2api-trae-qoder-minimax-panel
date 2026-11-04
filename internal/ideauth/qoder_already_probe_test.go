package ideauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// qoderAlreadyProbeEnv 起一个假上游 + 一个两号号池，专门演「今天这条已领活动」。
//
// bBlocked=true  → B 被服务端判同人（B 的 reward 回 BLOCKED/SAME_PERSON）；
// bBlocked=false → B 有**自己的** grantId（各领各的，实测 2026-11-04 的真实形态）。
func qoderAlreadyProbeEnv(t *testing.T, bBlocked bool) {
	t.Helper()
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	if sys := os.Getenv("SystemRoot"); sys != "" {
		t.Setenv("QODER_UMID_EXE", filepath.Join(sys, "System32", "cmd.exe"))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok := r.Header.Get("Authorization")
		startAt := time.Now().Add(-2 * time.Hour).Unix()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == qoderPathCampaigns:
			// 两个号看到**同一条**已领活动（同人组共享活动列表）。
			fmt.Fprintf(w, `{"campaigns":[{"campaignId":"cid-daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","startAt":%d,"benefit":{"amount":100}}]}`, startAt)
		case r.Method == http.MethodGet && r.URL.Path == "/sash/api/v1/me/campaigns/cid-daily/reward":
			switch tok {
			case "Bearer tok-a":
				_, _ = w.Write([]byte(`{"grantId":"grant-a","status":"CLAIMED","campaignId":"cid-daily","benefit":{"amount":100}}`))
			case "Bearer tok-b":
				if bBlocked {
					_, _ = w.Write([]byte(`{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED"}`))
					return
				}
				_, _ = w.Write([]byte(`{"grantId":"grant-b","status":"CLAIMED","campaignId":"cid-daily","benefit":{"amount":100}}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	pool := &QoderPoolFile{Accounts: []*QoderSnapshot{
		{Email: "a@test.com", UserID: "ua", Name: "A", Auth: &QoderAuth{Token: "tok-a", User: QoderUser{Email: "a@test.com", ID: "ua", Name: "A"}}},
		{Email: "b@test.com", UserID: "ub", Name: "B", Auth: &QoderAuth{Token: "tok-b", User: QoderUser{Email: "b@test.com", ID: "ub", Name: "B"}}},
	}}
	if err := SaveQoderPool(pool); err != nil {
		t.Fatal(err)
	}
}

// TestQoderAlreadyOwnershipProbeOwnGrants 「各领各的」：两个号看到同一条已领活动，
// 但 grantId 不同 —— 各自都真的入账了一份额度。
//
// 实测依据（2026-11-04 11:28，GET .../campaigns/{cid}/reward 原始响应）：
//
//	qq     grantId=01a1a477-96d3-…  claimedAt=2026-11-04T02:51:03Z
//	gmail  grantId=01a1a463-e178-…  claimedAt=2026-11-04T02:29:31Z
//
// ⇒ 两个号都必须显示「今日已签到 +100」，strategy=isolated（额度独立），
// 绝不能因为看到同一条 campaign 就把其中一个说成「同一人已领」。
func TestQoderAlreadyOwnershipProbeOwnGrants(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	qoderAlreadyProbeEnv(t, false)

	out, err := QoderCheckinPool(nil)
	if err != nil {
		t.Fatalf("QoderCheckinPool: %v", err)
	}
	by := map[string]QoderCheckinItem{}
	for _, r := range out.Results {
		by[r.Email] = r
	}
	for _, email := range []string{"a@test.com", "b@test.com"} {
		r, ok := by[email]
		if !ok {
			t.Fatalf("%s 应有结果：%+v", email, out.Results)
		}
		if r.Phase != QoderPhaseAlready {
			t.Errorf("%s 应为 already，实际 %+v", email, r)
		}
		if r.Credits != 100 {
			t.Errorf("%s credits 应为 100（自己领的），实际 %v", email, r.Credits)
		}
		if r.QuotaTakenBy != "" {
			t.Errorf("%s 不该有额度归属（各领各的），实际 %q", email, r.QuotaTakenBy)
		}
		if r.Strategy != QoderStrategyIsolated {
			t.Errorf("%s 策略应为 isolated（本号自己的 grantId），实际 %q", email, r.Strategy)
		}
		if !r.CheckedInToday {
			t.Errorf("%s 自己领到了，checked_in_today 应为 true", email)
		}
	}
	if out.AlreadyCredits != 200 {
		t.Errorf("两号各入账 100，already 合计应为 200，实际 %v", out.AlreadyCredits)
	}
	if !out.CheckedInToday {
		t.Error("两号都真领到了，顶层 checked_in_today 应为 true")
	}
}

// TestQoderAlreadyOwnershipProbeSamePerson 服务端明确判同人（B 被拒）时：
// B 的 credits 必须归零、点名 A、checked_in_today=false；A 归入同人组
// ⇒ strategy=shared_person。
func TestQoderAlreadyOwnershipProbeSamePerson(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	qoderAlreadyProbeEnv(t, true)

	out, err := QoderCheckinPool(nil)
	if err != nil {
		t.Fatalf("QoderCheckinPool: %v", err)
	}
	by := map[string]QoderCheckinItem{}
	for _, r := range out.Results {
		by[r.Email] = r
	}
	a, okA := by["a@test.com"]
	b, okB := by["b@test.com"]
	if !okA || !okB {
		t.Fatalf("两个号都应有结果：%+v", out.Results)
	}

	// ① B：服务端判同人 ⇒ 没领到，积分归零 + 点名 + 不是「今日已签到」。
	if b.Phase != QoderPhaseAlready {
		t.Fatalf("B 号应为 already（上游显示同一条已领活动），实际 %+v", b)
	}
	if b.Credits != 0 {
		t.Errorf("B 号 credits 必须归零（额度在 A 账上），实际 %v", b.Credits)
	}
	if b.BenefitAmount != 100 {
		t.Errorf("B 号面值应单列 benefit_amount=100，实际 %v", b.BenefitAmount)
	}
	if b.QuotaTakenBy != "a@test.com" {
		t.Errorf("B 号必须点名额度归属 a@test.com，实际 %q", b.QuotaTakenBy)
	}
	if b.CheckedInToday {
		t.Error("B 号被服务端判同人，没签到，checked_in_today 必须为 false")
	}
	if b.Strategy != QoderStrategySharedPerson {
		t.Errorf("B 号策略应为 shared_person（服务端判定同人），实际 %q", b.Strategy)
	}

	// ② A：被服务端判进同人组 ⇒ 不是独立额度，strategy=shared_person。
	if a.Credits != 100 {
		t.Errorf("A 号 credits 应为 100（它自己领的），实际 %v", a.Credits)
	}
	if a.Strategy != QoderStrategySharedPerson {
		t.Errorf("A 号已进同人组，策略应为 shared_person，实际 %q", a.Strategy)
	}

	// ③ 顶层 checked_in_today：B 没签上 ⇒ 整轮不得报「今日已签到」。
	if out.CheckedInToday {
		t.Error("整轮存在没签到的号时，顶层 checked_in_today 必须为 false")
	}
	// ④ 汇总里的入账积分只算 A 的 100（B 的已归零，不得虚记）。
	if out.AlreadyCredits != 100 {
		t.Errorf("already 入账合计应为 100（A 自己的），实际 %v", out.AlreadyCredits)
	}
}
