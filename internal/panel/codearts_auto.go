// codearts_auto.go 华为云 CodeArts 的每日自动领取 + 会话静默续期。
//
// 为什么不是"每天定点签一次"（2026-10-30 → 11-01 实测）：
//   - 网页会话只活大约 **7 小时**（10-30 11:19 存的 Cookie，18:10 就判死）；
//   - 期间每 15 分钟一次的保活**从未**收到过续期 Cookie（全量日志里
//     "收到续期 Cookie" 出现 0 次），也就是说保活只能防"闲置判死"，
//     挡不住服务端按绝对时限把会话收走；
//   - 号池里从来没有过期时间信息（Cookie 结构只存 name/value/domain/path，
//     CDP 其实给了 expires 却被丢掉），所以面板既无法提前续，也无法告诉用户
//     "还剩多久"，只能等接口报"会话已失效" —— 这就是"每天自己退掉、
//     不能自动续期"的完整成因。
//
// 所以这里做三件事：
//  1. **巡检领取**：每 15 分钟对今天还没领到的账号试一次，领到当天收工。
//     用户任何时候重新登录，下一轮（≤15 分钟）自动把 100 积分领到手，
//     不需要再手动点「签到」。定点签到会正好落在会话已死的时段，巡检不会。
//  2. **登录后立刻领**：抓到新会话的那一刻是会话最值钱的时候，当场领掉。
//  3. **静默续期**：会话死了、但华为 ID/CAS 的持久票据还没过期，就用这批票据
//     走一遍 `authui/login?service=…`，换回新会话并把 Cookie 收回号池。
//
// ⚠️ 续期这一环的**决策**全部在 internal/codearts/renewplan.go（C-CA-RENEW@1）。
// 旧版在 TicketDeadline 为空时直接 continue + 打一条"下次重新登录会采集"，
// 而那条日志承诺的代码根本不存在 —— 旧号池（没有 expires）于是永远不续期。
// 现在：无票据时间 → 先补采（T2 的 codeartsBackfillAccount）→ 判 login_dead /
// probed / session_only → 再决定是否续期；失败 2 小时节流、日志每天最多一条。
package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"

	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/clockskew"
	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

const (
	// codeartsAutoInterval 自动领取/续期巡检的间隔。
	//
	// ⚠ 2026-11-08：从 15 分钟（= codearts.AliveRecheck）收到 **5 分钟**。
	//   理由：华为云的应用会话是「空闲超时」型 —— 实测寿命约 7 小时，
	//   但服务端**不续 SessionID**（保活只回一个 BENSESSCC_TAG），
	//   所以保活能做的只是「别让它因闲置而过期」。
	//   巡检越慢，一次请求的间隔就越可能踩在服务端的闲置阈值边上，
	//   于是号莫名其妙就死了，只能重新扫码。
	//   5 分钟一次只是打一个轻量接口（/chat/snap-manager/v1/package/overview），
	//   成本可以忽略，却把「因闲置掉线」的概率压下去。
	//   真正换不回会话时（票据过期）仍然是 needs_login —— 那是另一回事，
	//   调快巡检解决不了，只能人工登录一次。
	codeartsAutoInterval = 5 * time.Minute
)

// ── 可注入的执行钩子（生产默认指向真实实现；测试用来避免打上游/起浏览器） ──

var (
	// codeartsRenewKeepaliveHook 替换验活（默认 codearts.Keepalive）。
	codeartsRenewKeepaliveHook func(a *codearts.Account) (bool, error)
	// codeartsRenewBackfillHook 替换补采（默认 (*Panel).codeartsBackfillAccount）。
	codeartsRenewBackfillHook func(p *Panel, a *codearts.Account) (*codearts.CaptureResult, error)
	// codeartsRenewSSOHook 替换 SSO 换会话（默认 (*Panel).codeartsSilentRenew）。
	codeartsRenewSSOHook func(p *Panel, a *codearts.Account) (phase, msg string)
	// codeartsRenewLogf 日志出口（测试里换成计数器来断言"每天最多一条"）。
	codeartsRenewLogf = log.Printf
)

// codeartsNoteOnce 同一条提示每天只播一次（key 建议用 "kind:账号ID"）。
//
// 为什么需要：会话判死是**不可自愈**的状态，旧版每 15 分钟打一条
// 「账号 xxx 会话已失效，需要重新登录」，一天 96 条，日志里真正有用的
// 错误被冲没了，用户看到的也只有"它一直在报"。
var codeartsNoteOnce_ struct {
	sync.Mutex
	seen map[string]string // key → 最近一次播出的日期（本地日）
}

func codeartsNoteOnce(key string) bool {
	codeartsNoteOnce_.Lock()
	defer codeartsNoteOnce_.Unlock()
	if codeartsNoteOnce_.seen == nil {
		codeartsNoteOnce_.seen = map[string]string{}
	}
	today := time.Now().Format("2006-01-02")
	if codeartsNoteOnce_.seen[key] == today {
		return false
	}
	codeartsNoteOnce_.seen[key] = today
	return true
}

// hostOf 取 URL 的 host（解析失败返回空串）。
//
// ⚠️ 判断"停在哪个站点"必须看 host，不能用整串 URL 做子串匹配：
// `authui/login?service=https://devcloud…/chat/home` 里本身就带着 devcloud 字样，
// 子串匹配会把"停在登录页"误判成"已经回到控制台"（实测第一版探针就栽在这）。
func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return p.Host
}

