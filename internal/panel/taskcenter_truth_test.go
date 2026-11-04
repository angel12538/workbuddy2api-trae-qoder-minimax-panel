package panel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 夹具：假上游（httptest）
// ---------------------------------------------------------------------------

// taskCallLog 记录假上游收到的调用（key = uid|platform），用于断言门控生效。
type taskCallLog struct {
	mu    sync.Mutex
	calls map[string]int
}

func newTaskCallLog() *taskCallLog {
	return &taskCallLog{calls: map[string]int{}}
}

func (l *taskCallLog) add(key string) {
	l.mu.Lock()
	l.calls[key]++
	l.mu.Unlock()
}

func (l *taskCallLog) count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[key]
}

// newTaskTestPanel 构造只含内存号池 + 假上游 client 的面板。
func newTaskTestPanel(t *testing.T, srv *httptest.Server) *Panel {
	t.Helper()
	c := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	return &Panel{cfg: Config{Pool: pool.New(""), Upstream: c}}
}

// newTaskTestServer 启动假任务列表服务。respond 按 (uid, platform) 返回
// (状态码, body)；platform = cn（无 mp 头）/ mp（X-Client-Platform: miniprogram）。
func newTaskTestServer(t *testing.T, log *taskCallLog,
	respond func(uid, platform string) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/activity/growth/tasks" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		uid := r.Header.Get("X-User-Id")
		platform := queueSourceCN
		if r.Header.Get("X-Client-Platform") == "miniprogram" {
			platform = queueSourceMP
		}
		log.add(uid + "|" + platform)
		status, body := respond(uid, platform)
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// addTaskAccount 往号池加一个账号。global=true 用 workbuddy.ai 域判定；
// enterprise=true 带 EnterpriseID。
func addTaskAccount(p *Panel, uid, token string, global, enterprise bool) *auth.Auth {
	a := &auth.Auth{UID: uid, Nickname: uid, AccessToken: token}
	if global {
		a.Domain = "www.workbuddy.ai"
	}
	if enterprise {
		a.EnterpriseID = "ent-" + uid
	}
	p.cfg.Pool.Add(a)
	return a
}

// ---------------------------------------------------------------------------
// 响应结构
// ---------------------------------------------------------------------------

type scanResponse struct {
	OK              bool              `json:"ok"`
	Accounts        []scanAccountItem `json:"accounts"`
	PendingCount    int               `json:"pending_count"`
	LockedCount     int               `json:"locked_count"`
	ManualCount     int               `json:"manual_count"`
	AccountCount    int               `json:"account_count"`
	ScannedCount    int               `json:"scanned_count"`
	FailedCount     int               `json:"failed_count"`
	ScanOK          bool              `json:"scan_ok"`
	ScanState       string            `json:"scan_state"`
	UpstreamTotal   int               `json:"upstream_total"`
	IncompleteTotal int               `json:"incomplete_total"`
	Errors          []scanError       `json:"errors"`
}

type runQueueResponse struct {
	OK              bool        `json:"ok"`
	Started         bool        `json:"started"`
	Total           int         `json:"total"`
	Seq             int         `json:"seq"`
	Message         string      `json:"message"`
	Errors          []scanError `json:"errors"`
	SkippedAccounts []string    `json:"skipped_accounts"`
}

func doScan(t *testing.T, p *Panel) scanResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	p.tasksScanAll(rec, httptest.NewRequest(http.MethodPost, "/panel/api/tasks/scan_all", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scan status=%d want 200（契约要求扫描恒 200）", rec.Code)
	}
	var out scanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode scan: %v body=%s", err, rec.Body.String())
	}
	return out
}

func doRunQueue(t *testing.T, p *Panel) runQueueResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	p.tasksRunQueue(rec, httptest.NewRequest(http.MethodPost, "/panel/api/tasks/run_queue", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("run_queue status=%d want 200", rec.Code)
	}
	var out runQueueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode run_queue: %v body=%s", err, rec.Body.String())
	}
	return out
}

// ---------------------------------------------------------------------------
// 验收 ①：合法空数组 → scan_state=empty / scan_ok=true
// ---------------------------------------------------------------------------

