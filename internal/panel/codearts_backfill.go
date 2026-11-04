// codearts_backfill.go CodeArts 票据寿命补采（C-CA-CAPTURE@1 的面板侧实现）。
//
// 为什么需要补采：2026-11-02 及更早的号池是"只存 name/value"的旧快照（实测
// 51/55 条 Cookie 里**没有一条**带 expires），因此 ticket_deadline 永远为空，
// 自动续期永远判不出"票据还能活多久"，只能等接口报死再人工扫码。
//
// 补采做的事：拿号池里的 Cookie 注入一个全新的无头浏览器 → 打开华为云 SSO 链
// → 若仍能回到 devcloud 域，说明这批票据还活着 → 用 CDP Storage.getCookies
// 读回**浏览器自己认的**过期时间（含服务端这次新下发的 Max-Age）→ 归一后回写。
//
// 两条硬约束：
//  1. 浏览器**进程**必须无条件清理（曾攒出 97 个孤儿 Chrome，把网关自己挤挂过）；
//     但槽 profile 是长期资产，绝不随补采结束删除；
//  2. 判不准就如实说"会话已死/没采到"，**不编造** ticket_deadline。
package panel

import (
	"errors"
	"fmt"
	"log"
	"net/url"

	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

const (
	// codeartsProbeGap 补采探测的节流窗口（与 C-CA-RENEW@1 的 ProbeGap 同值）。
	//
	// 在这里单独定义而不是 import T3 的 renewplan.go：依赖方向是 T3 → T2，
	// 反过来会让 T2 编译依赖尚未落地的 T3。
	codeartsProbeGap = 6 * time.Hour

	// codeartsProbeFailGap 探测**失败**后的重试间隔（R1.3.2：≥2 小时）。
	// 成功一次要等 6 小时，失败后 2 小时就可以再试 —— 否则一次 DNS 抖动就
	// 会把账号锁在"6 小时内不再探测"里。
	codeartsProbeFailGap = 2 * time.Hour

	// codeartsCaptureWaitSteps/Step 等浏览器回到 devcloud 域的上限（约 50 秒）。
	codeartsCaptureWaitSteps = 25
	codeartsCaptureWaitStep  = 2 * time.Second
)

// caEvidenceTicketNames Evidence 里逐个报状态的 SSO 票据名。
//
// 与 internal/codearts 的 ssoTicketNames 必须是**同一份名单**（那份未导出，
// 只能在这里镜像一份；两边改名时一起改）。
var caEvidenceTicketNames = []string{"SSOTGC", "SSOJTC", "CASTGCLOW", "hwid_cas_sid", "cbc-sid"}

// ── 归一与 Evidence ─────────────────────────────────────────────────────

// codeartsExpiresKnownCount 已真实观测到过期信息的 Cookie 条数。
func codeartsExpiresKnownCount(cs []codearts.Cookie) int {
	n := 0
	for _, c := range cs {
		if c.ExpiresKnown {
			n++
		}
	}
	return n
}

// codeartsSessionCookieCount 会话级 Cookie 条数（CDP expires=-1 且已观测）。
func codeartsSessionCookieCount(cs []codearts.Cookie) int {
	n := 0
	for _, c := range cs {
		if c.Session && c.ExpiresKnown {
			n++
		}
	}
	return n
}

// codeartsEvidenceOf 拼 C-CA-CAPTURE@1 规定的 Evidence 串。
//
// 固定格式（顺序 = caEvidenceTicketNames，末尾两个固定统计项）：
//
//	SSOTGC=session;SSOJTC=1767225600;CASTGCLOW=unknown;expires_known=37/55;session_cookies=14
//
// 每张票据的取值只有三种：`session`（已观测的会话 Cookie）、Unix 秒（已观测的
// 绝对过期时刻，同名多条取最早）、`unknown`（没采到 / 没观测过）。
func codeartsEvidenceOf(cs []codearts.Cookie) string {
	byName := map[string][]codearts.Cookie{}
	for _, c := range cs {
		if c.Name == "" {
			continue
		}
		k := strings.ToLower(c.Name)
		byName[k] = append(byName[k], c)
	}
	parts := make([]string, 0, len(caEvidenceTicketNames)+2)
	for _, n := range caEvidenceTicketNames {
		parts = append(parts, n+"="+codeartsTicketStateOf(byName[strings.ToLower(n)]))
	}
	parts = append(parts, fmt.Sprintf("expires_known=%d/%d",
		codeartsExpiresKnownCount(cs), len(cs)))
	parts = append(parts, "session_cookies="+strconv.Itoa(codeartsSessionCookieCount(cs)))
	return strings.Join(parts, ";")
}

// codeartsTicketStateOf 同一条票据（可能跨多个 domain/path）在 Evidence 里的取值。
//
// 优先级：有绝对过期 → 报最早时刻（这才是决定"什么时候必须续期"的那个数）；
// 否则只要有会话级声明 → session；再否则 → unknown（未知，不许当成 0）。
func codeartsTicketStateOf(sameName []codearts.Cookie) string {
	var best float64
	seenSession := false
	for _, c := range sameName {
		if c.ExpiresKnown && c.Expires > 0 {
			if best == 0 || c.Expires < best {
				best = c.Expires
			}
			continue
		}
		if c.Session && c.ExpiresKnown {
			seenSession = true
		}
	}
	if best > 0 {
		return strconv.FormatInt(int64(best), 10)
	}
	if seenSession {
		return "session"
	}
	return "unknown"
}

// codeartsCaptureResultFromCookies 把一批归一后的 Cookie 变成 CaptureResult。
//
// 这是登录保存与补采**共用**的唯一判定入口：两边各写一份迟早漂移，而
// "重新登录也不采集过期时间"正是两条路径语义不一致造成的。
func codeartsCaptureResultFromCookies(cs []codearts.Cookie) *codearts.CaptureResult {
	res := &codearts.CaptureResult{
		Cookies:            cs,
		ExpiresKnownCount:  codeartsExpiresKnownCount(cs),
		SessionCookieCount: codeartsSessionCookieCount(cs),
		Evidence:           codeartsEvidenceOf(cs),
	}
	if len(cs) == 0 {
		res.Phase = "no_cookie"
		res.Message = "没有采集到华为云 Cookie"
		return res
	}
	res.OK = true
	if deadline, ok := codearts.IDPDeadline(cs); ok {
		res.Phase = "probed"
		res.TicketSource = "cookie"
		res.TicketDeadline = deadline.Format(time.RFC3339)
		res.Message = fmt.Sprintf("已登录；票据最早于 %s 过期（已观测 %d/%d 条 Cookie）",
			res.TicketDeadline, res.ExpiresKnownCount, len(cs))
		return res
	}
	// 没采到绝对过期：可能是会话级（expires=-1），也可能是这批 Cookie 根本不带
	// 过期信息（旧快照注入后浏览器按会话 Cookie 处理）。两者都**允许续期**，
	// 但必须如实说出是哪一种，不能笼统写成"已采到寿命"。
	res.Phase = "session_only"
	res.TicketSource = "session"
	res.TicketDeadline = ""
	res.Message = fmt.Sprintf("已登录；SSO 票据无绝对过期（会话级 %d 条 / 已观测 %d/%d 条），仍可尝试续期",
		res.SessionCookieCount, res.ExpiresKnownCount, len(cs))
	return res
}

// codeartsProbeStateOf CaptureResult.Phase → Account.ProbeState 的固定映射。
func codeartsProbeStateOf(phase string) string {
	switch phase {
	case "probed":
		return "ok"
	case "session_only":
		return "session_only"
	case "login_dead":
		return "login_dead"
	case "no_cookie", "error":
		return "failed"
	}
	return ""
}

// ── 抓取 ────────────────────────────────────────────────────────────────

// codeartsCaptureFromPort 从**已打开**的调试端口抓取全部 Cookie 并归一。
//
// 返回的 error 只表示"抓不到"（端口没了/CDP 出错），此时 Phase=error；
// 抓到了但没 Cookie 时 Phase=no_cookie 且 err=nil —— 调用方据此区分
// "浏览器不可用"与"浏览器里确实没登录态"。
func (p *Panel) codeartsCaptureFromPort(port int) (*codearts.CaptureResult, error) {
	all, err := cdpAllCookies(port)
	if err != nil {
		return &codearts.CaptureResult{
			Phase:   "error",
			Message: "读取浏览器 Cookie 失败: " + err.Error(),
		}, err
	}
	return codeartsCaptureResultFromCookies(codeartsCookiesOf(all)), nil
}

// ── 补采（注入 → 打开 SSO 链 → 判登录 → 读 Cookie → 回写） ──────────────

// codeartsBackfillAccount 给一个旧账号补采 Cookie 过期信息。
//
// 流程与判据（C-CA-CAPTURE@1）：
//  1. 号池里没有 Cookie → phase=no_cookie（不起浏览器，不写盘）；
//  2. 找不到 Chrome / 没空闲端口 → phase=error（Message 原文，**不改写号池**）；
//  3. 注入 Cookie → 打开 authui/login?service=<APIBase>/chat/home；
//  4. 停在 auth 域 → 会话已死 → phase=login_dead（写 ProbeState/ProbeMsg/ProbeAt，
//     清空 TicketDeadline）；
//  5. 回到 devcloud 域 → 抓 Cookie 归一 → probed（有绝对票据）/ session_only。
//
// 无论走哪条分支，浏览器进程都在 defer 里清理；槽 profile 保留。
func (p *Panel) codeartsBackfillAccount(a *codearts.Account) (*codearts.CaptureResult, error) {
	if a == nil || a.ID == "" {
		return &codearts.CaptureResult{Phase: "error", Message: "账号为空"}, errors.New("账号为空")
	}
	if len(a.Cookies) == 0 {
		return &codearts.CaptureResult{
			Phase:   "no_cookie",
			Message: "这个账号没有 Cookie，无法补采；请先重新登录或粘贴 Cookie",
		}, nil
	}

	// 登录窗口是独占资源：同时开两个会抢同一个调试端口区间，还会让用户看到
	// 两个莫名其妙的窗口。
	codeartsBrowserSession.mu.Lock()
	busy := codeartsBrowserSession.cmd != nil
	codeartsBrowserSession.mu.Unlock()
	if busy {
		return &codearts.CaptureResult{
			Phase:   "error",
			Message: "登录窗口正开着，请先关闭再补采",
		}, errors.New("登录窗口正开着")
	}

	chrome, err := findChrome()
	if err != nil {
		return codeartsBackfillError("找不到 Chrome / Edge: " + err.Error())
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		return codeartsBackfillError("没有空闲调试端口: " + err.Error())
	}
	// 补采必须在**该号自己的槽**里跑：每次 os.MkdirTemp 都是"一台新设备"，
	// 对服务端就是设备频繁变更（风控异常信号，见 §H.12 冲突点）。
	// 槽不存在时先建空槽再注入 Cookie；**不能**复制别的号的 profile ——
	// 那会把 A 的设备身份/会话带进 B（违反 §H.I3）。
	//
	// 整个浏览器生命周期持槽锁：同一账号并发补采时不能两个 Chrome 抢同一个
	// user-data-dir（Chrome 会直接复用/拒绝，采到的可能是上一次的残留）。
	unlockSlot := acquireCodeartsSlotLock(codeartsSlotLockKey(a))
	defer unlockSlot()
	dir, derr := p.ensureCodeartsSlotDir(a)
	if derr != nil {
		return codeartsBackfillError("建账号身份槽失败: " + derr.Error())
	}
	cleanup, err := launchChromeWithArgs(chrome, codeartsHeadlessChromeArgs(port, dir, a))
	if err != nil {
		return codeartsBackfillError("起无头浏览器失败: " + err.Error())
	}
	// ⚠️ 必须清理进程：孤儿 Chrome 会一直占着调试端口与内存（实测攒出 97 个）。
	// 槽目录不删：它是这个账号的"设备"，删了下次补采就是新设备。
	defer cleanup()

	var ws *wsConn
	for i := 0; i < 30 && ws == nil; i++ {
		time.Sleep(500 * time.Millisecond)
		ws, err = caOpenTab(port, "about:blank")
	}
	if ws == nil {
		return codeartsBackfillError("连不上无头浏览器: " + errStr(err))
	}
	defer ws.Close()

	// 注入时带真实 expires（caSetCookies 已按 C-CA-COOKIE@1 保留过期信息）：
	// 漏掉它会让所有票据变成会话 Cookie，补采出来的只有 session、没有绝对时刻。
	// 同一入口顺带应用该账号的槽指纹（UA/语言/时区）：必须在 Page.navigate 之前，
	// 否则首个请求已经带着真实 UA 发出去。指纹失败只记日志，不阻断补采。
	if err := caSetCookiesAndFingerprint(port, ws, a); err != nil {
		return codeartsBackfillError("注入 Cookie 失败: " + err.Error())
	}
	loginURL := "https://auth.huaweicloud.com/authui/login?service=" +
		url.QueryEscape(codearts.APIBase+"/chat/home")
	if err := caNavigate(ws, loginURL); err != nil {
		return codeartsBackfillError("导航失败: " + err.Error())
	}

	back := ""
	for i := 0; i < codeartsCaptureWaitSteps; i++ {
		time.Sleep(codeartsCaptureWaitStep)
		cur := caPageURL(port, "")
		if cur == "" || strings.Contains(cur, "about:blank") {
			continue
		}
		back = cur
		// ⚠️ 必须按 host 判：authui/login?service=… 的整串 URL 里就带着 devcloud，
		// 用子串匹配会把"停在登录页"误判成"已经回到控制台"。
		if strings.Contains(hostOf(cur), "devcloud") {
			break
		}
	}
	if !strings.Contains(hostOf(back), "devcloud") {
		res := &codearts.CaptureResult{
			Phase:   "login_dead",
			Message: "注入后仍停在 " + back + "，会话已失效，需要人工重新登录",
		}
		codeartsPersistCaptureLogged(a.ID, res, time.Now())
		return res, nil
	}

	res, cerr := p.codeartsCaptureFromPort(port)
	if cerr != nil {
		// 已经回到控制台但读不到 Cookie：不改写号池（判不准就别动数据）。
		return res, cerr
	}
	codeartsPersistCaptureLogged(a.ID, res, time.Now())
	return res, nil
}

// codeartsBackfillError 造一个"浏览器不可用"的结果（这类错误一律不改写号池）。
func codeartsBackfillError(msg string) (*codearts.CaptureResult, error) {
	return &codearts.CaptureResult{Phase: "error", Message: msg}, errors.New(msg)
}

// codeartsPersistCaptureLogged 回写补采结果并记一条日志（失败只记日志，不改变
// 已返回给调用方的结果）。
func codeartsPersistCaptureLogged(id string, res *codearts.CaptureResult, now time.Time) {
	if err := codeartsPersistCapture(id, res, now); err != nil {
		log.Printf("[codearts] 账号 %s 补采结果回写号池失败: %v", id, err)
		return
	}
	log.Printf("[codearts] 账号 %s 补采结果: phase=%s evidence=%s", id, res.Phase, res.Evidence)
}

// codeartsPersistCapture 把一次补采结果写回号池（必须幂等）。
//
// 幂等要点：
//   - 只在 UpdatePool 里按 ID 定位原记录（保留 ID/Aliases/Slot/签到历史）；
//   - Cookie 快照以**采集时刻**为准；旧 SavedAt 更新（并发登录/续期）则保留旧值；
//   - Phase=error 一律不写盘（契约要求"不得改写号池"）。
func codeartsPersistCapture(id string, res *codearts.CaptureResult, now time.Time) error {
	if res == nil || id == "" {
		return nil
	}
	switch res.Phase {
	case "probed", "session_only", "login_dead", "no_cookie":
	default:
		return nil // error：判不准，不改写号池
	}
	at := now.Format(time.RFC3339)
	return codearts.UpdatePool(func(pool *codearts.Pool) error {
		acc := codeartsFindAccount(pool, id)
		if acc == nil {
			return fmt.Errorf("账号已不在号池: %s", id)
		}
		if res.Phase == "probed" || res.Phase == "session_only" {
			// 快照覆盖要判"新不新"；票据/探测状态由下面的统一入口写。
			if codeartsShouldTakeCaptureCookies(acc, res.Cookies, now) {
				acc.Cookies = append([]codearts.Cookie(nil), res.Cookies...)
				acc.SavedAt = at
			}
		}
		codeartsApplyCaptureMeta(acc, res, at)
		return nil
	})
}

// codeartsApplyCaptureMeta 把一次归一结论（登录或补采）写进账号记录。
//
// 登录保存与补采**共用**这一个函数：两条路径各写一份是缺陷一的直接成因——
// 补采能判出"全是会话级"，登录保存却只会把 TicketDeadline 算成空、
// TicketSource 留空。
func codeartsApplyCaptureMeta(acc *codearts.Account, res *codearts.CaptureResult, at string) {
	if acc == nil || res == nil {
		return
	}
	acc.ProbeAt = at
	acc.ProbeMsg = res.Message
	switch res.Phase {
	case "probed", "session_only":
		acc.ProbeState = codeartsProbeStateOf(res.Phase)
		acc.TicketDeadline = res.TicketDeadline
		acc.TicketSource = res.TicketSource
	case "login_dead":
		acc.ProbeState = "login_dead"
		// 会话已死就不该继续挂着旧的绝对票据时间（面板会显示"还没到期"，
		// 与实际相反）。RefreshTicketDeadline 对"未命中"的规则也是清空。
		acc.TicketDeadline = ""
		acc.TicketSource = ""
	case "no_cookie":
		acc.ProbeState = "failed"
	}
}

// codeartsShouldTakeCaptureCookies 补采快照能不能覆盖号池里的旧快照。
//
// 契约澄清（C-CA-CAPTURE@1 @1 向后兼容更新）：**以采集时刻为准**。
//   - 旧 SavedAt 比本次采集更新（并发登录/续期刚写过）→ 保留旧 Cookie，
//     即使这次的条数更多也不覆盖；
//   - 旧 SavedAt 可解析且不晚于本次采集 → 用本次观测覆盖；
//   - 旧 SavedAt 缺失/解析失败 → 无从比较，以本次采集为准。
func codeartsShouldTakeCaptureCookies(acc *codearts.Account, fresh []codearts.Cookie, now time.Time) bool {
	if acc == nil || len(fresh) == 0 {
		return false
	}
	old, err := time.Parse(time.RFC3339, acc.SavedAt)
	if err != nil {
		return true
	}
	return !old.After(now)
}

// codeartsFindAccount 按 ID 在号池里找账号（找不到返回 nil）。
func codeartsFindAccount(pool *codearts.Pool, id string) *codearts.Account {
	if pool == nil {
		return nil
	}
	for _, a := range pool.Accounts {
		if a != nil && a.ID == id {
			return a
		}
	}
	return nil
}

// ── 节流 ────────────────────────────────────────────────────────────────

// codeartsProbeThrottled 该账号现在是否还在补采节流窗口内。
//
// 返回 (是否节流, 下次可探测时刻)。ProbeAt 缺失/解析失败 → 不节流（宁可多采
// 一次，也不能因为一个坏字段永远不再采集）。
func codeartsProbeThrottled(a *codearts.Account, now time.Time) (bool, time.Time) {
	if a == nil || strings.TrimSpace(a.ProbeAt) == "" {
		return false, time.Time{}
	}
	at, err := time.Parse(time.RFC3339, a.ProbeAt)
	if err != nil {
		return false, time.Time{}
	}
	next := at.Add(codeartsProbeGapFor(a))
	if now.Before(next) {
		return true, next
	}
	return false, next
}

// codeartsProbeGapFor 该账号下一次探测要等多久：成功 6 小时、失败 2 小时。
func codeartsProbeGapFor(a *codearts.Account) time.Duration {
	if a != nil && (a.ProbeState == "login_dead" || a.ProbeState == "failed") {
		return codeartsProbeFailGap
	}
	return codeartsProbeGap
}

// codeartsNextProbeAt 下一次可探测时刻（ProbeAt 无效时从 now 起算）。
func codeartsNextProbeAt(a *codearts.Account, now time.Time) time.Time {
	if a == nil {
		return time.Time{}
	}
	if at, err := time.Parse(time.RFC3339, a.ProbeAt); err == nil {
		return at.Add(codeartsProbeGapFor(a))
	}
	return now.Add(codeartsProbeGapFor(a))
}