// StartCodeArtsAuto 启动 CodeArts 的自动领取与静默续期巡检。
func (p *Panel) StartCodeArtsAuto(ctx context.Context) {
	go func() {
		// 起来后先等 90 秒：启动瞬间面板要做号池整理/模型目录预热，别抢。
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second):
		}
		for {
			p.codeartsAutoCheckinOnce()
			p.codeartsSilentRenewOnce()
			timer := time.NewTimer(codeartsAutoInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// codeartsAutoCheckinOnce 给今天还没领到的账号补一次领取。
//
// 同一个人（PersonKey 相同）的多个号**每天只让一个去打上游**：同一人的额度
// 通常共享，每个号都打一遍既拿不到第二份积分，又会在风控侧堆出一串同人请求。
// 组内已有号今天成功（LastClaimedAt / LastOKAt）→ 其余号标 skipped_same_person。
func (p *Panel) codeartsAutoCheckinOnce() {
	pool, err := codearts.LoadPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		return
	}
	now := clockskew.Now()
	plan := codeartsSamePersonDailyPlan(pool.Accounts, now)
	claimed := 0
	for _, a := range pool.Accounts {
		if a == nil || len(a.Cookies) == 0 {
			continue
		}
		// 登录窗口开着时不抢（那个浏览器里就是最新会话，由登录流程自己领）。
		if codeartsBrowserBusy() {
			return
		}
		if codeartsCheckedInToday(a) {
			continue
		}
		group := codeartsSamePersonDayKey(a)
		if codeartsSamePersonPlanSkipped(plan, a) || codeartsSamePersonDailyFailed(group, now, a) {
			if codeartsNoteOnce("codearts:same-person:checkin:" + group) {
				log.Printf("[codearts] 同人组 %s 今天已有一个号执行过签到，账号 %s 跳过（skipped_same_person）", group, a.ID)
			}
			continue
		}
		ph, msg, ok := codeartsCheckinHTTP(a)
		if !ok {
			// 会话死了/接口不通：保活那边已经按天播过一条，这里不重复刷。
			// 同时把该人标记为"今天已打过一次"，不再每 15 分钟重试上游 ——
			// 签到失败基本等于会话失效，接下来由续期巡检接手。
			codeartsSamePersonMarkDailyFailure(group, now)
			continue
		}
		_ = ideauth.MarkCheckedIn("codearts", a.ID)
		claimed++
		log.Printf("panel: CodeArts 自动签到 %s(%s): %s %s", a.ID, a.Label, ph, msg)
	}
	if claimed > 0 {
		// 领到的积分/快照已由 codeartsCheckinHTTP 内部落盘。
		log.Printf("panel: CodeArts 自动签到本轮 %d 个账号完成", claimed)
	}
}

// codeartsAutoClaimAfterLogin 登录成功后当场领掉今天的份。
//
// 为什么要插在这一步：会话实测只有约 7 小时寿命，而登录时刻往往是任意时间
// （用户想起来才扫一次码）。等下一个排程点再领，很可能正落在会话已死的窗口里，
// 于是"明明刚登录过，今天的 100 分却没到账"。
func (p *Panel) codeartsAutoClaimAfterLogin(acc *codearts.Account) {
	if acc == nil || len(acc.Cookies) == 0 {
		return
	}
	if codeartsCheckedInToday(acc) {
		return
	}
	ph, msg, ok := codeartsCheckinHTTP(acc)
	if !ok {
		log.Printf("[codearts] 账号 %s 登录后自动签到没成功（会话或活动接口不通）", acc.ID)
		return
	}
	_ = ideauth.MarkCheckedIn("codearts", acc.ID)
	log.Printf("panel: CodeArts 登录后自动签到 %s(%s): %s %s", acc.ID, acc.Label, ph, msg)
}

// ── 同人分桶：同一个人每天只打一次上游 ──────────────────────────────────
//
// 为什么需要：换设备/多号登录常常是"同一个人"（同 masked_phone / 同 user_tag）
// 在号池里留下好几条记录。额度一般按人给，几个号一起打既拿不到第二份积分，
// 又会在华为云风控里堆出一串同人、同设备的高频请求 —— 用户最后看到的是
// "号全被风控了，只能手动重登"。
//
// 规则（每个 UTC+8 自然日）：
//  1. 组内**已有号成功**（今天 LastOKAt 或今天真正领到积分）→ 其余号标
//     skipped_same_person，不碰上游；
//  2. 组内今天**已经打过一次但没成功**（RenewAttemptAt / RenewFailAt / ProbeAt
//     落在今天）→ 当日不再重试（记录在号池现有字段里，不新增契约）；
//  3. 都没有 → 只让**最久没成功**的号打一次（LastOKAt 空的最优先），其余号
//     当日标 skipped_same_person；
//  4. 进程重启后不依赖内存：上面的判断全部读号池字段，只有"签到失败"这种
//     没有落盘字段的情况才用内存做当日抑制（重启后最多多打一次）。

const (
	// codeartsPersonDayExecute 本轮该号可以打上游主链（签到/续期）。
	codeartsPersonDayExecute = "execute"
	// codeartsPersonDaySkippedSamePerson 同人组当日已经有号执行过，本号跳过。
	codeartsPersonDaySkippedSamePerson = "skipped_same_person"
)

// codeartsAutoDayLocation 自动链路的"今天"按 UTC+8 算（与签到账本同口径）。
var codeartsAutoDayLocation = time.FixedZone("UTC+8", 8*60*60)

// codeartsAutoDayOf 返回 t 在 UTC+8 的日期串（2006-01-02）。
func codeartsAutoDayOf(t time.Time) string {
	return t.In(codeartsAutoDayLocation).Format("2006-01-02")
}

// codeartsSamePersonDayKey 同人分桶键。
//
// PersonKey 由 codearts.EnsureSlot 确定性派生（LoadPool 已经在内存里补全）；
// 万一为空则退回账号 ID —— 绝不把多个"没有 PersonKey"的号塌进同一个组，
// 那会让一个号成功就把全号池都标成跳过。
func codeartsSamePersonDayKey(a *codearts.Account) string {
	if a == nil {
		return ""
	}
	if key := strings.TrimSpace(a.PersonKey); key != "" {
		return key
	}
	if id := strings.TrimSpace(a.ID); id != "" {
		return "id:" + id
	}
	return ""
}

// codeartsPersonLastSuccessAt 该号最近一次"今天的份到手/会话确认可用"的时刻。
//
// 两个来源：号池的 LastOKAt（验活/续期成功都会写）+ 签到账本的 LastClaimedAt
// （真正领到积分）。取较晚的那个，避免"签到成功但 LastOKAt 还停在昨天"被误判。
func codeartsPersonLastSuccessAt(a *codearts.Account) time.Time {
	if a == nil {
		return time.Time{}
	}
	var out time.Time
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(a.LastOKAt)); err == nil {
		out = t
	}
	for _, id := range a.AllIDs() {
		if t, ok := ideauth.LastClaimedAt("codearts", id); ok && t.After(out) {
			out = t
		}
	}
	return out
}

// codeartsPersonDaySucceeded 组内是否已有账号在今天成功过。
func codeartsPersonDaySucceeded(members []*codearts.Account, day string) bool {
	for _, a := range members {
		if t := codeartsPersonLastSuccessAt(a); !t.IsZero() && codeartsAutoDayOf(t) == day {
			return true
		}
	}
	return false
}

// codeartsPersonDayAttempted 组内是否今天已经打过一次（成功与否不论）。
//
// RenewAttemptAt / RenewFailAt 是续期结论，ProbeAt 是补采结论：三者任一落在
// 今天都说明"这组今天的上游额度已经消耗过一次"，不该再试。
//
// ⚠️ 但**结论之后又重新登录过**的必须放过：用户扫码登录后拿到了新会话，
// 早先那次"会话失效"的结论已经过时，继续拿它压着当天的领取就等于
// "明明登录了却领不到积分"（实测 2026-11-04）。
func codeartsPersonDayAttempted(members []*codearts.Account, day string) bool {
	for _, a := range members {
		if a == nil {
			continue
		}
		saved, _ := time.Parse(time.RFC3339, strings.TrimSpace(a.SavedAt))
		for _, raw := range []string{a.RenewAttemptAt, a.RenewFailAt, a.ProbeAt} {
			t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
			if err != nil || codeartsAutoDayOf(t) != day {
				continue
			}
			if !saved.IsZero() && saved.After(t) {
				continue // 结论已过时（之后又登录过）
			}
			return true
		}
	}
	return false
}

