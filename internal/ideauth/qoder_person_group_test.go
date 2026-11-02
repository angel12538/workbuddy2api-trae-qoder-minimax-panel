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
// 每日额度分组：**按人**（服务端判定），不按 scope、不按设备、不按账号
//
// 2026-11-02 实测（真打上游）钉死的结论：
//
//	Qoder 的每日 100 Credits 发给**一个人**，不是一个账号。实测证据：
//	  1. 两个账号拿到的是**同一条**活动实例（campaignId 相同），先领的记在自己
//	     的 grantId 上，另一个号去查会得到
//	     status=BLOCKED / failureCode=SAME_PERSON_ALREADY_CLAIMED，
//	     而且服务端给它**单独建了一条 grant**（grantId 不同）—— 这是服务端对
//	     「人」的判定。
//	  2. 两个号的 **agentAccountScope 其实不同**（用客户端数据里的 serverRuntimeId
//	     与服务端 /api/v1/agents 交叉验证得到），却照样共享同一份额度
//	     ⇒ scope 不是分组键（上一版拿 scope 分组是错的）。
//	  3. 两个号的 machine_id / device_id 也不同 ⇒ 设备不是分组键。
//
// 这组测试守住三条线：
//   ① 服务端判定「同一人」→ 后一个号必须被如实判成 blocked（**不能**记成已签到）；
//   ② 服务端判定「不是同一人」→ 绝不能把人家领走的额度说成本号被占；
//   ③ 判定必须是**只读**的（GET reward），不能拿别人的实例 id 去 POST claim ——
//      万一不是同一个人，那一发就会把别人的额度领走。
// ---------------------------------------------------------------------------

