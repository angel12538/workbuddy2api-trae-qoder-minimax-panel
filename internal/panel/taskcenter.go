// taskcenter.go 面板「任务中心」：全账号任务扫描 + 执行队列（可配并发）+
// 成长任务队列 + 任务列表诊断。
//
// 语义（C-WB-SCAN@1 / C-WB-QUEUE@1 / C-WB-DIAG@1）：
//   - 扫描（scan_all）：并发拉取每账号的成长任务列表（默认 + 小程序口径，
//     按 task_code 去重），分三桶（可自动化 / 上游锁定 / 需手动）如实展示，
//     并显式输出 scan_ok/scan_state/errors —— 上游 401/空 body/形态变化
//     绝不能再被渲染成「没有待办任务」。
//   - 执行队列（run_queue + queue）：与扫描同一分类函数生成待办，账号内
//     串行、账号间并发；每项动作后**强制回读**，经 classifyAutoOutcome 判定，
//     未达标不得写 done（verified 字段可核对）。global 账号同样入队执行，
//     企业版/禁用账号跳过并写入 skipped_accounts。
//   - 诊断（diag）：按账号输出 http/body_bytes/top_keys/task_count/error，
//     区分「账号为空 / 401 / 空 body / 形态变化 / mp 失败」。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 契约常量（C-WB-SCAN@1 / C-WB-QUEUE@1 冻结词表）
// ---------------------------------------------------------------------------

// 扫描状态（账号级）。C-WB-SCAN@1 契约枚举，不得新增/改名。
const (
	scanStateOK                = "ok"
	scanStateEmpty             = "empty"
	scanStateError             = "error"
	scanStateSkippedEnterprise = "skipped_enterprise"
	scanStateSkippedDisabled   = "skipped_disabled"
)

// 扫描总状态（响应级）：在账号级四态之外增加 partial（部分账号成功、部分失败）。
const scanStatePartial = "partial"

// 任务动作（scanTask.Action / queueItem.action）。
const (
	scanActionAuto       = "auto"        // 有自动动作，可直接执行
	scanActionClaimOnly  = "claim_only"  // 已达标，无需动作，直接领奖
	scanActionLockedAuto = "locked_auto" // 有自动动作但上游锁定
	scanActionManual     = "manual"      // 面板无自动动作，需真实操作
)

// errors[].scope：标识错误来源口径（R3.2.1「账号、口径、真实错误」）。
// 账号是 cn 还是 global 由 accounts[].global / diag.realm 给出。
const (
	scanScopeGrowth = "growth" // 默认口径（cn/global）
	scanScopeMP     = "mp"     // 小程序口径
	scanScopeAuth   = "auth"   // 账号凭证缺失
	scanScopePool   = "pool"   // 号池为空
)

// 队列条目状态：pending/running 为运行期状态，其余复用 C-WB-AUTO@1 词表。
const (
	queueStatusPending = "pending"
	queueStatusRunning = "running"
)

// 队列条目来源（附加字段，便于前端区分 global / 小程序条目）。
const (
	queueSourceCN     = "cn"
	queueSourceGlobal = "global"
	queueSourceMP     = "mp"
)

// ---------------------------------------------------------------------------
// 扫描（只读）
// ---------------------------------------------------------------------------

// scanTask 单条待办任务：上游任务快照 + 面板判定的动作类型。
type scanTask struct {
	upstream.Task
	Action string `json:"action"` // auto|claim_only|locked_auto|manual
}

// scanAccountItem 单账号扫描结果（C-WB-SCAN@1）。
//
// 三个桶合起来 = 该账号**全部未完成任务**，保证扫描不漏项：
//   - Growth：未完成且可自动化（auto）或已达标待领奖（claim_only）→ 进待办队列；
//   - GrowthLocked：有自动动作但上游还没解锁 → 不进待办，但显式列出；
//   - Manual：未完成、但面板没有自动动作 → 显式列出并说明原因。
//
// 计数不变量：IncompleteTotal == len(Growth)+len(GrowthLocked)+len(Manual)。
type scanAccountItem struct {
	UID             string     `json:"uid"`
	Nickname        string     `json:"nickname"`
	Global          bool       `json:"global,omitempty"`
	Enterprise      bool       `json:"enterprise,omitempty"`
	Disabled        bool       `json:"disabled,omitempty"`
	Growth          []scanTask `json:"growth,omitempty"`
	GrowthLocked    []scanTask `json:"growth_locked,omitempty"`
	Manual          []scanTask `json:"manual,omitempty"`
	GrowthErr       string     `json:"growth_error,omitempty"`
	MPErr           string     `json:"mp_error,omitempty"`
	AuthErr         string     `json:"auth_error,omitempty"`
	UpstreamTotal   int        `json:"upstream_total"`
	IncompleteTotal int        `json:"incomplete_total"`
	ScanState       string     `json:"scan_state"`
}

