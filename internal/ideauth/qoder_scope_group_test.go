package ideauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 额度组（scope）回归测试
//
// 2026-11-02 实测（真打上游）钉死的结论：
//
//	Qoder 的每日 100 Credits 按**账号归属的 scope**（服务端认定的实名/租户）发，
//	**不是按设备**。证据：
//	  1. 伪造整套 Cosy-* 设备头（machineToken/machineCode/machineId 全换）服务端
//	     照样返回同一条活动 —— 设备头只当「有没有设备」的开关；
//	  2. POST /api/v1/deviceToken/refresh 的响应里没有任何 scope/设备字段 —— 绑定
//	     发生在登录/注册阶段；
//	  3. 两个账号各自登录后拿到的 agentAccountScope 完全一致，user id 却不同。
//
// 于是「一个设备只能签一个号」的真相是「一个实名（scope）只能签一个号」。
//
// 这组测试守住两条线：
//   ① 同一 scope 的两个号 → 后一个必须被如实判成「额度被同组账号领走」；
//   ② 不同 scope 的两个号 → **绝不能**拿别的组的活动实例去探测（那会把一个本来
//      自己能领的号误判成额度被占，甚至动到别人的额度）。
// ---------------------------------------------------------------------------

// scopeSnap 造一个带 scope 的号池快照。
func scopeSnap(email, token, scope string) *QoderSnapshot {
	return &QoderSnapshot{
		Email:  email,
		UserID: "uid-" + email,
		Name:   email,
		Scope:  scope,
		Auth: &QoderAuth{
			Token: token,
			User:  QoderUser{ID: "uid-" + email, Email: email, Name: email},
		},
	}
}

// scopeFakeUpstream 假的活动服务：
//   - GET  /sash/api/v1/me/campaigns  → 按 token 返回该号的今日活动
//   - POST /sash/api/v1/me/campaigns/{cid}/claim → 记录调用（谁、用哪个 cid）
//
// claimBy 返回「用哪个 token 领了哪个 cid」的调用序列（按发生顺序）。
func scopeFakeUpstream(t *testing.T, campaignsByToken map[string]string,
	claimReply func(tok, cid string) (int, string)) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var claims []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == qoderPathCampaigns:
			body, ok := campaignsByToken[tok]
			if !ok {
				body = `{"campaigns":[]}`
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/sash/api/v1/me/campaigns/"):
			cid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sash/api/v1/me/campaigns/"), "/claim")
			mu.Lock()
			claims = append(claims, tok+"|"+cid)
			mu.Unlock()
			code, body := claimReply(tok, cid)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(claims))
		copy(out, claims)
		return out
	}
}

// TestQoderSameScopeSharesDailyQuota 同一 scope 的两个号：后一个被如实判成
// 「额度已被同组账号领走」，且**不能**被记成已签到（假签到）。
func TestQoderSameScopeSharesDailyQuota(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	srv, claims := scopeFakeUpstream(t,
		map[string]string{
			"tok-a": todayCampaignJSON("CLAIMABLE"),
			"tok-b": `{"campaigns":[]}`,
		},
		func(tok, cid string) (int, string) {
			if tok == "tok-a" {
				return 200, `{"status":"CLAIMED","benefit":{"amount":100}}`
			}
			// 同组另一账号领过 → 服务端判定同一人已领
			return 200, `{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED","benefit":{"amount":100}}`
		})
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	writeQoderPool(t, dir,
		scopeSnap("a@x.com", "tok-a", "scope-A"),
		scopeSnap("b@x.com", "tok-b", "scope-A"),
	)

	out, err := QoderCheckinPool(nil)
	if err != nil {
		t.Fatalf("QoderCheckinPool: %v", err)
	}
	by := map[string]QoderCheckinResult{}
	for _, r := range out.Results {
		by[r.Email] = r
	}
	a, b := by["a@x.com"], by["b@x.com"]

	if a.Phase != "claimed" {
		t.Errorf("a@x.com：应签到成功，实际 phase=%s msg=%q", a.Phase, a.Msg)
	}
	if b.Phase != "blocked" {
		t.Errorf("b@x.com：同 scope 应被判成额度被同组领走，实际 phase=%s msg=%q", b.Phase, b.Msg)
	}
	if b.CheckedInToday {
		t.Error("b@x.com：额度被别人领走不等于本号已签到，不能报已签到（假签到）")
	}
	if b.QuotaGroup != 2 {
		t.Errorf("b@x.com：额度组大小应为 2，实际 %d", b.QuotaGroup)
	}
	if b.QuotaTakenBy != "a@x.com" {
		t.Errorf("b@x.com：应点名是谁领到的额度，实际 %q", b.QuotaTakenBy)
	}
	if out.Blocked != 1 {
		t.Errorf("应统计 1 个 blocked，实际 %d", out.Blocked)
	}
	// 同组才允许拿兄弟号的 cid 去探测
	if len(claims()) == 0 {
		t.Error("同组时应探测一次兄弟号的实例 id")
	}
}