// codeartsSamePersonPick 组内选出本轮唯一一个可以打主链的账号。
//
// 排序：本轮"能打"（没有被 RenewNextAt / 失败节流挡住）的优先，其次最久没成功
// 的优先，最后按 ID 稳定排序 —— 稳定才有意义：每次都换一个号去打，等于让
// 同人组里所有号轮流吃风控。
func codeartsSamePersonPick(members []*codearts.Account, now time.Time) string {
	best := ""
	var bestReady bool
	var bestLast time.Time
	for _, a := range members {
		if a == nil || strings.TrimSpace(a.ID) == "" {
			continue
		}
		ready := !codeartsRenewBackoff(a, now)
		last := codeartsPersonLastSuccessAt(a)
		switch {
		case best == "":
		case ready && !bestReady:
		case ready == bestReady && last.Before(bestLast):
		case ready == bestReady && last.Equal(bestLast) && a.ID < best:
		default:
			continue
		}
		best, bestReady, bestLast = a.ID, ready, last
	}
	return best
}

// codeartsSamePersonDailyPlan 返回 id → 当轮决策（execute / skipped_same_person）。
//
// 纯函数：只读号池字段与签到账本，不写盘、不打上游，单测可以直接断言。
func codeartsSamePersonDailyPlan(accs []*codearts.Account, now time.Time) map[string]string {
	day := codeartsAutoDayOf(now)
	groups := make(map[string][]*codearts.Account, len(accs))
	keyOrder := make([]string, 0, len(accs))
	for _, a := range accs {
		if a == nil || strings.TrimSpace(a.ID) == "" {
			continue
		}
		key := codeartsSamePersonDayKey(a)
		if _, ok := groups[key]; !ok {
			keyOrder = append(keyOrder, key)
		}
		groups[key] = append(groups[key], a)
	}

	plan := make(map[string]string, len(accs))
	for _, key := range keyOrder {
		members := groups[key]
		chosen := ""
		if !codeartsPersonDaySucceeded(members, day) && !codeartsPersonDayAttempted(members, day) {
			chosen = codeartsSamePersonPick(members, now)
		}
		for _, a := range members {
			if a.ID == chosen {
				plan[a.ID] = codeartsPersonDayExecute
			} else {
				plan[a.ID] = codeartsPersonDaySkippedSamePerson
			}
		}
	}
	return plan
}

// codeartsSamePersonPlanSkipped 本轮该号是否被同人分桶挡掉。
func codeartsSamePersonPlanSkipped(plan map[string]string, a *codearts.Account) bool {
	if a == nil {
		return false
	}
	return plan[a.ID] == codeartsPersonDaySkippedSamePerson
}

// codeartsSamePersonDayFailures "签到失败后当日不再重试"的进程内记忆。
//
// 签到失败没有对应的号池字段（不想把签到失败写成续期失败，污染 RenewResult），
// 所以只在内存里按 UTC+8 日期抑制；进程重启后最多多试一次，不会长期失效。
//
// failed 记录的是"失败发生的时刻"：用户在那之后**重新登录过**（SavedAt 更新）
// 就说明拿到了新会话，这条抑制必须解除 —— 否则用户明明扫码登录了，
// 当天的 100 积分却因为早先那次失败被一直压着领不到（实测 2026-11-04）。
var codeartsSamePersonDayFailures struct {
	sync.Mutex
	day    string
	keys   map[string]bool
	failed map[string]time.Time
}

// codeartsSamePersonMarkDailyFailure 记录"这个同人组今天已经失败过一次"。
func codeartsSamePersonMarkDailyFailure(group string, now time.Time) {
	if group == "" {
		return
	}
	day := codeartsAutoDayOf(now)
	codeartsSamePersonDayFailures.Lock()
	defer codeartsSamePersonDayFailures.Unlock()
	if codeartsSamePersonDayFailures.day != day {
		codeartsSamePersonDayFailures.day = day
		codeartsSamePersonDayFailures.keys = map[string]bool{}
		codeartsSamePersonDayFailures.failed = map[string]time.Time{}
	}
	if codeartsSamePersonDayFailures.keys == nil {
		codeartsSamePersonDayFailures.keys = map[string]bool{}
	}
	if codeartsSamePersonDayFailures.failed == nil {
		codeartsSamePersonDayFailures.failed = map[string]time.Time{}
	}
	codeartsSamePersonDayFailures.keys[group] = true
	codeartsSamePersonDayFailures.failed[group] = now
}

// codeartsSamePersonDailyFailed 这个同人组今天是否已经失败过。
//
// a 为该号当前状态：若它的会话是在失败**之后**才采集/登录的（SavedAt 更新），
// 说明失败原因（会话失效）已经消除，这次抑制不再适用。
func codeartsSamePersonDailyFailed(group string, now time.Time, a *codearts.Account) bool {
	if group == "" {
		return false
	}
	day := codeartsAutoDayOf(now)
	codeartsSamePersonDayFailures.Lock()
	defer codeartsSamePersonDayFailures.Unlock()
	if codeartsSamePersonDayFailures.day != day {
		return false
	}
	if !codeartsSamePersonDayFailures.keys[group] {
		return false
	}
	if a != nil {
		if failedAt, ok := codeartsSamePersonDayFailures.failed[group]; ok {
			if saved, err := time.Parse(time.RFC3339, strings.TrimSpace(a.SavedAt)); err == nil && saved.After(failedAt) {
				return false
			}
		}
	}
	return true
}

// codeartsResetSamePersonDailyFailures 清空当日抑制（测试与日切用）。
func codeartsResetSamePersonDailyFailures() {
	codeartsSamePersonDayFailures.Lock()
	defer codeartsSamePersonDayFailures.Unlock()
	codeartsSamePersonDayFailures.day = ""
	codeartsSamePersonDayFailures.keys = nil
	codeartsSamePersonDayFailures.failed = nil
}

// ── 静默续期巡检 ────────────────────────────────────────────────────────

