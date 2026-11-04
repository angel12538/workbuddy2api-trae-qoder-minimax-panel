package ideauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// C-QD-PROBE@1 分类测试
//
// 这些测试只喂纯函数（不碰网络/文件），逐条覆盖计划 2.2.9 的机械规则：
// isolated_viable / server_person_binding / inconclusive 三分支，
// 以及 criterion_inferred 的 phone/real_name/email_domain/device/ip/scope/
// server_person_binding/unknown 赋值顺序。
// ---------------------------------------------------------------------------

func probeTestAccount(email, name, scope, domain, phone string, phonePresent bool, machineID string) QoderProbeAccount {
	return QoderProbeAccount{
		Email:        email,
		UserID:       "uid-" + email,
		Name:         name,
		Scope:        scope,
		PhonePresent: phonePresent,
		Phone:        phone,
		EmailDomain:  domain,
		MachineID:    machineID,
		DeviceID:     "did-" + email,
	}
}

func probeTestResult(email, mode, proxy, mid, tok string, httpStatus, claimCount int, states []string) QoderProbeAccountResult {
	r := QoderProbeAccountResult{
		Email:          email,
		DeviceMode:     mode,
		Proxy:          proxy,
		CampaignsHTTP:  httpStatus,
		ClaimCampaigns: claimCount,
		CampaignIDs:    []string{},
		CampaignStates: []string{},
		MachineID:      mid,
		MachineToken:   tok,
	}
	if claimCount > 0 {
		r.CampaignIDs = []string{"cid-" + email}
		r.CampaignStates = states
	}
	return r
}

func probeTestPairs(variant, a, b, verdict string) []QoderProbePairVerdict {
	return []QoderProbePairVerdict{
		{A: a, B: b, Variant: variant, Verdict: verdict},
		{A: b, B: a, Variant: variant, Verdict: verdict},
	}
}

func probeTestHasMissing(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// 规则 1：两号都看到可领的 CLAIM_BENEFIT，且互相探测明确不是同人 → isolated_viable。
func TestProbeClassifyIsolatedViable(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "A", "s1", "x.com", "", false, "m-a"),
		probeTestAccount("b@y.com", "B", "s2", "y.com", "", false, "m-b"),
	}
	variants := []QoderProbeVariant{{
		Name: "baseline",
		PerAccount: []QoderProbeAccountResult{
			probeTestResult("a@x.com", "real", "", "m-a", "t-a", 200, 1, []string{"CLAIMABLE"}),
			probeTestResult("b@y.com", "real", "", "m-b", "t-b", 200, 1, []string{"CLAIMABLE"}),
		},
	}}
	pairs := probeTestPairs("baseline", "a@x.com", "b@y.com", QoderVerdictOther)
	out := ClassifyQoderProbe(accounts, variants, pairs, nil)
	if out.Conclusion != QoderProbeConclusionIsolatedViable {
		t.Fatalf("两号都可领且互相明确不是同人 → isolated_viable，实际 conclusion=%s criterion=%s missing=%v",
			out.Conclusion, out.CriterionInferred, out.Missing)
	}
	if out.CriterionInferred != QoderProbeCriterionUnknown {
		t.Errorf("没有同人判定时 criterion 只能是 unknown，实际 %s", out.CriterionInferred)
	}
}

// 规则 1 的反例：pair 探测问不出结论（unknown）不能当「不是同人」→ 不得 isolated。
func TestProbeClassifyUnknownPairNotIsolated(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "A", "s1", "x.com", "", false, "m-a"),
		probeTestAccount("b@y.com", "B", "s2", "y.com", "", false, "m-b"),
	}
	variants := []QoderProbeVariant{{
		Name: "baseline",
		PerAccount: []QoderProbeAccountResult{
			probeTestResult("a@x.com", "real", "", "m-a", "t-a", 200, 1, []string{"CLAIMABLE"}),
			probeTestResult("b@y.com", "real", "", "m-b", "t-b", 200, 1, []string{"CLAIMABLE"}),
		},
	}}
	pairs := probeTestPairs("baseline", "a@x.com", "b@y.com", QoderVerdictUnknown)
	out := ClassifyQoderProbe(accounts, variants, pairs, nil)
	if out.Conclusion != QoderProbeConclusionInconclusive {
		t.Fatalf("unknown 不能证明可隔离，结论必须 inconclusive，实际 %s", out.Conclusion)
	}
	if !probeTestHasMissing(out.Missing, QoderProbeMissingProxyNotConfigured) {
		t.Errorf("无代理时 missing 必须含 proxy_not_configured，实际 %v", out.Missing)
	}
}

