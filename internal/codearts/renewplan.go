// renewplan.go CodeArts 静默续期的**纯决策**（C-CA-RENEW@1）。
//
// 为什么要把决策单独抽成一个纯函数：
//
//	旧版 codeartsSilentRenewOnce 在 TicketDeadline 为空时直接 continue，只打一条
//	"下次重新登录会采集"的日志 —— 而那条日志承诺的代码根本不存在，于是旧号池
//	（没有 expires 信息）永远不续期，会话到期后只能人工扫码。
//
// 现在把"什么时候该续、该先补采、还是只能人工登录"全部收敛到 DecideRenew，
// 它不碰磁盘、不起浏览器、不打日志，只有 Account + 当前时刻 + 两个外部事实
// （会话活没活、登录窗口占没占），因此七条分支可以用表驱动测试逐条钉死。
package codearts

import (
	"strings"
	"time"
)

const (
	// ProbeGap 补采探测成功后的节流窗口：会话票据 6 小时内不会凭空获得绝对
	// 过期时间，重复补采只会多起一次无头浏览器。
	ProbeGap = 6 * time.Hour
	// RenewMinGap 两次静默续期之间的最小间隔（可恢复失败后的重试间隔）。
	// 续期要起一个无头浏览器，不能跟着 15 分钟的巡检反复开。
	RenewMinGap = 2 * time.Hour
	// AliveRecheck 会话/续期都正常时的下一次检查间隔（与巡检周期同值）。
	AliveRecheck = 15 * time.Minute
	// ManualRecheck 需要人工登录（会话已死/票据已过期）后的检查间隔。
	// 这种状态**不可自愈**，15 分钟重试只会把日志和浏览器都刷爆；
	// 一天一次足够发现"用户已经重新登录"。
	ManualRecheck = 24 * time.Hour
)

// RenewDecision 一轮静默续期的决策结果（纯值，可安全并发读取）。
type RenewDecision struct {
	// Attempt 本轮是否发起静默续期（起无头浏览器走 SSO 链）。
	Attempt bool
	// Probe 续期前是否先补采（无票据时间时先探测，不能拿"没有寿命信息"当"不可续期"）。
	Probe bool
	// Reason 固定枚举：probe_required|ticket_expired|renew_due|alive|busy|throttled|needs_login。
	Reason string
	// NextAt 下一次检查时刻（调用方据此写 RenewNextAt / 决定 15 分钟巡检是否跳过）。
	NextAt time.Time
}