// codeartsSilentRenewOnce 对号池里的账号逐轮跑一次续期决策。
//
// 与旧版的差别：
//   - 不再"没有 TicketDeadline 就 continue"，而是交给 DecideRenew；
//   - 2 小时内失败过就跳过本轮（不再每 15 分钟起浏览器刷日志）；
//   - 失败字段（RenewAttemptAt / RenewFailAt / RenewFailMsg / RenewNextAt /
//     RenewResult）全部落盘，面板能看见"它试过了、为什么没成"。
func (p *Panel) codeartsSilentRenewOnce() {
	pool, err := codearts.LoadPool()
	if err != nil || pool == nil {
		return
	}
	// 用**校准后**的时间：本机钟实测快 8 小时，拿未校准的 time.Now() 去比
	// 服务端下发的票据到期时间，会提前 8 小时判成 ticket_expired →
	// 直接 needs_login（跳过续期）→ 用户看到的就是「每次都要重新认证」。
	now := clockskew.Now()
	// 同人组当日只打一次：两个号属于同一个人时，第二个号本轮直接跳过，
	// 不重复起浏览器、不重复打上游。
	plan := codeartsSamePersonDailyPlan(pool.Accounts, now)
	for _, a := range pool.Accounts {
		if a == nil || len(a.Cookies) == 0 {
			continue
		}
		// 已有结论且未到下次检查点（或 2 小时内失败过）→ 跳过，避免刷浏览器。
		if codeartsRenewBackoff(a, now) {
			continue
		}
		if codeartsSamePersonPlanSkipped(plan, a) {
			group := codeartsSamePersonDayKey(a)
			if codeartsNoteOnce("codearts:same-person:renew:" + group) {
				codeartsRenewLogf("[codearts] 同人组 %s 今天已有一个号执行过，账号 %s 跳过（skipped_same_person）", group, a.ID)
			}
			continue
		}
		// 成功/失败日志都在 codeartsRenewAccount 内（失败按天节流），这里不再重复打。
		_, _ = p.codeartsRenewAccount(a, false)
	}
}

// codeartsRenewBackoff 本轮巡检是否应该直接跳过这个账号。
//
// 两个来源：
//  1. RenewNextAt 还没到（renewed 后 15 分钟、needs_login/ticket_expired 后 24 小时、
//     失败后 2 小时）—— 这是"决策给出的下次检查点"，不能提前打；
//  2. 2 小时内失败过 —— 与任务要求一致，避免每 15 分钟重试一次。
//
// 例外：失败/等待之后用户**重新登录**过（SavedAt 晚于结论时刻）→ 旧结论作废，
// 立刻重新判断（否则刚登录的账号会被旧 RenewNextAt 锁 24 小时）。
func codeartsRenewBackoff(a *codearts.Account, now time.Time) bool {
	if a == nil {
		return true
	}
	if codeartsRenewHasFreshLogin(a) {
		return false
	}
	if next, err := time.Parse(time.RFC3339, strings.TrimSpace(a.RenewNextAt)); err == nil && now.Before(next) {
		return true
	}
	if failAt, err := time.Parse(time.RFC3339, strings.TrimSpace(a.RenewFailAt)); err == nil {
		if now.Before(failAt.Add(codearts.RenewMinGap)) {
			return true
		}
	}
	return false
}

// codeartsRenewHasFreshLogin SavedAt 是否晚于最近一次续期/补采结论时刻。
func codeartsRenewHasFreshLogin(a *codearts.Account) bool {
	if a == nil {
		return false
	}
	saved, err := time.Parse(time.RFC3339, strings.TrimSpace(a.SavedAt))
	if err != nil {
		return false
	}
	for _, s := range []string{a.RenewAttemptAt, a.RenewFailAt, a.ProbeAt} {
		if t, e := time.Parse(time.RFC3339, strings.TrimSpace(s)); e == nil && saved.After(t) {
			return true
		}
	}
	return false
}

// codeartsBrowserBusy 登录窗口是否占着浏览器（独占资源）。
func codeartsBrowserBusy() bool {
	codeartsBrowserSession.mu.Lock()
	defer codeartsBrowserSession.mu.Unlock()
	return codeartsBrowserSession.cmd != nil
}

// codeartsRenewAccount 对一个账号执行一轮静默续期，返回面板契约的 phase 与说明。
//
// phase 允许：renewed|alive|ticket_expired|needs_login|probe_required|rejected|busy|failed。
// 本函数只产出去掉 rejected 的子集（throttled 在契约 D 里叫 rejected，由
// codeartsRenewHTTP 统一映射），因此返回值使用 "throttled"，HTTP 层转 "rejected"。
//
// force=true **只**跳过 2 小时流量节流（第 6 条），不得跳过 needs_login /
// ticket_expired（死票据硬试无意义），也不跳过 probe_required（没有票据证据时
// 直接续期等于放弃补采，旧号池会再次退化成"永远不续"）。
func (p *Panel) codeartsRenewAccount(a *codearts.Account, force bool) (phase, msg string) {
	if a == nil || a.ID == "" {
		return "failed", "账号为空"
	}
	if len(a.Cookies) == 0 {
		return "failed", "账号没有 Cookie，无法续期；请重新登录或粘贴 Cookie"
	}
	// 校准后时间（见 codeartsSilentRenewOnce 处的说明）
	now := clockskew.Now()

	// 浏览器是独占资源：登录窗口开着时起第二个会抢调试端口，还会把用户
	// 正在操作的窗口弄乱。
	if codeartsBrowserBusy() {
		return "busy", "登录窗口正开着，跳过本轮"
	}

	// 第 2 条判定所需的事实：会话到底还活不活。
	// 用克隆快照验活：Keepalive 会往账号里写 LastOKAt / 新 Cookie，
	// 成功后再按"不覆盖并发更新"的规则落盘。
	snap := codeartsCloneAccount(a)
	alive, kerr := codeartsRenewKeepalive(snap)
	if kerr == nil && alive {
		if err := codeartsPersistRenewAlive(snap, now); err != nil {
			log.Printf("[codearts] 账号 %s 验活结果写号池失败: %v", a.ID, err)
		}
		return "alive", "会话可用"
	}
	if kerr != nil && !errors.Is(kerr, codearts.ErrSessionExpired) {
		// 网络/DNS/超时：**不是**会话失效（这台机器一天里出现过两次 no such host）。
		// 按可恢复失败节流，绝不起浏览器白跑一趟。
		rmsg := "验活失败（网络/接口错误，不是会话失效）: " + kerr.Error()
		codeartsRenewFailLogged(a.ID, now, "failed", rmsg, now.Add(codearts.RenewMinGap))
		return "failed", rmsg
	}
	if kerr == nil && !alive {
		rmsg := "验活返回非 200，暂不续期"
		codeartsRenewFailLogged(a.ID, now, "failed", rmsg, now.Add(codearts.RenewMinGap))
		return "failed", rmsg
	}

	d := codearts.DecideRenew(a, now, false, false)
	if force {
		// force 只把节流升级为续期；其余分支原样保留。
		d = codearts.ForceRenewDecision(d, now)
	}

	switch d.Reason {
	case "busy":
		return "busy", "登录窗口正开着，跳过本轮"
	case "alive":
		return "alive", "会话可用"
	case "needs_login":
		rmsg := codeartsRenewNeedsLoginMsg(a)
		// 本轮**没有**真的发起续期（只是判定结论）→ 只写结论，不写 RenewAttemptAt。
		codeartsRenewConclusionLogged(a.ID, now, "needs_login", rmsg, d.NextAt)
		return "needs_login", rmsg
	case "ticket_expired":
		rmsg := "票据已于 " + strings.TrimSpace(a.TicketDeadline) + " 过期，静默续期无法成立，需要人工重新登录"
		codeartsRenewConclusionLogged(a.ID, now, "ticket_expired", rmsg, d.NextAt)
		return "ticket_expired", rmsg
	case "probe_required":
		return p.codeartsRenewAfterProbe(a, now, force)
	case "throttled":
		return "throttled", "距上次续期不满 2 小时（下次 " +
			d.NextAt.Format(time.RFC3339) + "），需要立即执行请带 force=true"
	}
	// renew_due
	return p.codeartsRenewExecute(a, now)
}

