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
		case r.Method == http.MethodGet && r.URL.Path == "/sash/api/v1/me/campaigns/c1/reward":
			// 奖励查询（只读）：实例主人查 → CLAIMED；同一人查 → BLOCKED。
			// 2026-11-02 实测上游就是这个形状（服务端还会给同一人单独建一条 grant）。
			if tok == "tok-a" {
				_, _ = w.Write([]byte(`{"grantId":"grant-a","status":"CLAIMED","campaignId":"c1","benefit":{"amount":100}}`))
				return
			}
			_, _ = w.Write([]byte(`{"grantId":"grant-b","status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED","campaignId":"c1","benefit":{"amount":100}}`))
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
	if out.Claimed != 1 || out.Already != 0 || out.Blocked != 1 || out.Failed != 0 {
		t.Fatalf("应为一个号真领到、另一个号被判定同一人已领走（单列 blocked），实际 %+v", out)
	}
	if out.Credits != 100 {
		t.Errorf("新增积分应为 100（只有 A 号真领到），实际 %v", out.Credits)
	}
	byEmail := map[string]QoderCheckinItem{}
	for _, r := range out.Results {
		byEmail[r.Email] = r
	}
	a := byEmail["a@test.com"]
	if a.Phase != "claimed" || !a.CheckedInToday {
		t.Errorf("A 号应真领到：phase=%s checked=%v", a.Phase, a.CheckedInToday)
	}
	b := byEmail["b@test.com"]
	// ⚠️ 关键断言：B 号**没签到**（服务端 BLOCKED）。
	//    绝不能把它标成"已签到"（那是假签到），也不能写进本地签到记录。
	if b.Phase != "blocked" {
		t.Errorf("B 号应为 blocked（今天没签上），实际 phase=%s msg=%q", b.Phase, b.Msg)
	}
	if b.CheckedInToday {
		t.Error("B 号没领到积分，checked_in_today 必须是 false（否则面板会显示成已签到）")
	}
	if !strings.Contains(b.Msg, "SAME_PERSON_ALREADY_CLAIMED") {
		t.Errorf("B 号的说明应带上服务端判定码，实际 %q", b.Msg)
	}
	if strings.Contains(b.Msg, "已领取") {
		t.Errorf("B 号的消息不能说「已领取」—— 它今天根本没领到，实际 %q", b.Msg)
	}
	// 本地签到记录里**不能**有 B 号（否则明天也判成已签到）
	if CheckedInToday("qoder", "b@test.com") {
		t.Error("B 号被服务端拒绝后不得写入本地签到记录（假签到）")
	}
	if !CheckedInToday("qoder", "a@test.com") {
		t.Error("A 号真领到了，本地签到记录应存在")
	}

	// ---- T6 契约补强（C-QD-API@1 / C-QD-LEDGER@1）----------------------
	// ① blocked 是「没领到」：credits 必须 0、活动面值单列 benefit_amount、
	//    strategy=shared_person、quota_taken_by 点名；顶层 checked_in_today=false。
	if b.Credits != 0 {
		t.Errorf("blocked 号的 credits 必须为 0（不能把面值当入账），实际 %v", b.Credits)
	}
	if b.BenefitAmount != 100 {
		t.Errorf("blocked 号的活动面值应放在 benefit_amount=100，实际 %v", b.BenefitAmount)
	}
	if b.Strategy != QoderStrategySharedPerson {
		t.Errorf("blocked 号策略应为 shared_person，实际 %q", b.Strategy)
	}
	if b.QuotaTakenBy != "a@test.com" {
		t.Errorf("blocked 号必须点名额度归属，实际 %q", b.QuotaTakenBy)
	}
	if b.RotationRank <= 0 {
		t.Errorf("每个结果都必须有 rotation_rank（1 起），实际 %d", b.RotationRank)
	}
	if b.QuotaGroup != 2 {
		t.Errorf("A/B 被服务端判定同人后组大小应为 2，实际 %d", b.QuotaGroup)
	}
	if out.CheckedInToday {
		t.Error("存在 blocked 号时顶层 checked_in_today 必须为 false（不能虚报已签到）")
	}
	if n := out.Claimed + out.Already + out.Blocked + out.Pending + out.Failed + out.LoginRequired + out.Error; n != len(out.Results) {
		t.Errorf("七相计数之和必须等于尝试数：%d != %d", n, len(out.Results))
	}
	// ② 账本必须能审计「某号某天被同人阻塞」这件事。
	if recs := QoderClaimHistory("b@test.com", 1); len(recs) != 1 || recs[0].Phase != QoderPhaseBlocked {
		t.Errorf("B 的 blocked 必须写进逐日账本，实际 %+v", recs)
	} else if recs[0].QuotaTakenBy != "a@test.com" || recs[0].Credits != 0 {
		t.Errorf("账本里的 blocked 记录应 credits=0 且点名额度归属，实际 %+v", recs[0])
	}

	// priority：B 号优先打（用户想「今天这 100 给谁就点谁」）
	mu.Lock()
	order = nil
	mu.Unlock()
	priorityOut, err := QoderCheckinPoolOrdered(nil, "b@test.com")
	if err != nil {
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
	if len(priorityOut.RotationOrder) == 0 || priorityOut.RotationOrder[0] != "b@test.com" {
		t.Errorf("priority 指定的账号必须在 rotation_order[0]，实际 %v", priorityOut.RotationOrder)
	}
}

