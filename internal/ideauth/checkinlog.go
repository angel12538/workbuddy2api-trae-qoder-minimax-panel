package ideauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 签到日期记录：面板无法可靠地向上游「查询今天是否已签到」，因此对每一次成功的签到
// （无论 claimed 还是 already）落一条「uid → 时刻」。UI 据此把已签到账号的按钮变灰。
//
// 记录只代表「本面板观察到该账号在今天完成过签到」，不代表上游的权威状态。
//
// v2 起存完整时间戳（RFC3339）而不是纯日期：Qoder 的每日活动在**上午 10:00**
// （UTC+8）才刷新，此前的「已签」只可能来自昨天的陈旧活动——重试循环与扫描
// 需要知道「这条记录是几点写的」，才能区分「10 点前的可疑记录」与「10 点后的
// 可信记录」。旧格式（纯日期字符串）仍然兼容读取。

var checkinLogMu sync.Mutex

type checkinLog struct {
	// WorkBuddy / Qoder / 豆包共用一个文件，按 "kind:key" 分桶。
	// 值为 RFC3339 时间戳；历史文件可能是纯日期（2006-01-02）。
	Entries map[string]string `json:"entries"`
}

func checkinLogPath() string {
	return filepath.Join(PoolDir(), "checkin_log.json")
}

func loadCheckinLog() map[string]string {
	raw, err := os.ReadFile(checkinLogPath())
	if err != nil {
		return map[string]string{}
	}
	var f checkinLog
	if json.Unmarshal(raw, &f) != nil || f.Entries == nil {
		return map[string]string{}
	}
	return f.Entries
}

// MarkCheckedIn 记录某签到对象今日已签到。kind ∈ {workbuddy, qoder, doubao}。
func MarkCheckedIn(kind, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	m := loadCheckinLog()
	m[kind+":"+key] = time.Now().Format(time.RFC3339)
	raw, err := json.MarshalIndent(checkinLog{Entries: m}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(checkinLogPath(), raw)
}

// parseCheckinStamp 解析记录值：RFC3339 时间戳优先，纯日期回落当日 00:00。
func parseCheckinStamp(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// CheckedInToday 报告某签到对象今日是否已签到（本地时区的自然日）。
func CheckedInToday(kind, key string) bool {
	t, ok := CheckedInAt(kind, key)
	if !ok {
		return false
	}
	return t.Local().Format("2006-01-02") == time.Now().Format("2006-01-02")
}

// CheckedInAt 返回最近一次签到记录的时刻。历史纯日期记录回落为当日 00:00。
func CheckedInAt(kind, key string) (time.Time, bool) {
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	t, ok := parseCheckinStamp(loadCheckinLog()[kind+":"+strings.TrimSpace(key)])
	return t, ok
}

// CheckinRecordTrusted 报告本地签到记录是否「可信」：
// 今日 10:05（活动刷新 + 5 分钟宽限）之后写入的记录才可信。
//
// 背景：Qoder 每日活动 10:00 (UTC+8) 刷新，此前版本会把昨天还没刷新掉的已领
// 活动误判成"今日已领取"，在 10:00 前写入假记录。带时间戳的记录让我们能把
// 这些历史假记录自动降级为「可疑」——重试循环会继续巡检，10 点后真活动下发
// 时照样能补签，旧假记录不再阻塞任何流程。
func CheckinRecordTrusted(kind, key string) bool {
	t, ok := CheckedInAt(kind, key)
	if !ok {
		return false
	}
	if t.Local().Format("2006-01-02") != time.Now().Format("2006-01-02") {
		return false // 非今日记录一律不可信（已过期）
	}
	return t.After(qoderCheckinTrustLine())
}

// qoderCheckinTrustLine 本地签到记录的可信分界线（今日 10:05 本地时间）。
func qoderCheckinTrustLine() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 10, 5, 0, 0, time.Local)
}

// CheckinDates 返回 kind 下全部 key → 日期 映射（用于批量列表渲染）。
func CheckinDates(kind string) map[string]string {
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	out := map[string]string{}
	prefix := kind + ":"
	for k, v := range loadCheckinLog() {
		if strings.HasPrefix(k, prefix) {
			if t, ok := parseCheckinStamp(v); ok {
				out[strings.TrimPrefix(k, prefix)] = t.Local().Format("2006-01-02")
			} else {
				out[strings.TrimPrefix(k, prefix)] = v
			}
		}
	}
	return out
}