// codeartsRenewNeedsLoginMsg 把"为什么必须人工登录"说清楚（面板/日志共用）。
func codeartsRenewNeedsLoginMsg(a *codearts.Account) string {
	if a == nil {
		return "账号为空"
	}
	switch {
	case a.ProbeState == "login_dead":
		return "补采确认会话已失效，只能人工重新登录"
	case a.RenewResult == "needs_login":
		return "上次续期停在人工登录页，只能重新登录"
	}
	return "会话已失效，需要人工重新登录"
}

// codeartsRenewAfterProbe 无票据时间时的"先补采，再决定续不续"路径。
//
// 这是缺陷一的正解：TicketDeadline 为空**不代表**票据死了（会话级票据是常态），
// 只代表"还没观测过"。补采的三种结论分别处理：
//   - login_dead → 只能人工登录（写 needs_login，24 小时后再看）；
//   - probed/session_only → 继续走续期（两种都允许续）；
//   - no_cookie/error → 如实记 failed，并把探测节流与续期节流串起来。
func (p *Panel) codeartsRenewAfterProbe(a *codearts.Account, now time.Time, force bool) (phase, msg string) {
	res, berr := codeartsRenewBackfill(p, a)
	if berr != nil {
		emsg := "补采失败: " + errStr(berr)
		if res != nil && strings.TrimSpace(res.Message) != "" {
			emsg = "补采失败: " + res.Message
		}
		// 契约要求：补采失败也必须写 ProbeState=failed / ProbeMsg / ProbeAt，
		// 否则 DecideRenew 每 15 分钟都会认为"没探测过"，反复重开浏览器。
		codeartsRenewMarkProbeFailed(a.ID, now, emsg)
		codeartsRenewFailLogged(a.ID, now, "failed", emsg, now.Add(codearts.RenewMinGap))
		return "failed", emsg
	}
	if res == nil {
		emsg := "补采没有返回结果"
		codeartsRenewMarkProbeFailed(a.ID, now, emsg)
		codeartsRenewFailLogged(a.ID, now, "failed", emsg, now.Add(codearts.RenewMinGap))
		return "failed", emsg
	}

	switch res.Phase {
	case "login_dead":
		// 补采已经由 T2 写盘（ProbeState=login_dead / ProbeAt），这里补续期结论。
		rmsg := res.Message
		if strings.TrimSpace(rmsg) == "" {
			rmsg = "补采确认会话已失效，只能人工重新登录"
		}
		codeartsRenewFailLogged(a.ID, now, "needs_login", rmsg, now.Add(codearts.ManualRecheck))
		return "needs_login", rmsg
	case "probed", "session_only":
		// 补采成功：继续往下判断续期。
	case "no_cookie":
		emsg := res.Message
		if strings.TrimSpace(emsg) == "" {
			emsg = "补采没有采到 Cookie"
		}
		codeartsRenewMarkProbeFailed(a.ID, now, emsg)
		codeartsRenewFailLogged(a.ID, now, "failed", emsg, now.Add(codearts.RenewMinGap))
		return "failed", emsg
	default: // error
		emsg := res.Message
		if strings.TrimSpace(emsg) == "" {
			emsg = "补采环境错误"
		}
		codeartsRenewMarkProbeFailed(a.ID, now, emsg)
		codeartsRenewFailLogged(a.ID, now, "failed", emsg, now.Add(codearts.RenewMinGap))
		return "failed", emsg
	}

	// 补采已落盘，必须用**最新**快照继续判断：拿旧 Cookie 硬跑一次浏览器没有意义。
	latest := a
	if pool, lerr := codearts.LoadPool(); lerr == nil {
		if x := codeartsFindAccount(pool, a.ID); x != nil {
			latest = x
		}
	}
	d := codearts.DecideRenew(latest, now, false, false)
	if force {
		// 补采后若仍落在 2 小时节流窗口内，force 同样只跳过第 6 条。
		d = codearts.ForceRenewDecision(d, now)
	}
	switch d.Reason {
	case "needs_login":
		rmsg := codeartsRenewNeedsLoginMsg(latest)
		codeartsRenewConclusionLogged(latest.ID, now, "needs_login", rmsg, d.NextAt)
		return "needs_login", rmsg
	case "ticket_expired":
		rmsg := "补采到的票据最早于 " + strings.TrimSpace(latest.TicketDeadline) +
			" 过期，静默续期无法成立，需要人工重新登录"
		codeartsRenewConclusionLogged(latest.ID, now, "ticket_expired", rmsg, d.NextAt)
		return "ticket_expired", rmsg
	case "throttled":
		// 刚补采完，但 2 小时内已经尝试过续期 —— 等节流窗口，不重复起浏览器。
		return "throttled", "补采完成，但距上次续期不满 2 小时（下次 " +
			d.NextAt.Format(time.RFC3339) + "），需要立即执行请带 force=true"
	}
	return p.codeartsRenewExecute(latest, now)
}

// codeartsRenewExecute 执行真正的 SSO 换会话链，并按契约落盘成功/失败。
func (p *Panel) codeartsRenewExecute(a *codearts.Account, now time.Time) (phase, msg string) {
	if a == nil {
		return "failed", "账号为空"
	}
	snap := codeartsCloneAccount(a)
	phase, msg = codeartsRenewSSO(p, snap)
	switch phase {
	case "renewed":
		if err := codeartsPersistRenewSuccess(snap, now); err != nil {
			log.Printf("[codearts] 账号 %s 续期成功但写号池失败: %v", a.ID, err)
			return "failed", "已换回新会话但写入号池失败: " + err.Error()
		}
		log.Printf("[codearts] 账号 %s 静默续期成功: %s", a.ID, msg)
		return "renewed", msg
	case "needs_login":
		if strings.TrimSpace(msg) == "" {
			msg = "票据换不回会话，需要人工重新登录"
		}
		codeartsRenewFailLogged(a.ID, now, "needs_login", msg, now.Add(codearts.ManualRecheck))
		return "needs_login", msg
	default: // failed
		// 复验失败也可能已经换回了新 Cookie：先保住这份快照，别白跑一次浏览器。
		codeartsPersistRenewCookiesIfNew(snap, now)
		if strings.TrimSpace(msg) == "" {
			msg = "静默续期未成功"
		}
		codeartsRenewFailLogged(a.ID, now, "failed", msg, now.Add(codearts.RenewMinGap))
		return "failed", msg
	}
}