// 规则 2：所有已执行变体都复现 same_person，且 device/ip/scope/domain 全排除
// → server_person_binding。baseline 设备相同不阻碍排除（derived_proxy 已证明设备不同）。
func TestProbeClassifyServerPersonBinding(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "A", "s1", "x.com", "13800000001", true, "mid-a"),
		probeTestAccount("b@y.com", "B", "s2", "y.com", "13800000002", true, "mid-b"),
	}
	variants := []QoderProbeVariant{
		{
			Name: "baseline",
			PerAccount: []QoderProbeAccountResult{
				probeTestResult("a@x.com", "real", "", "real-machine", "real-token", 200, 1, []string{"CLAIMED"}),
				probeTestResult("b@y.com", "real", "", "real-machine", "real-token", 200, 0, nil),
			},
		},
		{
			Name: "derived_proxy",
			PerAccount: []QoderProbeAccountResult{
				probeTestResult("a@x.com", "derived", "http://p-a:8080", "dev-a", "tok-a", 200, 1, []string{"CLAIMED"}),
				probeTestResult("b@y.com", "derived", "http://p-b:8080", "dev-b", "tok-b", 200, 0, nil),
			},
		},
	}
	pairs := append(
		probeTestPairs("baseline", "a@x.com", "b@y.com", QoderVerdictSamePerson),
		probeTestPairs("derived_proxy", "a@x.com", "b@y.com", QoderVerdictSamePerson)...,
	)
	out := ClassifyQoderProbe(accounts, variants, pairs, nil)
	if out.Conclusion != QoderProbeConclusionServerPersonBinding {
		t.Fatalf("全变体复现且四维度全排除 → server_person_binding，实际 %s（criterion=%s missing=%v）",
			out.Conclusion, out.CriterionInferred, out.Missing)
	}
	if out.CriterionInferred != QoderProbeCriterionServerPersonBinding {
		t.Errorf("criterion 应为 server_person_binding，实际 %s", out.CriterionInferred)
	}
}

// 规则 3：复现但缺代理（IP 维度无法验证）→ 只能 inconclusive + proxy_not_configured。
func TestProbeClassifyInconclusiveNoProxy(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "A", "s1", "x.com", "13800000001", true, "mid-a"),
		probeTestAccount("b@y.com", "B", "s2", "y.com", "13800000002", true, "mid-b"),
	}
	variants := []QoderProbeVariant{
		{
			Name: "baseline",
			PerAccount: []QoderProbeAccountResult{
				probeTestResult("a@x.com", "real", "", "real-machine", "real-token", 200, 1, []string{"CLAIMED"}),
				probeTestResult("b@y.com", "real", "", "real-machine", "real-token", 200, 0, nil),
			},
		},
		{
			Name: "derived",
			PerAccount: []QoderProbeAccountResult{
				probeTestResult("a@x.com", "derived", "", "dev-a", "tok-a", 200, 1, []string{"CLAIMED"}),
				probeTestResult("b@y.com", "derived", "", "dev-b", "tok-b", 200, 0, nil),
			},
		},
	}
	pairs := append(
		probeTestPairs("baseline", "a@x.com", "b@y.com", QoderVerdictSamePerson),
		probeTestPairs("derived", "a@x.com", "b@y.com", QoderVerdictSamePerson)...,
	)
	out := ClassifyQoderProbe(accounts, variants, pairs, nil)
	if out.Conclusion == QoderProbeConclusionServerPersonBinding {
		t.Fatal("缺代理（IP 未验证）时绝不能输出 server_person_binding")
	}
	if out.Conclusion != QoderProbeConclusionInconclusive {
		t.Fatalf("结论应为 inconclusive，实际 %s", out.Conclusion)
	}
	if !probeTestHasMissing(out.Missing, QoderProbeMissingProxyNotConfigured) {
		t.Errorf("missing 必须含 proxy_not_configured，实际 %v", out.Missing)
	}
	if out.CriterionInferred != QoderProbeCriterionUnknown {
		t.Errorf("设备已排除但 IP 未验证 → criterion 只能 unknown，实际 %s", out.CriterionInferred)
	}
}