// scanError errors[] 条目（字段名与 C-WB-SCAN@1 / C-WB-QUEUE@1 一致）。
type scanError struct {
	UID     string `json:"uid"`
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

// growthPending 任务是否"未完成且可自动化"（含已达标待领奖）。
//
// 保留该分类函数供既有测试与其它调用方复用；scanActionFor 与本函数口径一致。
func growthPending(t upstream.Task) bool {
	if t.Claimed {
		return false
	}
	// 上游锁定的任务不出待办：Sequential 族每日零点解锁一环，刚做完上一环时
	// 下一环以下发但 locked 形态出现在列表里——扫进队列只会 accept 不落账报
	// 失败（每日锁定窗口），零点解锁后自然回到待办。其余 locked（上游未开放）
	// 同语义：不该被自动化尝试。
	if t.Locked {
		return false
	}
	// 已达标（可领奖）：**无论有没有自动动作**都入队。有动作的跑完自动领奖；
	// 没动作的（关注公众号 / 真实捐款 / 客户端行为）走「只领奖」路径——用户
	// 做完真实操作后，面板到点自动把奖励领回来，不必手动点。
	if t.Target > 0 && t.Current >= t.Target {
		return true
	}
	return autoActionFor(t.TaskCode) != nil
}

// growthLocked 任务是否「有自动动作、但上游未解锁」（扫描结果里单独展示）。
func growthLocked(t upstream.Task) bool {
	return t.Locked && !t.Claimed && autoActionFor(t.TaskCode) != nil
}

// growthManual 任务是否「未完成、但面板没有自动动作」——需真实世界行为或
// 桌面/小程序客户端才能点亮。显式列出，保证扫描覆盖全部未完成任务。
//
// 注意**不排除 Locked**：锁定且没有动作的任务永远不会自动执行，标成
// 「未解锁·到点自动执行」是误导；它的真实状态就是「面板帮不上忙」，应落 Manual 桶。
func growthManual(t upstream.Task) bool {
	return !t.Claimed && autoActionFor(t.TaskCode) == nil
}

// scanActionFor 按 C-WB-SCAN@1 2.2.13 规则 2 判定任务动作。
//
// 顺序与契约一致：已达标（!Locked && Target>0 && Current>=Target）→ claim_only；
// 有自动动作（未锁定）→ auto；有自动动作（锁定）→ locked_auto；其余 → manual。
func scanActionFor(t upstream.Task) string {
	if t.Locked {
		if autoActionFor(t.TaskCode) != nil {
			return scanActionLockedAuto
		}
		return scanActionManual
	}
	if t.Target > 0 && t.Current >= t.Target {
		return scanActionClaimOnly
	}
	if autoActionFor(t.TaskCode) != nil {
		return scanActionAuto
	}
	return scanActionManual
}

// scanAccountTasks 拉取并分类单个账号的任务（C-WB-SCAN@1 单账号口径）。
//
// 门控顺序：禁用 → skipped_disabled；企业版 → skipped_enterprise（不发起任何
// 上游请求）；其余拉取默认口径与小程序口径，任一失败即 error。返回的第二项
// 是该账号产生的 errors[] 条目（scope=growth/mp/auth）。
//
// 关键缺陷修复：
//   - 错误不再静默：ListTasksDetailed/ListTasksMPDetailed 的错误全部写入
//     GrowthErr/MPErr 并计入 errors[]；
//   - mp 合并失败不再等价于「没有 mp 任务」：MPErr 与 errors[] 同时可见。
func (p *Panel) scanAccountTasks(st pool.Status) (scanAccountItem, []scanError) {
	it := scanAccountItem{
		UID:        st.UID,
		Nickname:   st.Nickname,
		Disabled:   st.Disabled,
		Enterprise: st.Enterprise,
	}
	a := p.cfg.Pool.AuthByUID(st.UID)
	if a != nil {
		it.Nickname = a.Nickname
		it.Global = a.IsGlobal()
		it.Enterprise = a.IsEnterprise()
	}

	// 规则 5：禁用账号直接跳过（不发起上游请求）。
	if st.Disabled {
		it.ScanState = scanStateSkippedDisabled
		return it, nil
	}
	// 规则 6：号池里有条目但拿不到凭证 → auth 失败。
	if a == nil {
		it.ScanState = scanStateError
		it.AuthErr = "账号凭证缺失（AuthByUID 未命中）"
		return it, []scanError{{UID: st.UID, Scope: scanScopeAuth, Message: it.AuthErr}}
	}
	// 规则 4：企业版无个人成长体系，不调用上游，也不计失败。
	if it.Enterprise {
		it.ScanState = scanStateSkippedEnterprise
		return it, nil
	}

	var errs []scanError
	var defTasks, mpTasks []upstream.Task

	dl, derr := p.cfg.Upstream.ListTasksDetailed(a)
	if derr != nil {
		it.GrowthErr = derr.Error()
		errs = append(errs, scanError{UID: st.UID, Scope: scanScopeGrowth, Message: derr.Error()})
	} else if dl != nil {
		defTasks = dl.Items
	}

	// 国际版两个口径返回同一份清单（已实测），跳过 mp 调用避免无谓请求；
	// 因此 global 账号没有 mp 错误（其待办全部由默认口径覆盖）。
	if !it.Global {
		ml, merr := p.cfg.Upstream.ListTasksMPDetailed(a)
		if merr != nil {
			it.MPErr = merr.Error()
			errs = append(errs, scanError{UID: st.UID, Scope: scanScopeMP, Message: merr.Error()})
		} else if ml != nil {
			mpTasks = ml.Items
		}
	}

	// 规则 1：按 task_code 合并去重（mp 专属码以 mp 条目为准）。
	merged := mergeTaskSnapshot(defTasks, mpTasks)
	it.UpstreamTotal = len(merged)

	for _, t := range merged {
		if t.Claimed {
			continue // 已领取不是待办
		}
		action := scanActionFor(t)
		entry := scanTask{Task: t, Action: action}
		switch action {
		case scanActionAuto, scanActionClaimOnly:
			it.Growth = append(it.Growth, entry)
		case scanActionLockedAuto:
			it.GrowthLocked = append(it.GrowthLocked, entry)
		default:
			it.Manual = append(it.Manual, entry)
		}
	}
	// 规则 3：三桶不变量。
	it.IncompleteTotal = len(it.Growth) + len(it.GrowthLocked) + len(it.Manual)
	switch {
	case len(errs) > 0:
		it.ScanState = scanStateError
	case it.IncompleteTotal > 0:
		it.ScanState = scanStateOK
	default:
		it.ScanState = scanStateEmpty
	}
	return it, errs
}

// scanOverallState 汇总响应级 scan_state：ok|empty|error|partial。
//
//   - 号池为空，或所有可扫描账号都失败 → error；
//   - 部分账号失败 → partial；
//   - 有成功扫描但没有未完成任务 → empty；
//   - 其余 → ok。
func scanOverallState(accountCount, scannedCount, failedCount, incompleteTotal int) string {
	switch {
	case accountCount == 0:
		return scanStateError
	case failedCount > 0 && scannedCount == 0:
		return scanStateError
	case failedCount > 0:
		return scanStatePartial
	case scannedCount == 0:
		return scanStateEmpty
	case incompleteTotal == 0:
		return scanStateEmpty
	default:
		return scanStateOK
	}
}

// tasksScanAll 扫描全部账号（C-WB-SCAN@1）。
//
// HTTP 恒 200 + ok=true（便于前端渲染诊断）；失败通过 scan_ok=false、
// scan_state 与 errors[] 表达。号池为空返回 scan_ok=false 与 pool 错误，
// 绝不返回「没有待办」。
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	items := make([]scanAccountItem, len(states))
	errGroups := make([][]scanError, len(states))
	var wg sync.WaitGroup
	for i := range states {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			items[i], errGroups[i] = p.scanAccountTasks(states[i])
		}(i)
	}
	wg.Wait()

	var errs []scanError
	for _, g := range errGroups {
		errs = append(errs, g...)
	}
	if len(states) == 0 {
		// 规则 7：号池为空 → scan_ok=false + 明确错误。
		errs = []scanError{{UID: "", Scope: scanScopePool, Message: "号池为空"}}
	}

	pending, locked, manual := 0, 0, 0
	upstreamTotal, incompleteTotal := 0, 0
	scanned, failed := 0, 0
	for _, it := range items {
		pending += len(it.Growth)
		locked += len(it.GrowthLocked)
		manual += len(it.Manual)
		upstreamTotal += it.UpstreamTotal
		incompleteTotal += it.IncompleteTotal
		switch it.ScanState {
		case scanStateOK, scanStateEmpty:
			scanned++
		case scanStateError:
			failed++
		}
	}
	if incompleteTotal != pending+locked+manual {
		// 防御性自检：构造上恒等，出现偏差说明分类漏项，必须留痕。
		log.Printf("WARN: panel: 扫描三桶不变量异常 incomplete=%d pending=%d locked=%d manual=%d",
			incompleteTotal, pending, locked, manual)
	}
	scanOK := len(states) > 0 && failed == 0
	overall := scanOverallState(len(states), scanned, failed, incompleteTotal)

	log.Printf("panel: 队列扫描完成：账号 %d（成功 %d / 失败 %d），待办 %d / 未解锁 %d / 需手动 %d，scan_ok=%v state=%s",
		len(states), scanned, failed, pending, locked, manual, scanOK, overall)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"accounts":         items,
		"pending_count":    pending,
		"locked_count":     locked,
		"manual_count":     manual,
		"account_count":    len(states),
		"scanned_count":    scanned,
		"failed_count":     failed,
		"scan_ok":          scanOK,
		"scan_state":       overall,
		"upstream_total":   upstreamTotal,
		"incomplete_total": incompleteTotal,
		"errors":           nonNilScanErrors(errs),
	})
}