// ── 续期结果落盘（全部走 UpdatePool 原子写） ─────────────────────────────

// codeartsPersistRenewAlive 会话仍然可用时的落盘。
//
// 只更新 LastOKAt（若保活收到了新 Cookie 也一并收回），**不**写 RenewAttemptAt
// —— 本轮根本没发起续期，写 AttemptAt 会让面板显示成"试过又失败了"。
// 会话活着说明上次失败已自愈，顺手清掉旧的失败结论。
func codeartsPersistRenewAlive(snap *codearts.Account, now time.Time) error {
	if snap == nil || snap.ID == "" {
		return errors.New("账号为空")
	}
	at := now.Format(time.RFC3339)
	return codearts.UpdatePool(func(pool *codearts.Pool) error {
		x := codeartsFindAccount(pool, snap.ID)
		if x == nil {
			return fmt.Errorf("账号已不在号池: %s", snap.ID)
		}
		if codeartsCookieDigest(x.Cookies) != codeartsCookieDigest(snap.Cookies) &&
			codeartsShouldTakeCaptureCookies(x, snap.Cookies, now) {
			x.Cookies = append([]codearts.Cookie(nil), snap.Cookies...)
			x.SavedAt = at
			x.RefreshTicketDeadline()
		}
		if t := strings.TrimSpace(snap.LastOKAt); t != "" {
			x.LastOKAt = t
		} else {
			x.LastOKAt = at
		}
		x.RenewResult = "alive"
		x.RenewNextAt = now.Add(codearts.AliveRecheck).Format(time.RFC3339)
		x.RenewFailAt = ""
		x.RenewFailMsg = ""
		return nil
	})
}

// codeartsPersistRenewSuccess 续期成功后的落盘（契约 C-CA-RENEW@1 成功分支）。
//
// 成功 = 新 Cookie + 重算票据寿命 + SavedAt + LastOKAt + RenewResult=renewed +
// RenewNextAt=now+15m。Keepalive 复验已经在 codeartsSilentRenew 内做过。
func codeartsPersistRenewSuccess(snap *codearts.Account, now time.Time) error {
	if snap == nil || snap.ID == "" {
		return errors.New("账号为空")
	}
	at := now.Format(time.RFC3339)
	return codearts.UpdatePool(func(pool *codearts.Pool) error {
		x := codeartsFindAccount(pool, snap.ID)
		if x == nil {
			return fmt.Errorf("账号已不在号池: %s", snap.ID)
		}
		if codeartsShouldTakeCaptureCookies(x, snap.Cookies, now) {
			x.Cookies = append([]codearts.Cookie(nil), snap.Cookies...)
			x.SavedAt = at
		}
		// 换了 Cookie 必须重算票据寿命，否则下一轮还按旧票据判断。
		x.RefreshTicketDeadline()
		if t := strings.TrimSpace(snap.LastOKAt); t != "" {
			x.LastOKAt = t
		} else {
			x.LastOKAt = at
		}
		x.RenewAttemptAt = at
		x.RenewResult = "renewed"
		x.RenewNextAt = now.Add(codearts.AliveRecheck).Format(time.RFC3339)
		// 成功即清掉上次失败结论，避免面板同时显示 renewed 和旧失败原因。
		x.RenewFailAt = ""
		x.RenewFailMsg = ""
		// 续期成功说明会话/票据是有效的；旧的 login_dead 结论必须撤销，
		// 否则 CanAutoRenew 会一直返回 false（第 1 条规则优先看 ProbeState）。
		if _, ok := codearts.IDPDeadline(x.Cookies); ok {
			x.ProbeState = "ok"
		} else {
			x.ProbeState = "session_only"
		}
		return nil
	})
}

// codeartsPersistRenewCookiesIfNew 续期"失败但已换回 Cookie"时保住快照。
func codeartsPersistRenewCookiesIfNew(snap *codearts.Account, now time.Time) {
	if snap == nil || snap.ID == "" || len(snap.Cookies) == 0 {
		return
	}
	at := now.Format(time.RFC3339)
	err := codearts.UpdatePool(func(pool *codearts.Pool) error {
		x := codeartsFindAccount(pool, snap.ID)
		if x == nil {
			return nil
		}
		if codeartsCookieDigest(x.Cookies) == codeartsCookieDigest(snap.Cookies) {
			return nil
		}
		if !codeartsShouldTakeCaptureCookies(x, snap.Cookies, now) {
			return nil
		}
		x.Cookies = append([]codearts.Cookie(nil), snap.Cookies...)
		x.SavedAt = at
		x.RefreshTicketDeadline()
		return nil
	})
	if err != nil {
		log.Printf("[codearts] 账号 %s 续期失败后的 Cookie 快照写号池失败: %v", snap.ID, err)
	}
}

// codeartsRenewMarkProbeFailed 补采环境失败时的 Probe* 落盘。
//
// 只有这里写 ProbeState=failed，DecideRenew 第 5 条才会被 ProbeAt 拦住，
// 不会每 15 分钟重开一次浏览器（契约 2.2.5 的硬要求）。
func codeartsRenewMarkProbeFailed(id string, now time.Time, msg string) {
	if id == "" {
		return
	}
	at := now.Format(time.RFC3339)
	err := codearts.UpdatePool(func(pool *codearts.Pool) error {
		x := codeartsFindAccount(pool, id)
		if x == nil {
			return nil
		}
		x.ProbeState = "failed"
		x.ProbeMsg = msg
		x.ProbeAt = at
		return nil
	})
	if err != nil {
		log.Printf("[codearts] 账号 %s 补采失败状态写号池失败: %v", id, err)
	}
}

// codeartsPersistRenewFail 失败分支落盘（契约要求字段一个都不能少）。
func codeartsPersistRenewFail(id string, now time.Time, result, msg string, nextAt time.Time) error {
	if id == "" {
		return errors.New("账号为空")
	}
	if nextAt.IsZero() {
		nextAt = now.Add(codearts.RenewMinGap)
	}
	at := now.Format(time.RFC3339)
	return codearts.UpdatePool(func(pool *codearts.Pool) error {
		x := codeartsFindAccount(pool, id)
		if x == nil {
			return fmt.Errorf("账号已不在号池: %s", id)
		}
		x.RenewAttemptAt = at
		x.RenewFailAt = at
		x.RenewFailMsg = msg
		x.RenewResult = result
		x.RenewNextAt = nextAt.Format(time.RFC3339)
		return nil
	})
}

