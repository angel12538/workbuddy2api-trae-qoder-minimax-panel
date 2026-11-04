package ideauth

import (
	"strings"
	"testing"
)

// TestQoderFillAlreadyQuotaOwnerSamePersonBlocked 服务端明确回
// SAME_PERSON_ALREADY_CLAIMED（额度被同人的另一个号领走）时，already 号的
// credits 必须归零并点名归属。
//
// 判据来源：GET .../campaigns/{cid}/reward 返回
// status=BLOCKED + failureCode=SAME_PERSON_ALREADY_CLAIMED。
func TestQoderFillAlreadyQuotaOwnerSamePersonBlocked(t *testing.T) {
	results := []QoderCheckinItem{
		{Email: "qq@test.com", Phase: QoderPhaseClaimed, Credits: 100, CheckedInToday: true,
			ClaimCID: "cid-1"},
		{Email: "gmail@test.com", Phase: QoderPhaseAlready, Credits: 100, CheckedInToday: true,
			ClaimCID: "cid-1", QuotaTakenBy: "qq@test.com"},
	}
	qoderFillAlreadyQuotaOwner(results)

	gm := results[1]
	if gm.Credits != 0 {
		t.Errorf("额度被同人另号领走的 already 号 credits 必须归零，实际 %v", gm.Credits)
	}
	if gm.BenefitAmount != 100 {
		t.Errorf("面值应移到 benefit_amount=100，实际 %v", gm.BenefitAmount)
	}
	if !strings.Contains(gm.Msg, "qq@test.com") {
		t.Errorf("文案应点名额度归属，实际 %q", gm.Msg)
	}
	if results[0].Credits != 100 {
		t.Errorf("claimed 号的 credits 必须是它自己的 100，实际 %v", results[0].Credits)
	}
}

// TestQoderFillAlreadyQuotaOwnerKeepsOwnCredits **各领各的**时不能误杀。
//
// 实测 2026-11-04 11:28（GET .../campaigns/{cid}/reward 原始响应）：
//
//	qq     grantId=01a1a477-96d3-…  claimedAt=2026-11-04T02:51:03Z
//	gmail  grantId=01a1a463-e178-…  claimedAt=2026-11-04T02:29:31Z
//
// 两个号看到同一条 campaign，但 grantId / claimedAt 各不相同 —— **各领各的一份**。
// 额度侧也印证：qq 附加包 total=1700（17 份），gmail total=900（9 份）。
//
// 所以在没有明确「被同人另号领走」证据时，already 号的 credits 必须保留。
func TestQoderFillAlreadyQuotaOwnerKeepsOwnCredits(t *testing.T) {
	results := []QoderCheckinItem{
		{Email: "gmail@test.com", Phase: QoderPhaseAlready, Credits: 100, CheckedInToday: true,
			ClaimCID: "cid-1"},
	}
	qoderFillAlreadyQuotaOwner(results)
	if results[0].Credits != 100 {
		t.Errorf("本号自己领到的 already 必须保留 credits，实际 %v", results[0].Credits)
	}
	if results[0].QuotaTakenBy != "" {
		t.Errorf("不该有额度归属，实际 %q", results[0].QuotaTakenBy)
	}
}

// TestQoderClaimRecordAlreadySharedNotCounted 账本里「额度归属别人」的 already
// 不能记 credits（否则两号相加凭空多出 100）。
func TestQoderClaimRecordAlreadySharedNotCounted(t *testing.T) {
	rec, err := normalizeQoderClaimRecord(QoderClaimRecord{
		Email: "gmail@test.com", Date: "2026-11-04", Phase: QoderPhaseAlready,
		Credits: 100, QuotaTakenBy: "qq@test.com", At: "2026-11-04T10:51:07+08:00",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.Credits != 0 {
		t.Errorf("额度归属别人的 already 记录 credits 必须为 0，实际 %v", rec.Credits)
	}
	// 本号自己领的 already 仍然保留 credits。
	rec2, err := normalizeQoderClaimRecord(QoderClaimRecord{
		Email: "a@test.com", Date: "2026-11-04", Phase: QoderPhaseAlready,
		Credits: 100, At: "2026-11-04T10:51:07+08:00",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec2.Credits != 100 {
		t.Errorf("本号自己领的 already 必须保留 credits，实际 %v", rec2.Credits)
	}
}