// ---------------------------------------------------------------------------
// 执行队列
// ---------------------------------------------------------------------------

// queueItem 队列执行单元（C-WB-QUEUE@1）。
//
// 关键缺陷修复：新增 Verified/Claimed/ProgressBefore/ProgressAfter —— 此前
// 队列只凭「动作无错误」写 done，产生与一键完成同款的幻觉完成。
type queueItem struct {
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	Kind           string `json:"kind"`             // growth
	Code           string `json:"code"`             // task_code
	Source         string `json:"source,omitempty"` // cn|global|mp（附加字段，供前端区分）
	Status         string `json:"status"`           // 见 C-WB-QUEUE@1 词表
	Action         string `json:"action,omitempty"` // auto|claim_only
	Message        string `json:"message,omitempty"`
	ProgressBefore string `json:"progress_before,omitempty"`
	ProgressAfter  string `json:"progress_after,omitempty"`
	Verified       bool   `json:"verified"` // done 必须 true
	Claimed        bool   `json:"claimed"`
}

// queueState 队列运行状态。Seq 每次启动 +1——前端只渲染"自己启动的那一轮"，
// 执行结束后的残留 items 不会覆盖后续的扫描结果视图。
type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	items     []queueItem
	conc      int
	seq       int
}

// Panel 队列字段在 Panel 结构体上（panel.go）由 initQueue 惰性初始化；
// 这里集中访问器，避免改动 New 构造链。
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// queueAccount 队列执行的账号单元。
type queueAccount struct {
	a    *auth.Auth
	grow []upstream.Task
}