// codeartsRenewFailLogged 落盘失败结论 + 每天最多一条日志。
//
// 只用于**真的发起过续期**的失败（起过无头浏览器、链路没走通）。
// 没试过的"判定结论"走 codeartsRenewConclusionLogged。
func codeartsRenewFailLogged(id string, now time.Time, result, msg string, nextAt time.Time) {
	if err := codeartsPersistRenewFail(id, now, result, msg, nextAt); err != nil {
		log.Printf("[codearts] 账号 %s 续期结论写号池失败: %v", id, err)
	}
	if !codeartsNoteOnce("codearts:renew:" + id) {
		return
	}
	codeartsRenewLogf("[codearts] 账号 %s 静默续期未成功（%s）: %s；下次检查 %s",
		id, result, msg, nextAt.Format(time.RFC3339))
}

// codeartsRenewConclusionLogged 落盘"**本轮没有真的发起续期**"的结论。
//
// 与 codeartsRenewFailLogged 的区别只有一处，但很关键：
// **不写 RenewAttemptAt**。RenewAttemptAt 的语义是"真的起过浏览器试过了"，
// 旧实现把判定结论也写成"刚刚试过"，于是面板显示成"刚试过又失败"，
// 2 小时节流也误判成"刚失败过" —— 用户看到的"续期不工作"就是这么来的。
func codeartsRenewConclusionLogged(id string, now time.Time, result, msg string, nextAt time.Time) {
	if id == "" {
		return
	}
	if nextAt.IsZero() {
		nextAt = now.Add(codearts.ManualRecheck)
	}
	at := now.Format(time.RFC3339)
	err := codearts.UpdatePool(func(pool *codearts.Pool) error {
		x := codeartsFindAccount(pool, id)
		if x == nil {
			return fmt.Errorf("账号已不在号池: %s", id)
		}
		x.RenewResult = result
		x.RenewFailAt = at
		x.RenewFailMsg = msg
		x.RenewNextAt = nextAt.Format(time.RFC3339)
		// RenewAttemptAt 保持原值：本轮没起过浏览器。
		return nil
	})
	if err != nil {
		log.Printf("[codearts] 账号 %s 续期结论写号池失败: %v", id, err)
	}
	if !codeartsNoteOnce("codearts:renew:" + id) {
		return
	}
	codeartsRenewLogf("[codearts] 账号 %s 静默续期未成功（%s）: %s；下次检查 %s",
		id, result, msg, nextAt.Format(time.RFC3339))
}

// ── 执行钩子（生产直连真实实现） ────────────────────────────────────────

func codeartsRenewKeepalive(a *codearts.Account) (bool, error) {
	if codeartsRenewKeepaliveHook != nil {
		return codeartsRenewKeepaliveHook(a)
	}
	return codearts.Keepalive(a)
}

func codeartsRenewBackfill(p *Panel, a *codearts.Account) (*codearts.CaptureResult, error) {
	if codeartsRenewBackfillHook != nil {
		return codeartsRenewBackfillHook(p, a)
	}
	return p.codeartsBackfillAccount(a)
}

func codeartsRenewSSO(p *Panel, a *codearts.Account) (phase, msg string) {
	if codeartsRenewSSOHook != nil {
		return codeartsRenewSSOHook(p, a)
	}
	return p.codeartsSilentRenew(a)
}

// ── HTTP 端点（C-CA-API@1 D，路由由 T11 注册） ──────────────────────────

// codeartsRenewResult POST renew 的单账号结果（契约 2.2.4 D）。
type codeartsRenewResult struct {
	ID             string `json:"id"`
	Phase          string `json:"phase"`
	Message        string `json:"message"`
	SavedAt        string `json:"saved_at"`
	CookieCount    int    `json:"cookie_count"`
	TicketDeadline string `json:"ticket_deadline"`
	RenewNextAt    string `json:"renew_next_at"`
	RenewResult    string `json:"renew_result"`
}