// DecideRenew 判断这一轮该不该续期（判定顺序必须与 C-CA-RENEW@1 完全一致）。
//
// 参数：
//   - sessionAlive 调用方刚验活的结果（true = 会话还能用）；
//   - busy 登录窗口/其它浏览器会话是否占用（浏览器是独占资源）。
//
// 七条分支按顺序短路，**顺序本身就是契约**：
//  1. busy → 不动，15 分钟后再看；
//  2. 会话活着 → 不用续，15 分钟后再看；
//  3. 补采确认会话已死 / 上次续期停在人工登录页 → 只能人工登录，24 小时后再看
//     （**且只在没有新证据时**——用户重新登录后旧结论作废，见 HasFreshLogin；
//     force 可以越过这条，让手动「立即续期」真的试一次）；
//  4. 已知的绝对票据时间已过 → 同上（票据死了静默链不可能成立，force 也不越过）；
//  5. 没有绝对票据时间，且 6 小时内没补采过 → **先补采**（这条是缺陷一的修复点：
//     旧版在这里 continue 了，等于永远不续）；
//  6. 2 小时内尝试过 → 节流（force 能越过这一条）；
//  7. 其余 → 正常续期。
//
// 防御：a == nil 时按 busy 处理（没有账号就不该动浏览器）。契约的 Reason 枚举里
// 没有单独的错误值，用 busy 表达"本轮不执行"最贴近。
func DecideRenew(a *Account, now time.Time, sessionAlive bool, busy bool) RenewDecision {
	// 1. 浏览器被占用：不起第二个（会抢同一个调试端口区间，还会弄乱用户的窗口）。
	if busy || a == nil {
		return RenewDecision{Attempt: false, Probe: false, Reason: "busy", NextAt: now.Add(AliveRecheck)}
	}
	// 2. 会话还活着：没有任何理由续期，便宜地再等一轮。
	if sessionAlive {
		return RenewDecision{Attempt: false, Probe: false, Reason: "alive", NextAt: now.Add(AliveRecheck)}
	}
	// 3. 会话/票据已死：续期链只会停在人工登录页，只能等用户重新登录。
	//
	//    ⚠️ 但这只是**上一次尝试的结论**，不是账号的固有属性：
	//    用户重新登录成功后 SavedAt 会更新到结论之后，旧结论必须作废，
	//    否则账号被永久锁死（实测 2026-11-04：用户登录成功了，面板仍显示
	//    「只能重新登录」，手动「立即续期」也秒回 needs_login）。
	//    所以这一条只在**没有新证据**时成立。
	if !a.HasFreshLogin() {
		if a.ProbeState == "login_dead" || a.RenewResult == "needs_login" {
			return RenewDecision{Attempt: false, Probe: false, Reason: "needs_login", NextAt: now.Add(ManualRecheck)}
		}
	}
	// 4. 绝对票据时间已过：票据本身失效，静默链不可能换回会话。
	//    TicketDeadline 解析失败按"未知"处理（不 panic、不当作已过期）。
	if ddl, err := time.Parse(time.RFC3339, strings.TrimSpace(a.TicketDeadline)); err == nil && !now.Before(ddl) {
		return RenewDecision{Attempt: false, Probe: false, Reason: "ticket_expired", NextAt: now.Add(ManualRecheck)}
	}
	// 5. 没有绝对票据时间：先补采。**这就是旧版逃掉的分支** ——
	//    TicketDeadline 为空不代表票据已死（会话级票据是常态），只代表"还没观测"。
	//    补采成功后才可能拿到绝对时刻或确认会话级，两种情况都允许继续续期。
	if strings.TrimSpace(a.TicketDeadline) == "" {
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(a.ProbeAt))
		if err != nil || !now.Before(at.Add(ProbeGap)) {
			return RenewDecision{Attempt: true, Probe: true, Reason: "probe_required", NextAt: now.Add(ProbeGap)}
		}
	}
	// 6. 2 小时节流：上一次已经试过了，别跟着 15 分钟巡检反复起浏览器。
	if at, err := time.Parse(time.RFC3339, strings.TrimSpace(a.RenewAttemptAt)); err == nil {
		if next := at.Add(RenewMinGap); now.Before(next) {
			return RenewDecision{Attempt: false, Probe: false, Reason: "throttled", NextAt: next}
		}
	}
	// 7. 正常续期。
	return RenewDecision{Attempt: true, Probe: false, Reason: "renew_due", NextAt: now.Add(AliveRecheck)}
}

// ForceRenewDecision force=true 时把"节流"和"上次结论是需人工登录"升级为立即续期。
//
// force 的语义必须精确：
//   - 跳过第 6 条（2 小时流量节流）——这是 force 本来就有的能力；
//   - 跳过第 3 条（needs_login）——**这是 2026-11-04 新增的**：
//     手动点「立即续期」= 用户明确要求"再试一次"。旧实现对 needs_login
//     直接短路，于是按钮 386ms 就返回一句 needs_login，什么都没做，
//     用户看到的就是"手动续期没用"。现在 force 必须**真的发起一次尝试**
//     （起一次无头浏览器走 SSO 链）；失败再如实报 needs_login 不迟。
//   - 仍**不得**跳过 ticket_expired（绝对票据已经过期，硬试只是白起浏览器）、
//     alive（会话还活着，没什么可续）、busy（浏览器被占）、probe_required
//     （没有票据证据时必须先补采，否则旧号池又会退化成"永远不续"）。
func ForceRenewDecision(d RenewDecision, now time.Time) RenewDecision {
	switch d.Reason {
	case "throttled":
		return RenewDecision{Attempt: true, Probe: false, Reason: "renew_due", NextAt: now.Add(AliveRecheck)}
	case "needs_login":
		return RenewDecision{Attempt: true, Probe: false, Reason: "renew_due", NextAt: now.Add(AliveRecheck)}
	}
	return d
}