// personSnap 造一个带 scope 的号池快照。
func personSnap(email, token, scope string) *QoderSnapshot {
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

// personFakeUpstream 假的活动服务：
//   - GET  /sash/api/v1/me/campaigns        → 按 token 返回该号的今日活动
//   - GET  /sash/api/v1/me/campaigns/{cid}/reward → 奖励查询（只读判定用）
//   - POST /sash/api/v1/me/campaigns/{cid}/claim → 记录调用（谁、用哪个 cid）
//
// rewardReply 返回「用哪个 token 查这条实例」的服务端答复。
// claims 返回「用哪个 token 领了哪个 cid」的调用序列（按发生顺序）。
func personFakeUpstream(t *testing.T, campaignsByToken map[string]string,
	rewardReply func(tok, cid string) (int, string),
	claimReply func(tok, cid string) (int, string)) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var claims []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == qoderPathCampaigns:
			body, ok := campaignsByToken[tok]
			if !ok {
				body = `{"campaigns":[]}`
			}
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sash/api/v1/me/campaigns/") &&
			strings.HasSuffix(r.URL.Path, "/reward"):
			cid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sash/api/v1/me/campaigns/"), "/reward")
			code, body := rewardReply(tok, cid)
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/sash/api/v1/me/campaigns/"):
			cid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sash/api/v1/me/campaigns/"), "/claim")
			mu.Lock()
			claims = append(claims, tok+"|"+cid)
			mu.Unlock()
			code, body := claimReply(tok, cid)
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

// samePersonRewardReply 模拟真实上游的奖励查询（2026-11-02 实测形状）：
// 实例主人查 → CLAIMED（自己的 grantId）；同一人查 → BLOCKED + SAME_PERSON。
func samePersonRewardReply(tok, cid string) (int, string) {
	if tok == "tok-a" {
		return 200, `{"grantId":"grant-a","status":"CLAIMED","campaignId":"` + cid +
			`","benefit":{"amount":100}}`
	}
	return 200, `{"grantId":"grant-b","status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED",` +
		`"campaignId":"` + cid + `","benefit":{"amount":100}}`
}

// TestQoderSamePersonBlocked 服务端判定「同一人」→ 后一个号必须被如实判成
// blocked（今天没签上），且**不能**被记成已签到（假签到）。
func TestQoderSamePersonBlocked(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	srv, claims := personFakeUpstream(t,
		map[string]string{
			"tok-a": todayCampaignJSON("CLAIMABLE"),
			"tok-b": `{"campaigns":[]}`,
		},
		samePersonRewardReply,
		func(tok, cid string) (int, string) {
			return 200, `{"status":"CLAIMED","benefit":{"amount":100}}`
		})
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	writeQoderPool(t, dir,
		personSnap("a@x.com", "tok-a", "scope-A"),
		personSnap("b@x.com", "tok-b", "scope-B"), // scope 不同也照样可能是同一个人
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
		t.Errorf("b@x.com：服务端判定同一人已领，应判成 blocked，实际 phase=%s msg=%q", b.Phase, b.Msg)
	}
	if b.CheckedInToday {
		t.Error("b@x.com：额度被别人领走不等于本号已签到，不能报已签到（假签到）")
	}
	if b.QuotaTakenBy != "a@x.com" {
		t.Errorf("b@x.com：应点名是谁领到的额度，实际 %q", b.QuotaTakenBy)
	}
	if !strings.Contains(b.Msg, "SAME_PERSON_ALREADY_CLAIMED") {
		t.Errorf("b@x.com：说明里应带服务端判定码，实际 %q", b.Msg)
	}
	if out.Blocked != 1 {
		t.Errorf("应统计 1 个 blocked，实际 %d", out.Blocked)
	}
	// ⚠️ 跨号判定必须是**只读**的：b 号绝不能拿 a 号的实例 id 去 POST claim。
	for _, c := range claims() {
		if strings.HasPrefix(c, "tok-b|") {
			t.Errorf("b@x.com 不该发起跨号领取（那可能动到别人的额度），实际：%s", c)
		}
	}
}

// TestQoderDifferentPersonNotBlocked 服务端判定「不是同一个人」→ 绝不能把人家
// 领走的额度说成本号被占；本号只是「服务端今天没给它下发」。
func TestQoderDifferentPersonNotBlocked(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	srv, claims := personFakeUpstream(t,
		map[string]string{
			"tok-a": todayCampaignJSON("CLAIMABLE"),
			"tok-b": `{"campaigns":[]}`,
		},
		func(tok, cid string) (int, string) {
			if tok == "tok-a" {
				return 200, `{"grantId":"grant-a","status":"CLAIMED","campaignId":"` + cid + `","benefit":{"amount":100}}`
			}
			// 不是同一个人：本号查这条实例 → 服务端直接 404（这条实例不归它）。
			return 404, `{"errorCode":"NotFound"}`
		},
		func(tok, cid string) (int, string) {
			return 200, `{"status":"CLAIMED","benefit":{"amount":100}}`
		})
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	writeQoderPool(t, dir,
		personSnap("a@x.com", "tok-a", "scope-A"),
		personSnap("b@x.com", "tok-b", "scope-B"),
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
		t.Errorf("b@x.com：不是同一个人，不该被判成额度被领走，实际 phase=%s msg=%q", b.Phase, b.Msg)
	}
	if b.QuotaTakenBy != "" {
		t.Errorf("b@x.com：不是同一个人，不该有 quota_taken_by，实际 %q", b.QuotaTakenBy)
	}
	if out.Blocked != 0 {
		t.Errorf("不应统计 blocked，实际 %d", out.Blocked)
	}
	for _, c := range claims() {
		if strings.HasPrefix(c, "tok-b|") {
			t.Errorf("b@x.com 不该发起跨号领取，实际：%s", c)
		}
	}
}

// TestQoderPersonVerdict 奖励查询的判定规则。
func TestQoderPersonVerdict(t *testing.T) {
	same := QoderRewardProbe{Status: "BLOCKED", FailureCode: "SAME_PERSON_ALREADY_CLAIMED", GrantID: "g-b"}
	owner := QoderRewardProbe{Status: "CLAIMED", GrantID: "g-a"}
	if v := qoderPersonVerdict(same, owner); v != QoderVerdictSamePerson {
		t.Errorf("BLOCKED+SAME_PERSON 应判成同一人，实际 %q", v)
	}
	if v := qoderPersonVerdict(owner, owner); v != QoderVerdictOwner {
		t.Errorf("grantId 相同应判成主人，实际 %q", v)
	}
	other := QoderRewardProbe{HTTPStatus: http.StatusNotFound}
	if v := qoderPersonVerdict(other, owner); v != QoderVerdictOther {
		t.Errorf("404 应判成不是同一人，实际 %q", v)
	}
}

// TestQoderPersonGroups 组键规则：判定过同一人 → 同组；没判定 → 各自一组。
func TestQoderPersonGroups(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)
	writeQoderPool(t, dir,
		personSnap("a@x.com", "tok-a", "scope-A"),
		personSnap("b@x.com", "tok-b", "scope-B"),
		personSnap("c@x.com", "tok-c", "scope-C"),
	)
	// 先记一条「a 与 b 是同一个人」的服务端判定。
	saveQoderPersonLink("a@x.com", "b@x.com")

	a := personSnap("a@x.com", "tok-a", "scope-A")
	b := personSnap("b@x.com", "tok-b", "scope-B")
	c := personSnap("c@x.com", "tok-c", "scope-C")

	if QoderQuotaGroupKey(a) != QoderQuotaGroupKey(b) {
		t.Error("服务端判定为同一个人的账号应在同一组")
	}
	if QoderQuotaGroupKey(a) == QoderQuotaGroupKey(c) {
		t.Error("没有判定关系的账号不该被并到同一组")
	}
	groups := QoderQuotaGroups([]*QoderSnapshot{a, b, c})
	if len(groups) != 2 {
		t.Errorf("应分成 2 组（ab 一组、c 一组），实际 %d", len(groups))
	}
	sizes := QoderQuotaGroupSizes([]*QoderSnapshot{a, b, c})
	if sizes["a@x.com"] != 2 || sizes["b@x.com"] != 2 {
		t.Errorf("ab 组应有 2 个号，实际 a=%d b=%d", sizes["a@x.com"], sizes["b@x.com"])
	}
	if sizes["c@x.com"] != 1 {
		t.Errorf("c 组应有 1 个号，实际 %d", sizes["c@x.com"])
	}
}