// TestQoderDifferentScopeMustNotCrossProbe 不同 scope 的两个号：**绝不能**拿另一个
// scope 的活动实例 id 去探测 —— 那会把一个本来自己能领的号误判成额度被占。
//
// 这是 2026-11-02 修掉的真 bug：旧实现按「号池里任意另一个号」取 cid，
// 跨 scope 的探测一旦被服务端判成 SAME_PERSON，就会给用户一句彻底的假话。
func TestQoderDifferentScopeMustNotCrossProbe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	srv, claims := scopeFakeUpstream(t,
		map[string]string{
			"tok-a": todayCampaignJSON("CLAIMABLE"),
			"tok-b": `{"campaigns":[]}`,
		},
		func(tok, cid string) (int, string) {
			// 任何跨账号的领取都当成"服务端放行"，让误判的后果暴露出来。
			return 200, `{"status":"CLAIMED","benefit":{"amount":100}}`
		})
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	writeQoderPool(t, dir,
		scopeSnap("a@x.com", "tok-a", "scope-A"),
		scopeSnap("b@x.com", "tok-b", "scope-B"),
	)

	out, err := QoderCheckinPool(nil)
	if err != nil {
		t.Fatalf("QoderCheckinPool: %v", err)
	}
	by := map[string]QoderCheckinResult{}
	for _, r := range out.Results {
		by[r.Email] = r
	}
	b := by["b@x.com"]
	if b.Phase == "blocked" {
		t.Errorf("b@x.com：不同 scope 不该被判成额度被领走，实际 phase=%s msg=%q", b.Phase, b.Msg)
	}
	// 关键断言：服务端从未收到 b 号发起的领取请求（跨 scope 探测根本没发生）。
	for _, c := range claims() {
		if strings.HasPrefix(c, "tok-b|") {
			t.Errorf("b@x.com 不该拿别的 scope 的实例 id 去领，实际发起了：%s", c)
		}
	}
}

