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

	"github.com/linguo2625469/workbuddy2api-panel/internal/doubao"
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
			log.Printf("panel: IDE 号池自动签到已排程（Qoder/WorkBuddy/MiniMax/豆包），下次 %s", next.Format("01-02 15:04"))
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

// StartMinimaxRenewAuto 启动 MiniMax 凭证自动续期。
//
// access token 只有 1 小时、refresh token 每刷必轮换 —— 号池副本不续就会全部
// 过期（401）。这里每 30 分钟巡检一次：临期（30 分钟内过期）的账号自动续期，
// 续出的新 refresh token 同步回客户端 auth.json（与客户端共享同一条 RT 链，
// 谁先刷都不掉）。与签到排程独立：签到 1 天 1 次，续期 1 天 ~48 次。
func (p *Panel) StartMinimaxRenewAuto(ctx context.Context) {
	if p.minimaxClient == nil {
		return
	}
	go func() {
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				results := p.minimaxClient.RenewAll(ctx, false)
				renewed, adopted, failed := 0, 0, 0
				for _, r := range results {
					switch r.Phase {
					case "renewed":
						renewed++
					case "adopted":
						adopted++
					case "error":
						failed++
						log.Printf("panel: MiniMax 自动续期失败 %s: %s", r.Label, r.Msg)
					}
				}
				if renewed > 0 || adopted > 0 || failed > 0 {
					log.Printf("panel: MiniMax 自动续期巡检：续期 %d / 同步 %d / 失败 %d", renewed, adopted, failed)
				}
			}
		}
	}()
}

// ideCheckinAll 自动排程用的 IDE 号池全量签到（无 HTTP 上下文，结果只进日志）。
func (p *Panel) ideCheckinAll(ctx context.Context) {
	p.ideQoderCheckinAll()
	p.ideWorkBuddyCheckinAll()
	p.ideMiniMaxCheckinAll(ctx)
	p.ideDoubaoCheckinAll(ctx)
	// 不再有「切号逐签」兜底：2026-10-24 实测证明 Qoder 的每日额度是**按人**给的
	// （服务端 failureCode=SAME_PERSON_ALREADY_CLAIMED），跟「这个号有没有在客户端
	// 登录过」无关 —— 切号并不会让同一个人的另一个号领到额度，只会把客户端反复
	// 重启（每号一次）。所以这里改成：每个号都用它自己的 token 直接打接口，
	// 领不到就如实报「同一人另一账号已领」。
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

// ideDoubaoCheckinAll 豆包号池逐号签到（CDP 通道，串行，每号一个无头会话）。
// 结果同样写本地签到记录（checkin_log.json），失败只进日志。
func (p *Panel) ideDoubaoCheckinAll(ctx context.Context) {
	pool, err := doubao.LoadPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		return
	}
	ok, already, failed := 0, 0, 0
	for _, a := range pool.Accounts {
		if a == nil || a.SessionID == "" {
			continue
		}
		// 可信的今日记录：跳过（CDP 每号要开一个无头浏览器，能省则省）。
		if ideauth.CheckinRecordTrusted("doubao", a.ID) {
			already++
			continue
		}
		res := p.doubaoSigninOne(a)
		switch res.Phase {
		case "claimed":
			_ = ideauth.MarkCheckedIn("doubao", a.ID)
			ok++
		case "already":
			_ = ideauth.MarkCheckedIn("doubao", a.ID)
			already++
		default:
			failed++
			log.Printf("panel: 豆包自动签到失败 %s: %s", a.Label, res.Msg)
		}
	}
	if len(pool.Accounts) > 0 {
		log.Printf("panel: 豆包自动签到完成：成功 %d / 已签 %d / 失败 %d", ok, already, failed)
	}
}