func TestTaskCenterScanEmptyArrayIsEmpty(t *testing.T) {
	log := newTaskCallLog()
	srv := newTaskTestServer(t, log, func(uid, platform string) (int, string) {
		return http.StatusOK, `{"code":0,"msg":"OK","data":{"tasks":[]}}`
	})
	p := newTaskTestPanel(t, srv)
	addTaskAccount(p, "u-empty", "tok-empty", false, false)

	resp := doScan(t, p)
	if !resp.ScanOK {
		t.Errorf("scan_ok=false，合法空数组应视为成功扫描")
	}
	if resp.ScanState != scanStateEmpty {
		t.Errorf("scan_state=%q want empty", resp.ScanState)
	}
	if resp.AccountCount != 1 || resp.ScannedCount != 1 || resp.FailedCount != 0 {
		t.Errorf("account/scanned/failed = %d/%d/%d want 1/1/0",
			resp.AccountCount, resp.ScannedCount, resp.FailedCount)
	}
	if len(resp.Errors) != 0 {
		t.Errorf("errors=%v want empty", resp.Errors)
	}
	if len(resp.Accounts) != 1 {
		t.Fatalf("accounts=%d want 1", len(resp.Accounts))
	}
	acc := resp.Accounts[0]
	if acc.ScanState != scanStateEmpty {
		t.Errorf("账号 scan_state=%q want empty", acc.ScanState)
	}
	if acc.UpstreamTotal != 0 || acc.IncompleteTotal != 0 {
		t.Errorf("upstream/incomplete=%d/%d want 0/0", acc.UpstreamTotal, acc.IncompleteTotal)
	}
}

// ---------------------------------------------------------------------------
// 验收 ②：401 → scan_state=error / scan_ok=false / errors 有该账号
// ---------------------------------------------------------------------------

func TestTaskCenterScan401ReportsError(t *testing.T) {
	log := newTaskCallLog()
	srv := newTaskTestServer(t, log, func(uid, platform string) (int, string) {
		return http.StatusUnauthorized, `{"code":401,"msg":"invalid token"}`
	})
	p := newTaskTestPanel(t, srv)
	addTaskAccount(p, "u-401", "tok-401", false, false)

	resp := doScan(t, p)
	if resp.ScanOK {
		t.Error("401 时 scan_ok 必须为 false，不能显示成「没有待办」")
	}
	if resp.ScanState != scanStateError {
		t.Errorf("scan_state=%q want error", resp.ScanState)
	}
	if resp.FailedCount != 1 || resp.ScannedCount != 0 {
		t.Errorf("failed/scanned=%d/%d want 1/0", resp.FailedCount, resp.ScannedCount)
	}
	if len(resp.Accounts) != 1 || resp.Accounts[0].ScanState != scanStateError {
		t.Fatalf("账号状态=%v want error", resp.Accounts)
	}
	if !strings.Contains(resp.Accounts[0].GrowthErr, "401") {
		t.Errorf("growth_error=%q 应包含真实 HTTP 401", resp.Accounts[0].GrowthErr)
	}
	foundGrowth := false
	for _, e := range resp.Errors {
		if e.UID == "u-401" && e.Scope == scanScopeGrowth {
			foundGrowth = true
		}
	}
	if !foundGrowth {
		t.Errorf("errors=%v 必须包含 u-401/growth 的失败原因", resp.Errors)
	}
}

// ---------------------------------------------------------------------------
// 验收 ③⑤：三桶不变量 + 企业/禁用门控 + global 覆盖
// ---------------------------------------------------------------------------

