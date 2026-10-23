// trae.go 面板「Trae 签到」：把 Trae（api.trae.cn）的每日签到并进本面板。
//
// 背景：用户原本要同时开「Trae Work 助手」和本面板、各点一次签到。本模块让
// 面板一站式完成——账号直接复用 Trae 助手落盘的 checkin_accounts.json
// （同一份数据，不重复维护），设备指纹也按同一算法派生，两边签到互不冲突。
//
// 接口：
//   - GET  /panel/api/trae/accounts  账号列表 + 今日签到状态（只读）
//   - POST /panel/api/trae/checkin   一键签到（可选 {"user_ids":[...]} 指定账号）
//
// 自动签到：StartTraeAuto 按配置时点跑，与主池的签到排程互不影响。
package panel

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// traeMu 串行化 Trae 签到：手动点击与自动排程共用一把锁，避免同一账号并发 claim。
var traeMu sync.Mutex

// traeAccountView 面板展示口径的 Trae 账号条目。
type traeAccountView struct {
	Name      string  `json:"name"`
	UserID    string  `json:"user_id"`
	CheckedIn bool    `json:"checked_in"`
	Credits   float64 `json:"credits,omitempty"`
	Message   string  `json:"message,omitempty"`
	Error     string  `json:"error,omitempty"`
	// Expired 凭证已过期（前端用独立样式 + 明确引导，不与「网络/上游故障」混为一谈）。
	Expired bool `json:"expired,omitempty"`
	// ExpiresAt 凭证到期时刻（本地 JWT exp，未过期时也透出，便于用户提前安排刷新）。
	ExpiresAt string `json:"expires_at,omitempty"`
	// Refreshable 该账号带 refresh_token → 可走 API 通道自动续期（最快）。
	Refreshable bool `json:"refreshable,omitempty"`
	// HasProfile 该账号有绑定的浏览器会话 → 可走无头浏览器通道自动续期。
	HasProfile bool `json:"has_profile,omitempty"`
}

// traeCheckinResult 单账号签到结果。
type traeCheckinResult struct {
	Name    string `json:"name"`
	UserID  string `json:"user_id"`
	Status  string `json:"status"` // claimed | already | fail
	Message string `json:"message,omitempty"`
}

// traeLoadAccounts 读取 Trae 账号（配置目录优先，回落 %APPDATA%\TraeWorkAssistant）。
func (p *Panel) traeLoadAccounts() ([]upstream.TraeAccount, error) {
	return upstream.LoadTraeAccounts(p.cfg.TraeDataDir)
}

// traeAccounts 列出 Trae 账号及其今日签到状态（只读，逐个查上游 status）。
//
// 上游 status 失败不视为整体错误：该账号标 error，其余照常返回——
// 用户最需要的是"哪些号还没签"，不该因一个号网络抖动整页打不开。
func (p *Panel) traeAccounts(w http.ResponseWriter, r *http.Request) {
	accts, err := p.traeLoadAccounts()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "读取 Trae 账号失败: "+err.Error())
		return
	}
	views := make([]traeAccountView, len(accts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3) // 上游限流：最多 3 并发
	for i, a := range accts {
		wg.Add(1)
		go func(i int, a upstream.TraeAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v := traeAccountView{Name: a.Name, UserID: a.UserID}
			// 续期能力透出给前端：有 refresh_token 走 API，否则看有没有绑定的浏览器会话。
			v.Refreshable = strings.TrimSpace(a.RefreshToken) != ""
			if st, err := os.Stat(traeProfileDirFor(p, a.UserID)); err == nil && st.IsDir() {
				v.HasProfile = true
			}
			// 本地 JWT 已过期就直接判死并跳过上游请求：既省一次必然失败的调用，
			// 也避免把上游那句英文鉴权错误原样丢给用户（他无从得知该去重登账号）。
			if exp, ok := upstream.TraeJWTExpiry(a.JWT); ok {
				v.ExpiresAt = exp.Local().Format("2006-01-02 15:04")
				if time.Now().After(exp) {
					v.Expired = true
					v.Error = upstream.TraeAuthHint(a)
					views[i] = v
					return
				}
			}
			checkedIn, credits, msg, err := upstream.TraeStatus(a)
			if err != nil {
				v.Error = err.Error()
			} else {
				v.CheckedIn, v.Credits, v.Message = checkedIn, credits, msg
			}
			views[i] = v
		}(i, a)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"enabled":  p.cfg.TraeEnabled,
		"data_dir": p.cfg.TraeDataDir,
		"accounts": views,
	})
}

