package ideauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 回归：09:00 的自动签到会把「昨天还没刷新掉的已领活动」误判成今日已领取，
// 写进本地签到记录后，10:00 真活动下发时重试循环永远跳过。
// 修复由三层组成，这里逐层验证：
//  1. qoderClassifyCampaigns 只认「今天下发的活动」（startAt ≥ 今日零点 CST）；
//  2. 本地签到记录带时间戳，CheckinRecordTrusted 只信任今日 10:05 后写入的记录；
//  3. 旧格式（纯日期）记录兼容读取，但同样受可信线约束。

func testCampaign(action, status string, startAt int64) QoderCampaign {
	return QoderCampaign{CampaignID: "c-" + action + "-" + status, ActionType: action, ClaimStatus: status, StartAt: startAt, Benefit: QoderBenefit{Amount: 100}}
}

func TestQoderClassifyCampaignsStaleClaimedIsNotToday(t *testing.T) {
	yesterdayStart := time.Now().In(qoderCST).AddDate(0, 0, -1).Unix()
	todayStart := time.Now().In(qoderCST).Add(-time.Hour).Unix()

	claimable, claimedToday, stale := qoderClassifyCampaigns([]QoderCampaign{
		testCampaign("VIEW_DETAILS", "CLAIMABLE", yesterdayStart), // 营销活动：忽略
		testCampaign(qoderClaimAction, "CLAIMED", yesterdayStart), // 昨天的签到活动（陈旧）
	})
	if len(claimable) != 0 || claimedToday != nil {
		t.Fatalf("昨天的已领活动不能算今日已领：claimable=%d claimedToday=%v", len(claimable), claimedToday)
	}
	if stale == nil {
		t.Fatal("昨天的已领活动应落入 stale 桶")
	}

	claimable, claimedToday, stale = qoderClassifyCampaigns([]QoderCampaign{
		testCampaign(qoderClaimAction, "CLAIMED", todayStart),
	})
	if claimedToday == nil || stale != nil {
		t.Fatalf("今天下发的已领活动应算 claimedToday：got claimedToday=%v stale=%v", claimedToday, stale)
	}
	if len(claimable) != 0 {
		t.Fatal("不应有 claimable")
	}

	claimable, _, _ = qoderClassifyCampaigns([]QoderCampaign{
		testCampaign(qoderClaimAction, "CLAIMABLE", todayStart),
	})
	if len(claimable) != 1 {
		t.Fatal("CLAIMABLE 应进 claimable 桶")
	}
}

func TestCheckinRecordTrustedRequiresPost1005(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	// 今日 09:00 的记录：旧版误报形态 → 不可信
	writeLog(t, dir, "qoder:a@x.com", time.Now().Format("2006-01-02")+"T09:00:00+08:00")
	if CheckinRecordTrusted("qoder", "a@x.com") {
		t.Fatal("10:05 前写入的记录不可信")
	}
	if !CheckedInToday("qoder", "a@x.com") {
		t.Fatal("CheckedInToday 仍应按自然日报告 true（用于展示）")
	}

	// 今日 11:00 的记录：可信
	writeLog(t, dir, "qoder:b@x.com", time.Now().Format("2006-01-02")+"T11:00:00+08:00")
	if !CheckinRecordTrusted("qoder", "b@x.com") {
		t.Fatal("10:05 后写入的记录应可信")
	}

	// 旧格式（纯日期）兼容读取；但无时刻信息 → 落在线前 → 不可信
	writeLog(t, dir, "qoder:c@x.com", time.Now().Format("2006-01-02"))
	if !CheckedInToday("qoder", "c@x.com") {
		t.Fatal("旧格式纯日期记录应按当日解析")
	}
	if CheckinRecordTrusted("qoder", "c@x.com") {
		t.Fatal("旧格式记录无法证明在 10:05 后写入，应不可信")
	}

	// 昨天的记录：既不是今日，也不可信
	writeLog(t, dir, "qoder:d@x.com", time.Now().AddDate(0, 0, -1).Format(time.RFC3339))
	if CheckedInToday("qoder", "d@x.com") || CheckinRecordTrusted("qoder", "d@x.com") {
		t.Fatal("昨天的记录不应算今日已签")
	}
}

func writeLog(t *testing.T, dir, key, stamp string) {
	t.Helper()
	path := filepath.Join(dir, "checkin_log.json")
	m := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &struct {
			Entries map[string]string `json:"entries"`
		}{Entries: m})
	}
	m[key] = stamp
	buf, _ := json.Marshal(checkinLog{Entries: m})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
}
