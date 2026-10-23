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
// （无论 claimed 还是 already）落一条「uid → 日期」。UI 据此把已签到账号的按钮变灰。
//
// 记录只代表「本面板观察到该账号在今天完成过签到」，不代表上游的权威状态。

var checkinLogMu sync.Mutex

type checkinLog struct {
	// WorkBuddy / Qoder 共用一个文件，按 "kind:key" 分桶。
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

// MarkCheckedIn 记录某签到对象今日已签到。kind ∈ {workbuddy, qoder}。
func MarkCheckedIn(kind, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	m := loadCheckinLog()
	m[kind+":"+key] = time.Now().Format("2006-01-02")
	raw, err := json.MarshalIndent(checkinLog{Entries: m}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(checkinLogPath(), raw)
}

// CheckedInToday 报告某签到对象今日是否已签到。
func CheckedInToday(kind, key string) bool {
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	return loadCheckinLog()[kind+":"+strings.TrimSpace(key)] == time.Now().Format("2006-01-02")
}

// CheckinDates 返回 kind 下全部 key → 日期 映射（用于批量列表渲染）。
func CheckinDates(kind string) map[string]string {
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	out := map[string]string{}
	prefix := kind + ":"
	for k, v := range loadCheckinLog() {
		if strings.HasPrefix(k, prefix) {
			out[strings.TrimPrefix(k, prefix)] = v
		}
	}
	return out
}
