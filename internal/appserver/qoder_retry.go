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
			// 全部账号今天都已签过就不打上游。
			pending := false
			for _, a := range pool.Accounts {
				if a == nil {
					continue
				}
				if !ideauth.CheckedInToday("qoder", a.Email) {
					pending = true
					break
				}
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
