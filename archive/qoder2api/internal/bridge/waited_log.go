package bridge

import (
	"sync"
	"time"
)

// WaitEpisode 记录一次「排队等待」片段：从首次命中排队到最终离开队列。
// 用于控制台「最近等待记录」面板，帮助判断各模型/账号的排队时长。
type WaitEpisode struct {
	Time         time.Time `json:"time"`           // 记录时间
	Model        string    `json:"model"`          // 模型 key
	Account      string    `json:"account"`        // 账号名
	Region       string    `json:"region"`         // cn / global
	Tries        int       `json:"tries"`          // 排队重试次数
	TotalWaitSec int       `json:"total_wait_sec"` // 累计等待秒数
	Outcome      string    `json:"outcome"`        // success / gave_up / cancelled / error
}

// 结果枚举
const (
	WaitOutcomeSuccess   = "success"   // 等到首字、请求成功
	WaitOutcomeGaveUp    = "gave_up"   // 按次数封顶用尽仍未穿过
	WaitOutcomeCancelled = "cancelled" // 等待期间客户端断开
	WaitOutcomeError     = "error"     // 排队后遇到其它上游错误
)

// waitLog 是固定容量的环形缓冲，保存最近 N 条排队记录（仅内存，不落盘）。
type waitLog struct {
	mu      sync.Mutex
	cap     int
	entries []WaitEpisode
}

const waitLogCapacity = 50

var recentWaitLog = &waitLog{cap: waitLogCapacity}

// record 追加一条排队记录（超出容量时淘汰最旧）。
func (w *waitLog) record(e WaitEpisode) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.entries) >= w.cap {
		// 淘汰最旧一条，保持容量
		w.entries = append(w.entries[1:], e)
		return
	}
	w.entries = append(w.entries, e)
}

// snapshot 返回最近记录的副本（最新在前）。
func (w *waitLog) snapshot() []WaitEpisode {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]WaitEpisode, len(w.entries))
	for i, e := range w.entries {
		out[len(w.entries)-1-i] = e // 反转：最新在前
	}
	return out
}

// RecordWait 记录一次排队片段（供 bridge 内部调用）。
func RecordWait(model, account, region, outcome string, tries int, totalWait time.Duration) {
	recentWaitLog.record(WaitEpisode{
		Model:        model,
		Account:      account,
		Region:       region,
		Tries:        tries,
		TotalWaitSec: int(totalWait.Round(time.Second) / time.Second),
		Outcome:      outcome,
	})
}

// RecentWaitLog 返回最近排队记录（最新在前），供控制台 API 使用。
func RecentWaitLog() []WaitEpisode {
	return recentWaitLog.snapshot()
}