func TestTaskCenterScanBucketsInvariant(t *testing.T) {
	const (
		bodyOKDefault = `{"code":0,"msg":"OK","data":{"tasks":[` +
			`{"task_code":"chat_5","target":5,"current":2},` +
			`{"task_code":"Expert_Philanthropy","target":1,"current":0,"task_desc":"真实捐款"},` +
			`{"task_code":"done_1","target":1,"current":1,"accept_status":"claimed"}]}}`
		bodyOKMP = `{"code":0,"msg":"OK","data":{"tasks":[` +
			`{"task_code":"chat_5","target":5,"current":2},` +
			`{"task_code":"school_season","target":1,"current":0}]}}`
		bodyLocked = `{"code":0,"msg":"OK","data":{"tasks":[` +
			`{"task_code":"Sequential_Tasks_4","locked":true,"target":1,"current":0},` +
			`{"task_code":"wb_wechat_oa_subscribe_task","locked":true,"target":1,"current":0}]}}`
		bodyGlobal = `{"code":0,"msg":"OK","data":{"tasks":[` +
			`{"code":"first_chat","title":"完成一次对话","level_name":"养虾尝试","status":"available"},` +
			`{"code":"skill_installed","title":"安装技能","level_name":"养虾尝试","status":"available"}]}}`
	)

	log := newTaskCallLog()
	srv := newTaskTestServer(t, log, func(uid, platform string) (int, string) {
		switch uid {
		case "a-ok":
			if platform == queueSourceMP {
				return http.StatusOK, bodyOKMP
			}
			return http.StatusOK, bodyOKDefault
		case "a-lock":
			return http.StatusOK, bodyLocked
		case "a-global":
			if platform == queueSourceMP {
				return http.StatusInternalServerError, `{"code":500,"msg":"should not be called"}`
			}
			return http.StatusOK, bodyGlobal
		case "a-401":
			return http.StatusUnauthorized, `{"code":401,"msg":"invalid token"}`
		default:
			return http.StatusInternalServerError, `{"code":500,"msg":"unexpected account"}`
		}
	})
	p := newTaskTestPanel(t, srv)
	addTaskAccount(p, "a-ok", "tok-ok", false, false)
	addTaskAccount(p, "a-lock", "tok-lock", false, false)
	addTaskAccount(p, "a-global", "tok-global", true, false)
	addTaskAccount(p, "a-ent", "tok-ent", false, true)
	addTaskAccount(p, "a-dis", "tok-dis", false, false)
	addTaskAccount(p, "a-401", "tok-401", false, false)
	p.cfg.Pool.Disable("a-dis", "test")

	resp := doScan(t, p)

	if resp.AccountCount != 6 {
		t.Errorf("account_count=%d want 6（含禁用）", resp.AccountCount)
	}
	if resp.ScannedCount != 3 {
		t.Errorf("scanned_count=%d want 3（a-ok/a-lock/a-global）", resp.ScannedCount)
	}
	if resp.FailedCount != 1 {
		t.Errorf("failed_count=%d want 1（a-401）", resp.FailedCount)
	}
	if resp.ScanOK {
		t.Error("存在失败账号时 scan_ok 必须为 false")
	}
	if resp.ScanState != scanStatePartial {
		t.Errorf("scan_state=%q want partial", resp.ScanState)
	}

	byUID := map[string]scanAccountItem{}
	for _, it := range resp.Accounts {
		byUID[it.UID] = it
	}
	// 三桶不变量：每个账号 incomplete == 三桶之和，且上游总数 >= 未完成数。
	for uid, it := range byUID {
		sum := len(it.Growth) + len(it.GrowthLocked) + len(it.Manual)
		if it.IncompleteTotal != sum {
			t.Errorf("%s: incomplete_total=%d != 三桶之和 %d", uid, it.IncompleteTotal, sum)
		}
		if it.UpstreamTotal < it.IncompleteTotal {
			t.Errorf("%s: upstream_total=%d < incomplete_total=%d", uid, it.UpstreamTotal, it.IncompleteTotal)
		}
	}

	ok := byUID["a-ok"]
	if ok.ScanState != scanStateOK {
		t.Errorf("a-ok state=%q want ok", ok.ScanState)
	}
	if ok.UpstreamTotal != 4 || ok.IncompleteTotal != 3 {
		t.Errorf("a-ok upstream/incomplete=%d/%d want 4/3（mp 专属 school_season 已合并）",
			ok.UpstreamTotal, ok.IncompleteTotal)
	}
	if len(ok.Growth) != 2 || len(ok.GrowthLocked) != 0 || len(ok.Manual) != 1 {
		t.Errorf("a-ok 三桶=%d/%d/%d want 2/0/1", len(ok.Growth), len(ok.GrowthLocked), len(ok.Manual))
	}
	if len(ok.Growth) == 2 && ok.Growth[1].TaskCode != "school_season" {
		t.Errorf("a-ok growth[1]=%q want school_season（mp 专属码合并）", ok.Growth[1].TaskCode)
	}

	lock := byUID["a-lock"]
	if len(lock.GrowthLocked) != 1 || lock.GrowthLocked[0].Action != scanActionLockedAuto {
		t.Errorf("a-lock locked 桶=%v want 1 个 locked_auto", lock.GrowthLocked)
	}
	if len(lock.Manual) != 1 || lock.Manual[0].TaskCode != "wb_wechat_oa_subscribe_task" {
		t.Errorf("a-lock manual=%v want 锁定且无动作任务", lock.Manual)
	}

	global := byUID["a-global"]
	if !global.Global || global.ScanState != scanStateOK {
		t.Errorf("a-global global=%v state=%q want true/ok", global.Global, global.ScanState)
	}
	if len(global.Growth) != 2 {
		t.Errorf("a-global growth=%d want 2（国际版 checklist 必须可见）", len(global.Growth))
	}

	if byUID["a-ent"].ScanState != scanStateSkippedEnterprise {
		t.Errorf("a-ent state=%q want skipped_enterprise", byUID["a-ent"].ScanState)
	}
	if byUID["a-dis"].ScanState != scanStateSkippedDisabled {
		t.Errorf("a-dis state=%q want skipped_disabled", byUID["a-dis"].ScanState)
	}
	if byUID["a-401"].ScanState != scanStateError {
		t.Errorf("a-401 state=%q want error", byUID["a-401"].ScanState)
	}

	// 顶层计数求和必须与三个桶总和一致。
	if resp.PendingCount != len(ok.Growth)+len(global.Growth) {
		t.Errorf("pending_count=%d want %d", resp.PendingCount, len(ok.Growth)+len(global.Growth))
	}
	if resp.LockedCount != 1 {
		t.Errorf("locked_count=%d want 1", resp.LockedCount)
	}
	if resp.ManualCount != 2 {
		t.Errorf("manual_count=%d want 2", resp.ManualCount)
	}
	if resp.IncompleteTotal != resp.PendingCount+resp.LockedCount+resp.ManualCount {
		t.Errorf("incomplete_total=%d != pending+locked+manual=%d",
			resp.IncompleteTotal, resp.PendingCount+resp.LockedCount+resp.ManualCount)
	}

	// 门控：企业/禁用不发上游请求；global 不调用 mp 口径。
	if n := log.count("a-ent|cn"); n != 0 {
		t.Errorf("企业版被调用了 %d 次上游（应跳过）", n)
	}
	if n := log.count("a-dis|cn"); n != 0 {
		t.Errorf("禁用账号被调用了 %d 次上游（应跳过）", n)
	}
	if n := log.count("a-global|mp"); n != 0 {
		t.Errorf("global 账号被调用了 %d 次 mp 口径（实测两口径同一份清单）", n)
	}
}

