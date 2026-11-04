package panel

import (
	"encoding/json"
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

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// Qoder 多账号签到 UI 回归测试。
//
// 背景：签到池曾只渲染「当前登录」一个 Qoder 账号，号池里第二个号永远没有签到入口。
// 这里锁死三件事，防止以后改回去：
//  1. 签到池按号池全量渲染（每行一个签到按钮，data-ck-qoder 带 email）；
//  2. 存在「全部签到」按钮，且调用时不带 email（= 号池全部）；
//  3. 每行额度按账号查询（usage?email=），不再只查当前号。
func TestQoderCheckinUIListsAllAccounts(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	body := grabJSFunc(t, src, "loadCkQoder")
	if !strings.Contains(body, "rows.map(") {
		t.Error("loadCkQoder 应按号池全量渲染（rows.map）")
	}
	// 属性名由 UI.act(...) 生成（渲染结果仍是 data-ck-qoder="…"），
	// 所以这里断言属性名本身，而不是字面量写法。
	if !strings.Contains(body, "data-ck-qoder") {
		t.Error("每行应有独立签到按钮（data-ck-qoder 带账号）")
	}
	if !strings.Contains(body, "usage?email=") {
		t.Error("积分应按账号查询（usage?email=）")
	}
	if strings.Contains(body, `$('ckQoderSummary').textContent = cur ?`) {
		t.Error("汇总文案又退回成「只显示当前账号」了")
	}

	// 签到函数必须支持按账号，且默认=全部
	ck := grabJSFunc(t, src, "doQoderCheckin")
	if !strings.Contains(ck, "emails") {
		t.Error("doQoderCheckin 应支持传 emails（按账号/全部）")
	}

	// 全部签到按钮
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="btnCkQoderCheckinAll"`) {
		t.Error("index.html 缺少「全部签到」按钮")
	}

	// 积分总览也应列全部账号
	qk := grabJSFunc(t, src, "loadQkCredits")
	if !strings.Contains(qk, "rows.map(") || !strings.Contains(qk, "usage?email=") {
		t.Error("积分总览应列全部 Qoder 账号并逐号查额度")
	}
}

// qoderTestPanel 构造一个只用于 handler 直调的面板（不注册路由）。
//
// IDEAUTH_POOL_DIR / QODER_DATA_DIR 全部指向临时目录：绝不读写真实
// D:\WorkBuddy\data，也不会去解真实客户端的 auth.v1.dat。
func qoderTestPanel(t *testing.T) *Panel {
	t.Helper()
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	t.Setenv("QODER_DATA_DIR", t.TempDir())
	t.Setenv("QODER_PROBE_FILE", "")
	if sys := os.Getenv("SystemRoot"); sys != "" {
		t.Setenv("QODER_UMID_EXE", filepath.Join(sys, "System32", "cmd.exe"))
	}
	return &Panel{}
}

// qoderFakeUpstream 复刻 C-QD-API@1 验收用的两号场景：
// A 号有可领活动并领到，B 号看不到活动、被服务端判定「同一个人已领」。
func qoderFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	personTaken := false
	claimedBy := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		startAt := time.Now().Add(-2 * time.Hour).Unix()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sash/api/v1/me/campaigns":
			mu.Lock()
			me, taken := claimedBy[tok], personTaken
			mu.Unlock()
			if me {
				fmt.Fprintf(w, `{"campaigns":[{"campaignId":"cid-daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","startAt":%d,"benefit":{"amount":100}}]}`, startAt)
				return
			}
			if taken || tok == "tok-b" {
				// 同一人另一号已领走 → 本号列表里根本没有这条活动。
				_, _ = w.Write([]byte(`{"campaigns":[]}`))
				return
			}
			fmt.Fprintf(w, `{"campaigns":[{"campaignId":"cid-daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","startAt":%d,"benefit":{"amount":100}}]}`, startAt)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			mu.Lock()
			defer mu.Unlock()
			if personTaken {
				_, _ = w.Write([]byte(`{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED","benefit":{"amount":100}}`))
				return
			}
			personTaken = true
			claimedBy[tok] = true
			_, _ = w.Write([]byte(`{"status":"CLAIMED","claimedAt":"` + time.Now().UTC().Format(time.RFC3339) + `","benefit":{"amount":100}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reward"):
			mu.Lock()
			me, taken := claimedBy[tok], personTaken
			mu.Unlock()
			if me {
				_, _ = w.Write([]byte(`{"grantId":"grant-a","status":"CLAIMED","campaignId":"cid-daily","benefit":{"amount":100}}`))
				return
			}
			if taken {
				_, _ = w.Write([]byte(`{"grantId":"grant-b","status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED","campaignId":"cid-daily","benefit":{"amount":100}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

// TestQoderCheckinResponseContract 验收点①/②：两号 fixture（A claimed、B blocked）
// 的七相计数、rotation_order、quota_groups、results 字段与 checked_in_today 口径。
func TestQoderCheckinResponseContract(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	p := qoderTestPanel(t)
	srv := qoderFakeUpstream(t)
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	pool := &ideauth.QoderPoolFile{Accounts: []*ideauth.QoderSnapshot{
		{Email: "a@test.com", UserID: "ua", Name: "A", Auth: &ideauth.QoderAuth{Token: "tok-a", User: ideauth.QoderUser{Email: "a@test.com", ID: "ua", Name: "A"}}},
		{Email: "b@test.com", UserID: "ub", Name: "B", Auth: &ideauth.QoderAuth{Token: "tok-b", User: ideauth.QoderUser{Email: "b@test.com", ID: "ub", Name: "B"}}},
	}}
	if err := ideauth.SaveQoderPool(pool); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	p.qoderCheckin(rec, httptest.NewRequest(http.MethodPost, "/panel/api/ide/qoder/checkin", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin 应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		OK             bool                          `json:"ok"`
		Results        []ideauth.QoderCheckinItem    `json:"results"`
		Claimed        int                           `json:"claimed_accounts"`
		Already        int                           `json:"already_accounts"`
		Blocked        int                           `json:"blocked_accounts"`
		Pending        int                           `json:"pending_accounts"`
		Failed         int                           `json:"failed_accounts"`
		LoginRequired  int                           `json:"login_required_accounts"`
		Error          int                           `json:"error_accounts"`
		Credits        float64                       `json:"credits"`
		RotationOrder  []string                      `json:"rotation_order"`
		QuotaGroups    []ideauth.QoderQuotaGroupView `json:"quota_groups"`
		CheckedInToday bool                          `json:"checked_in_today"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !resp.OK {
		t.Error("ok 应为 true")
	}
	// 验收①：A claimed、B blocked → claimed=1, blocked=1, already=0。
	if resp.Claimed != 1 || resp.Blocked != 1 || resp.Already != 0 {
		t.Fatalf("两号 fixture 应为 claimed=1/blocked=1/already=0，实际 claimed=%d blocked=%d already=%d", resp.Claimed, resp.Blocked, resp.Already)
	}
	if n := resp.Claimed + resp.Already + resp.Blocked + resp.Pending + resp.Failed + resp.LoginRequired + resp.Error; n != len(resp.Results) {
		t.Fatalf("七相计数之和必须等于尝试数：%d != %d", n, len(resp.Results))
	}
	if resp.CheckedInToday {
		t.Error("存在 blocked 号时顶层 checked_in_today 必须为 false")
	}
	if len(resp.RotationOrder) != 2 {
		t.Fatalf("rotation_order 应含两个账号，实际 %v", resp.RotationOrder)
	}
	// results[] 按 email 升序返回（稳定展示）；实际尝试顺序看 rotation_order /
	// rotation_rank，两者必须自洽。
	for i := 1; i < len(resp.Results); i++ {
		if resp.Results[i-1].Email > resp.Results[i].Email {
			t.Errorf("results 应按 email 升序，实际 %v", resp.Results)
			break
		}
	}
	rankByEmail := map[string]int{}
	for _, r := range resp.Results {
		rankByEmail[r.Email] = r.RotationRank
	}
	for i, em := range resp.RotationOrder {
		if rankByEmail[em] != i+1 {
			t.Errorf("rotation_rank 必须与实际尝试顺序一致：%s 应排 #%d，实际 #%d", em, i+1, rankByEmail[em])
		}
	}
	// 验收②：每个结果都有 strategy/rotation_rank/quota_group/quota_taken_by 字段。
	for _, r := range resp.Results {
		if r.Strategy == "" {
			t.Errorf("%s 缺少 strategy", r.Email)
		}
		if r.RotationRank <= 0 {
			t.Errorf("%s 缺少 rotation_rank", r.Email)
		}
		if r.QuotaGroup <= 0 {
			t.Errorf("%s 缺少 quota_group", r.Email)
		}
	}
	byEmail := map[string]ideauth.QoderCheckinItem{}
	for _, r := range resp.Results {
		byEmail[r.Email] = r
	}
	b := byEmail["b@test.com"]
	if b.Phase != ideauth.QoderPhaseBlocked {
		t.Errorf("B 号应 blocked（同人已领，未签到），实际 %q", b.Phase)
	}
	if b.CheckedInToday {
		t.Error("B 号 blocked，checked_in_today 必须为 false")
	}
	if b.Credits != 0 || b.BenefitAmount != 100 {
		t.Errorf("B 号 credits 必须 0、benefit_amount=100，实际 %v/%v", b.Credits, b.BenefitAmount)
	}
	if b.QuotaTakenBy != "a@test.com" || b.Strategy != ideauth.QoderStrategySharedPerson {
		t.Errorf("B 号应点名额度归属并标 shared_person，实际 taken_by=%q strategy=%q", b.QuotaTakenBy, b.Strategy)
	}
	// 分桶：两个号被服务端判定同人 → 一个 quota_groups 条目。
	if len(resp.QuotaGroups) != 1 {
		t.Fatalf("应有一个同人分桶（2 个号），实际 %d: %+v", len(resp.QuotaGroups), resp.QuotaGroups)
	}
	if resp.QuotaGroups[0].QuotaGroup != 2 || len(resp.QuotaGroups[0].Emails) != 2 {
		t.Errorf("分桶应含两个账号，实际 %+v", resp.QuotaGroups[0])
	}
	if resp.QuotaGroups[0].ClaimedBy != "a@test.com" {
		t.Errorf("分桶里应点名 claimed_by=a@test.com，实际 %q", resp.QuotaGroups[0].ClaimedBy)
	}
	// 账本审计：B 的 blocked 也要落盘。
	if recs := ideauth.QoderClaimHistory("b@test.com", 1); len(recs) != 1 || recs[0].Phase != ideauth.QoderPhaseBlocked {
		t.Errorf("B 的 blocked 尝试必须进账本，实际 %+v", recs)
	}
}

// TestQoderCheckinStatusContract 契约 C-QD-API@1 B：每号必须带
// phase / quota_group / quota_taken_by / strategy / last_claimed_at /
// claim_history_days。
func TestQoderCheckinStatusContract(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	p := qoderTestPanel(t)
	srv := qoderFakeUpstream(t)
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	// 账本里预置昨天的一次 claimed：面板要能显示「历史已领天数 / 上次领到」。
	yesterday := time.Now().In(time.FixedZone("UTC+8", 8*60*60)).AddDate(0, 0, -1)
	if err := ideauth.AppendQoderClaim(ideauth.QoderClaimRecord{
		Email: "a@test.com", Phase: ideauth.QoderPhaseClaimed, Credits: 100,
		Date: yesterday.Format("2006-01-02"), At: yesterday.Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	pool := &ideauth.QoderPoolFile{Accounts: []*ideauth.QoderSnapshot{
		{Email: "a@test.com", UserID: "ua", Name: "A", Auth: &ideauth.QoderAuth{Token: "tok-a", User: ideauth.QoderUser{Email: "a@test.com", ID: "ua", Name: "A"}}},
		{Email: "b@test.com", UserID: "ub", Name: "B", Auth: &ideauth.QoderAuth{Token: "tok-b", User: ideauth.QoderUser{Email: "b@test.com", ID: "ub", Name: "B"}}},
	}}
	if err := ideauth.SaveQoderPool(pool); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	p.qoderCheckinStatus(rec, httptest.NewRequest(http.MethodGet, "/panel/api/ide/qoder/checkin-status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin-status 应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []ideauth.QoderScanAccount `json:"results"`
		Claimed int                        `json:"claimed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("应返回两个账号，实际 %d", len(resp.Results))
	}
	byEmail := map[string]ideauth.QoderScanAccount{}
	for _, r := range resp.Results {
		byEmail[strings.ToLower(r.Email)] = r
	}
	// 扫描是「只查不领」：上游今天的活动仍是 CLAIMABLE，所以 phase=claimable。
	a := byEmail["a@test.com"]
	if a.Phase != "claimable" || a.CheckedInToday {
		t.Errorf("A 号上游今日活动可领，应 claimable 且未签到，实际 %+v", a)
	}
	if a.ClaimHistoryDays != 1 {
		t.Errorf("A 号账本里有 1 个 claimed 日期，claim_history_days 应为 1，实际 %d", a.ClaimHistoryDays)
	}
	if a.LastClaimedAt == "" {
		t.Error("A 号应能从账本回落到 last_claimed_at")
	}
	// claimable = 今天还没领、可以领。归属**未核实** → unknown：
	// 不得默认扣上「服务端判定同人」的帽子（2026-11-04 实测：服务端并没判同人，
	// 两个号各自都领到了各自的 100）。
	if a.Strategy != ideauth.QoderStrategyUnknown {
		t.Errorf("未核实的归属策略应为 unknown，实际 %q", a.Strategy)
	}
	b := byEmail["b@test.com"]
	if b.Phase != "pending" || b.CheckedInToday {
		t.Errorf("B 号上游未下发活动，应 pending 且未签到，实际 %+v", b)
	}
	if b.QuotaGroup == 0 {
		t.Error("每个账号都必须带 quota_group")
	}
}

// TestQoderEgressEndpoints 契约 C-QD-API@1 D/E：GET 返回每号配置（未配置按默认
// real 展示），POST 校验失败返回 400 + 错误码且**不落盘**。
func TestQoderEgressEndpoints(t *testing.T) {
	p := qoderTestPanel(t)

	// 号池里放一个号，确保 GET 会把「未配置」的账号补成默认值。
	pool := &ideauth.QoderPoolFile{Accounts: []*ideauth.QoderSnapshot{
		{Email: "a@test.com", Auth: &ideauth.QoderAuth{Token: "tok-a", User: ideauth.QoderUser{Email: "a@test.com"}}},
	}}
	if err := ideauth.SaveQoderPool(pool); err != nil {
		t.Fatal(err)
	}

	// 未配置时 GET 也要返回默认 real（前端要能对每个号做配置）。
	rec := httptest.NewRecorder()
	p.qoderEgressGet(rec, httptest.NewRequest(http.MethodGet, "/panel/api/ide/qoder/egress", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("egress GET 应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK       bool             `json:"ok"`
		Profiles []map[string]any `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET 响应不是合法 JSON: %v", err)
	}
	if !got.OK || len(got.Profiles) != 1 {
		t.Fatalf("GET 应返回 1 个账号配置，实际 ok=%v profiles=%v", got.OK, got.Profiles)
	}
	if got.Profiles[0]["device_mode"] != "real" {
		t.Errorf("未配置账号的 device_mode 应为 real，实际 %v", got.Profiles[0]["device_mode"])
	}
	if _, ok := got.Profiles[0]["last_probe"]; !ok {
		t.Error("GET 必须返回 last_probe 字段（无结论时 null）")
	}

	// POST 合法配置。
	body := `{"email":"a@test.com","device_mode":"derived","proxy_url":"http://127.0.0.1:18080"}`
	rec = httptest.NewRecorder()
	p.qoderEgressSet(rec, httptest.NewRequest(http.MethodPost, "/panel/api/ide/qoder/egress", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("合法配置应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var setResp struct {
		OK      bool                       `json:"ok"`
		Profile ideauth.QoderEgressProfile `json:"profile"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &setResp); err != nil {
		t.Fatalf("POST 响应不是合法 JSON: %v", err)
	}
	if !setResp.OK || setResp.Profile.DeviceMode != "derived" || setResp.Profile.ProxyURL != "http://127.0.0.1:18080" {
		t.Fatalf("POST 应回写配置，实际 %+v", setResp)
	}
	if _, err := os.Stat(filepath.Join(ideauth.PoolDir(), "qoder_egress.json")); err != nil {
		t.Errorf("配置应落盘到 qoder_egress.json: %v", err)
	}

	// 校验失败：mode 非法 → 400 invalid_device_mode，且不覆盖已有配置。
	rec = httptest.NewRecorder()
	p.qoderEgressSet(rec, httptest.NewRequest(http.MethodPost, "/panel/api/ide/qoder/egress",
		strings.NewReader(`{"email":"a@test.com","device_mode":"fake"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_device_mode") {
		t.Fatalf("非法 device_mode 应 400 + invalid_device_mode，实际 %d：%s", rec.Code, rec.Body.String())
	}
	// 校验失败：socks5 代理 → 400 invalid_proxy_url。
	rec = httptest.NewRecorder()
	p.qoderEgressSet(rec, httptest.NewRequest(http.MethodPost, "/panel/api/ide/qoder/egress",
		strings.NewReader(`{"email":"a@test.com","device_mode":"derived","proxy_url":"socks5://127.0.0.1:1080"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_proxy_url") {
		t.Fatalf("socks5 代理应 400 + invalid_proxy_url，实际 %d：%s", rec.Code, rec.Body.String())
	}
	prof := ideauth.QoderEgressFor("a@test.com")
	if prof.DeviceMode != "derived" || prof.ProxyURL != "http://127.0.0.1:18080" {
		t.Errorf("校验失败不得改写已保存的配置，实际 %+v", prof)
	}
}

// TestQoderClaimHistoryEndpoint 契约 C-QD-API@1 C：按账号/天数过滤，
// email 为空返回全部账号（at 降序、最多 500 条）。
func TestQoderClaimHistoryEndpoint(t *testing.T) {
	p := qoderTestPanel(t)
	now := time.Now().In(time.FixedZone("UTC+8", 8*60*60))

	records := []ideauth.QoderClaimRecord{
		{Email: "a@test.com", Phase: ideauth.QoderPhaseClaimed, Credits: 100, Date: now.Format("2006-01-02"), At: now.Format(time.RFC3339)},
		{Email: "a@test.com", Phase: ideauth.QoderPhaseBlocked, Date: now.AddDate(0, 0, -1).Format("2006-01-02"), At: now.AddDate(0, 0, -1).Format(time.RFC3339)},
		{Email: "b@test.com", Phase: ideauth.QoderPhasePending, Date: now.Format("2006-01-02"), At: now.Add(-time.Minute).Format(time.RFC3339)},
	}
	for _, r := range records {
		if err := ideauth.AppendQoderClaim(r); err != nil {
			t.Fatalf("预置账本失败: %v", err)
		}
	}

	// 按账号 + days=7。
	rec := httptest.NewRecorder()
	p.qoderClaimHistory(rec, httptest.NewRequest(http.MethodGet, "/panel/api/ide/qoder/claim_history?email=a@test.com&days=7", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("claim_history 应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK      bool                       `json:"ok"`
		Records []ideauth.QoderClaimRecord `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !got.OK || len(got.Records) != 2 {
		t.Fatalf("a@test.com 近 7 天应有 2 条记录，实际 ok=%v n=%d: %+v", got.OK, len(got.Records), got.Records)
	}
	if got.Records[0].Date >= got.Records[1].Date {
		t.Errorf("按账号查询应按 date 升序，实际 %+v", got.Records)
	}

	// email 为空：全部账号、at 降序。
	rec = httptest.NewRecorder()
	p.qoderClaimHistory(rec, httptest.NewRequest(http.MethodGet, "/panel/api/ide/qoder/claim_history?days=7", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(got.Records) != 3 {
		t.Fatalf("全部账号应有 3 条记录，实际 %d", len(got.Records))
	}
	for i := 1; i < len(got.Records); i++ {
		if got.Records[i-1].At < got.Records[i].At {
			t.Errorf("email 为空时应按 at 降序，实际 %+v", got.Records)
			break
		}
	}

	// 非法 days：<=0 视为 30，超上限按 180 —— 不能报错、不能返空。
	for _, days := range []string{"0", "-5", "999"} {
		rec = httptest.NewRecorder()
		p.qoderClaimHistory(rec, httptest.NewRequest(http.MethodGet, "/panel/api/ide/qoder/claim_history?days="+days, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("days=%s 应正常返回，实际 %d", days, rec.Code)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Errorf("days=%s 响应非法 JSON: %v", days, err)
		}
		if len(got.Records) != 3 {
			t.Errorf("days=%s 应返回 3 条（记录都在 180 天内），实际 %d", days, len(got.Records))
		}
	}

	// 空账本必须返回 []（不是 null）。
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	rec = httptest.NewRecorder()
	p.qoderClaimHistory(rec, httptest.NewRequest(http.MethodGet, "/panel/api/ide/qoder/claim_history", nil))
	if !strings.Contains(rec.Body.String(), `"records":[]`) {
		t.Errorf("空账本应返回 records:[]，实际 %s", rec.Body.String())
	}
}

// qoderFakeUpstreamALeads 复刻 T14 实测的上游状态：A 号今天已领取（campaign
// CLAIMED），B 号上游没下发活动（campaigns=[]），且 B 用 A 的实例做只读查询
// 时返回 404 GRANT_NOT_FOUND → 服务端明确判定「不是同一个人」。
func qoderFakeUpstreamALeads(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		startAt := time.Now().Add(-2 * time.Hour).Unix()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sash/api/v1/me/campaigns":
			if tok == "tok-a" {
				fmt.Fprintf(w, `{"campaigns":[{"campaignId":"cid-daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","startAt":%d,"benefit":{"amount":100}}]}`, startAt)
				return
			}
			_, _ = w.Write([]byte(`{"campaigns":[]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reward"):
			if tok == "tok-a" {
				_, _ = w.Write([]byte(`{"grantId":"grant-a","status":"CLAIMED","campaignId":"cid-daily","benefit":{"amount":100}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorCode":"GRANT_NOT_FOUND"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// qoderFakeUpstreamBothClaimed 两个号各自已领取（两个独立额度组）。
func qoderFakeUpstreamBothClaimed(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		startAt := time.Now().Add(-2 * time.Hour).Unix()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sash/api/v1/me/campaigns":
			fmt.Fprintf(w, `{"campaigns":[{"campaignId":"cid-%s","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","startAt":%d,"benefit":{"amount":100}}]}`, tok, startAt)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestQoderCheckinAndStatusAgreeOnStrategy D-Q1 回归：同一时刻、同一上游状态，
// POST checkin 与 GET checkin-status 对同一账号必须给出一致的 strategy /
// quota_taken_by / same_person_taken，且文案不得与 strategy 自相矛盾。
func TestQoderCheckinAndStatusAgreeOnStrategy(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	p := qoderTestPanel(t)
	srv := qoderFakeUpstreamALeads(t)
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	pool := &ideauth.QoderPoolFile{Accounts: []*ideauth.QoderSnapshot{
		{Email: "a@test.com", UserID: "ua", Name: "A", Auth: &ideauth.QoderAuth{Token: "tok-a", User: ideauth.QoderUser{Email: "a@test.com", ID: "ua", Name: "A"}}},
		{Email: "b@test.com", UserID: "ub", Name: "B", Auth: &ideauth.QoderAuth{Token: "tok-b", User: ideauth.QoderUser{Email: "b@test.com", ID: "ub", Name: "B"}}},
	}}
	if err := ideauth.SaveQoderPool(pool); err != nil {
		t.Fatal(err)
	}

	// ① POST checkin：A already、B pending。
	rec := httptest.NewRecorder()
	p.qoderCheckin(rec, httptest.NewRequest(http.MethodPost, "/panel/api/ide/qoder/checkin", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin 应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var checkinResp struct {
		Results        []ideauth.QoderCheckinItem `json:"results"`
		Already        int                        `json:"already_accounts"`
		Pending        int                        `json:"pending_accounts"`
		CheckedInToday bool                       `json:"checked_in_today"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &checkinResp); err != nil {
		t.Fatalf("checkin 响应不是合法 JSON: %v", err)
	}
	checkinByEmail := map[string]ideauth.QoderCheckinItem{}
	for _, r := range checkinResp.Results {
		checkinByEmail[strings.ToLower(r.Email)] = r
	}
	ckB := checkinByEmail["b@test.com"]
	if ckB.Phase != ideauth.QoderPhasePending {
		t.Fatalf("B 号应 pending，实际 %+v", ckB)
	}

	// ② GET checkin-status：同一上游状态。
	rec = httptest.NewRecorder()
	p.qoderCheckinStatus(rec, httptest.NewRequest(http.MethodGet, "/panel/api/ide/qoder/checkin-status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin-status 应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var statusResp struct {
		Results        []ideauth.QoderScanAccount `json:"results"`
		Claimed        int                        `json:"claimed"`
		Pending        int                        `json:"pending"`
		CheckedInToday bool                       `json:"checked_in_today"`
		AllSettled     bool                       `json:"all_settled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("checkin-status 响应不是合法 JSON: %v", err)
	}
	statusByEmail := map[string]ideauth.QoderScanAccount{}
	for _, r := range statusResp.Results {
		statusByEmail[strings.ToLower(r.Email)] = r
	}
	stB := statusByEmail["b@test.com"]
	if stB.Phase != ideauth.QoderPhasePending {
		t.Fatalf("状态扫描里 B 号应 pending，实际 %+v", stB)
	}

	// 两端点必须一致：strategy / quota_taken_by / 同人占用标记。
	if ckB.Strategy != stB.Strategy {
		t.Errorf("同一状态两端点 strategy 必须一致：checkin=%q status=%q", ckB.Strategy, stB.Strategy)
	}
	if ckB.Strategy != ideauth.QoderStrategyUnassigned {
		t.Errorf("探测明确非同人时 strategy 应为 unassigned，实际 checkin=%q status=%q", ckB.Strategy, stB.Strategy)
	}
	if ckB.QuotaTakenBy != stB.QuotaTakenBy {
		t.Errorf("同一状态两端点 quota_taken_by 必须一致：checkin=%q status=%q", ckB.QuotaTakenBy, stB.QuotaTakenBy)
	}
	if stB.QuotaTakenBy != "" || ckB.QuotaTakenBy != "" {
		t.Errorf("不是同一个人时 quota_taken_by 必须为空：checkin=%q status=%q", ckB.QuotaTakenBy, stB.QuotaTakenBy)
	}
	if stB.DeviceQuotaUsed {
		t.Error("明确非同人时 same_person_taken 必须为 false（不能一边说不同人一边说额度被同人占用）")
	}
	if stB.QuotaGroup != 1 || ckB.QuotaGroup != 1 {
		t.Errorf("没有同人关系时组大小应为 1：checkin=%d status=%d", ckB.QuotaGroup, stB.QuotaGroup)
	}
	// 文案不得与 strategy 矛盾：unassigned 不得出现「同一个人」肯定句。
	if strings.Contains(ckB.Msg, "同一个人") || strings.Contains(stB.Msg, "同一个人") {
		t.Errorf("unassigned 的文案不得出现「同一个人」：checkin=%q status=%q", ckB.Msg, stB.Msg)
	}
	// 顶层 checked_in_today 两端点同义：存在 pending/claimable → false。
	if checkinResp.CheckedInToday {
		t.Error("POST checkin 有 pending 时顶层 checked_in_today 必须 false")
	}
	if statusResp.CheckedInToday {
		t.Error("GET checkin-status 有 pending 时顶层 checked_in_today 必须与 POST 同为 false")
	}
	// all_settled 保留旧语义（今天没有可领的了），与 checked_in_today 正交。
	if !statusResp.AllSettled {
		t.Error("没有 claimable 账号时 all_settled 应为 true（旧语义，与 checked_in_today 区分）")
	}
	// A 号两端点也自洽：上游说今天这条已领，且只读探测拿回**本号自己的**
	// grantId ⇒ 本号独占一份额度（isolated）。两个端点必须给同一个答案 ——
	// 「同一个号两个说法」是 D-Q1 类 bug（POST 侧不问就只能说未核实，扫描侧却
	// 问得出，于是两边打架）。
	ckA := checkinByEmail["a@test.com"]
	stA := statusByEmail["a@test.com"]
	if ckA.Strategy != ideauth.QoderStrategyIsolated {
		t.Errorf("A 号 POST 侧策略应为 isolated（本号自己的 grantId），实际 %q", ckA.Strategy)
	}
	if stA.Strategy != ideauth.QoderStrategyIsolated {
		t.Errorf("A 号状态扫描策略应为 isolated（本号自己的 grantId），实际 %q", stA.Strategy)
	}
	if ckA.Strategy != stA.Strategy {
		t.Errorf("同一状态两端点 strategy 必须一致：checkin=%q status=%q", ckA.Strategy, stA.Strategy)
	}
}

// TestQoderCheckedInTodaySameSemanticsBothEndpoints 顶层 checked_in_today 必须
// 两端口径一致：全部 claimed/already 才是 true。
func TestQoderCheckedInTodaySameSemanticsBothEndpoints(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	p := qoderTestPanel(t)
	srv := qoderFakeUpstreamBothClaimed(t)
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	pool := &ideauth.QoderPoolFile{Accounts: []*ideauth.QoderSnapshot{
		{Email: "a@test.com", UserID: "ua", Name: "A", Auth: &ideauth.QoderAuth{Token: "tok-a", User: ideauth.QoderUser{Email: "a@test.com", ID: "ua", Name: "A"}}},
		{Email: "b@test.com", UserID: "ub", Name: "B", Auth: &ideauth.QoderAuth{Token: "tok-b", User: ideauth.QoderUser{Email: "b@test.com", ID: "ub", Name: "B"}}},
	}}
	if err := ideauth.SaveQoderPool(pool); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	p.qoderCheckin(rec, httptest.NewRequest(http.MethodPost, "/panel/api/ide/qoder/checkin", strings.NewReader("{}")))
	var checkinResp struct {
		Results        []ideauth.QoderCheckinItem `json:"results"`
		Already        int                        `json:"already_accounts"`
		CheckedInToday bool                       `json:"checked_in_today"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &checkinResp); err != nil {
		t.Fatalf("checkin 响应不是合法 JSON: %v", err)
	}
	if checkinResp.Already != 2 || !checkinResp.CheckedInToday {
		t.Fatalf("两个号都已是 claimed → already=2 且顶层 checked_in_today=true，实际 already=%d checked=%v",
			checkinResp.Already, checkinResp.CheckedInToday)
	}

	rec = httptest.NewRecorder()
	p.qoderCheckinStatus(rec, httptest.NewRequest(http.MethodGet, "/panel/api/ide/qoder/checkin-status", nil))
	var statusResp struct {
		Results        []ideauth.QoderScanAccount `json:"results"`
		Claimed        int                        `json:"claimed"`
		Claimable      int                        `json:"claimable"`
		CheckedInToday bool                       `json:"checked_in_today"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("checkin-status 响应不是合法 JSON: %v", err)
	}
	if statusResp.Claimed != 2 || statusResp.Claimable != 0 {
		t.Fatalf("两个号都应 claimed，实际 claimed=%d claimable=%d", statusResp.Claimed, statusResp.Claimable)
	}
	if !statusResp.CheckedInToday {
		t.Error("全部 claimed 时顶层 checked_in_today 应与 POST checkin 同义为 true")
	}
}

// TestQoderCheckinStatusBackfillsClaimHistory D-Q2：今天真实领到过（Claims 有
// 今天的时刻）但账本当天没有 claimed 记录时，扫描要补写一条，让
// claim_history_days 反映真实历史；且重复扫描不得重复写。
func TestQoderCheckinStatusBackfillsClaimHistory(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本项目面向 Windows（runtime-info.exe 指纹）")
	}
	p := qoderTestPanel(t)
	srv := qoderFakeUpstreamALeads(t)
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	pool := &ideauth.QoderPoolFile{Accounts: []*ideauth.QoderSnapshot{
		{Email: "a@test.com", UserID: "ua", Name: "A", Auth: &ideauth.QoderAuth{Token: "tok-a", User: ideauth.QoderUser{Email: "a@test.com", ID: "ua", Name: "A"}}},
	}}
	if err := ideauth.SaveQoderPool(pool); err != nil {
		t.Fatal(err)
	}
	// 真实领取发生过（旧进程写入 Claims），但账本文件建立之前 → 当天无 claimed。
	claimedAt := time.Now().In(time.FixedZone("UTC+8", 8*60*60)).Add(-30 * time.Minute)
	if err := ideauth.MarkClaimedAt("qoder", "a@test.com", claimedAt); err != nil {
		t.Fatal(err)
	}
	if recs := ideauth.QoderClaimHistory("a@test.com", 1); len(recs) != 0 {
		t.Fatalf("前置条件：账本当天不应有记录，实际 %+v", recs)
	}

	rec := httptest.NewRecorder()
	p.qoderCheckinStatus(rec, httptest.NewRequest(http.MethodGet, "/panel/api/ide/qoder/checkin-status", nil))
	var statusResp struct {
		Results []ideauth.QoderScanAccount `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("checkin-status 响应不是合法 JSON: %v", err)
	}
	if len(statusResp.Results) != 1 {
		t.Fatalf("应返回 1 个账号，实际 %d", len(statusResp.Results))
	}
	row := statusResp.Results[0]
	if row.ClaimHistoryDays != 1 {
		t.Errorf("补写后 claim_history_days 应为 1，实际 %d", row.ClaimHistoryDays)
	}
	if row.LastClaimedAt == "" {
		t.Error("补写后 last_claimed_at 不应为空")
	}
	recs := ideauth.QoderClaimHistory("a@test.com", 1)
	if len(recs) != 1 || recs[0].Phase != ideauth.QoderPhaseClaimed {
		t.Fatalf("应补写一条 claimed 记录，实际 %+v", recs)
	}
	if recs[0].Credits != 100 {
		t.Errorf("补写的 credits 应取每日固定面值 100，实际 %v", recs[0].Credits)
	}
	if recs[0].Date != claimedAt.Format("2006-01-02") {
		t.Errorf("补写记录的 date 应为今天的 UTC+8 日期，实际 %q", recs[0].Date)
	}

	// 幂等：再扫一次不得新增/重复写。
	rec = httptest.NewRecorder()
	p.qoderCheckinStatus(rec, httptest.NewRequest(http.MethodGet, "/panel/api/ide/qoder/checkin-status", nil))
	if n := len(ideauth.QoderClaimHistory("a@test.com", 0)); n != 1 {
		t.Errorf("重复扫描不得重复补写，实际账本 %d 条", n)
	}
}