// growthQueuePlan 一次队列扫描的结果（账号分组 + 待执行条目 + 错误 + 跳过账号）。
type growthQueuePlan struct {
	accounts []queueAccount
	items    []queueItem
	errors   []scanError
	skipped  []string
}

// buildGrowthQueue 扫描全部账号并生成执行队列（与 tasksScanAll 共用
// scanAccountTasks，保证扫描与队列口径一致）。
//
// 口径：
//   - disabled → 计入 skipped；
//   - enterprise → 计入 skipped（不发起上游请求）；
//   - growth/ mp / auth 失败 → 计入 errors（账号不入队，避免按不可信清单执行）；
//   - 其余账号的 Growth 桶（auto + claim_only）全部入队 —— **包含 global**。
func (p *Panel) buildGrowthQueue(growth bool) growthQueuePlan {
	states := p.cfg.Pool.List()
	plan := growthQueuePlan{}
	if len(states) == 0 {
		plan.errors = []scanError{{UID: "", Scope: scanScopePool, Message: "号池为空"}}
		return plan
	}

	type row struct {
		acct *queueAccount
		errs []scanError
		skip bool
	}
	rows := make([]row, len(states))
	var wg sync.WaitGroup
	for i := range states {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			it, errs := p.scanAccountTasks(states[i])
			rows[i].errs = errs
			switch it.ScanState {
			case scanStateSkippedDisabled, scanStateSkippedEnterprise:
				rows[i].skip = true
				return
			case scanStateError:
				// 清单不可信：不入队，错误已在 errors[] 中如实上报。
				return
			}
			if !growth || len(it.Growth) == 0 {
				return
			}
			a := p.cfg.Pool.AuthByUID(states[i].UID)
			if a == nil {
				return
			}
			// 按 autoActions 表序执行（依赖先行）；claim_only 无动作，排在表后。
			sort.SliceStable(it.Growth, func(x, y int) bool {
				return autoActionIndex(it.Growth[x].TaskCode) < autoActionIndex(it.Growth[y].TaskCode)
			})
			acc := &queueAccount{a: a, grow: make([]upstream.Task, 0, len(it.Growth))}
			for _, st := range it.Growth {
				acc.grow = append(acc.grow, st.Task)
			}
			rows[i].acct = acc
		}(i)
	}
	wg.Wait()

	for i := range rows {
		plan.errors = append(plan.errors, rows[i].errs...)
		if rows[i].skip {
			plan.skipped = append(plan.skipped, states[i].UID)
		}
		if rows[i].acct != nil {
			plan.accounts = append(plan.accounts, *rows[i].acct)
		}
	}
	for _, acc := range plan.accounts {
		global := acc.a.IsGlobal()
		for _, t := range acc.grow {
			plan.items = append(plan.items, queueItem{
				UID:      acc.a.UID,
				Nickname: acc.a.Nickname,
				Kind:     "growth",
				Code:     t.TaskCode,
				Source:   queueTaskSource(global, t.TaskCode),
				Status:   queueStatusPending,
				Action:   scanActionFor(t),
			})
		}
	}
	return plan
}