// qoderPersonLinkFixture 构造「A 已经领到、B 今天没活动」的两号场景，并预置一条
// 历史同人关系（模拟 qoder_accounts.json.person.json 里过期的判定）。
//
// rewardOther 决定 B 用 A 的活动实例去只读查询时服务端怎么答：
//   - true  → 404 GRANT_NOT_FOUND（明确「不是同一个人」）
//   - false → 500（证据不足，无法判定）
func qoderPersonLinkFixture(t *testing.T, rewardOther bool) (*httptest.Server, *QoderPoolFile) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == qoderPathCampaigns:
			if tok == "tok-a" {
				_, _ = w.Write([]byte(`{"campaigns":[{"campaignId":"c1","name":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"campaigns":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/sash/api/v1/me/campaigns/c1/claim":
			_, _ = w.Write([]byte(`{"status":"CLAIMED","grantId":"grant-a","benefit":{"amount":100}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/sash/api/v1/me/campaigns/c1/reward":
			if rewardOther && tok != "tok-a" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"errorCode":"GRANT_NOT_FOUND"}`))
				return
			}
			if !rewardOther {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"errorCode":"INTERNAL"}`))
				return
			}
			_, _ = w.Write([]byte(`{"grantId":"grant-a","status":"CLAIMED","campaignId":"c1","benefit":{"amount":100}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	pool := &QoderPoolFile{Accounts: []*QoderSnapshot{
		{Email: "a@test.com", UserID: "ua", Name: "A", Auth: &QoderAuth{Token: "tok-a", RefreshToken: "rt-a", User: QoderUser{Email: "a@test.com", ID: "ua", Name: "A"}}},
		{Email: "b@test.com", UserID: "ub", Name: "B", Auth: &QoderAuth{Token: "tok-b", RefreshToken: "rt-b", User: QoderUser{Email: "b@test.com", ID: "ub", Name: "B"}}},
	}}
	if err := SaveQoderPool(pool); err != nil {
		t.Fatal(err)
	}
	// 预置「过期的同人判定」：两个号曾经被服务端判为同一个人。
	if err := saveQoderPersonLink("a@test.com", "b@test.com"); err != nil {
		t.Fatal(err)
	}
	return server, pool
}

// TestQoderPersonLinkClearedWhenProbeSaysOther 钉死 D-Q1 的修复：
// 历史同人 link 只有在**探测明确 other** 时才撤销；撤销后 pending 号的
// strategy 必须是 unassigned（而不是继续依据历史 link 说 shared_person），
// quota_taken_by 必须是空（不能一边说「额度被 qq 领走」一边说「不是同一个人」）。
func TestQoderPersonLinkClearedWhenProbeSaysOther(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	t.Setenv("QODER_PROBE_FILE", "")
	if sys := os.Getenv("SystemRoot"); sys != "" {
		t.Setenv("QODER_UMID_EXE", filepath.Join(sys, "System32", "cmd.exe"))
	}

	srv, _ := qoderPersonLinkFixture(t, true)
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	out, err := QoderCheckinPool(nil)
	if err != nil {
		t.Fatalf("QoderCheckinPool: %v", err)
	}
	byEmail := map[string]QoderCheckinItem{}
	for _, r := range out.Results {
		byEmail[r.Email] = r
	}
	b := byEmail["b@test.com"]
	if b.Phase != QoderPhasePending {
		t.Fatalf("B 号今天没有活动实例，应为 pending，实际 %+v", b)
	}
	if b.Strategy != QoderStrategyUnassigned {
		t.Errorf("探测明确 other 后 strategy 必须是 unassigned，实际 %q（msg=%q）", b.Strategy, b.Msg)
	}
	if b.QuotaTakenBy != "" {
		t.Errorf("不是同一个人时 quota_taken_by 必须为空，实际 %q", b.QuotaTakenBy)
	}
	if b.CheckedInToday {
		t.Error("pending 号 checked_in_today 必须为 false")
	}
	if b.QuotaGroup != 1 {
		t.Errorf("反证清除 link 后组大小应回到 1，实际 %d", b.QuotaGroup)
	}
	if strings.Contains(b.Msg, "同一个人") {
		t.Errorf("unassigned 的文案不得出现「同一个人」这类肯定句，实际 %q", b.Msg)
	}
	links := loadQoderPersonLinks()
	if links["a@test.com"] == "b@test.com" || links["b@test.com"] == "a@test.com" {
		t.Errorf("明确 other 后必须撤销历史同人 link，实际 %v", links)
	}
	if peers := QoderSamePersonPeers("b@test.com"); len(peers) != 0 {
		t.Errorf("撤销后 B 不应再有同人 peers，实际 %v", peers)
	}
}

// TestQoderPersonLinkKeptWhenProbeUnknown 证据不足（上游 5xx / 无法解析）时
// **不得**清除历史 link：一次网络故障不是「不是同一个人」的反证。
func TestQoderPersonLinkKeptWhenProbeUnknown(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	t.Setenv("QODER_PROBE_FILE", "")
	if sys := os.Getenv("SystemRoot"); sys != "" {
		t.Setenv("QODER_UMID_EXE", filepath.Join(sys, "System32", "cmd.exe"))
	}

	srv, _ := qoderPersonLinkFixture(t, false)
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	out, err := QoderCheckinPool(nil)
	if err != nil {
		t.Fatalf("QoderCheckinPool: %v", err)
	}
	byEmail := map[string]QoderCheckinItem{}
	for _, r := range out.Results {
		byEmail[r.Email] = r
	}
	b := byEmail["b@test.com"]
	if b.Phase != QoderPhasePending {
		t.Fatalf("B 号应为 pending，实际 %+v", b)
	}
	// 探测证据不足（5xx）→ 不清 link；此时「同组里今天已有持有者」这条规则生效
	// （与 GET checkin-status 的 QuotaHolderInGroup 同口径）→ shared_person。
	if b.Strategy != QoderStrategySharedPerson {
		t.Errorf("未知证据下保留同人 link，strategy 应为 shared_person，实际 %q", b.Strategy)
	}
	if b.QuotaTakenBy != "a@test.com" {
		t.Errorf("未知证据下应点名同组持有者 a@test.com，实际 %q", b.QuotaTakenBy)
	}
	if b.QuotaGroup != 2 {
		t.Errorf("unknown 不得清除历史 link，组大小应保持 2，实际 %d", b.QuotaGroup)
	}
	links := loadQoderPersonLinks()
	if links["a@test.com"] != "b@test.com" || links["b@test.com"] != "a@test.com" {
		t.Errorf("unknown 必须保留历史同人 link，实际 %v", links)
	}
	if peers := QoderSamePersonPeers("b@test.com"); len(peers) != 1 {
		t.Errorf("unknown 后 B 应仍有 1 个同人 peer，实际 %v", peers)
	}
}

// TestQoderForgetPersonLinkRemovesOnlyContradictedPair 直接测清除路径：
//   - 只删指定的一对（A~B 删掉后，B~C 仍然保留）；
//   - 不存在的 pair 不写盘；
//   - 重复调用幂等。
//
// 注意关系表是 map[email]peer 的单值结构（既有格式，未在本次改动中扩容），
// 所以「B 同时与 A、C 同人」时 B 的记录只保留最后一次写入的 C；删 A~B 只删
// A 的条目（B 指向 C，不属于 A~B 这对）。
func TestQoderForgetPersonLinkRemovesOnlyContradictedPair(t *testing.T) {
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	if err := saveQoderPersonLink("a@x.com", "b@x.com"); err != nil {
		t.Fatal(err)
	}
	if err := saveQoderPersonLink("b@x.com", "c@x.com"); err != nil {
		t.Fatal(err)
	}
	if err := QoderForgetPersonLink("a@x.com", "b@x.com"); err != nil {
		t.Fatalf("QoderForgetPersonLink: %v", err)
	}
	links := loadQoderPersonLinks()
	if _, ok := links["a@x.com"]; ok {
		t.Errorf("A 的同人关系应被删除，实际 %v", links)
	}
	if links["b@x.com"] != "c@x.com" {
		t.Errorf("B 应只保留与 C 的关系（B~C 不受影响），实际 %v", links)
	}
	if links["c@x.com"] != "b@x.com" {
		t.Errorf("B~C 关系不应受影响，实际 %v", links)
	}
	// 幂等：再删一次不报错，文件内容不变。
	before := loadQoderPersonLinks()
	if err := QoderForgetPersonLink("a@x.com", "b@x.com"); err != nil {
		t.Fatalf("重复清除应幂等: %v", err)
	}
	after := loadQoderPersonLinks()
	if len(before) != len(after) {
		t.Errorf("重复清除不应改动关系表：before=%v after=%v", before, after)
	}
}