// ---------------------------------------------------------------------------
// 验收 ④：global 账号出现在队列 items（不再被直接 return）
// ---------------------------------------------------------------------------

func TestTaskCenterQueueIncludesGlobal(t *testing.T) {
	const bodyGlobal = `{"code":0,"msg":"OK","data":{"tasks":[` +
		`{"code":"first_chat","level_name":"养虾尝试","status":"available"},` +
		`{"code":"skill_installed","level_name":"养虾尝试","status":"available"}]}}`

	log := newTaskCallLog()
	srv := newTaskTestServer(t, log, func(uid, platform string) (int, string) {
		switch uid {
		case "u-global":
			return http.StatusOK, bodyGlobal
		default:
			return http.StatusInternalServerError, `{"code":500,"msg":"should not be called"}`
		}
	})
	p := newTaskTestPanel(t, srv)
	addTaskAccount(p, "u-global", "tok-global", true, false)
	addTaskAccount(p, "u-ent", "tok-ent", false, true)
	addTaskAccount(p, "u-dis", "tok-dis", false, false)
	p.cfg.Pool.Disable("u-dis", "test")

	plan := p.buildGrowthQueue(true)
	if len(plan.items) == 0 {
		t.Fatal("队列 items 为空：global 账号的 checklist 必须入队")
	}
	for _, it := range plan.items {
		if it.UID != "u-global" {
			t.Errorf("非预期条目 uid=%q want u-global", it.UID)
		}
		if it.Source != queueSourceGlobal {
			t.Errorf("条目 %s source=%q want global", it.Code, it.Source)
		}
		if it.Action != scanActionAuto {
			t.Errorf("条目 %s action=%q want auto", it.Code, it.Action)
		}
		if it.Status != queueStatusPending {
			t.Errorf("条目 %s status=%q want pending", it.Code, it.Status)
		}
	}
	if len(plan.accounts) != 1 || !plan.accounts[0].a.IsGlobal() {
		t.Errorf("队列账号组=%v want 仅 global", plan.accounts)
	}
	skipped := map[string]bool{}
	for _, uid := range plan.skipped {
		skipped[uid] = true
	}
	if !skipped["u-ent"] || !skipped["u-dis"] {
		t.Errorf("skipped=%v 必须包含企业版与禁用账号", plan.skipped)
	}
	if len(plan.errors) != 0 {
		t.Errorf("errors=%v want empty", plan.errors)
	}
	if n := log.count("u-global|cn"); n == 0 {
		t.Error("global 账号应调用默认口径（workbuddy.ai 成长路径）")
	}
	if n := log.count("u-ent|cn"); n != 0 {
		t.Errorf("企业版被调用 %d 次上游（应跳过）", n)
	}
	if n := log.count("u-dis|cn"); n != 0 {
		t.Errorf("禁用账号被调用 %d 次上游（应跳过）", n)
	}
}