// traeCheckin 一键签到。body 可给 {"user_ids":[...]} 只签指定账号；空则全部。
//
// 流程对齐 auto_checkin.py：先 status 预检，已签则跳过（不重复 claim），
// 否则 claim。预检失败仍尝试 claim（上游 status 偶发失败不应阻断签到）。
func (p *Panel) traeCheckin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserIDs []string `json:"user_ids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	target := map[string]bool{}
	for _, u := range body.UserIDs {
		if u != "" {
			target[u] = true
		}
	}

	accts, err := p.traeLoadAccounts()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "读取 Trae 账号失败: "+err.Error())
		return
	}
	if len(accts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "total": 0,
			"message": "未找到 Trae 账号（请先在 Trae Work 助手中添加）",
			"results": []any{},
		})
		return
	}

	traeMu.Lock()
	defer traeMu.Unlock()

	results := make([]traeCheckinResult, 0, len(accts))
	earned := make([]traeCreditsEntry, 0, len(accts))
	totalOK, already, failed := 0, 0, 0
	for _, a := range accts {
		if len(target) > 0 && !target[a.UserID] {
			continue
		}
		res := traeCheckinResult{Name: a.Name, UserID: a.UserID}

		// 预检：已签到直接跳过（claim 虽幂等，但少一次无谓请求）。
		// 顺带取回「签到可得额度」（quota）——签到成功后按它记账（见 trae_credits.go）。
		checkedIn, quota, _, sErr := upstream.TraeStatus(a)
		if sErr == nil && checkedIn {
			res.Status, res.Message = "already", "今日已签到"
			already++
			// 已签到同样计入「当日所得」：这个号今天确实拿到了 quota（只是不是本次）。
			// 同日重复记录在 traeRecordCredits 内按账号覆盖，所以重复点不会累加。
			if quota > 0 {
				earned = append(earned, traeCreditsEntry{UserID: a.UserID, Name: a.Name, Earned: quota})
			}
			results = append(results, res)
			continue
		}

		msg, cErr := upstream.TraeClaim(a)
		if cErr != nil {
			res.Status, res.Message = "fail", cErr.Error()
			failed++
			log.Printf("panel: Trae 签到失败 uid=%s: %v", a.UserID, cErr)
		} else {
			res.Status, res.Message = "claimed", msg
			totalOK++
			// 只有「本次真的签到成功」才计入当日所得；already 的不重复计。
			if sErr == nil && quota > 0 {
				earned = append(earned, traeCreditsEntry{UserID: a.UserID, Name: a.Name, Earned: quota})
			}
			log.Printf("panel: Trae 签到成功 uid=%s (%s) +%g", a.UserID, a.Name, quota)
		}
		results = append(results, res)
		time.Sleep(1200 * time.Millisecond) // 账号间节流，避免触发上游风控
	}
	p.traeRecordCredits(earned)

	log.Printf("panel: Trae 签到完成：成功 %d / 已签 %d / 失败 %d", totalOK, already, failed)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": len(results),
		"total_ok": totalOK, "already": already, "failed": failed,
		"results": results,
	})
}

// StartTraeAuto 启动 Trae 自动签到排程（hours 为空则不启动）。
//
// 与主池排程相互独立：本循环只看自己的时点，不参与 scheduler 的 taskKind 分发，
// 因此不改动调度器核心（降低回归面）。
func (p *Panel) StartTraeAuto(ctx context.Context, hours []int) {
	if !p.cfg.TraeEnabled || len(hours) == 0 {
		return
	}
	go func() {
		for {
			now := time.Now()
			next := nextTraeFire(now, hours)
			log.Printf("panel: Trae 自动签到已排程，下次 %s", next.Format("01-02 15:04"))
			// 用 NewTimer 而不是 time.After：ctx 取消时 time.After 的定时器不会被回收，
			// 下一次时点最远 24h 才触发，等于白挂一个 24h 的 timer（进程长期运行会累积）。
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			p.traeCheckinAll()
		}
	}()
}

// traeCheckinAll 自动排程用的全量签到（无 HTTP 上下文，结果只进日志）。
func (p *Panel) traeCheckinAll() {
	accts, err := p.traeLoadAccounts()
	if err != nil {
		log.Printf("panel: Trae 自动签到读取账号失败: %v", err)
		return
	}
	if len(accts) == 0 {
		return
	}
	traeMu.Lock()
	defer traeMu.Unlock()
	ok, already, failed := 0, 0, 0
	earned := make([]traeCreditsEntry, 0, len(accts))
	for _, a := range accts {
		checkedIn, quota, _, sErr := upstream.TraeStatus(a)
		if sErr == nil && checkedIn {
			already++
			// 已签到同样计入当日所得（同日按账号覆盖，不会累加）。
			if quota > 0 {
				earned = append(earned, traeCreditsEntry{UserID: a.UserID, Name: a.Name, Earned: quota})
			}
			continue
		}
		if _, cErr := upstream.TraeClaim(a); cErr != nil {
			failed++
			log.Printf("panel: Trae 自动签到失败 uid=%s: %v", a.UserID, cErr)
		} else {
			ok++
			// 记积分：只有本次真签成功才计入（already 的不重复计）。
			if sErr == nil && quota > 0 {
				earned = append(earned, traeCreditsEntry{UserID: a.UserID, Name: a.Name, Earned: quota})
			}
			log.Printf("panel: Trae 自动签到成功 uid=%s (%s) +%g", a.UserID, a.Name, quota)
		}
		time.Sleep(1200 * time.Millisecond)
	}
	p.traeRecordCredits(earned)
	log.Printf("panel: Trae 自动签到完成：成功 %d / 已签 %d / 失败 %d", ok, already, failed)
}

// nextTraeFire 计算下一次触发时刻：今天剩余时点里最早的一个，都过了则取明天最早的。
func nextTraeFire(now time.Time, hours []int) time.Time {
	loc := now.Location()
	best := time.Time{}
	for _, h := range hours {
		if h < 0 || h > 23 {
			continue
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, loc)
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	if best.IsZero() {
		best = now.Add(24 * time.Hour)
	}
	return best
}

// ── Trae 积分 ────────────────────────────────────────────────────────────────
//
// ⚠️ 两个「积分」语义完全不同，必须分开标注，别混：
//   - **积分余额**（用户最关心的「我还有多少」）：上游**没有独立查询接口**——余额只在
//     聊天流的 `cn_credits_remain_info` 里推送（见助手 scripts/test_deep.py 的
//     ide_credits / work_credits）。故余额取助手留下的本地快照 remaining_credits.json，
//     属**快照值**（可能滞后，前端明确标注 + 给出快照时间）。
//   - **签到可得**（今天签到能拿多少）：上游 status 接口的 credits 字段。助手
//     auto_checkin.py 注释原文：「credits_before 来自 status 接口，表示签到可获得的
//     积分额度」——**不是余额**。这个可以实时查（凭证有效时）。

// traeCreditsView 面板「Trae 积分」单账号条目。
type traeCreditsView struct {
	Name   string `json:"name"`
	UserID string `json:"user_id"`
	// Balance 积分余额（上游 ide_user_ent_usage 实时口径；失败才回落本地快照）。
	Balance    float64 `json:"balance"`
	HasBalance bool    `json:"has_balance"`
	// BalanceExpiresAt 余额最近到期时刻（上游口径）；BalanceExpired 是否已过期。
	BalanceExpiresAt string `json:"balance_expires_at,omitempty"`
	BalanceExpired   bool   `json:"balance_expired,omitempty"`
	// BalanceSource 余额来源：live（上游实时）| local（本地快照）。
	BalanceSource string `json:"balance_source,omitempty"`
	// Packs 逐包明细（积分构成用）。
	Packs []upstream.TraeCreditPack `json:"packs,omitempty"`
	// Limit/Used 额度合计与已用（上游 usage_summary 口径）。
	Limit float64 `json:"limit,omitempty"`
	Used  float64 `json:"used,omitempty"`
	// CheckinCredits 本次签到可获得的积分额度（上游 status 的 credits 字段，实时）。
	CheckinCredits float64 `json:"checkin_credits"`
	HasCheckin     bool    `json:"has_checkin"`
	// CheckedIn 今日是否已完成签到（用于积分总览）。
	CheckedIn bool `json:"checked_in"`
	// CheckinSource 签到额度的来源：live（实时查上游）| none（凭证不可用）。
	CheckinSource string `json:"checkin_source"`
	// CredentialExpired 该账号凭证是否已过期（前端据此解释「为什么没有实时值」）。
	CredentialExpired bool   `json:"credential_expired,omitempty"`
	Note              string `json:"note,omitempty"`
}

// traeCredits 汇总 Trae 账号积分：余额与签到额度均实时查上游，凭证不可用时余额回落本地快照。
func (p *Panel) traeCredits(w http.ResponseWriter, r *http.Request) {
	accts, err := p.traeLoadAccounts()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "读取 Trae 账号失败: "+err.Error())
		return
	}
	local := upstream.LoadTraeCreditsLocal(p.cfg.TraeDataDir)
	daily := upstream.LoadTraeCreditsDaily(p.cfg.TraeDataDir)

	views := make([]traeCreditsView, len(accts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3) // 上游限流：最多 3 并发（与 traeAccounts 同口径）
	for i, a := range accts {
		wg.Add(1)
		go func(i int, a upstream.TraeAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v := traeCreditsView{Name: a.Name, UserID: a.UserID, CheckinSource: "none"}

			expired := false
			if exp, ok := upstream.TraeJWTExpiry(a.JWT); ok && time.Now().After(exp) {
				expired = true
			}
			v.CredentialExpired = expired

			// ① 余额：上游 ide_user_ent_usage 实时口径（凭证过期就不发注定失败的请求）
			if !expired {
				if rem, limit, used, exp, packs, uerr := upstream.TraeCreditsUsage(a); uerr == nil {
					v.Balance, v.HasBalance, v.BalanceSource = rem, true, "live"
					v.Limit, v.Used, v.Packs = limit, used, packs
					if exp > 0 {
						t := time.Unix(exp, 0)
						v.BalanceExpiresAt = t.Local().Format("2006-01-02 15:04")
						v.BalanceExpired = time.Now().After(t)
					}
				}
			}
			// 余额实时失败时回落本地快照（只作兜底，标注来源）
			if !v.HasBalance && local != nil {
				if c, ok := local.Credits[a.UserID]; ok {
					v.Balance, v.HasBalance, v.BalanceSource = c, true, "local"
				}
				if ts, ok := local.ExpireTimes[a.UserID]; ok && ts > 0 && v.BalanceExpiresAt == "" {
					t := time.Unix(ts, 0)
					v.BalanceExpiresAt = t.Local().Format("2006-01-02 15:04")
					v.BalanceExpired = time.Now().After(t)
				}
			}

			// ② 签到可得额度：实时查上游
			if !expired {
				if checkedIn, credits, _, serr := upstream.TraeStatus(a); serr == nil {
					v.CheckinCredits, v.HasCheckin, v.CheckinSource = credits, true, "live"
					v.CheckedIn = checkedIn
				}
			}
			if v.CheckinSource == "none" {
				if expired {
					v.Note = "凭证已过期，签到额度无法实时查询"
				} else {
					v.Note = "上游未返回签到额度"
				}
			}
			views[i] = v
		}(i, a)
	}
	wg.Wait()

	// 趋势：**面板自建快照为主**（签到实测所得），助手历史快照作补充（它才有消耗）。
	// 同一天两边都有时以面板为准——面板是签到执行方，数据更准。
	type trendRow struct {
		Date     string  `json:"date"`
		Earned   float64 `json:"earned"`
		Consumed float64 `json:"consumed"`
		Total    float64 `json:"total"`
		Source   string  `json:"source"` // panel | helper
	}
	byDate := map[string]*trendRow{}
	var dates []string
	if daily != nil {
		for _, s := range daily.Snapshots {
			if _, ok := byDate[s.Date]; !ok {
				dates = append(dates, s.Date)
			}
			byDate[s.Date] = &trendRow{Date: s.Date, Earned: s.Earned, Consumed: s.Consumed, Total: s.Total, Source: "helper"}
		}
	}
	if self := p.loadTraeCreditsSelf(); self != nil {
		for _, d := range self.Daily {
			if row, ok := byDate[d.Date]; ok {
				row.Earned, row.Source = d.Total, "panel"
			} else {
				dates = append(dates, d.Date)
				byDate[d.Date] = &trendRow{Date: d.Date, Earned: d.Total, Source: "panel"}
			}
		}
	}
	sort.Strings(dates)
	trend := make([]trendRow, 0, len(dates))
	for _, dt := range dates {
		trend = append(trend, *byDate[dt])
	}
	if len(trend) > 30 {
		trend = trend[len(trend)-30:] // 最多回看 30 天
	}
	updatedAt := ""
	if local != nil {
		updatedAt = local.UpdatedAt
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accounts": views, "trend": trend, "local_updated_at": updatedAt,
	})
}
