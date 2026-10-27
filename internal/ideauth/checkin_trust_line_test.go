package ideauth

import (
	"testing"
	"time"
)

// 回归：可信分界线曾是一条**全局**的「今日 10:05」线，那是 Qoder 专属规则
// （Qoder 每日活动 10:00 刷新，10:05 前写下的记录可能只是昨天那波陈旧活动）。
// 豆包 / WorkBuddy / CodeArts 的每日签到是**自然日**刷新，被套上同一条线后，
// 上午就签到成功的号在面板上一整天显示「未签」，按钮一直是蓝底可点 —— 用户
// 看到的就是「签到失效」（2026-10-27 实测：08:47 豆包两个号签到成功，
// /panel/api/ide/doubao/accounts 仍返回 checked_in_today=false）。
//
// 这里钉死：分界线按平台区分 —— Qoder 仍是 10:05，其余平台是今日零点。

func todayAt(t *testing.T, h, m int) string {
	t.Helper()
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, time.Local).Format(time.RFC3339)
}

func TestCheckinTrustLinePerPlatform(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	cases := []struct {
		kind string
		key  string
		stamp string
		want bool
		why  string
	}{
		// 豆包：自然日刷新，上午签到成功就算可信（**本次回归点**）。
		{"doubao", "d1", todayAt(t, 8, 47), true, "豆包 08:47 的签到记录应可信"},
		{"doubao", "d2", todayAt(t, 0, 1), true, "豆包 00:01 的签到记录应可信"},
		{"doubao", "d3", todayAt(t, 9, 0), true, "豆包 09:00 的签到记录应可信"},
		// 豆包：旧格式纯日期记录 = 那天签的，自然日平台可信。
		{"doubao", "d4", time.Now().Format("2006-01-02"), true, "豆包纯日期记录应按当日可信"},
		// WorkBuddy / CodeArts 同理（自然日刷新）。
		{"workbuddy", "w1", todayAt(t, 7, 30), true, "WorkBuddy 07:30 的签到记录应可信"},
		{"codearts", "c1", todayAt(t, 7, 30), true, "CodeArts 07:30 的签到记录应可信"},
		// Qoder 保持 10:05 线不变（活动 10:00 刷新）。
		{"qoder", "q1", todayAt(t, 9, 0), false, "Qoder 10:05 前写入的记录不可信"},
		{"qoder", "q2", todayAt(t, 11, 0), true, "Qoder 10:05 后写入的记录可信"},
	}
	for _, c := range cases {
		writeLog(t, dir, c.kind+":"+c.key, c.stamp)
		if got := CheckinRecordTrusted(c.kind, c.key); got != c.want {
			t.Errorf("%s：%s（got=%v want=%v）", c.kind, c.why, got, c.want)
		}
	}
}

func TestCheckinRecordTrustedRejectsYesterday(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	// 昨天的记录在任何平台都不可信（自然日平台也不能拿昨天的当今天）。
	for _, kind := range []string{"doubao", "workbuddy", "codearts", "qoder"} {
		key := kind + "-yesterday"
		writeLog(t, dir, kind+":"+key, time.Now().AddDate(0, 0, -1).Format(time.RFC3339))
		if CheckinRecordTrusted(kind, key) {
			t.Errorf("%s：昨天的记录不应算今日已签", kind)
		}
		if CheckedInToday(kind, key) {
			t.Errorf("%s：CheckedInToday 不应把昨天的记录算成今日", kind)
		}
	}
}

// 兜底：没有记录时一律不可信。
func TestCheckinRecordTrustedWithoutRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)
	for _, kind := range []string{"doubao", "workbuddy", "codearts", "qoder"} {
		if CheckinRecordTrusted(kind, "nobody") {
			t.Errorf("%s：没有记录时不应算已签", kind)
		}
	}
}