// ---------------------------------------------------------------------------
// 验收 ⑥：run_queue 全失败 → started=false 且 errors 非空
// ---------------------------------------------------------------------------

func TestTaskCenterRunQueueAllFailed(t *testing.T) {
	log := newTaskCallLog()
	srv := newTaskTestServer(t, log, func(uid, platform string) (int, string) {
		return http.StatusUnauthorized, `{"code":401,"msg":"invalid token"}`
	})
	p := newTaskTestPanel(t, srv)
	addTaskAccount(p, "u1", "tok1", false, false)
	addTaskAccount(p, "u2", "tok2", false, false)

	resp := doRunQueue(t, p)
	if resp.Started {
		t.Error("所有账号任务列表都失败时 started 必须为 false")
	}
	if len(resp.Errors) == 0 {
		t.Fatal("started=false 时 errors 必须非空（不得只回「全部账号没有待办任务」）")
	}
	if !strings.Contains(resp.Message, "401") && !strings.Contains(resp.Message, "失败") {
		t.Errorf("message=%q 必须包含真实失败原因", resp.Message)
	}
	if strings.Contains(resp.Message, "全部账号没有待办任务") {
		t.Errorf("message=%q 不得把上游失败说成没有待办", resp.Message)
	}
	q := p.queue()
	q.mu.Lock()
	running := q.running
	q.mu.Unlock()
	if running {
		t.Error("启动失败后队列 running 必须复位为 false")
	}
}

// ---------------------------------------------------------------------------
// 队列 verified 判定：动作无错误但回读未达标，绝不写 done
// ---------------------------------------------------------------------------

// outcomeDeps 构造队列单测依赖：假任务快照 + 假动作 + 假领奖。
func outcomeDeps(before, after *upstream.Task, runErr, claimErr error) *queueDeps {
	return &queueDeps{
		readBefore: func(a *auth.Auth, code, source string) (*upstream.Task, error) {
			return before, nil
		},
		readAfter: func(a *auth.Auth, code, source string) (*upstream.Task, error) {
			return after, nil
		},
		claim: func(a *auth.Auth, isMP bool, code string) (int64, int64, error) {
			if claimErr != nil {
				return 0, 0, claimErr
			}
			return 10, 2, nil
		},
		actionFor: func(code string) *autoAction {
			return &autoAction{
				TaskCode: code,
				run: func(*Panel, *auth.Auth) (string, error) {
					return "动作已执行", runErr
				},
			}
		},
		gap: 0,
	}
}