// queueTaskSource 队列条目来源：global 账号 → global；mp 专属码 → mp；其余 cn。
func queueTaskSource(global bool, code string) string {
	if global {
		return queueSourceGlobal
	}
	if isMPTaskCode(code) {
		return queueSourceMP
	}
	return queueSourceCN
}

// growthQueueStart startGrowthQueue 的返回值（聚合 run_queue 响应所需字段）。
type growthQueueStart struct {
	started bool
	total   int
	seq     int
	msg     string
	errors  []scanError
	skipped []string
}

// tasksRunQueue 启动执行队列：{concurrency:1-4, growth:bool}。
//
// 响应（C-WB-QUEUE@1）：started=true 时带 total/seq/errors/skipped_accounts；
// started=false 时 message 必须包含失败原因，且 errors 非空，不得只回
// 「全部账号没有待办任务」。
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Growth {
		body.Growth = true
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}

	res := p.startGrowthQueue(body.Concurrency, body.Growth)
	switch {
	case res.seq == -1:
		writeErr(w, http.StatusConflict, res.msg)
	case !res.started:
		payload := map[string]any{
			"ok":      true,
			"started": false,
			"message": res.msg,
			"errors":  nonNilScanErrors(res.errors),
		}
		if len(res.skipped) > 0 {
			payload["skipped_accounts"] = res.skipped
		}
		writeJSON(w, http.StatusOK, payload)
	default:
		payload := map[string]any{
			"ok":      true,
			"started": true,
			"total":   res.total,
			"seq":     res.seq,
			"errors":  nonNilScanErrors(res.errors),
		}
		if len(res.skipped) > 0 {
			payload["skipped_accounts"] = res.skipped
		}
		writeJSON(w, http.StatusOK, payload)
	}
}

// startGrowthQueue 扫描全部账号待办并启动队列（HTTP「执行全部待办」与调度器
// growth 时点共用核心）。seq==-1 表示队列已在执行（冲突）；started=false 时
// msg 为未启动原因（失败账号 / 号池为空 / 确实没有待办）。
func (p *Panel) startGrowthQueue(concurrency int, growth bool) growthQueueStart {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}
	q := p.queue()
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return growthQueueStart{seq: -1, msg: "队列正在执行中（可在任务中心查看进度）"}
	}
	// 先占位：扫描（数秒级网络耗时）期间若并发再次触发，直接命中上面的 running
	// 判拒，避免两个 goroutine 同时启动互相覆盖 q.items/q.seq。无待办时回滚。
	q.running = true
	q.startedAt = time.Now()
	q.mu.Unlock()

	plan := p.buildGrowthQueue(growth)

	if len(plan.items) == 0 {
		q.mu.Lock()
		q.running = false
		q.startedAt = time.Time{}
		q.mu.Unlock()

		res := growthQueueStart{
			errors:  nonNilScanErrors(plan.errors),
			skipped: append([]string(nil), plan.skipped...),
		}
		if len(plan.errors) > 0 {
			res.msg = queueAbortMessage(plan.errors)
		} else {
			res.msg = "全部账号没有待办任务"
		}
		log.Printf("panel: 队列启动：未启动（%s）", res.msg)
		return res
	}

	q.mu.Lock()
	q.items = plan.items
	q.conc = concurrency
	q.seq++
	seq := q.seq
	q.mu.Unlock()

	go p.runQueueItems(plan.accounts, plan.items, concurrency)
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v，跳过 %d 个账号，错误 %d 条）",
		len(plan.items), concurrency, growth, len(plan.skipped), len(plan.errors))
	return growthQueueStart{
		started: true,
		total:   len(plan.items),
		seq:     seq,
		errors:  nonNilScanErrors(plan.errors),
		skipped: append([]string(nil), plan.skipped...),
	}
}

