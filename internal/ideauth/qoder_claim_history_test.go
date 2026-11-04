package ideauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 契约 C-QD-LEDGER@1 的行为锁死：
//  1) 文件固定 <PoolDir>/qoder_claim_history.json、version=1；
//  2) 按 (email,date) 覆盖，同日粘性：claimed 不被后来的 blocked/pending/failed 擦掉；
//  3) 逐日可审计：claimed / blocked / pending / failed 都能查；
//  4) 保留 180 天；超过 5000 条按 at 淘汰最旧；
//  5) LastClaimedAt 在 checkin_log 的 Claims 缺失时回落到账本。

func ledgerToday(t *testing.T) string {
	t.Helper()
	return time.Now().In(qoderCST).Format("2006-01-02")
}

// TestQoderClaimHistoryStickyClaimed 同一天先 claimed 后 blocked：
// 账本必须保留 claimed（真领到的事实），但允许更新 strategy / quota_taken_by / at。
func TestQoderClaimHistoryStickyClaimed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	now := time.Now().In(qoderCST)
	first := now.Add(-2 * time.Hour).Format(time.RFC3339)
	second := now.Add(-1 * time.Hour).Format(time.RFC3339)

	if err := AppendQoderClaim(QoderClaimRecord{
		Email: "A@Test.com", Phase: QoderPhaseClaimed, Credits: 100,
		CampaignID: "cid-1", GrantID: "grant-1", Strategy: QoderStrategySharedPerson, At: first,
	}); err != nil {
		t.Fatalf("追加 claimed 失败: %v", err)
	}
	// 同日第二次尝试（服务端判定同人）——不得把 claimed 覆盖成 blocked。
	if err := AppendQoderClaim(QoderClaimRecord{
		Email: "a@test.com", Phase: QoderPhaseBlocked, Credits: 999,
		QuotaTakenBy: "b@test.com", Strategy: QoderStrategySharedPerson, At: second,
	}); err != nil {
		t.Fatalf("追加 blocked 失败: %v", err)
	}

	recs := QoderClaimHistory("a@test.com", 0)
	if len(recs) != 1 {
		t.Fatalf("同日同账号应只有一条记录（按 (email,date) 覆盖），实际 %d 条: %+v", len(recs), recs)
	}
	r := recs[0]
	if r.Phase != QoderPhaseClaimed {
		t.Errorf("粘性规则失效：claimed 被覆盖成了 %q", r.Phase)
	}
	if r.Credits != 100 {
		t.Errorf("claimed 的 credits 应保持 100，实际 %v", r.Credits)
	}
	if r.CampaignID != "cid-1" || r.GrantID != "grant-1" {
		t.Errorf("claimed 的活动/奖励标识不应被擦掉：campaign=%q grant=%q", r.CampaignID, r.GrantID)
	}
	if r.QuotaTakenBy != "b@test.com" {
		t.Errorf("粘性规则允许更新 quota_taken_by，实际 %q", r.QuotaTakenBy)
	}
	if r.At != second {
		t.Errorf("粘性规则允许更新 at（最后一次尝试时刻），实际 %q want %q", r.At, second)
	}
	if r.Date != ledgerToday(t) {
		t.Errorf("date 应是 UTC+8 的今天，实际 %q", r.Date)
	}

	// 文件必须是契约形状：version=1 + records 数组，且没有残留临时文件。
	path := filepath.Join(dir, "qoder_claim_history.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("账本文件不存在: %v", err)
	}
	var f struct {
		Version int                `json:"version"`
		Records []QoderClaimRecord `json:"records"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("账本不是合法 JSON: %v", err)
	}
	if f.Version != 1 {
		t.Errorf("账本 version 必须是 1，实际 %d", f.Version)
	}
	if len(f.Records) != 1 {
		t.Errorf("文件里应只有 1 条记录，实际 %d", len(f.Records))
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("原子写不应留下临时文件: %v", err)
	}
}

// TestQoderClaimHistoryAuditsAllPhases 三种可解释的状态都要能审计：
// 真领到（claimed）、被同人阻塞（blocked）、上游未下发（pending）、失败（failed）。
func TestQoderClaimHistoryAuditsAllPhases(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	now := time.Now().In(qoderCST)
	yesterday := now.AddDate(0, 0, -1)
	day := func(t time.Time) string { return t.Format("2006-01-02") }

	cases := []QoderClaimRecord{
		{Email: "a@test.com", Phase: QoderPhaseClaimed, Credits: 100, At: now.Add(-3 * time.Hour).Format(time.RFC3339)},
		{Email: "b@test.com", Phase: QoderPhaseBlocked, QuotaTakenBy: "a@test.com", Strategy: QoderStrategySharedPerson, At: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		{Email: "b@test.com", Phase: QoderPhasePending, Strategy: QoderStrategyUnassigned, Date: day(yesterday), At: yesterday.Format(time.RFC3339)},
		{Email: "c@test.com", Phase: QoderPhaseFailed, At: now.Add(-1 * time.Hour).Format(time.RFC3339)},
		{Email: "c@test.com", Phase: QoderPhaseLoginRequired, Date: day(yesterday), At: yesterday.Format(time.RFC3339)},
	}
	for _, c := range cases {
		// 契约：blocked/pending/failed/login_required 的积分必须为 0 —— 调用方传了
		// 非 0 也要被账本归一化（防止把面值当成入账）。
		c.Credits = 999
		if c.Phase == QoderPhaseClaimed || c.Phase == QoderPhaseAlready {
			c.Credits = 100
		}
		if err := AppendQoderClaim(c); err != nil {
			t.Fatalf("追加 %+v 失败: %v", c, err)
		}
	}

	// 按账号过滤 + 日期升序。
	b := QoderClaimHistory("B@TEST.COM", 0)
	if len(b) != 2 {
		t.Fatalf("b 应有 2 条记录（昨天 pending + 今天 blocked），实际 %d: %+v", len(b), b)
	}
	if b[0].Date >= b[1].Date {
		t.Errorf("QoderClaimHistory 必须按 date 升序: %+v", b)
	}
	if b[0].Phase != QoderPhasePending || b[1].Phase != QoderPhaseBlocked {
		t.Errorf("b 的两种状态必须可区分: %+v", b)
	}
	if b[0].Credits != 0 || b[1].Credits != 0 {
		t.Errorf("pending/blocked 的 credits 必须为 0，实际 pending=%v blocked=%v", b[0].Credits, b[1].Credits)
	}
	if b[1].QuotaTakenBy != "a@test.com" {
		t.Errorf("blocked 记录必须能审计是谁领走的，实际 %q", b[1].QuotaTakenBy)
	}

	// days=1 只看今天：昨天的 pending/login_required 不应出现。
	today := QoderClaimHistory("", 1)
	for _, r := range today {
		if r.Date != ledgerToday(t) {
			t.Errorf("days=1 不应返回非今天的记录: %+v", r)
		}
	}

	// 只有 claimed 的日期才是「历史已领天数」。
	if dates := QoderClaimedDates("a@test.com", 0); len(dates) != 1 || dates[0] != ledgerToday(t) {
		t.Errorf("QoderClaimedDates 只应统计 claimed 日期，实际 %v", dates)
	}
	if dates := QoderClaimedDates("b@test.com", 0); len(dates) != 0 {
		t.Errorf("blocked/pending 不是领取，QoderClaimedDates 应为空，实际 %v", dates)
	}

	// LastClaimedAt：checkin_log 的 Claims 里没有该账号 → 必须回落到账本。
	if ts, ok := LastClaimedAt("qoder", "a@test.com"); !ok || ts.In(qoderCST).Format("2006-01-02") != ledgerToday(t) {
		t.Errorf("LastClaimedAt 应回落到账本的 claimed 记录，实际 %v ok=%v", ts, ok)
	}
}

// TestQoderClaimHistoryPrune 预置 5001 条 + 一条 200 天前记录，追加一次触发保留策略：
// 180 天外删除；超过 5000 条按 at 淘汰最旧；新记录必须保留。
func TestQoderClaimHistoryPrune(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	now := time.Now().In(qoderCST)
	old := qoderClaimHistoryFile{Version: 1}
	// 200 天前的记录：必须在 prune 时被删掉。
	old.Records = append(old.Records, QoderClaimRecord{
		Email: "old@test.com", Date: now.AddDate(0, 0, -200).Format("2006-01-02"),
		Phase: QoderPhasePending, At: now.AddDate(0, 0, -200).Format(time.RFC3339),
	})
	// 5000 条今天的记录（at 各不相同；i 越大越早）。
	for i := 0; i < 5000; i++ {
		at := now.Add(-time.Duration(i+1) * time.Minute)
		old.Records = append(old.Records, QoderClaimRecord{
			Email: fmt.Sprintf("u%04d@test.com", i), Date: now.Format("2006-01-02"),
			Phase: QoderPhasePending, At: at.Format(time.RFC3339),
		})
	}
	raw, err := json.MarshalIndent(old, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qoder_claim_history.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := AppendQoderClaim(QoderClaimRecord{
		Email: "new@test.com", Phase: QoderPhaseClaimed, Credits: 100,
	}); err != nil {
		t.Fatalf("追加新记录失败: %v", err)
	}

	recs := QoderClaimHistory("", 0)
	if len(recs) != 5000 {
		t.Fatalf("账本应被裁到 5000 条，实际 %d", len(recs))
	}
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.Email] = true
	}
	if seen["old@test.com"] {
		t.Error("180 天外的记录应被淘汰")
	}
	if !seen["new@test.com"] {
		t.Error("刚写入的新记录不能被淘汰")
	}
	if seen["u4999@test.com"] {
		t.Error("超过 5000 条时应按 at 淘汰最旧（u4999 是最旧的一条）")
	}
	if !seen["u0000@test.com"] {
		t.Error("较新的记录不应被淘汰")
	}
}

// TestQoderClaimHistoryRejectsInvalid 非法 phase / date / at 必须报错且不落盘，
// 避免把脏数据写进审计账本。
func TestQoderClaimHistoryRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	bad := []QoderClaimRecord{
		{Email: "", Phase: QoderPhaseClaimed},
		{Email: "a@test.com", Phase: "success"},
		{Email: "a@test.com", Phase: QoderPhaseClaimed, Date: "2026/11/03"},
		{Email: "a@test.com", Phase: QoderPhaseClaimed, At: "2026-11-03 10:00"},
	}
	for _, rec := range bad {
		if err := AppendQoderClaim(rec); err == nil {
			t.Errorf("非法记录应返回错误: %+v", rec)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "qoder_claim_history.json")); !os.IsNotExist(err) {
		t.Errorf("校验失败不应落盘: %v", err)
	}
}

// TestQoderClaimHistoryEmpty 空账本必须返回空切片（JSON 序列化成 [] 而不是 null）。
func TestQoderClaimHistoryEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)
	recs := QoderClaimHistory("", 0)
	if recs == nil {
		t.Fatal("空账本应返回空切片而不是 nil")
	}
	if len(recs) != 0 {
		t.Fatalf("空账本不应有记录，实际 %d", len(recs))
	}
}

// TestQoderProbeConclusionDefaults 探测结论缺失/未知时返回空串（默认策略），
// 只有三分支合法值才被采纳 —— 避免把未验证的隔离说成可用。
func TestQoderProbeConclusionDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)
	t.Setenv("QODER_PROBE_FILE", "")

	if got := qoderProbeConclusion(); got != "" {
		t.Fatalf("没有探测文件时应返回空串（默认 shared_person），实际 %q", got)
	}

	path := filepath.Join(dir, "probe.json")
	t.Setenv("QODER_PROBE_FILE", path)
	cases := []struct {
		body string
		want string
	}{
		{`{"conclusion":"isolated_viable"}`, "isolated_viable"},
		{`{"conclusion":"server_person_binding"}`, "server_person_binding"},
		{`{"conclusion":"inconclusive"}`, "inconclusive"},
		{`{"conclusion":"totally_made_up"}`, ""},
		{`{"detail":"x"}`, ""},
		{`not json`, ""},
	}
	for _, c := range cases {
		if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := qoderProbeConclusion(); got != c.want {
			t.Errorf("body=%s：conclusion=%q want %q", c.body, got, c.want)
		}
	}
}