// TestQoderSamePersonPeers 同一人关系要能查出来（且只认判定过的）。
func TestQoderSamePersonPeers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)
	writeQoderPool(t, dir,
		personSnap("a@x.com", "tok-a", "scope-A"),
		personSnap("b@x.com", "tok-b", "scope-B"),
		personSnap("c@x.com", "tok-c", "scope-C"),
	)
	saveQoderPersonLink("a@x.com", "b@x.com")

	peers := QoderScopePeers("a@x.com")
	if len(peers) != 1 || peers[0] != "b@x.com" {
		t.Errorf("a@x.com 的同一人账号应为 [b@x.com]，实际 %v", peers)
	}
	if p := QoderScopePeers("c@x.com"); len(p) != 0 {
		t.Errorf("c@x.com 没有判定关系，不该有同一人账号，实际 %v", p)
	}
	if p := QoderScopePeers("nobody@x.com"); len(p) != 0 {
		t.Errorf("号池里没有的账号不该有同一人账号，实际 %v", p)
	}
}

// TestQoderSnapshotScopeRoundTrip scope 要能存进号池文件并读回来（面板要显示它）。
func TestQoderSnapshotScopeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)
	writeQoderPool(t, dir, personSnap("a@x.com", "tok-a", "qoder:prod:global:abc123"))
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

// TestQoderScanPersonExplanation 扫描时也要用「同一人」口径解释，而不是按 scope 猜。
func TestQoderScanPersonExplanation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == qoderPathCampaigns:
			body := `{"campaigns":[]}`
			if tok == "tok-a" {
				body = todayCampaignJSON("CLAIMED")
			}
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reward"):
			cid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sash/api/v1/me/campaigns/"), "/reward")
			if tok == "tok-a" {
				_, _ = w.Write([]byte(`{"grantId":"grant-a","status":"CLAIMED","campaignId":"` + cid + `","benefit":{"amount":100}}`))
				return
			}
			if tok == "tok-b" {
				_, _ = w.Write([]byte(`{"grantId":"grant-b","status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED","campaignId":"` + cid + `","benefit":{"amount":100}}`))
				return
			}
			// c 号不是同一个人：这条实例不归它。
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorCode":"NotFound"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	writeQoderPool(t, dir,
		personSnap("a@x.com", "tok-a", "scope-A"),
		personSnap("b@x.com", "tok-b", "scope-B"),
		personSnap("c@x.com", "tok-c", "scope-C"),
	)
	writeLog(t, dir, "qoder:a@x.com", time.Now().Format("2006-01-02")+"T23:00:00+08:00")

	rows := QoderPoolScan(nil)
	by := map[string]QoderScanAccount{}
	for _, r := range rows {
		by[r.Email] = r
	}

	b := by["b@x.com"]
	if !b.DeviceQuotaUsed {
		t.Errorf("b@x.com：服务端判定同一人，应说明额度被同一个人的另一个号领走（msg=%q）", b.Msg)
	}
	if b.QuotaTakenBy != "a@x.com" {
		t.Errorf("b@x.com：应点名 a@x.com 领到了额度，实际 %q", b.QuotaTakenBy)
	}
	if b.QuotaGroup != 2 {
		t.Errorf("b@x.com：同一人组大小应为 2，实际 %d", b.QuotaGroup)
	}

	c := by["c@x.com"]
	if c.DeviceQuotaUsed {
		t.Errorf("c@x.com：服务端判定它与 a 不是同一个人，不该被说成额度被占（msg=%q）", c.Msg)
	}
	if c.QuotaGroup != 1 {
		t.Errorf("c@x.com：同一人组大小应为 1，实际 %d", c.QuotaGroup)
	}
}