// TestQoderScanQuotaExplanationIsScopeAware 扫描解释「额度被占」时也必须按 scope：
// 同 scope 的兄弟号领过 → 置位并点名；不同 scope 的兄弟号领过 → 不置位。
func TestQoderScanQuotaExplanationIsScopeAware(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Method == http.MethodGet && r.URL.Path == qoderPathCampaigns {
			body := `{"campaigns":[]}`
			if tok == "tok-a" {
				body = todayCampaignJSON("CLAIMED")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	// a@x.com（scope-A）今天已领；b@x.com（scope-A）同组；c@x.com（scope-B）异组。
	writeQoderPool(t, dir,
		scopeSnap("a@x.com", "tok-a", "scope-A"),
		scopeSnap("b@x.com", "tok-b", "scope-A"),
		scopeSnap("c@x.com", "tok-c", "scope-B"),
	)
	writeLog(t, dir, "qoder:a@x.com", time.Now().Format("2006-01-02")+"T23:00:00+08:00")

	rows := QoderPoolScan(nil)
	by := map[string]QoderScanAccount{}
	for _, r := range rows {
		by[r.Email] = r
	}

	b := by["b@x.com"]
	if !b.DeviceQuotaUsed {
		t.Errorf("b@x.com：与 a 同 scope，应说明额度被同组账号领走")
	}
	if b.QuotaTakenBy != "a@x.com" {
		t.Errorf("b@x.com：应点名 a@x.com 领到了额度，实际 %q", b.QuotaTakenBy)
	}
	if b.QuotaGroup != 2 {
		t.Errorf("b@x.com：额度组大小应为 2，实际 %d", b.QuotaGroup)
	}

	c := by["c@x.com"]
	if c.DeviceQuotaUsed {
		t.Errorf("c@x.com：与 a 不同 scope，各有各的额度，不该被说成额度被占（msg=%q）", c.Msg)
	}
	if c.QuotaTakenBy != "" {
		t.Errorf("c@x.com：不同 scope 不该有 quota_taken_by，实际 %q", c.QuotaTakenBy)
	}
	if c.QuotaGroup != 1 {
		t.Errorf("c@x.com：额度组大小应为 1，实际 %d", c.QuotaGroup)
	}
}

// TestQoderQuotaGroupKeying 组键规则：同 scope 同组、scope 缺失归入 unknown 组。
func TestQoderQuotaGroupKeying(t *testing.T) {
	a := scopeSnap("a@x.com", "tok-a", "scope-A")
	b := scopeSnap("b@x.com", "tok-b", "scope-A")
	c := scopeSnap("c@x.com", "tok-c", "scope-B")
	noScope := scopeSnap("d@x.com", "tok-d", "")

	if QoderQuotaGroupKey(a) != QoderQuotaGroupKey(b) {
		t.Error("同 scope 的账号应在同一额度组")
	}
	if QoderQuotaGroupKey(a) == QoderQuotaGroupKey(c) {
		t.Error("不同 scope 的账号不应在同一额度组")
	}
	if QoderQuotaGroupKey(noScope) != QoderQuotaGroupKey(noScope) {
		t.Error("scope 缺失的账号应稳定落在 unknown 组")
	}
	groups := QoderQuotaGroups([]*QoderSnapshot{a, b, c, noScope})
	if len(groups) != 3 {
		t.Errorf("应分成 3 组（scope-A / scope-B / unknown），实际 %d", len(groups))
	}
	sizes := QoderQuotaGroupSizes([]*QoderSnapshot{a, b, c, noScope})
	if sizes["a@x.com"] != 2 || sizes["b@x.com"] != 2 {
		t.Errorf("scope-A 组应有 2 个号，实际 a=%d b=%d", sizes["a@x.com"], sizes["b@x.com"])
	}
	if sizes["c@x.com"] != 1 {
		t.Errorf("scope-B 组应有 1 个号，实际 %d", sizes["c@x.com"])
	}
	if sizes["d@x.com"] != 1 {
		t.Errorf("unknown 组应有 1 个号（只有 d），实际 %d", sizes["d@x.com"])
	}
}

// TestQoderScopePeers 同 scope 的号互为「同额度组」；不同 scope 互不相干。
func TestQoderScopePeers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)
	writeQoderPool(t, dir,
		scopeSnap("a@x.com", "tok-a", "scope-A"),
		scopeSnap("b@x.com", "tok-b", "scope-A"),
		scopeSnap("c@x.com", "tok-c", "scope-B"),
	)
	peers := QoderScopePeers("a@x.com")
	if len(peers) != 1 || peers[0] != "b@x.com" {
		t.Errorf("a@x.com 的同组账号应为 [b@x.com]，实际 %v", peers)
	}
	if p := QoderScopePeers("c@x.com"); len(p) != 0 {
		t.Errorf("c@x.com 独属 scope-B，不该有同组账号，实际 %v", p)
	}
	if p := QoderScopePeers("nobody@x.com"); len(p) != 0 {
		t.Errorf("号池里没有的账号不该有同组账号，实际 %v", p)
	}
}

// TestQoderSnapshotScopeRoundTrip scope 要能存进号池文件并读回来（否则分组失准）。
func TestQoderSnapshotScopeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)
	writeQoderPool(t, dir, scopeSnap("a@x.com", "tok-a", "qoder:prod:global:abc123"))
	pool, err := LoadQoderPool()
	if err != nil {
		t.Fatalf("LoadQoderPool: %v", err)
	}
	if len(pool.Accounts) != 1 || pool.Accounts[0].Scope != "qoder:prod:global:abc123" {
		t.Fatalf("scope 未持久化：%+v", pool.Accounts)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "qoder_accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Accounts []struct {
			Scope string `json:"scope"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Accounts) != 1 || probe.Accounts[0].Scope != "qoder:prod:global:abc123" {
		t.Fatalf("号池文件里没有 scope 字段：%s", string(raw))
	}
}