func TestTaskCenterQueueVerifiedOutcome(t *testing.T) {
	before := &upstream.Task{TaskCode: "chat_5", Target: 5, Current: 2}
	same := &upstream.Task{TaskCode: "chat_5", Target: 5, Current: 2}
	advanced := &upstream.Task{TaskCode: "chat_5", Target: 5, Current: 5, Claimable: true}
	claimed := &upstream.Task{TaskCode: "chat_5", Target: 5, Current: 5, Claimed: true}

	p := &Panel{}
	a := &auth.Auth{UID: "u1"}

	// 1) 动作无错误但进度未变化 → unverified（不得 done）。
	it := queueItem{UID: "u1", Kind: "growth", Code: "chat_5", Source: queueSourceCN,
		Status: queueStatusRunning, Action: scanActionAuto}
	got := p.runQueueItem(a, it, outcomeDeps(before, same, nil, nil))
	if got.Status != AutoStatusUnverified || got.Verified {
		t.Errorf("进度无变化 status=%q verified=%v want unverified/false", got.Status, got.Verified)
	}
	if !strings.Contains(got.Message, "未变化") {
		t.Errorf("message=%q 应说明进度未变化", got.Message)
	}
	if got.ProgressBefore != "2/5" || got.ProgressAfter != "2/5" {
		t.Errorf("progress=%q→%q want 2/5→2/5", got.ProgressBefore, got.ProgressAfter)
	}

	// 2) 回读达标 → done + verified=true。
	got = p.runQueueItem(a, it, outcomeDeps(before, advanced, nil, nil))
	if got.Status != AutoStatusDone || !got.Verified {
		t.Errorf("回读达标 status=%q verified=%v want done/true", got.Status, got.Verified)
	}
	// 达标后自动领奖应落 claimed。
	if !got.Claimed {
		t.Error("回读达标且可领奖时 claimed 应为 true")
	}

	// 3) 已领取 → done + verified。
	got = p.runQueueItem(a, it, outcomeDeps(before, claimed, nil, nil))
	if got.Status != AutoStatusDone || !got.Verified {
		t.Errorf("已领取 status=%q verified=%v want done/true", got.Status, got.Verified)
	}

	// 4) 动作报错 → failed（verified=false），保留上游错误文本。
	runErr := errors.New("upstream 500: boom")
	got = p.runQueueItem(a, it, outcomeDeps(before, same, runErr, nil))
	if got.Status != AutoStatusFailed || got.Verified {
		t.Errorf("动作报错 status=%q verified=%v want failed/false", got.Status, got.Verified)
	}
	if !strings.Contains(got.Message, "boom") {
		t.Errorf("message=%q 应保留上游错误文本", got.Message)
	}

	// 5) claim_only：已达标 → 领奖成功 = done + verified。
	claimBefore := &upstream.Task{TaskCode: "chat_5", Target: 1, Current: 1, Claimable: true}
	claimIt := queueItem{UID: "u1", Kind: "growth", Code: "chat_5", Source: queueSourceCN,
		Status: queueStatusRunning, Action: scanActionClaimOnly}
	got = p.runQueueItem(a, claimIt, outcomeDeps(claimBefore, claimBefore, nil, nil))
	if got.Status != AutoStatusDone || !got.Verified || !got.Claimed {
		t.Errorf("claim_only status=%q verified=%v claimed=%v want done/true/true",
			got.Status, got.Verified, got.Claimed)
	}

	// 6) 领奖失败 → failed（不能因为达标就写 done）。
	got = p.runQueueItem(a, claimIt, outcomeDeps(claimBefore, claimBefore, nil, errors.New("claim 400")))
	if got.Status != AutoStatusFailed || got.Verified {
		t.Errorf("领奖失败 status=%q verified=%v want failed/false", got.Status, got.Verified)
	}

	// 7) 每个条目的 JSON 都必须带 verified 字段（未证明时 false）。
	raw, err := json.Marshal(queueItem{UID: "u1", Code: "chat_5", Status: AutoStatusUnverified})
	if err != nil {
		t.Fatalf("marshal queueItem: %v", err)
	}
	if !strings.Contains(string(raw), `"verified":false`) {
		t.Errorf("queueItem JSON 缺少 verified:false：%s", raw)
	}
}

// ---------------------------------------------------------------------------
// 验收 ⑦：diag 的 http/body_bytes/top_keys/task_count 与真实响应一致
// ---------------------------------------------------------------------------