// codeartsRenewHTTP POST /panel/api/ide/codearts/renew
//
// 串行执行：浏览器是独占资源，不能并发起两个 Chrome。返回字段以**号池实际
// 落盘值**为准（续期会改 Cookie/票据/下次时刻，直接用内存快照会与文件不一致）。
func (p *Panel) codeartsRenewHTTP(w http.ResponseWriter, r *http.Request) {
	ids, force, err := codeartsIDsFromBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pool, err := codearts.LoadPool()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读号池失败: "+err.Error())
		return
	}
	targets := codeartsSelectAccounts(pool, ids)
	results := make([]codeartsRenewResult, 0, len(targets))
	for _, a := range targets {
		phase, msg := p.codeartsRenewAccount(a, force)
		out := codeartsRenewResult{ID: a.ID, Phase: phase, Message: msg}
		if phase == "throttled" {
			// 契约 D：未 force 且在 2 小时窗口内返回 rejected。
			out.Phase = "rejected"
		}
		// 手动续期走到 needs_login：如实说清楚"静默续期为什么没戏"，
		// 并把下一步指给用户（面板上的「打开登录窗口」按钮，人工扫码一次即可）。
		// 这里**不**自动弹浏览器 —— 用户是自己在面板上点的，替他弹窗没意义。
		if phase == "needs_login" {
			out.Message += "；静默续期走不通（华为云会话与 CAS 票据均已失效，实测停在人工登录页），" +
				"请点该号的「打开登录窗口」扫码登录一次，登录后当天积分会自动补领"
		}
		if latest, lerr := codearts.LoadPool(); lerr == nil {
			if x := codeartsFindAccount(latest, a.ID); x != nil {
				out.SavedAt = x.SavedAt
				out.CookieCount = len(x.Cookies)
				out.TicketDeadline = x.TicketDeadline
				out.RenewNextAt = x.RenewNextAt
				out.RenewResult = x.RenewResult
			}
		}
		results = append(results, out)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

// ── SSO 换会话链 ────────────────────────────────────────────────────────

// codeartsSilentRenew 用号池里的持久票据走一遍 SSO 链，换回新的应用会话。
//
// 步骤：无头浏览器 → 注入全部 Cookie（含 CAS 票据）→ 打开
// `authui/login?service=<控制台首页>` → 等 JS/302 链跑完 → 回到 devcloud 域
// 就说明静默登录成立，把新 Cookie 收回账号；停在 auth 域就是票据已经失效。
//
// 返回值 phase：renewed（换回且接口复验通过）/ needs_login（停在人工登录页）/
// failed（环境或复验失败，可 2 小时后重试）。
func (p *Panel) codeartsSilentRenew(acc *codearts.Account) (phase, msg string) {
	if acc == nil || len(acc.Cookies) == 0 {
		return "failed", "账号没有 Cookie"
	}
	if codeartsBrowserBusy() {
		return "failed", "登录窗口正开着，跳过本轮"
	}
	chrome, err := findChrome()
	if err != nil {
		return "failed", "找不到 Chrome: " + err.Error()
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		return "failed", "没有空闲调试端口: " + err.Error()
	}
	// 续期必须在该账号**自己的槽**里跑。
	//
	// 旧实现每次 os.MkdirTemp("", "ca-renew-*")：每续一次就是一台全新设备
	// （空 profile、新 Local State/Network Persistent State），对服务端是
	// "同一账号设备频繁变更" —— 典型的异常信号；而且槽里长期积累的设备身份
	// 在续期链上一次都用不上。
	//
	// 槽锁覆盖整个浏览器生命周期：同一账号不能两个 Chrome 抢同一个
	// user-data-dir（第二个会复用/被拒，拿到的可能是上一次的残留）。
	unlockSlot := acquireCodeartsSlotLock(codeartsSlotLockKey(acc))
	defer unlockSlot()
	dir, derr := p.ensureCodeartsSlotDir(acc)
	if derr != nil {
		return "failed", "定位账号身份槽失败: " + derr.Error()
	}
	cleanup, err := launchChromeWithArgs(chrome, codeartsHeadlessChromeArgs(port, dir, acc))
	if err != nil {
		return "failed", "起无头浏览器失败: " + err.Error()
	}
	// ⚠️ 必须清理**进程**：go test/长跑进程里遗留无头 Chrome 会一直占着调试端口
	// 和内存（踩过：攒出 97 个孤儿进程，把网关自己挤挂了）。
	// ⚠️ 但**绝不删除槽目录**：那是这个号的"设备"，删了下次续期就是新设备。
	defer cleanup()

	var ws *wsConn
	for i := 0; i < 30 && ws == nil; i++ {
		time.Sleep(500 * time.Millisecond)
		ws, err = caOpenTab(port, "about:blank")
	}
	if ws == nil {
		return "failed", "连不上无头浏览器: " + errStr(err)
	}
	defer ws.Close()

	// Cookie + 槽指纹（UA/语言/时区）一次做完：顺序必须是「Cookie → 指纹 →
	// Page.navigate」，否则首个请求已经带着真实 UA 发出去，再改只对后续 XHR 生效。
	// 指纹失败只记日志，不阻断（见 caSetCookiesAndFingerprint）。
	loginURL := "https://auth.huaweicloud.com/authui/login?service=" +
		url.QueryEscape(codearts.APIBase+"/chat/home")

	// ── 两次尝试 ──────────────────────────────────────────────────────
	// 第一次只带**长期票据**（不带已死的 SSO 会话 Cookie），第二次带全量。
	// 为什么要分两步，见 codeartsSSOSessionCookies 的说明。
	//
	// 每次尝试 = 注入 Cookie → 导航 → 等跳回 devcloud 域。
	tryOnce := func(cookies []codearts.Cookie) (string, bool, error) {
		if err := caSetCookiesAndFingerprintWith(port, ws, acc, cookies); err != nil {
			return "", false, err
		}
		if err := caNavigate(ws, loginURL); err != nil {
			return "", false, err
		}
		back := ""
		for i := 0; i < 25; i++ {
			time.Sleep(2 * time.Second)
			cur := caPageURL(port, "")
			if cur == "" || strings.Contains(cur, "about:blank") {
				continue
			}
			back = cur
			if strings.Contains(hostOf(cur), "devcloud") {
				break
			}
		}
		return back, strings.Contains(hostOf(back), "devcloud"), nil
	}

	back, ok, err := tryOnce(codeartsCookiesWithoutSSOSession(acc.Cookies))
	if err != nil {
		return "failed", "注入 Cookie/导航失败: " + err.Error()
	}
	if !ok {
		// 第二次：带上全量 Cookie。若浏览器里已经有了**新签发**的
		// SSOTGC/SSOJTC（第一次尝试过程中 authui 发的），以浏览器里的为准，
		// 别用号池里那条早就作废的旧值把它盖掉。
		full := codeartsCookiesMergedWithBrowser(acc.Cookies, port)
		back2, ok2, err2 := tryOnce(full)
		if err2 != nil {
			return "failed", "注入 Cookie/导航失败: " + err2.Error()
		}
		if ok2 {
			back = back2
			ok = true
		} else {
			back = back2
		}
	}
	if !ok {
		// 停在 auth 域 = 票据被服务端拒绝，只能人工重新登录。
		// 这不是"可重试的网络错误"：2 小时后重试同一个死票据仍会停在登录页。
		return "needs_login", "票据换不回会话（停在 " + back + "），需要人工重新登录"
	}
	// 回到控制台 = 静默登录成立；把新会话收回账号。
	all, cerr := cdpAllCookies(port)
	if cerr != nil {
		return "failed", "回控制台成功但取 Cookie 失败: " + cerr.Error()
	}
	fresh := codeartsCookiesOf(all)
	if len(fresh) == 0 {
		return "failed", "回控制台成功但没取到 Cookie"
	}
	acc.Cookies = fresh
	acc.SavedAt = codearts.Now()
	acc.RefreshTicketDeadline()
	// 会话此刻到底通不通，以接口为准（页面回到控制台不等于应用会话有效）。
	if ok, kerr := codearts.Keepalive(acc); !ok {
		return "failed", "回到控制台但接口仍不通: " + errStr(kerr)
	}
	return "renewed", "已换回新会话（Cookie " + codearts.Seq(len(fresh)) + " 条）"
}

// ── 无头浏览器 ──────────────────────────────────────────────────────────

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// codeartsHeadlessChromeArgs 无头浏览器（补采 / 续期）的完整启动参数。
//
// 抽成纯函数是为了让单测直接断言两条硬约束，而不是靠人眼看 exec.Command 实参：
//   - `--user-data-dir=<该账号的槽 profile>`：绝不能是 os.MkdirTemp 的临时目录，
//     否则每次续期/补采都是"一台新设备"（风控异常信号）；
//   - 槽指纹（`--window-size` / `--lang` / `--accept-lang`，有出口时还有
//     `--proxy-server`）：同槽跨调用必须完全一致。
//
// acc 为空时原样返回骨架参数（与引入指纹/槽之前的启动参数逐字节一致）。
func codeartsHeadlessChromeArgs(port int, profileDir string, acc *codearts.Account) []string {
	return codeartsChromeArgsForAccount(codeartsChromeArgs(port, profileDir, "about:blank", true), acc)
}

// launchChromeHeadless 兼容旧签名：无账号 → 不追加指纹（调用方是测试工具）。
func launchChromeHeadless(chrome string, port int, dir string) (func(), error) {
	return launchChromeWithArgs(chrome, codeartsHeadlessChromeArgs(port, dir, nil))
}

// launchChromeWithArgs 用给定参数起无头浏览器，返回**只清理进程**的清理函数。
//
// ⚠️ 必须把清理函数 defer 掉：子进程会被**遗留**（孤儿 Chrome），一个个占着
// 9333-9341 调试端口还吃内存 —— 实测漏了 97 个进程，最后把网关自己挤挂了。
// 注意清理函数不碰 profile 目录：那是账号的设备身份，归槽生命周期管。
func launchChromeWithArgs(chrome string, args []string) (func(), error) {
	cmd := exec.Command(chrome, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	cleanup := func() {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(8 * time.Second):
		}
	}
	return cleanup, nil
}