// criterion=phone：两号手机号相同。
func TestProbeClassifyCriterionPhone(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "A", "s1", "x.com", "13800000009", true, "mid-a"),
		probeTestAccount("b@y.com", "B", "s2", "y.com", "13800000009", true, "mid-b"),
	}
	variants, pairs := probeTestSamePersonFixture()
	out := ClassifyQoderProbe(accounts, variants, pairs, nil)
	if out.CriterionInferred != QoderProbeCriterionPhone {
		t.Fatalf("手机号相同 → criterion=phone，实际 %s", out.CriterionInferred)
	}
}

// criterion=real_name：手机号缺失、真实姓名相同 → real_name。
func TestProbeClassifyCriterionRealName(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "帆 许", "s1", "x.com", "", false, "mid-a"),
		probeTestAccount("b@y.com", "帆 许", "s2", "y.com", "", false, "mid-b"),
	}
	variants, pairs := probeTestSamePersonFixture()
	out := ClassifyQoderProbe(accounts, variants, pairs, nil)
	if out.CriterionInferred != QoderProbeCriterionRealName {
		t.Fatalf("姓名相同且手机号未提供 → criterion=real_name，实际 %s", out.CriterionInferred)
	}
}

// criterion=email_domain：只有邮箱域名相同，设备/IP/scope 均不同。
func TestProbeClassifyCriterionEmailDomain(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "A", "s1", "x.com", "1", true, "mid-a"),
		probeTestAccount("b@x.com", "B", "s2", "x.com", "2", true, "mid-b"),
	}
	variants, pairs := probeTestFixturePairWithProxy("a@x.com", "b@x.com")
	out := ClassifyQoderProbe(accounts, variants, pairs, nil)
	if out.CriterionInferred != QoderProbeCriterionEmailDomain {
		t.Fatalf("设备/IP/scope 均不同、只有域名相同 → criterion=email_domain，实际 %s（missing=%v）",
			out.CriterionInferred, out.Missing)
	}
}

// criterion=device：设备始终相同（没有 derived 变体证据），无法排除设备。
func TestProbeClassifyCriterionDevice(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "A", "s1", "x.com", "1", true, "mid-a"),
		probeTestAccount("b@y.com", "B", "s2", "y.com", "2", true, "mid-b"),
	}
	variants := []QoderProbeVariant{{
		Name: "baseline",
		PerAccount: []QoderProbeAccountResult{
			probeTestResult("a@x.com", "real", "", "same-machine", "same-token", 200, 1, []string{"CLAIMED"}),
			probeTestResult("b@y.com", "real", "", "same-machine", "same-token", 200, 0, nil),
		},
	}}
	pairs := probeTestPairs("baseline", "a@x.com", "b@y.com", QoderVerdictSamePerson)
	out := ClassifyQoderProbe(accounts, variants, pairs, nil)
	if out.CriterionInferred != QoderProbeCriterionDevice {
		t.Fatalf("设备相同仍复现 → criterion=device，实际 %s", out.CriterionInferred)
	}
}

// criterion=ip：设备已排除、但两号走同一代理 → IP 无法排除。
func TestProbeClassifyCriterionIP(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "A", "s1", "x.com", "1", true, "mid-a"),
		probeTestAccount("b@y.com", "B", "s2", "y.com", "2", true, "mid-b"),
	}
	variants := []QoderProbeVariant{{
		Name: "derived_proxy",
		PerAccount: []QoderProbeAccountResult{
			probeTestResult("a@x.com", "derived", "http://same-proxy:8080", "dev-a", "tok-a", 200, 1, []string{"CLAIMED"}),
			probeTestResult("b@y.com", "derived", "http://same-proxy:8080", "dev-b", "tok-b", 200, 0, nil),
		},
	}}
	pairs := probeTestPairs("derived_proxy", "a@x.com", "b@y.com", QoderVerdictSamePerson)
	out := ClassifyQoderProbe(accounts, variants, pairs, nil)
	if out.CriterionInferred != QoderProbeCriterionIP {
		t.Fatalf("设备不同、出口相同 → criterion=ip，实际 %s", out.CriterionInferred)
	}
}