// queueAbortMessage 队列未启动时的说明：必须带真实失败原因（R3.5.3）。
func queueAbortMessage(errs []scanError) string {
	first := errs[0]
	who := first.UID
	if who == "" {
		who = "号池"
	}
	return fmt.Sprintf("队列未启动：%d 个账号扫描失败（首个：%s/%s %s）",
		len(errs), who, first.Scope, first.Message)
}

// RunGrowthQueueOnce 调度器 growth 时点回调（sch.SetGrowthHook 挂载）：与
// 「执行全部待办」按钮完全同管线（成长，串行并发 1）。Sequential 族
// 每日零点解锁一环，此前只能手动扫描推进；此回调让链条每天自动走一环。
// 异步执行（startGrowthQueue 启动 goroutine 即返），已在跑/无待办安全跳过。
func (p *Panel) RunGrowthQueueOnce() {
	res := p.startGrowthQueue(1, true)
	if res.started {
		log.Printf("panel: 定时成长任务队列已启动（%d 项）", res.total)
	}
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int) {
	p.runQueueItemsWithDeps(accts, items, concurrency, p.defaultQueueDeps())
}

// runQueueItemsWithDeps 可注入依赖的队列执行主体（单测用假任务表/假动作，
// 不真打上游）。
func (p *Panel) runQueueItemsWithDeps(accts []queueAccount, items []queueItem, concurrency int, deps *queueDeps) {
	q := p.queue()
	defer func() {
		q.mu.Lock()
		q.running = false
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, one := range accts {
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = AutoStatusSkipped, "该账号有其它任务动作在执行，跳过"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)

			// 前置：批量接受尚未接受的任务（行为事件才是进度判据，失败不阻塞）。
			// 国际版 checklist 没有 accept 这一步（提交会逐条 task not found），
			// 故 global 账号不做批量接受。
			if !one.a.IsGlobal() {
				if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
					time.Sleep(reportGap)
				}
			}

			for i := range items {
				it := q.itemAt(i)
				if it.UID != one.a.UID {
					continue
				}
				it.Status = queueStatusRunning
				p.queueReplaceAt(i, it)

				updated := p.runQueueItem(one.a, it, deps)
				p.queueReplaceAt(i, updated)

				if deps.gap > 0 {
					time.Sleep(deps.gap) // 项间节流
				}
			}
		}(one)
	}
	wg.Wait()
}

// queueDeps 队列执行的外部依赖（读取/领奖/动作表）。
//
// 抽出来让「动作后强制回读 + classifyAutoOutcome 判定」能在不真实打上游
// 的前提下做单元测试；生产路径由 defaultQueueDeps 提供真实实现。
type queueDeps struct {
	readBefore func(a *auth.Auth, code, source string) (*upstream.Task, error)
	readAfter  func(a *auth.Auth, code, source string) (*upstream.Task, error)
	claim      func(a *auth.Auth, isMP bool, code string) (int64, int64, error)
	actionFor  func(code string) *autoAction
	gap        time.Duration // 项间节流（测试置 0）
}

// defaultQueueDeps 生产实现：真实上游读取 / 领奖 / 动作表。
func (p *Panel) defaultQueueDeps() *queueDeps {
	return &queueDeps{
		readBefore: func(a *auth.Auth, code, source string) (*upstream.Task, error) {
			if source == queueSourceMP || isMPTaskCode(code) {
				return p.taskByCodeMP(a, code)
			}
			t, err := p.taskByCode(a, code)
			if err == nil && t == nil && !a.IsGlobal() {
				// 默认口径没有：可能是尚未登记的新 mp 专属码，回落 mp 口径。
				if mt, merr := p.taskByCodeMP(a, code); merr == nil && mt != nil {
					return mt, nil
				}
			}
			return t, err
		},
		readAfter: func(a *auth.Auth, code, source string) (*upstream.Task, error) {
			// mp 专属码只在 mp 口径下发；其余走带轮询等待的默认口径
			// （上游计分是异步的，一次性回读会把刚点亮的任务误判为未达标）。
			if source == queueSourceMP || isMPTaskCode(code) {
				return p.taskByCodeMP(a, code)
			}
			return p.taskByCodeWaiting(a, code)
		},
		claim:     p.claimFor,
		actionFor: autoActionFor,
		gap:       reportGap,
	}
}