func TestTasksDiagHandlerMatchesResponse(t *testing.T) {
	bodyDefault := `{"code":0,"msg":"OK","data":{"tasks":[{"task_code":"chat_5"},{"task_code":"first_buddy"}],"total":2}}`
	bodyMP := `{"code":0,"msg":"OK","data":{"tasks":[{"task_code":"chat_5"},{"task_code":"school_season"},{"task_code":"Sequential_Tasks_1"}]}}`

	log := newTaskCallLog()
	srv := newTaskTestServer(t, log, func(uid, platform string) (int, string) {
		if platform == queueSourceMP {
			return http.StatusOK, bodyMP
		}
		return http.StatusOK, bodyDefault
	})
	p := newTaskTestPanel(t, srv)
	a := addTaskAccount(p, "u-diag", "SECRET-TOKEN-abc", false, false)
	a.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()

	rec := httptest.NewRecorder()
	p.tasksDiag(rec, httptest.NewRequest(http.MethodGet, "/panel/api/tasks/diag?uid=u-diag", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("diag status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		OK             bool                  `json:"ok"`
		UID            string                `json:"uid"`
		Realm          string                `json:"realm"`
		Global         bool                  `json:"global"`
		Enterprise     bool                  `json:"enterprise"`
		TokenExpiresAt string                `json:"token_expires_at"`
		Growth         upstream.TaskListDiag `json:"growth"`
		MP             upstream.TaskListDiag `json:"mp"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode diag: %v body=%s", err, rec.Body.String())
	}
	if !got.OK || got.UID != "u-diag" || got.Realm != "cn" || got.Global || got.Enterprise {
		t.Errorf("diag 头部=%+v want ok/u-diag/cn/false/false", got)
	}
	if got.Growth.HTTP != http.StatusOK || got.Growth.BodyBytes != len(bodyDefault) || got.Growth.TaskCount != 2 {
		t.Errorf("growth diag http/bytes/count=%d/%d/%d want 200/%d/2",
			got.Growth.HTTP, got.Growth.BodyBytes, got.Growth.TaskCount, len(bodyDefault))
	}
	if keys := strings.Join(got.Growth.TopKeys, ","); keys != "tasks,total" {
		t.Errorf("growth top_keys=%q want tasks,total", keys)
	}
	if got.MP.HTTP != http.StatusOK || got.MP.BodyBytes != len(bodyMP) || got.MP.TaskCount != 3 {
		t.Errorf("mp diag http/bytes/count=%d/%d/%d want 200/%d/3",
			got.MP.HTTP, got.MP.BodyBytes, got.MP.TaskCount, len(bodyMP))
	}
	if keys := strings.Join(got.MP.TopKeys, ","); keys != "tasks" {
		t.Errorf("mp top_keys=%q want tasks", keys)
	}
	if got.TokenExpiresAt == "" {
		t.Error("token_expires_at 应按账号 ExpiresAt 输出")
	} else if _, err := time.Parse(time.RFC3339, got.TokenExpiresAt); err != nil {
		t.Errorf("token_expires_at=%q 不是 RFC3339: %v", got.TokenExpiresAt, err)
	}
	if strings.Contains(rec.Body.String(), "SECRET-TOKEN-abc") {
		t.Error("diag 响应泄露 access token")
	}
	if n := log.count("u-diag|cn"); n == 0 {
		t.Error("diag 应调用默认口径")
	}
	if n := log.count("u-diag|mp"); n == 0 {
		t.Error("diag 应调用小程序口径")
	}
}

// diag 缺 uid / 账号不存在时的错误语义。
func TestTasksDiagHandlerBadRequest(t *testing.T) {
	log := newTaskCallLog()
	srv := newTaskTestServer(t, log, func(uid, platform string) (int, string) {
		return http.StatusOK, `{"code":0,"data":{"tasks":[]}}`
	})
	p := newTaskTestPanel(t, srv)

	rec := httptest.NewRecorder()
	p.tasksDiag(rec, httptest.NewRequest(http.MethodGet, "/panel/api/tasks/diag", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺 uid status=%d want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	p.tasksDiag(rec, httptest.NewRequest(http.MethodGet, "/panel/api/tasks/diag?uid=missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("未知账号 status=%d want 404", rec.Code)
	}
}