// criterion=scope：设备/IP 均排除、scope 相同 → scope。
func TestProbeClassifyCriterionScope(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "A", "same-scope", "x.com", "1", true, "mid-a"),
		probeTestAccount("b@y.com", "B", "same-scope", "y.com", "2", true, "mid-b"),
	}
	variants, pairs := probeTestSamePersonFixtureWithProxy()
	out := ClassifyQoderProbe(accounts, variants, pairs, nil)
	if out.CriterionInferred != QoderProbeCriterionScope {
		t.Fatalf("设备/IP 排除、scope 相同 → criterion=scope，实际 %s", out.CriterionInferred)
	}
}

// 没有复现（无 same_person）→ criterion=unknown。
func TestProbeClassifyCriterionUnknown(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "A", "s1", "x.com", "1", true, "mid-a"),
		probeTestAccount("b@y.com", "B", "s2", "y.com", "2", true, "mid-b"),
	}
	variants, _ := probeTestSamePersonFixture()
	out := ClassifyQoderProbe(accounts, variants, nil, nil)
	if out.CriterionInferred != QoderProbeCriterionUnknown {
		t.Fatalf("没有同人判定 → criterion=unknown，实际 %s", out.CriterionInferred)
	}
	if out.Conclusion != QoderProbeConclusionInconclusive {
		t.Fatalf("没有同人判定 → conclusion=inconclusive，实际 %s", out.Conclusion)
	}
}

// 请求失败要如实进 missing，不能当成「没活动」或「独立」。
func TestProbeClassifyRequestFailureMissing(t *testing.T) {
	accounts := []QoderProbeAccount{
		probeTestAccount("a@x.com", "A", "s1", "x.com", "", false, "mid-a"),
		probeTestAccount("b@y.com", "B", "s2", "y.com", "", false, "mid-b"),
	}
	variants := []QoderProbeVariant{{
		Name: "baseline",
		PerAccount: []QoderProbeAccountResult{
			{Email: "a@x.com", DeviceMode: "real", CampaignsHTTP: 0, CampaignIDs: []string{}, CampaignStates: []string{}, Error: "dial tcp: i/o timeout"},
			{Email: "b@y.com", DeviceMode: "real", CampaignsHTTP: 0, CampaignIDs: []string{}, CampaignStates: []string{}, Error: "token_expired: 只读探测不刷新令牌"},
		},
	}}
	out := ClassifyQoderProbe(accounts, variants, nil, nil)
	if out.Conclusion != QoderProbeConclusionInconclusive {
		t.Fatalf("网络失败 → inconclusive，实际 %s", out.Conclusion)
	}
	if !probeTestHasMissing(out.Missing, "request_failed:a@x.com") {
		t.Errorf("missing 应含 request_failed:a@x.com，实际 %v", out.Missing)
	}
	if !probeTestHasMissing(out.Missing, "token_expired:b@y.com") {
		t.Errorf("missing 应含 token_expired:b@y.com，实际 %v", out.Missing)
	}
}

// 只读硬约束：探测代码里不得出现 claim 请求或 token 刷新。
func TestProbeClassifyReadOnlyNoClaimRequest(t *testing.T) {
	files := []string{
		"qoder_probe_classify.go",
		filepath.Join("..", "..", "cmd", "qoderprobe", "main.go"),
	}
	// 这些串一旦出现就意味着「写了上游」或「可能轮换 refresh_token」。
	forbidden := []string{
		"qoderPathClaim",
		"qoderClaimInstance",
		"qoderCheckinAuth",
		"qoderPathDeviceRefresh",
		"MethodPost",
		"/claim",
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		src := string(raw)
		for _, bad := range forbidden {
			if strings.Contains(src, bad) {
				t.Errorf("%s 含禁止串 %q：C-QD-PROBE@1 只允许 GET campaigns / GET reward", f, bad)
			}
		}
	}
}