// runQueueItem 执行单个队列条目并返回更新后的条目。
//
// 判定（C-WB-QUEUE@1 + C-WB-AUTO@1）：
//   - claim_only：已达标，直接领奖；领奖成功 = done + verified；
//   - auto：动作 → **强制回读** → classifyAutoOutcome；未达标不得 done；
//   - 回读失败/任务消失 → unverified；动作报错 → failed。
func (p *Panel) runQueueItem(a *auth.Auth, it queueItem, deps *queueDeps) queueItem {
	if deps == nil {
		deps = p.defaultQueueDeps()
	}
	if it.Action == "" {
		it.Action = scanActionAuto
	}
	code := it.Code
	isMP := it.Source == queueSourceMP || isMPTaskCode(code)
	it.Verified = false
	it.Claimed = false
	it.ProgressBefore = "?"
	it.ProgressAfter = "?"

	before, berr := deps.readBefore(a, code, it.Source)
	if berr != nil {
		it.Status = AutoStatusFailed
		it.Message = "读取任务状态失败: " + berr.Error()
		return it
	}
	it.ProgressBefore = taskProgressText(before)
	it.ProgressAfter = it.ProgressBefore
	switch {
	case before == nil:
		it.Status = AutoStatusSkipped
		it.Message = "该账号无此任务"
		return it
	case before.Claimed:
		it.Status = AutoStatusSkipped
		it.Claimed = true
		it.Message = "已完成（已领取）"
		return it
	}

	// 非执行类条目（锁定/需手动）即使混进队列也不跑动作，按真实状态回报。
	switch it.Action {
	case scanActionLockedAuto:
		it.Status = AutoStatusLocked
		it.Message = "上游未解锁，未执行"
		return it
	case scanActionManual:
		it.Status = AutoStatusManual
		it.Message = "面板无自动动作，需在官方客户端完成真实操作"
		return it
	}

	if it.Action == scanActionClaimOnly {
		if !before.Claimable {
			// 扫描时已达标、执行时状态已变化：不能凭「曾经达标」写 done。
			it.Status = AutoStatusUnverified
			it.Message = "扫描时已达标，执行时状态已变化（未证实完成）"
			return it
		}
		credit, energy, cerr := deps.claim(a, isMP, code)
		if cerr != nil {
			it.Status = AutoStatusFailed
			it.Message = "已达标但领奖失败: " + cerr.Error()
			return it
		}
		it.Status = AutoStatusDone
		it.Verified = true
		it.Claimed = true
		it.Message = fmt.Sprintf("已达标，自动领奖 +%d 分 +%d 能", credit, energy)
		log.Printf("panel: 队列执行 uid=%s code=%s action=claim_only status=done verified=true", a.UID, code)
		return it
	}

	act := deps.actionFor(code)
	if act == nil {
		it.Status = AutoStatusManual
		it.Message = "面板无自动动作，需在官方客户端完成真实操作"
		return it
	}

	msg, runErr := act.run(p, a)
	after, readErr := deps.readAfter(a, code, it.Source)
	status, verified := classifyAutoOutcome(before, after, runErr)
	it.Status, it.Verified = status, verified
	it.ProgressAfter = taskProgressText(after)
	if after != nil && after.Claimed {
		it.Claimed = true
	}

	switch status {
	case AutoStatusFailed:
		it.Message = runErr.Error()
	case AutoStatusDone:
		it.Message = msg
	case AutoStatusPartial:
		it.Message = msg + "；进度 " + it.ProgressBefore + " → " + it.ProgressAfter + "（未达标）"
	default: // unverified
		if after == nil {
			it.Message = msg + "；已执行动作但回读失败，无法确认完成"
			if readErr != nil {
				it.Message += "（" + readErr.Error() + "）"
			}
		} else {
			it.Message = msg + "；动作已执行但进度未变化（" + it.ProgressAfter + "）"
		}
	}

	// 达标即自动领奖（mp 任务走 mp 口径）；领奖失败不掩盖「已完成」，
	// 但必须留痕（message 追加失败原因）。
	if status == AutoStatusDone && after != nil && after.Claimable && !after.Claimed {
		credit, energy, cerr := deps.claim(a, isMP, code)
		if cerr == nil {
			it.Claimed = true
			if credit > 0 || energy > 0 {
				it.Message += fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
			} else {
				it.Message += "；奖励此前已领取"
			}
		} else {
			it.Message += "；达标但领奖失败（可在列表手动重试）"
		}
	}

	log.Printf("panel: 队列执行 uid=%s code=%s action=%s status=%s verified=%v progress %s -> %s",
		a.UID, code, it.Action, it.Status, it.Verified, it.ProgressBefore, it.ProgressAfter)
	return it
}

