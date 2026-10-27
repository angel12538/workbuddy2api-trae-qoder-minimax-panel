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
	// Claims 记录**真实领取**（phase=claimed）的时刻，与 Entries 分开。
	//
	// 为什么分开：Entries 记的是「今天签过」（claimed 与 already 都算），用来
	// 展示；Claims 记的是「这一天真正把 100 积分领到手」的时刻。Qoder 的每日
	// 额度服务端按**人**给（同一人多个账号一天只有一份，见 qoder_pool_checkin.go
	// 的实测记录），所以「让哪个号先打」决定了谁拿到那 100 积分 —— 要做公平
	// 轮换，就必须知道每个号上次真正领到是什么时候。
	Claims map[string]string `json:"claims,omitempty"`
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

// loadCheckinLogFile 读整个日志文件（entries + claims）。
func loadCheckinLogFile() checkinLog {
	var f checkinLog
	raw, err := os.ReadFile(checkinLogPath())
	if err != nil {
		return checkinLog{Entries: map[string]string{}, Claims: map[string]string{}}
	}
	if json.Unmarshal(raw, &f) != nil {
		return checkinLog{Entries: map[string]string{}, Claims: map[string]string{}}
	}
	if f.Entries == nil {
		f.Entries = map[string]string{}
	}
	if f.Claims == nil {
		f.Claims = map[string]string{}
	}
	return f
}

// saveCheckinLogFile 写回整个日志文件。
func saveCheckinLogFile(f checkinLog) error {
	buf, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(checkinLogPath(), buf)
}

// MarkCheckedIn 记录某签到对象今日已签到。kind ∈ {workbuddy, qoder, doubao}。
func MarkCheckedIn(kind, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	f := loadCheckinLogFile()
	f.Entries[kind+":"+key] = time.Now().Format(time.RFC3339)
	return saveCheckinLogFile(f)
}

// MarkClaimed 记录某账号**真正领到**积分的时刻（phase=claimed，不是 already）。
//
// 用途：Qoder 的每日额度服务端按「人」给，同一人的多个账号一天只有一份，
// 谁先打谁拿到。轮换策略据此排序，保证两个号都能轮到。
func MarkClaimed(kind, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	f := loadCheckinLogFile()
	f.Claims[kind+":"+key] = time.Now().Format(time.RFC3339)
	return saveCheckinLogFile(f)
}

// LastClaimedAt 返回某账号最后一次**真正领到**积分的时刻（没有记录返回 false）。
func LastClaimedAt(kind, key string) (time.Time, bool) {
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	f := loadCheckinLogFile()
	return parseCheckinStamp(f.Claims[kind+":"+strings.TrimSpace(key)])
}

// UnmarkCheckedIn 撤销某签到对象的「今日已签到」记录。
//
// 什么时候需要：服务端明确答复「同一个人今天已经领过了」时，说明**本号**今天
// 并没有签到 —— 之前若有本地记录，那是误记（例如把被服务端拒绝的领取也写进了
// 记录），必须清掉，否则面板会显示成"今日已签到"（假签到）。
func UnmarkCheckedIn(kind, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	f := loadCheckinLogFile()
	k := kind + ":" + key
	if _, ok := f.Entries[k]; !ok {
		return nil
	}
	delete(f.Entries, k)
	delete(f.Claims, k)
	return saveCheckinLogFile(f)
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

// CheckinRecordTrusted 报告本地签到记录是否「可信」。
//
// 「可信」的分界线**按平台区分**（见 checkinTrustLine）：各平台活动刷新时刻
// 不同，用一条线套所有平台会误伤。
func CheckinRecordTrusted(kind, key string) bool {
	t, ok := CheckedInAt(kind, key)
	if !ok {
		return false
	}
	if t.Local().Format("2006-01-02") != time.Now().Format("2006-01-02") {
		return false // 非今日记录一律不可信（已过期）
	}
	return !t.Before(checkinTrustLine(kind))
}

// checkinTrustLine 本地签到记录的可信分界线（今日本地时间的时刻）。
//
// 分平台的原因：
//   - Qoder：每日活动 10:00 (UTC+8) 刷新。10:05 前写下的记录可能只是**昨天**
//     那波还没刷新的陈旧活动（旧版把「昨天已领」当成「今天已领」写进记录），
//     所以要等到 10:05 真活动下发后才可信。带时间戳的记录让我们能把这些历史
//     假记录自动降级为「可疑」——重试循环会继续巡检，旧假记录不阻塞任何流程。
//   - 豆包 / WorkBuddy / CodeArts / MiniMax 等：每日签到按**自然日**刷新
//     （零点起算），上游接口直接给出 checked_in_today / last_checkin_date，
//     不存在「昨天的活动冒充今天」的问题。对它们套用 10:05 线会让**上午就
//     签到成功**的号在面板上一整天显示「未签」——用户看到的就是「签到失效」
//     （2026-10-27 实测：08:47 豆包两个号签到成功，号池列表仍返回
//     checked_in_today=false，按钮一直是蓝底可点）。
func checkinTrustLine(kind string) time.Time {
	now := time.Now()
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "qoder":
		return time.Date(now.Year(), now.Month(), now.Day(), 10, 5, 0, 0, time.Local)
	default:
		// 自然日刷新：今日零点起就算可信。
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	}
}

// qoderCheckinTrustLine 本地签到记录的可信分界线（今日 10:05 本地时间）。
func qoderCheckinTrustLine() time.Time {
	return checkinTrustLine("qoder")
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
