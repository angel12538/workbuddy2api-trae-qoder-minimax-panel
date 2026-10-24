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
func startQoderAutoCheckin(ctx context.Context) {
	const interval = 30 * time.Minute
	go func() {
		// 启动后先等 3 分钟，避开启动瞬间的模型目录预热/余额刷新。
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Minute):
		}
		run := func() {
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
			if res.Claimed > 0 || res.Already > 0 {
				log.Printf("[qoder-retry] %s", qoderRetrySummary(res))
			}
		}
		run()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				run()
			}
		}
	}()
}

func qoderRetrySummary(res *ideauth.QoderCheckinPoolResult) string {
	s := fmt.Sprintf("本次签到 %d 个 · 今日已领 %d 个 · 失败 %d 个", res.Claimed, res.Already, res.Failed)
	if res.Credits > 0 {
		s += fmt.Sprintf("，新增 +%.0f Credits", res.Credits)
	}
	return s
}
