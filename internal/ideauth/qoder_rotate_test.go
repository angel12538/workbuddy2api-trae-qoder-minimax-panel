package ideauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestQoderPoolRotatesClaimPriority 钉死两件事：
//
//  1. **自动路径必须轮换"谁先打"**。Qoder 的每日额度服务端按**人**给，一天只有一份，
//     先打的那个号拿到，同一个人的其余号一律被判 SAME_PERSON_ALREADY_CLAIMED
//     （2026-10-24 实测；2026-11-01 换整套伪造设备指纹复测，返回一字不变）。
//     此前自动签到调 QoderCheckinPool(nil) → 顺序 = 号池文件顺序 → 排第一的号
//     天天赢，第二个号一年轮不到一次（Claims/LastClaimedAt 当初就是为轮换存的，
//     但全仓零调用点）。现在：最久没真正领到的号先打。
//  2. **签到记录用上游的 claimedAt 落时间戳**，不再依赖本机时钟（本机钟慢 8 小时
//     时，真领到的那次会被可信线判成旧版误报，面板整天显示"没签上"）。
func TestQoderPoolRotatesClaimPriority(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	if sys := os.Getenv("SystemRoot"); sys != "" {
		t.Setenv("QODER_UMID_EXE", filepath.Join(sys, "System32", "cmd.exe"))
	}

	// 假上游：复刻真实服务端的"按人一份"规则。
	var mu sync.Mutex
	order := []string{}            // 上游被哪个 token 打的**先后**（验轮换顺序）
	ordered := map[string]bool{}   // token -> 是否已记进 order
	claimedBy := map[string]bool{} // token -> 本号是否已领
	personTaken := false           // 同一人今天是否已被某个号领走
	serverClaimedAt := time.Now().UTC().Add(-90 * time.Second).Format(time.RFC3339)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok := r.Header.Get("Authorization")
		mu.Lock()
		if !ordered[tok] {
			ordered[tok] = true
			order = append(order, tok)
		}
		mu.Unlock()
		startAt := time.Now().Add(-2 * time.Hour).Unix()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == qoderPathCampaigns:
			mu.Lock()
			me := claimedBy[tok]
			taken := personTaken
			mu.Unlock()
			if me {
				// 本号领过：今天那条显示 CLAIMED
				_, _ = w.Write([]byte(fmt.Sprintf(
					`{"campaigns":[{"campaignId":"cid-daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","startAt":%d,"benefit":{"amount":100}}]}`,
					startAt)))
				return
			}
			if taken {
				// 真实服务端的行为：**同一人另一号已领走 → 本号列表里根本没有这条活动**
				_, _ = w.Write([]byte(`{"campaigns":[],"claimable":false}`))
				return
			}
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"campaigns":[{"campaignId":"cid-daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","startAt":%d,"benefit":{"amount":100}}]}`, startAt)))
		case r.Method == http.MethodPost &&
			strings.HasPrefix(r.URL.Path, "/sash/api/v1/me/campaigns/") &&
			strings.HasSuffix(r.URL.Path, "/claim"):
			mu.Lock()
			defer mu.Unlock()
			if personTaken {
				_, _ = w.Write([]byte(`{"status":"BLOCKED","replayed":true,"failureCode":"SAME_PERSON_ALREADY_CLAIMED","benefit":{"amount":100}}`))
				return
			}
			personTaken = true
			claimedBy[tok] = true
			_, _ = w.Write([]byte(`{"status":"CLAIMED","claimedAt":"` + serverClaimedAt + `","benefit":{"amount":100}}`))
		case r.Method == http.MethodGet &&
			strings.HasPrefix(r.URL.Path, "/sash/api/v1/me/campaigns/") &&
			strings.HasSuffix(r.URL.Path, "/reward"):
			// 奖励查询（只读）：面板用它问「我是不是同一个人」。
			// 实例主人查 → CLAIMED；同一人查 → BLOCKED + SAME_PERSON（2026-11-02 实测形状）。
			mu.Lock()
			me := claimedBy[tok]
			taken := personTaken
			mu.Unlock()
			if me {
				_, _ = w.Write([]byte(`{"grantId":"grant-` + tok + `","status":"CLAIMED","benefit":{"amount":100}}`))
				return
			}
			if taken {
				_, _ = w.Write([]byte(`{"grantId":"grant-` + tok +
					`","status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED","benefit":{"amount":100}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	pool := &QoderPoolFile{Accounts: []*QoderSnapshot{
		{Email: "a@test.com", Auth: &QoderAuth{Token: "tok-a", User: QoderUser{Email: "a@test.com"}}},
		{Email: "b@test.com", Auth: &QoderAuth{Token: "tok-b", User: QoderUser{Email: "b@test.com"}}},
	}}
	if err := SaveQoderPool(pool); err != nil {
		t.Fatal(err)
	}

	// 第一轮：谁都没领过 → a 先打并拿到，b 被服务端判"同一人已领"。
	out1, err := QoderCheckinPool(nil)
	if err != nil {
		t.Fatal(err)
	}
	if out1.Claimed != 1 || out1.Blocked != 1 {
		t.Fatalf("第一轮应为「1 个领到 + 1 个被同一人挡住」：%+v", out1)
	}
	// 领到的那次必须用**上游**给的时刻落记录（比本机 now 早 37 分钟）。
	at, ok := LastClaimedAt("qoder", "a@test.com")
	if !ok {
		t.Fatal("领到后必须写 Claims 记录（轮换排序靠它）")
	}
	want, _ := time.Parse(time.RFC3339, serverClaimedAt)
	if !at.Equal(want) {
		t.Fatalf("Claims 记录应等于上游 claimedAt：got %s want %s", at, want)
	}
	// 真领到的号即使本机时刻落在可信线之前，也必须算"今日已签"（钟慢 8 小时的场景）。
	if !CheckinRecordTrusted("qoder", "a@test.com") {
		t.Fatal("有今日 claims 记录就该判可信，不能只认 Entries 那条时刻线")
	}

	mu.Lock()
	first1 := ""
	if len(order) > 0 {
		first1 = order[0]
	}
	mu.Unlock()
	if first1 != "Bearer tok-a" {
		t.Fatalf("第一轮 a/b 都没领过，按号池顺序 a 先打（实际先打 %q）", first1)
	}
	// 逐日账本（C-QD-LEDGER@1）：**每次尝试都写** —— 第一轮 a 真领到、b 被服务端
	// 判为同一人（blocked），两种状态都必须能在账本里审计到。必须在第二轮之前查，
	// 因为第二轮 b 领到后会按 (email,date) 把当天的 blocked 覆盖成 claimed。
	if recs := QoderClaimHistory("a@test.com", 1); len(recs) != 1 || recs[0].Phase != QoderPhaseClaimed {
		t.Errorf("a 的 claimed 必须进账本，实际 %+v", recs)
	}
	if recs := QoderClaimHistory("b@test.com", 1); len(recs) != 1 || recs[0].Phase != QoderPhaseBlocked {
		t.Errorf("b 第一轮 blocked 的尝试必须进账本，实际 %+v", recs)
	}
	if recs := QoderClaimHistory("b@test.com", 1); len(recs) == 1 && recs[0].QuotaTakenBy != "a@test.com" {
		t.Errorf("blocked 账本必须点名额度归属，实际 %q", recs[0].QuotaTakenBy)
	}

	// 第二轮：a 今天已真领到、b 没有 → **必须换成 b 先打**。
	// （服务端"一天一份"在这轮里由假上游继续模拟：谁先打谁有机会，b 先打也照样被挡，
	// 但它先打 = 下一天零点一刷新，额度归 b 而不是永远归 a。）
	mu.Lock()
	order = []string{}
	ordered = map[string]bool{}
	claimedBy = map[string]bool{}
	personTaken = false // 模拟新的一天：额度重新放出来
	mu.Unlock()

	out2, err := QoderCheckinPool(nil)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	first2 := ""
	if len(order) > 0 {
		first2 = order[0]
	}
	mu.Unlock()
	if first2 != "Bearer tok-b" {
		t.Fatalf("第二轮必须让最久没领到的 b 先打（实际先打 %q）", first2)
	}
	if out2.Claimed != 1 {
		t.Fatalf("第二轮应该是 b 领到：%+v", out2)
	}
	if _, ok := LastClaimedAt("qoder", "b@test.com"); !ok {
		t.Fatal("b 领到后也该写 Claims 记录")
	}

	// ---- T6 契约补强（C-QD-API@1 / C-QD-LEDGER@1）----------------------
	// ① rotation_order 是本轮实际尝试顺序，rotation_rank 从 1 起；两个号都必须
	//    带 strategy / quota_group，七相计数之和等于实际尝试数。
	if len(out1.RotationOrder) != 2 || out1.RotationOrder[0] != "a@test.com" || out1.RotationOrder[1] != "b@test.com" {
		t.Fatalf("第一轮 rotation_order 应为 [a b]（都没记录时按邮箱稳定排序），实际 %v", out1.RotationOrder)
	}
	if n := out1.Claimed + out1.Already + out1.Blocked + out1.Pending + out1.Failed + out1.LoginRequired + out1.Error; n != len(out1.Results) {
		t.Fatalf("七相计数之和必须等于尝试数：%d != %d（%+v）", n, len(out1.Results), out1)
	}
	if out1.CheckedInToday {
		t.Error("有号被 blocked 时顶层 checked_in_today 必须为 false")
	}
	rankOf := map[string]int{}
	itemOf := map[string]QoderCheckinItem{}
	for _, r := range out1.Results {
		rankOf[r.Email] = r.RotationRank
		itemOf[r.Email] = r
		if r.Strategy == "" {
			t.Errorf("%s 缺少 strategy（契约必填）", r.Email)
		}
		// 第一轮 b 被服务端判定与 a 同人（saveQoderPersonLink），组大小是 2。
		if r.QuotaGroup != 2 {
			t.Errorf("%s 与服务端判定同人后组大小应为 2，实际 %d", r.Email, r.QuotaGroup)
		}
	}
	if rankOf["a@test.com"] != 1 || rankOf["b@test.com"] != 2 {
		t.Errorf("rotation_rank 应为 a=1,b=2，实际 %v", rankOf)
	}
	blocked := itemOf["b@test.com"]
	if blocked.Phase != QoderPhaseBlocked || blocked.Credits != 0 || blocked.BenefitAmount != 100 {
		t.Errorf("blocked 号必须 credits=0 + benefit_amount=100，实际 %+v", blocked)
	}
	if blocked.Strategy != QoderStrategySharedPerson || blocked.QuotaTakenBy != "a@test.com" {
		t.Errorf("blocked 号应标 shared_person 并点名额度归属，实际 strategy=%q taken_by=%q", blocked.Strategy, blocked.QuotaTakenBy)
	}
	if blocked.CheckedInToday {
		t.Error("blocked 号 checked_in_today 必须为 false")
	}

	// ③ 第二轮：A 上次（第一轮）真领到、B 上次没领到 → B 必须先打。
	if len(out2.RotationOrder) == 0 || out2.RotationOrder[0] != "b@test.com" {
		t.Fatalf("第二轮必须让「上次没领到」的 b 先打，实际 rotation_order=%v", out2.RotationOrder)
	}
	second := map[string]QoderCheckinItem{}
	for _, r := range out2.Results {
		second[r.Email] = r
	}
	if second["b@test.com"].Phase != QoderPhaseClaimed || !second["b@test.com"].CheckedInToday {
		t.Errorf("第二轮 b 应先打并领到，实际 %+v", second["b@test.com"])
	}
	if second["a@test.com"].Phase != QoderPhaseBlocked || second["a@test.com"].CheckedInToday {
		t.Errorf("第二轮 a 后打被同人挡住，必须 blocked 且未签到，实际 %+v", second["a@test.com"])
	}
	if out2.CheckedInToday {
		t.Error("第二轮有 blocked，顶层 checked_in_today 必须为 false")
	}
}

// TestQoderRotationOrderUsesLedger 验收点③的独立回归：A **昨天**领到过、B 上次
// 没领到（blocked/pending）→ 今天 B 必须先打（rotation_order[0] == B）；显式
// priority 仍然能覆盖轮换（用户指定今天这份给谁）。
//
// 只测纯排序函数，不打上游：轮换依据必须来自逐日账本（C-QD-LEDGER@1），
// 而不是号池文件顺序 —— 否则排第一的号天天赢，同组的另一个号永远轮不到。
func TestQoderRotationOrderUsesLedger(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	now := time.Now().In(qoderCST)
	yesterday := now.AddDate(0, 0, -1)
	if err := AppendQoderClaim(QoderClaimRecord{
		Email: "a@test.com", Phase: QoderPhaseClaimed, Credits: 100,
		Date: yesterday.Format("2006-01-02"), At: yesterday.Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := AppendQoderClaim(QoderClaimRecord{
		Email: "b@test.com", Phase: QoderPhaseBlocked, QuotaTakenBy: "a@test.com",
		Date: yesterday.Format("2006-01-02"), At: yesterday.Add(time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	items := []qoderPoolItem{{email: "a@test.com"}, {email: "b@test.com"}}
	order := qoderRotationOrder(items, "")
	if len(order) != 2 || order[0] != "b@test.com" {
		t.Fatalf("上次没领到的 b 今天必须先打（rotation_order[0]=b），实际 %v", order)
	}
	// 显式 priority 覆盖轮换：今天这份给 a，就把 a 提到最前。
	pri := qoderRotationOrder(items, "a@test.com")
	if len(pri) != 2 || pri[0] != "a@test.com" {
		t.Fatalf("priority 应覆盖轮换顺序，实际 %v", pri)
	}
	// 从没尝试过的号与「上次没领到」同等优先（missed=true）。
	items2 := []qoderPoolItem{{email: "a@test.com"}, {email: "c@test.com"}}
	order2 := qoderRotationOrder(items2, "")
	if len(order2) != 2 || order2[0] != "c@test.com" {
		t.Fatalf("从没尝试过的 c 应优先于上次已领的 a，实际 %v", order2)
	}

	// already（本号今日已入账）同样不能算「上次没领到」：c 昨天 already、d 昨天
	// blocked/pending → d 先。若把 already 当 missed，c 的 lastTry 更早会错误地排第一。
	if err := AppendQoderClaim(QoderClaimRecord{
		Email: "c@test.com", Phase: QoderPhaseAlready, Credits: 100,
		Date: yesterday.Format("2006-01-02"), At: yesterday.Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := AppendQoderClaim(QoderClaimRecord{
		Email: "d@test.com", Phase: QoderPhasePending,
		Date: yesterday.Format("2006-01-02"), At: yesterday.Add(2 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	order4 := qoderRotationOrder([]qoderPoolItem{{email: "c@test.com"}, {email: "d@test.com"}}, "")
	if len(order4) != 2 || order4[0] != "d@test.com" {
		t.Fatalf("already 不是「没领到」，d 应排在 c 前面，实际 %v", order4)
	}
}

// TestQoderStrategySnapshot strategy 是**单一来源**：POST checkin 与 GET
// checkin-status 都调用 qoderStrategySnapshot。这里逐条钉死规则，
// 避免两端各写一套（D-Q1：签到侧 unassigned、状态侧 shared_person）。
func TestQoderStrategySnapshot(t *testing.T) {
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	t.Setenv("QODER_PROBE_FILE", "")

	const (
		realEmail    = "real@test.com"
		derivedEmail = "derived@test.com"
		proxyEmail   = "proxy@test.com"
	)
	if _, err := SetQoderEgress(derivedEmail, QoderDeviceModeDerived, "", ""); err != nil {
		t.Fatalf("SetQoderEgress derived: %v", err)
	}
	if _, err := SetQoderEgress(proxyEmail, QoderDeviceModeReal, "http://127.0.0.1:18080", ""); err != nil {
		t.Fatalf("SetQoderEgress proxy: %v", err)
	}

	cases := []struct {
		name       string
		email      string
		phase      string
		takenBy    string
		group      int
		verdict    string
		conclusion string
		want       string
	}{
		// blocked 一律 shared_person（服务端判定同人已领）。
		{"blocked", realEmail, QoderPhaseBlocked, "a@test.com", 2, QoderVerdictSamePerson, "", QoderStrategySharedPerson},
		// pending + 同组已有持有者 → shared_person。
		{"pending_holder", realEmail, QoderPhasePending, "a@test.com", 2, QoderVerdictUnknown, "", QoderStrategySharedPerson},
		// pending + 明确非同人 → unassigned。
		{"pending_not_same", realEmail, QoderPhasePending, "", 1, QoderVerdictOther, "", QoderStrategyUnassigned},
		// pending + 证据不足 → unknown（不得当成同人，也不得当成非同人）。
		{"pending_unknown", realEmail, QoderPhasePending, "", 2, QoderVerdictUnknown, "", QoderStrategyUnknown},
		{"pending_empty_verdict", realEmail, QoderPhasePending, "", 1, "", "", QoderStrategyUnknown},
		// claimed/already：本号自己的 grantId（verdict=owner）⇒ 独占一份 → isolated。
		// 2026-11-04 实测更正：旧实现一律回 shared_person（「服务端判定同人」），
		// 但服务端根本没判过同人，两号 grantId 不同、各领各的 100。
		{"claimed_real", realEmail, QoderPhaseClaimed, "", 1, QoderVerdictOwner, "", QoderStrategyIsolated},
		{"already_real", realEmail, QoderPhaseAlready, "", 1, QoderVerdictOwner, "", QoderStrategyIsolated},
		// 没有归属结论（没探测到）→ 归属未核实，绝不默认说「服务端判定同人」。
		{"claimable_real", realEmail, "claimable", "", 1, "", "", QoderStrategyUnknown},
		// 有独立出口但探测未证明隔离可用 → 仍按归属未核实（不假设隔离）。
		{"derived_no_probe", derivedEmail, QoderPhaseClaimed, "", 1, QoderVerdictOwner, "inconclusive", QoderStrategyIsolated},
		// 有独立出口 + probe=isolated_viable → isolated。
		{"derived_isolated", derivedEmail, QoderPhaseClaimed, "", 1, QoderVerdictOwner, "isolated_viable", QoderStrategyIsolated},
		{"proxy_isolated", proxyEmail, QoderPhaseAlready, "", 1, QoderVerdictOwner, "isolated_viable", QoderStrategyIsolated},
		// 已被判进同人组 → 绝不可能是 isolated。
		{"derived_but_group", derivedEmail, QoderPhaseClaimed, "", 2, QoderVerdictOwner, "isolated_viable", QoderStrategySharedPerson},
		// 额度明确归属同组另一个号 → shared_person（唯一真证据）。
		{"claimed_taken_by_peer", realEmail, QoderPhaseClaimed, "a@test.com", 1, QoderVerdictOwner, "", QoderStrategySharedPerson},
		// 登录失效 / 本地错误 → unknown。
		{"login_required", realEmail, QoderPhaseLoginRequired, "", 1, "", "", QoderStrategyUnknown},
		{"error", realEmail, QoderPhaseError, "", 1, "", "", QoderStrategyUnknown},
		{"failed", realEmail, QoderPhaseFailed, "", 1, "", "", QoderStrategyUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := qoderStrategySnapshot(c.email, c.phase, c.takenBy, c.group, c.verdict, c.conclusion)
			if got != c.want {
				t.Fatalf("strategy 应为 %q，实际 %q（phase=%s taken_by=%q group=%d verdict=%q probe=%q）",
					c.want, got, c.phase, c.takenBy, c.group, c.verdict, c.conclusion)
			}
		})
	}
}