// 令牌过期时探测必须直接返回错误，不得发出任何请求（否则 refresh 轮换后
// 号池里的 refresh_token 会失效）。
func TestProbeClassifyExpiredTokenDoesNotRefresh(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"CLAIMED","grantId":"g"}`))
	}))
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	auth := &QoderAuth{
		Token:        "dt-expired-token",
		RefreshToken: "drt-should-not-be-used",
		ExpiresAt:    time.Now().Add(-time.Hour).Format(time.RFC3339),
		User:         QoderUser{Email: "a@x.com", ID: "uid-a"},
	}
	_, err := QoderProbeRewardQuery(auth, &QoderDevice{}, "cid-1", "")
	if err == nil {
		t.Fatal("过期令牌必须返回错误")
	}
	if !strings.Contains(err.Error(), "token_expired") {
		t.Fatalf("错误应说明 token_expired，实际 %v", err)
	}
	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Fatalf("过期令牌不得发请求（实际 %d 次），否则会轮换 refresh_token", n)
	}
}

// 输出 JSON 字段必须与契约 2.2.9 完全一致（不多不少）。
func TestProbeClassifyJSONShape(t *testing.T) {
	rep := QoderProbeReport{
		Version: QoderProbeVersion,
		At:      "2026-11-03T10:00:00+08:00",
		Accounts: []QoderProbeAccount{{
			Email: "a@x.com", UserID: "u", Name: "n", Scope: "s",
			PhonePresent: false, EmailDomain: "x.com", MachineID: "m", DeviceID: "d",
			Phone: "13800000000",
		}},
		Variants: []QoderProbeVariant{{
			Name: "baseline",
			PerAccount: []QoderProbeAccountResult{{
				Email: "a@x.com", DeviceMode: "real", Proxy: "",
				CampaignsHTTP: 200, ClaimCampaigns: 1,
				CampaignIDs: []string{"c1"}, CampaignStates: []string{"CLAIMABLE"},
				StartAt: "2026-11-03T10:00:00+08:00", EndAt: "2026-11-04T09:59:00+08:00",
				Reward: QoderProbeReward{Status: "CLAIMED", FailureCode: "", GrantID: "g", CampaignID: "c1"},
				Error:  "",
			}},
		}},
		PairVerdicts:      []QoderProbePairVerdict{{A: "a@x.com", B: "b@y.com", Variant: "baseline", Verdict: "other", Evidence: "e"}},
		CriterionInferred: "unknown",
		Conclusion:        "inconclusive",
		Missing:           []string{"proxy_not_configured"},
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	wantTop := probeTestSorted([]string{"version", "at", "accounts", "variants", "pair_verdicts", "criterion_inferred", "conclusion", "missing"})
	if got := sortedQoderProbeJSONKeys(top); strings.Join(got, ",") != strings.Join(wantTop, ",") {
		t.Fatalf("顶层字段不符：want %v got %v", wantTop, got)
	}

	var accounts []map[string]json.RawMessage
	if err := json.Unmarshal(top["accounts"], &accounts); err != nil {
		t.Fatal(err)
	}
	wantAcc := probeTestSorted([]string{"email", "user_id", "name", "scope", "phone_present", "email_domain", "machine_id", "device_id"})
	if got := sortedQoderProbeJSONKeys(accounts[0]); strings.Join(got, ",") != strings.Join(wantAcc, ",") {
		t.Fatalf("accounts 字段不符：want %v got %v", wantAcc, got)
	}
	if _, ok := accounts[0]["phone"]; ok {
		t.Fatal("accounts 不得输出 phone 原文")
	}

	var variants []map[string]json.RawMessage
	if err := json.Unmarshal(top["variants"], &variants); err != nil {
		t.Fatal(err)
	}
	wantVar := probeTestSorted([]string{"name", "per_account"})
	if got := sortedQoderProbeJSONKeys(variants[0]); strings.Join(got, ",") != strings.Join(wantVar, ",") {
		t.Fatalf("variants 字段不符：want %v got %v", wantVar, got)
	}
	var pa []map[string]json.RawMessage
	if err := json.Unmarshal(variants[0]["per_account"], &pa); err != nil {
		t.Fatal(err)
	}
	wantPA := probeTestSorted([]string{"email", "device_mode", "proxy", "campaigns_http", "claim_campaigns", "campaign_ids", "campaign_states", "reward", "error", "start_at", "end_at"})
	if got := sortedQoderProbeJSONKeys(pa[0]); strings.Join(got, ",") != strings.Join(wantPA, ",") {
		t.Fatalf("per_account 字段不符：want %v got %v", wantPA, got)
	}
	var reward map[string]json.RawMessage
	if err := json.Unmarshal(pa[0]["reward"], &reward); err != nil {
		t.Fatal(err)
	}
	wantReward := probeTestSorted([]string{"status", "failure_code", "grant_id", "campaign_id"})
	if got := sortedQoderProbeJSONKeys(reward); strings.Join(got, ",") != strings.Join(wantReward, ",") {
		t.Fatalf("reward 字段不符：want %v got %v", wantReward, got)
	}

	var pairs []map[string]json.RawMessage
	if err := json.Unmarshal(top["pair_verdicts"], &pairs); err != nil {
		t.Fatal(err)
	}
	wantPair := probeTestSorted([]string{"a", "b", "variant", "verdict", "evidence"})
	if got := sortedQoderProbeJSONKeys(pairs[0]); strings.Join(got, ",") != strings.Join(wantPair, ",") {
		t.Fatalf("pair_verdicts 字段不符：want %v got %v", wantPair, got)
	}

	// start_at/end_at 是可选字段（omitempty）：为空时不得出现，保证旧消费方兼容。
	emptyRaw, err := json.Marshal(QoderProbeAccountResult{
		Email: "x@y.com", DeviceMode: "real", CampaignIDs: []string{}, CampaignStates: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var empty map[string]json.RawMessage
	if err := json.Unmarshal(emptyRaw, &empty); err != nil {
		t.Fatal(err)
	}
	if _, ok := empty["start_at"]; ok {
		t.Fatal("空窗口时 start_at 必须 omitempty 不输出")
	}
	if _, ok := empty["end_at"]; ok {
		t.Fatal("空窗口时 end_at 必须 omitempty 不输出")
	}
}

// 代理凭证不得出现在输出/日志里。
func TestProbeClassifyRedaction(t *testing.T) {
	got := RedactProxyURL("http://user:secret@127.0.0.1:1080")
	if strings.Contains(got, "secret") || strings.Contains(got, "user") {
		t.Fatalf("代理凭证未脱敏：%s", got)
	}
	if !strings.Contains(got, "127.0.0.1:1080") {
		t.Fatalf("脱敏应保留 host:port：%s", got)
	}
	txt := SanitizeQoderProbeText("proxy http://u:p@1.2.3.4:8080 failed with dt-abcdef123456")
	if strings.Contains(txt, "u:p@") || strings.Contains(txt, "abcdef123456") {
		t.Fatalf("错误原文未脱敏：%s", txt)
	}
}

// 派生设备：同一邮箱跨调用稳定、不同邮箱不同、代理被规范化写入。
func TestProbeClassifyDeriveDevice(t *testing.T) {
	base := &QoderDevice{
		ClientType: "10", MachineOS: "x86_64_windows", MachineHostname: "REAL-HOST",
		MachineID: "real-machine-id", MachineToken: "real-token", MachineCode: "real-code",
		MachineType: "desktop", Version: "1.2.3",
	}
	d1, err := DeriveQoderProbeDevice(base, "A@X.com", QoderDeviceModeDerived, "http://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := DeriveQoderProbeDevice(base, "a@x.com", QoderDeviceModeDerived, "http://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	d3, err := DeriveQoderProbeDevice(base, "b@y.com", QoderDeviceModeDerived, "")
	if err != nil {
		t.Fatal(err)
	}
	if d1.MachineID != d2.MachineID || d1.MachineToken != d2.MachineToken || d1.MachineCode != d2.MachineCode {
		t.Fatal("同一邮箱（大小写不同）派生必须完全一致")
	}
	if d1.MachineID == d3.MachineID {
		t.Fatal("不同邮箱必须派生不同设备")
	}
	if !d1.Derived || d1.Synthetic {
		t.Fatalf("derived 设备应 Derived=true, Synthetic=false，实际 %+v", d1)
	}
	if d1.ProxyURL != "http://127.0.0.1:1080" {
		t.Fatalf("代理应写入 ProxyURL，实际 %q", d1.ProxyURL)
	}
	real, err := DeriveQoderProbeDevice(base, "a@x.com", QoderDeviceModeReal, "")
	if err != nil {
		t.Fatal(err)
	}
	if real.MachineID != "real-machine-id" || real.Derived {
		t.Fatalf("real 模式必须是真实设备，实际 %+v", real)
	}
	if _, err := DeriveQoderProbeDevice(base, "a@x.com", QoderDeviceModeDerived, "socks5://127.0.0.1:1080"); err == nil {
		t.Fatal("socks5 必须报错（不静默直连）")
	}
}

// probeTestSamePersonFixture 两个变体都复现同人：baseline 设备相同、derived 设备不同；
// 无代理（用于 phone/real_name/device/unknown 场景）。
func probeTestSamePersonFixture() ([]QoderProbeVariant, []QoderProbePairVerdict) {
	return probeTestFixturePair("a@x.com", "b@y.com")
}

func probeTestFixturePair(emailA, emailB string) ([]QoderProbeVariant, []QoderProbePairVerdict) {
	variants := []QoderProbeVariant{
		{
			Name: "baseline",
			PerAccount: []QoderProbeAccountResult{
				probeTestResult(emailA, "real", "", "real-machine", "real-token", 200, 1, []string{"CLAIMED"}),
				probeTestResult(emailB, "real", "", "real-machine", "real-token", 200, 0, nil),
			},
		},
		{
			Name: "derived",
			PerAccount: []QoderProbeAccountResult{
				probeTestResult(emailA, "derived", "", "dev-a", "tok-a", 200, 1, []string{"CLAIMED"}),
				probeTestResult(emailB, "derived", "", "dev-b", "tok-b", 200, 0, nil),
			},
		},
	}
	pairs := append(
		probeTestPairs("baseline", emailA, emailB, QoderVerdictSamePerson),
		probeTestPairs("derived", emailA, emailB, QoderVerdictSamePerson)...,
	)
	return variants, pairs
}

// probeTestFixturePairWithProxy 同一个人判定 + derived_proxy 变体（设备/IP 均可排除）。
func probeTestSamePersonFixtureWithProxy() ([]QoderProbeVariant, []QoderProbePairVerdict) {
	return probeTestFixturePairWithProxy("a@x.com", "b@y.com")
}

func probeTestFixturePairWithProxy(emailA, emailB string) ([]QoderProbeVariant, []QoderProbePairVerdict) {
	variants := []QoderProbeVariant{
		{
			Name: "baseline",
			PerAccount: []QoderProbeAccountResult{
				probeTestResult(emailA, "real", "", "real-machine", "real-token", 200, 1, []string{"CLAIMED"}),
				probeTestResult(emailB, "real", "", "real-machine", "real-token", 200, 0, nil),
			},
		},
		{
			Name: "derived_proxy",
			PerAccount: []QoderProbeAccountResult{
				probeTestResult(emailA, "derived", "http://p-a:8080", "dev-a", "tok-a", 200, 1, []string{"CLAIMED"}),
				probeTestResult(emailB, "derived", "http://p-b:8080", "dev-b", "tok-b", 200, 0, nil),
			},
		},
	}
	pairs := append(
		probeTestPairs("baseline", emailA, emailB, QoderVerdictSamePerson),
		probeTestPairs("derived_proxy", emailA, emailB, QoderVerdictSamePerson)...,
	)
	return variants, pairs
}

// sortedQoderProbeJSONKeys 返回 JSON 对象的键（升序），用于字段契约断言。
func sortedQoderProbeJSONKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// probeTestSorted 返回升序副本（JSON 对象键的字典序）。
func probeTestSorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