// itemAt 锁内读取队列条目快照（避免锁外持有指针）。
func (q *queueState) itemAt(i int) queueItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i]
}

// queueReplaceAt 按索引整体替换队列条目（条目数组固定不再增删）。
func (p *Panel) queueReplaceAt(i int, it queueItem) {
	q := p.queue()
	q.mu.Lock()
	q.items[i] = it
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
func (p *Panel) queueSet(q *queueState, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// acceptPendingTasks 批量接受该账号未接受的任务，返回接受的个数（失败返回 0 不阻塞）。
func (p *Panel) acceptPendingTasks(a *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return 0
	}
	if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", a.UID, err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", a.UID, len(codes))
	return len(codes)
}

// claimFor 按口径领奖（mp 专属码走 mp 变体）。
func (p *Panel) claimFor(a *auth.Auth, isMP bool, code string) (int64, int64, error) {
	if isMP {
		return p.cfg.Upstream.ClaimRewardMP(a, code)
	}
	return p.cfg.Upstream.ClaimReward(a, code)
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	payload := map[string]any{
		"running":    q.running,
		"total":      len(items),
		"conc":       q.conc,
		"started":    !q.startedAt.IsZero(),
		"started_at": q.startedAt,
		"seq":        q.seq,
		"items":      items,
	}
	q.mu.Unlock()
	writeJSON(w, http.StatusOK, payload)
}

// ---------------------------------------------------------------------------
// 任务列表诊断（C-WB-DIAG@1）
// ---------------------------------------------------------------------------

// tasksDiag 按账号输出任务列表诊断（GET /panel/api/tasks/diag?uid=<uid>）。
//
// 只读：不 accept/claim、不写号池。growth 走默认口径，mp 走小程序口径；
// global 两个口径都探测（诊断目的就是看真实响应，不做业务侧跳过）。
func (p *Panel) tasksDiag(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "uid is required")
		return
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	payload := map[string]any{
		"ok":         true,
		"uid":        a.UID,
		"realm":      auth.ResolveRealm(a.RealmStored(), a.DomainValue()),
		"global":     a.IsGlobal(),
		"enterprise": a.IsEnterprise(),
		"growth":     p.cfg.Upstream.TasksListDiag(a, false),
		"mp":         p.cfg.Upstream.TasksListDiag(a, true),
	}
	if ts := tokenExpiresAtRFC3339(a); ts != "" {
		payload["token_expires_at"] = ts
	}
	writeJSON(w, http.StatusOK, payload)
}

// tokenExpiresAtRFC3339 账号 token 的绝对到期时刻（只读、加锁快照）。
// <=0（无绝对到期）返回空串——不臆造时间。
func tokenExpiresAtRFC3339(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	a.Lock()
	exp := a.ExpiresAt
	a.Unlock()
	if exp <= 0 {
		return ""
	}
	return time.Unix(exp, 0).Format(time.RFC3339)
}

// nonNilScanErrors 保证 JSON 输出是 [] 而不是 null（前端可安全遍历）。
func nonNilScanErrors(errs []scanError) []scanError {
	if errs == nil {
		return []scanError{}
	}
	return errs
}

// ---------------------------------------------------------------------------
// 开学季券码（与本任务无关的既有能力，保持原样）
// ---------------------------------------------------------------------------

// schoolVouchers 我的券码：逐 CN 账号查开学季 /vouchers（3 并发，与 packages
// 同款限流），失败只在对应账号标 error。global 账号无开学季，不发上游调用。
func (p *Panel) schoolVouchers(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Vouchers []upstream.SchoolVoucher `json:"vouchers"`
		Err      string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range accts {
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			it := row{UID: a.UID, Nickname: a.Nickname}
			switch {
			case a.IsGlobal():
				it.Err = "global realm（无开学季活动）"
			default:
				vs, err := p.cfg.Upstream.SchoolVouchers(a)
				if err != nil {
					it.Err = err.Error()
				} else {
					it.Vouchers = vs
				}
			}
			out[i] = it
		}(i, a)
	}
	wg.Wait()
	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}
