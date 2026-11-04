package appserver

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// Qoder 签到自动重试。
//
// 为什么需要：Qoder 的每日活动**不是零点/十点准时下发**，服务端会先返回空列表
// （社区实现同样把这种情况标成 pending）。手动点一次如果正好赶上"未下发"，
// 用户就会以为"签不了"。这里每 30 分钟补试一次：只要活动一出现就自动领，
// 已领过的账号由本地记录兜底，不会重复到账（上游幂等）。
//
// v2 语义修正：此前「全部账号本地已签就整体跳过」的短路被证明是误报放大器 ——
// 09:00 的自动签到会被**昨天还没刷新掉的已领活动**骗成"今日已领取"写进本地
// 记录，之后 10:00 真活动下发，重试循环却因为这条假记录永远不再跑。现在：
//   - 误报源头已修复（qoderCheckinAuth 只认「今天下发的活动」CLAIMED）；
//   - 本地记录改为带时间戳；10:05 前写入的记录视为「可疑」，不参与跳过判定
//     —— 每天上午重试循环照常巡检，真实签到只可能发生在活动下发之后。
//
// v3（2026-11-04）修的是「用户看不到任何动静」：
//   - 旧版只在"领到了 / 已领过"时才打日志。于是"上游还没下发"这一种状态
//     **完全静默** —— 日志里连一条记录都没有，用户打开面板只看到"没签上"，
//     根本不知道后台还在盯着。实测 11-04 10:00→10:10 全天零条 qoder-retry 日志。
//     现在 pending 也要打，并且写清"上游未下发 + 下次重试时刻"。
//   - 重试节奏按活动窗口加密：10:00 前后是活动下发的高峰（实测 11-03 是
//     10:00:02 失败、10:02:01 重试就领到了），这段用 5 分钟；其余时段 30 分钟。
const (
	// qoderRetryIdleInterval 非活动窗口的重试间隔。
	qoderRetryIdleInterval = 30 * time.Minute
	// qoderRetryHotInterval 活动窗口（10:00 前后）的重试间隔。
	qoderRetryHotInterval = 5 * time.Minute
	// qoderHotWindowStart/qoderHotWindowEnd 活动窗口（按 UTC+8 的钟点）。
	// 上游的活动窗口是 10:00 → 次日 09:59，所以 09:40 起就该进入加密巡检。
	qoderHotWindowStart = 9*60 + 40
	qoderHotWindowEnd   = 11*60 + 30
)

// qoderCST Qoder 活动窗口用的时区（上游按 UTC+8 刷新，实测 11-03 10:00 下发）。
var qoderCST = time.FixedZone("UTC+8", 8*60*60)

// qoderInHotWindow 当前是否处在活动下发的高峰时段。
func qoderInHotWindow(now time.Time) bool {
	m := now.In(qoderCST).Hour()*60 + now.In(qoderCST).Minute()
	return m >= qoderHotWindowStart && m <= qoderHotWindowEnd
}

// qoderRetryInterval 下一轮重试的间隔：活动窗口内 5 分钟，其余 30 分钟。
func qoderRetryInterval(now time.Time) time.Duration {
	if qoderInHotWindow(now) {
		return qoderRetryHotInterval
	}
	return qoderRetryIdleInterval
}

func startQoderAutoCheckin(ctx context.Context) {
	go func() {
		// 启动后先等 3 分钟，避开启动瞬间的模型目录预热/余额刷新。
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Minute):
		}
		run := func() {
			now := time.Now()
			pool, err := ideauth.LoadQoderPool()
			if err != nil || pool == nil || len(pool.Accounts) == 0 {
				return
			}
			// 可信记录（今日 10:05 后写入）的账号无需巡检；其余账号照常参与。
			pending := false
			for _, a := range pool.Accounts {
				if a == nil {
					continue
				}
				if ideauth.CheckinRecordTrusted("qoder", a.Email) {
					continue
				}
				pending = true
				break
			}
			if !pending {
				return
			}
			res, err := ideauth.QoderCheckinPool(nil)
			if err != nil || res == nil {
				if err != nil {
					log.Printf("[qoder-retry] 签到重试失败: %v", err)
				}
				return
			}
			// **任何一种结果都要留痕**：包括"上游还没下发"（pending）。
			// 静默是这次修复的核心 —— 用户看不到日志就以为功能坏了。
			log.Printf("[qoder-retry] %s；下次重试 %s", qoderRetrySummary(res),
				now.Add(qoderRetryInterval(now)).Format("15:04:05"))
		}
		run()
		for {
			// 间隔按当前时刻动态决定：活动窗口内 5 分钟，其余 30 分钟。
			wait := qoderRetryInterval(time.Now())
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				run()
			}
		}
	}()
}

func qoderRetrySummary(res *ideauth.QoderCheckinPoolResult) string {
	s := fmt.Sprintf("本次签到 %d 个 · 今日已领 %d 个 · 同一人另号已领 %d 个 · 未下发 %d 个 · 失败 %d 个",
		res.Claimed, res.Already, res.Blocked, res.Pending, res.Failed)
	if res.Credits > 0 {
		s += fmt.Sprintf("，新增 +%.0f Credits", res.Credits)
	}
	return s
}
