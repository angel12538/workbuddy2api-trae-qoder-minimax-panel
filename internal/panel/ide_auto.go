// ide_auto.go IDE 号池（Qoder / WorkBuddy / MiniMax）的每日自动签到排程。
//
// 背景：面板「一键签到」能覆盖主号池 + WorkBuddy + Qoder + MiniMax + Trae，
// 但**自动排程**此前只有主号池（scheduler）与 Trae（panel.StartTraeAuto）。
// Qoder / WorkBuddy / MiniMax 三个号池只能手动点按钮 —— 「每天一签到」对它们
// 并不成立，只能靠人记得打开面板。
//
// 本文件补上这一段：与 Trae 的 StartTraeAuto 同构（独立时点、独立开关、独立
// goroutine），不改动调度器核心（taskKind 分发），降低回归面。
//
// 三个平台的签到都只需要「该账号自己的 token + 设备指纹」，不需要切号、不需要
// 客户端在线，因此可以按号池逐个账号直接打接口（与面板按钮走的是同一批函数）。
package panel

import (
	"context"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/minimax"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// StartIDECheckinAuto 启动 IDE 号池每日自动签到（hours 为空则不启动）。
//
// 时点语义与主号池签到一致（本地时区整点）。单个平台失败不影响其它平台 ——
// 一个号的凭证过期不该让另外两个号也签不上。
func (p *Panel) StartIDECheckinAuto(ctx context.Context, hours []int) {
	if len(hours) == 0 {
		return
	}
	go func() {
		for {
			next := nextTraeFire(time.Now(), hours)
			log.Printf("panel: IDE 号池自动签到已排程（Qoder/WorkBuddy/MiniMax），下次 %s", next.Format("01-02 15:04"))
			// 用 NewTimer 而不是 time.After：ctx 取消时 time.After 的定时器不会被回收，
			// 下一次时点最远 24h 才触发，等于白挂一个 24h 的 timer（进程长期运行会累积）。
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			p.ideCheckinAll(ctx)
		}
	}()
}

// ideCheckinAll 自动排程用的 IDE 号池全量签到（无 HTTP 上下文，结果只进日志）。
func (p *Panel) ideCheckinAll(ctx context.Context) {
	p.ideQoderCheckinAll()
	p.ideWorkBuddyCheckinAll()
	p.ideMiniMaxCheckinAll(ctx)
}

// ideQoderCheckinAll Qoder 号池逐号签到。
func (p *Panel) ideQoderCheckinAll() {
	res, err := ideauth.QoderCheckinPool(nil) // nil = 号池全部账号
	if err != nil {
		log.Printf("panel: Qoder 自动签到失败: %v", err)
		return
	}
	log.Printf("panel: Qoder 自动签到完成：成功 %d / 已签 %d / 失败 %d",
		res.Claimed, res.Already, res.Failed)
}

// ideWorkBuddyCheckinAll WorkBuddy 号池逐号签到。
//
// 语义与面板按钮一致：先查 status 预检（失败不阻断），再 claim；
// 上游以「今天已签到」拒绝时同样计入「已签」。
func (p *Panel) ideWorkBuddyCheckinAll() {
	accounts, err := ideauth.ListWorkBuddyAccounts()
	if err != nil {
		log.Printf("panel: WorkBuddy 自动签到读取账号失败: %v", err)
		return
	}
	if len(accounts) == 0 {
		return
	}
	ok, already, failed := 0, 0, 0
	for _, a := range accounts {
		if a.UID == "" {
			continue
		}
		snap, err := ideauth.FindWorkBuddyAccount(a.UID)
		if err != nil {
			log.Printf("panel: WorkBuddy 自动签到跳过 uid=%s：%v", a.UID, err)
			failed++
			continue
		}
		// 预检：已签则不再 claim（与面板同一语义）。预检失败仍尝试 claim。
		_, _ = ideauth.WorkBuddyGetCheckinStatus(snap)
		if _, cerr := ideauth.WorkBuddyClaimDailyCheckin(snap); cerr != nil {
			if upstream.IsAlreadyCheckin(cerr) {
				_ = ideauth.MarkCheckedIn("workbuddy", a.UID)
				already++
			} else {
				log.Printf("panel: WorkBuddy 自动签到失败 uid=%s: %v", a.UID, cerr)
				failed++
			}
			continue
		}
		_ = ideauth.MarkCheckedIn("workbuddy", a.UID)
		ok++
	}
	log.Printf("panel: WorkBuddy 自动签到完成：成功 %d / 已签 %d / 失败 %d", ok, already, failed)
}

// ideMiniMaxCheckinAll MiniMax 号池逐号签到（token 级，无需切号）。
func (p *Panel) ideMiniMaxCheckinAll(ctx context.Context) {
	if p.minimaxClient == nil {
		return
	}
	results, err := minimax.SigninAccounts(ctx, p.minimaxClient, nil) // nil = 全部
	if err != nil {
		log.Printf("panel: MiniMax 自动签到失败: %v", err)
		return
	}
	ok, already, failed := 0, 0, 0
	for _, r := range results {
		switch r.Phase {
		case "claimed":
			ok++
		case "already":
			already++
		default:
			failed++
			log.Printf("panel: MiniMax 自动签到失败 %s: %s", r.Label, r.Msg)
		}
	}
	log.Printf("panel: MiniMax 自动签到完成：成功 %d / 已签 %d / 失败 %d", ok, already, failed)
}
