package panel

import (
	"errors"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 假执行环境：假任务表 + 假动作 + 假回读（不真打上游）
// ---------------------------------------------------------------------------

// fakeAutoEnv 一键完成的假依赖环境。
//
// 关键点：actionFor 复制一份动作再替换 run，避免改写全局 autoActions
// （autoActionFor 对静态码返回的是全局切片元素的指针）。
type fakeAutoEnv struct {
	tasks   []upstream.Task
	mpTasks []upstream.Task
	listErr error
	mpErr   error

	readTasks map[string]upstream.Task // code → 动作后的任务状态（回读结果）
	readErr   error                    // 非 nil = 回读整体失败

	executed map[string]int
	runErr   map[string]error

	claims      int
	claimCredit int64
	claimEnergy int64
	claimErr    error
}

func newFakeAutoEnv(tasks, mpTasks []upstream.Task) *fakeAutoEnv {
	return &fakeAutoEnv{
		tasks:     tasks,
		mpTasks:   mpTasks,
		readTasks: map[string]upstream.Task{},
		executed:  map[string]int{},
		runErr:    map[string]error{},
	}
}

// fakeTask 构造一个「已接受、未领取」的上游任务。
func fakeTask(code string, current, target int64) upstream.Task {
	return upstream.Task{
		TaskCode:     code,
		Title:        code,
		Target:       target,
		Current:      current,
		AcceptStatus: "accepted",
		Claimable:    target > 0 && current >= target,
	}
}

func (f *fakeAutoEnv) deps() *autoRunDeps {
	return &autoRunDeps{
		listTasks: func(*auth.Auth) ([]upstream.Task, error) { return f.tasks, f.listErr },
		listMP:    func(*auth.Auth) ([]upstream.Task, error) { return f.mpTasks, f.mpErr },
		readTask: func(_ *auth.Auth, code string) (*upstream.Task, error) {
			if f.readErr != nil {
				return nil, f.readErr
			}
			t, ok := f.readTasks[code]
			if !ok {
				return nil, nil
			}
			cp := t
			return &cp, nil
		},
		acceptTasks: func(*auth.Auth, []string) ([]upstream.AcceptResult, error) { return nil, nil },
		acceptMP:    func(*auth.Auth, []string) ([]upstream.AcceptResult, error) { return nil, nil },
		claim: func(*auth.Auth, bool, string) (int64, int64, error) {
			f.claims++
			return f.claimCredit, f.claimEnergy, f.claimErr
		},
		actions: autoActions,
		actionFor: func(code string) *autoAction {
			orig := autoActionFor(code)
			if orig == nil {
				return nil
			}
			act := *orig // 拷贝：不得改写全局动作表的 run
			act.run = func(*Panel, *auth.Auth) (string, error) {
				f.executed[code]++
				if err, ok := f.runErr[code]; ok {
					return "", err
				}
				return "假动作已执行:" + code, nil
			}
			return &act
		},
		gap: 0,
	}
}

func findAutoResult(results []map[string]any, code string) map[string]any {
	for _, r := range results {
		if r["task_code"] == code {
			return r
		}
	}
	return nil
}

func summaryInt(sum map[string]any, key string) int {
	v, ok := sum[key].(int)
	if !ok {
		return -1
	}
	return v
}

// ---------------------------------------------------------------------------
// classifyAutoOutcome 七态表驱动
// ---------------------------------------------------------------------------

// TestClassifyAutoOutcome 覆盖 C-WB-AUTO@1 的七种判定情形：
// done×5（claimed / 进度达标 / 目标 0 的三种状态字面）、partial×2、
// unverified×3（回读失败 / 无变化 / 无基线）、failed。
func TestClassifyAutoOutcome(t *testing.T) {
	task := func(cur, target int64, accept, status string, claimed bool) *upstream.Task {
		return &upstream.Task{
			TaskCode: "x", Target: target, Current: cur,
			AcceptStatus: accept, Status: status, Claimed: claimed,
		}
	}
	cases := []struct {
		name         string
		before       *upstream.Task
		after        *upstream.Task
		runErr       error
		wantStatus   string
		wantVerified bool
	}{
		{
			name: "动作报错→failed", before: task(0, 5, "accepted", "", false),
			after: task(5, 5, "completed", "", false), runErr: errors.New("boom"),
			wantStatus: AutoStatusFailed, wantVerified: false,
		},
		{
			name: "回读失败→unverified", before: task(0, 5, "accepted", "", false),
			after: nil, wantStatus: AutoStatusUnverified, wantVerified: false,
		},
		{
			name: "已领取→done", before: task(0, 5, "accepted", "", false),
			after:      task(5, 5, "claimed", "", true),
			wantStatus: AutoStatusDone, wantVerified: true,
		},
		{
			name: "进度达标→done", before: task(0, 5, "accepted", "", false),
			after:      task(5, 5, "completed", "", false),
			wantStatus: AutoStatusDone, wantVerified: true,
		},
		{
			name: "目标0+accept_status=completed→done", before: task(0, 0, "accepted", "", false),
			after:      task(0, 0, "completed", "", false),
			wantStatus: AutoStatusDone, wantVerified: true,
		},
		{
			name: "目标0+status=complete→done", before: task(0, 0, "", "available", false),
			after:      task(0, 0, "", "complete", false),
			wantStatus: AutoStatusDone, wantVerified: true,
		},
		{
			name: "目标0+status=claimed→done", before: task(0, 0, "", "available", false),
			after:      task(0, 0, "", "claimed", false),
			wantStatus: AutoStatusDone, wantVerified: true,
		},
		{
			name: "进度增加未达标→partial", before: task(0, 5, "accepted", "", false),
			after:      task(2, 5, "accepted", "", false),
			wantStatus: AutoStatusPartial, wantVerified: false,
		},
		{
			name: "状态键变化未达标→partial", before: task(0, 5, "not_accepted", "", false),
			after:      task(0, 5, "accepted", "", false),
			wantStatus: AutoStatusPartial, wantVerified: false,
		},
		{
			name: "进度无变化→unverified", before: task(1, 5, "accepted", "", false),
			after:      task(1, 5, "accepted", "", false),
			wantStatus: AutoStatusUnverified, wantVerified: false,
		},
		{
			name: "无基线且未达标→unverified", before: nil,
			after:      task(1, 5, "accepted", "", false),
			wantStatus: AutoStatusUnverified, wantVerified: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, verified := classifyAutoOutcome(c.before, c.after, c.runErr)
			if status != c.wantStatus || verified != c.wantVerified {
				t.Fatalf("classifyAutoOutcome = (%q,%v), want (%q,%v)",
					status, verified, c.wantStatus, c.wantVerified)
			}
			// 硬约束：done 必须 verified=true；非 done 必须 verified=false。
			if status == AutoStatusDone && !verified {
				t.Fatal("done 必须 verified=true")
			}
			if status != AutoStatusDone && verified {
				t.Fatalf("%s 不应 verified=true", status)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 动态任务覆盖（expert_use_*）
// ---------------------------------------------------------------------------

// TestAutoAllDynamicExpertUseExecuted 静态 autoActions 里没有 expert_use_maidehao，
// 但 autoActionFor 能解析——一键完成必须真的执行它，且按回读达标判 done。
func TestAutoAllDynamicExpertUseExecuted(t *testing.T) {
	env := newFakeAutoEnv([]upstream.Task{fakeTask("expert_use_maidehao", 0, 5)}, nil)
	env.readTasks["expert_use_maidehao"] = fakeTask("expert_use_maidehao", 5, 5)
	env.claimCredit, env.claimEnergy = 200, 5

	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())

	item := findAutoResult(out, "expert_use_maidehao")
	if item == nil {
		t.Fatalf("动态任务 expert_use_maidehao 未出现在结果中：%+v", out)
	}
	if env.executed["expert_use_maidehao"] != 1 {
		t.Fatalf("动态任务动作执行次数 = %d, want 1", env.executed["expert_use_maidehao"])
	}
	if item["status"] != AutoStatusDone || item["verified"] != true {
		t.Fatalf("达标后应 done+verified，got status=%v verified=%v", item["status"], item["verified"])
	}
	if item["action"] != "auto" {
		t.Errorf("action = %v, want auto", item["action"])
	}
	if item["progress_before"] != "0/5" || item["progress_after"] != "5/5" {
		t.Errorf("progress = %v -> %v, want 0/5 -> 5/5", item["progress_before"], item["progress_after"])
	}
	if env.claims != 1 || item["claimed"] != true {
		t.Errorf("达标应自动领奖一次（claims=%d, claimed=%v）", env.claims, item["claimed"])
	}
}

// TestAutoAllDynamicAndStaticAreDeduped mp 口径是默认口径超集：同一 code
// 只能出现一次结果（按 task_code 去重）。
func TestAutoAllDynamicAndStaticAreDeduped(t *testing.T) {
	tasks := []upstream.Task{fakeTask("chat_5", 0, 5)}
	mpTasks := []upstream.Task{
		fakeTask("chat_5", 0, 5),               // 与默认口径重复
		fakeTask("Sequential_Tasks_1", 0, 1),   // mp 专属
		fakeTask("expert_use_douyinyin", 0, 3), // 动态任务（仅 mp 列表有）
	}
	env := newFakeAutoEnv(tasks, mpTasks)
	env.readTasks["chat_5"] = fakeTask("chat_5", 5, 5)
	env.readTasks["Sequential_Tasks_1"] = fakeTask("Sequential_Tasks_1", 1, 1)
	env.readTasks["expert_use_douyinyin"] = fakeTask("expert_use_douyinyin", 3, 3)

	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())

	seen := map[string]int{}
	for _, r := range out {
		code, _ := r["task_code"].(string)
		seen[code]++
	}
	for code, n := range seen {
		if n != 1 {
			t.Errorf("task_code %q 出现 %d 次，应按 task_code 去重", code, n)
		}
	}
	for _, code := range []string{"chat_5", "Sequential_Tasks_1", "expert_use_douyinyin"} {
		if seen[code] != 1 {
			t.Errorf("任务 %q 未被执行/未出现在结果中: %+v", code, seen)
		}
		if env.executed[code] != 1 {
			t.Errorf("任务 %q 动作执行次数 = %d, want 1", code, env.executed[code])
		}
	}
}

// ---------------------------------------------------------------------------
// 「一键完成幻觉」回归：动作成功但进度不动，绝不 done
// ---------------------------------------------------------------------------

// TestAutoAllNoProgressNeverDone 动作返回 nil 但回读进度不变 → unverified。
// 这是用户报告的「一键完成幻觉」的直接回归用例。
func TestAutoAllNoProgressNeverDone(t *testing.T) {
	env := newFakeAutoEnv([]upstream.Task{fakeTask("chat_5", 3, 5)}, nil)
	env.readTasks["chat_5"] = fakeTask("chat_5", 3, 5) // 回读进度没动

	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())

	item := findAutoResult(out, "chat_5")
	if item == nil {
		t.Fatalf("chat_5 未出现在结果中: %+v", out)
	}
	if item["status"] != AutoStatusUnverified || item["verified"] != false {
		t.Fatalf("进度未变化必须 unverified+verified=false，got status=%v verified=%v",
			item["status"], item["verified"])
	}
	if msg, _ := item["message"].(string); !strings.Contains(msg, "未证实完成") {
		t.Errorf("message 应说明未证实，got %q", msg)
	}
	if item["reason"] != "动作无错误但进度未变化" {
		t.Errorf("reason = %v", item["reason"])
	}
	// 交付证据：未证实条目的完整字段（前端据此显示 partial/unverified）。
	t.Logf("unverified 结果样例: %+v", item)
	for _, r := range out {
		if r["status"] == AutoStatusDone {
			t.Fatalf("没有达标证据却出现 done：%+v", r)
		}
	}
}

// TestAutoAllReadbackFailureUnverified 动作成功但回读本身失败 → unverified，
// 不得 done（C-WB-AUTO@1：领奖成功但回读失败也不得 done）。
func TestAutoAllReadbackFailureUnverified(t *testing.T) {
	env := newFakeAutoEnv([]upstream.Task{fakeTask("template_5", 0, 5)}, nil)
	env.readErr = errors.New("HTTP 502 bad gateway")

	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())

	item := findAutoResult(out, "template_5")
	if item == nil {
		t.Fatalf("template_5 未出现在结果中: %+v", out)
	}
	if item["status"] != AutoStatusUnverified || item["verified"] != false {
		t.Fatalf("回读失败必须 unverified+verified=false，got status=%v verified=%v",
			item["status"], item["verified"])
	}
	if reason := item["reason"]; reason != "回读失败" {
		t.Errorf("reason = %v, want 回读失败", reason)
	}
	if re, _ := item["read_error"].(string); !strings.Contains(re, "502") {
		t.Errorf("read_error 应带真实原因，got %q", re)
	}
	if msg, _ := item["message"].(string); !strings.Contains(msg, "回读失败") {
		t.Errorf("message 应说明回读失败，got %q", msg)
	}
	if item["progress_after"] != "?" {
		t.Errorf("回读失败时 progress_after 应为 ?，got %v", item["progress_after"])
	}
	if env.executed["template_5"] != 1 {
		t.Errorf("动作仍应被执行一次（失败的是回读），got %d", env.executed["template_5"])
	}
}

// TestAutoAllActionErrorFailed 动作报错 → failed，且保留上游错误文本。
func TestAutoAllActionErrorFailed(t *testing.T) {
	env := newFakeAutoEnv([]upstream.Task{fakeTask("first_buddy", 0, 1)}, nil)
	env.runErr["first_buddy"] = errors.New("上游拒绝: report 403")
	env.readTasks["first_buddy"] = fakeTask("first_buddy", 0, 1)

	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())

	item := findAutoResult(out, "first_buddy")
	if item == nil {
		t.Fatalf("first_buddy 未出现在结果中: %+v", out)
	}
	if item["status"] != AutoStatusFailed || item["verified"] != false {
		t.Fatalf("动作报错必须 failed，got status=%v verified=%v", item["status"], item["verified"])
	}
	if msg, _ := item["message"].(string); !strings.Contains(msg, "report 403") {
		t.Errorf("message 应保留上游错误文本，got %q", msg)
	}
}

// ---------------------------------------------------------------------------
// 静态表之外 → manual；locked/skipped 不执行
// ---------------------------------------------------------------------------

// TestAutoAllUnknownTaskIsManual 无自动动作的未完成任务必须列 manual，
// 不得因为「不在静态表」而消失（R3.4.2）。
func TestAutoAllUnknownTaskIsManual(t *testing.T) {
	tasks := []upstream.Task{
		{TaskCode: "wb_wechat_oa_subscribe_task", Title: "关注公众号", Target: 1, Current: 0, AcceptStatus: "accepted", TaskDesc: "关注公众号满 24 小时"},
	}
	env := newFakeAutoEnv(tasks, nil)
	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())

	item := findAutoResult(out, "wb_wechat_oa_subscribe_task")
	if item == nil {
		t.Fatalf("无自动动作的未完成任务必须出现在结果中（manual），out=%+v", out)
	}
	if item["status"] != AutoStatusManual || item["action"] != "manual" {
		t.Fatalf("应 manual/manual，got status=%v action=%v", item["status"], item["action"])
	}
	if msg, _ := item["message"].(string); !strings.Contains(msg, "关注公众号满 24 小时") {
		t.Errorf("message 应带上游原因，got %q", msg)
	}
	if len(env.executed) != 0 {
		t.Errorf("manual 任务不应被执行，executed=%v", env.executed)
	}
}

// TestAutoAllLockedAndClaimedNotExecuted 上游锁定不执行；已领取的任务不进结果
// （不是待办，也不撑大 total）。
func TestAutoAllLockedAndClaimedNotExecuted(t *testing.T) {
	locked := fakeTask("first_buddy", 0, 1)
	locked.Locked = true
	locked.UnlockAt = "2026-11-04T00:00:00+08:00"
	claimed := fakeTask("chat_5", 5, 5)
	claimed.Claimed = true

	env := newFakeAutoEnv([]upstream.Task{locked, claimed}, nil)
	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())

	if len(env.executed) != 0 {
		t.Fatalf("locked/claimed 任务不应被执行，executed=%v", env.executed)
	}
	lockedItem := findAutoResult(out, "first_buddy")
	if lockedItem == nil {
		t.Fatalf("locked 任务应如实列出，out=%+v", out)
	}
	if lockedItem["status"] != AutoStatusLocked || lockedItem["action"] != "locked_auto" {
		t.Fatalf("应 locked/locked_auto，got %v/%v", lockedItem["status"], lockedItem["action"])
	}
	if msg, _ := lockedItem["message"].(string); !strings.Contains(msg, "11-04 00:00") {
		t.Errorf("locked 提示应带解锁时刻，got %q", msg)
	}
	if findAutoResult(out, "chat_5") != nil {
		t.Error("已领取任务不应出现在待办结果中")
	}
	sum := autoAllSummary(out)
	if summaryInt(sum, AutoStatusLocked) != 1 || summaryInt(sum, "total") != 1 {
		t.Errorf("summary = %v, want locked=1 total=1", sum)
	}
}

// TestAutoAllClaimOnlyNoAction 已达标未领取：直接领奖，不跑行为链路。
func TestAutoAllClaimOnlyNoAction(t *testing.T) {
	env := newFakeAutoEnv([]upstream.Task{fakeTask("Library_read", 1, 1)}, nil)
	env.claimCredit, env.claimEnergy = 100, 5

	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())

	item := findAutoResult(out, "Library_read")
	if item == nil {
		t.Fatalf("已达标任务应出现在结果中: %+v", out)
	}
	if item["status"] != AutoStatusDone || item["verified"] != true || item["action"] != "claim_only" {
		t.Fatalf("应 done/verified/claim_only，got %v/%v/%v", item["status"], item["verified"], item["action"])
	}
	if item["claimed"] != true || item["credit"] != int64(100) || item["energy"] != int64(5) {
		t.Errorf("领奖字段不对: %+v", item)
	}
	if env.executed["Library_read"] != 0 {
		t.Errorf("已达标不该再跑动作，executed=%d", env.executed["Library_read"])
	}
}

// TestAutoAllClaimOnlyFailureIsFailed 达标但领奖失败 → failed（不谎报 done）。
func TestAutoAllClaimOnlyFailureIsFailed(t *testing.T) {
	env := newFakeAutoEnv([]upstream.Task{fakeTask("Library_read", 1, 1)}, nil)
	env.claimErr = errors.New("claim 500")

	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())
	item := findAutoResult(out, "Library_read")
	if item == nil {
		t.Fatalf("Library_read 未出现在结果中: %+v", out)
	}
	if item["status"] != AutoStatusFailed || item["verified"] != false {
		t.Fatalf("领奖失败应 failed，got %v/%v", item["status"], item["verified"])
	}
}

// TestAutoAllPartialWhenProgressMoves 进度增加但未达标 → partial（不是 done）。
func TestAutoAllPartialWhenProgressMoves(t *testing.T) {
	env := newFakeAutoEnv([]upstream.Task{fakeTask("chat_5", 0, 5)}, nil)
	env.readTasks["chat_5"] = fakeTask("chat_5", 2, 5)

	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())
	item := findAutoResult(out, "chat_5")
	if item == nil {
		t.Fatalf("chat_5 未出现在结果中: %+v", out)
	}
	if item["status"] != AutoStatusPartial || item["verified"] != false {
		t.Fatalf("进度增加未达标应 partial，got %v/%v", item["status"], item["verified"])
	}
	if item["progress_before"] != "0/5" || item["progress_after"] != "2/5" {
		t.Errorf("progress = %v -> %v", item["progress_before"], item["progress_after"])
	}
}

// ---------------------------------------------------------------------------
// summary 核算
// ---------------------------------------------------------------------------

// TestAutoAllSummaryAccounting 混合结果集的汇总核算：
// total == len(真实任务结果)，且 total 等于各状态计数之和；所有 done 都 verified。
func TestAutoAllSummaryAccounting(t *testing.T) {
	locked := fakeTask("Sequential_Tasks_4", 0, 1)
	locked.Locked = true
	claimed := fakeTask("RichMeow_Chat", 5, 5)
	claimed.Claimed = true
	tasks := []upstream.Task{
		fakeTask("chat_5", 0, 5),              // done
		fakeTask("expert_use_maidehao", 0, 3), // partial
		fakeTask("skill_1", 0, 1),             // unverified
		{TaskCode: "Expert_Philanthropy", Title: "真实捐款", Target: 1, Current: 0, AcceptStatus: "accepted"},
		locked,
		fakeTask("Library_read", 1, 1), // claim_only → done
		claimed,                        // 已领取：不入结果
	}
	env := newFakeAutoEnv(tasks, nil)
	env.readTasks["chat_5"] = fakeTask("chat_5", 5, 5)
	env.readTasks["expert_use_maidehao"] = fakeTask("expert_use_maidehao", 1, 3)
	env.readTasks["skill_1"] = fakeTask("skill_1", 0, 1)
	env.claimCredit, env.claimEnergy = 100, 5

	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())
	sum := autoAllSummary(out)

	if len(out) != 6 {
		t.Fatalf("结果项数 = %d, want 6：%+v", len(out), out)
	}
	if got := summaryInt(sum, "total"); got != len(out) {
		t.Fatalf("summary.total = %d, want %d（无 control 时应等于 len(results)）", got, len(out))
	}
	wantCounts := map[string]int{
		AutoStatusDone:       2, // chat_5 + Library_read
		AutoStatusPartial:    1, // expert_use_maidehao
		AutoStatusUnverified: 1, // skill_1
		AutoStatusManual:     1, // Expert_Philanthropy
		AutoStatusLocked:     1, // Sequential_Tasks_4
		AutoStatusSkipped:    0,
		AutoStatusFailed:     0,
	}
	sumCounts := 0
	for status, want := range wantCounts {
		got := summaryInt(sum, status)
		if got != want {
			t.Errorf("summary.%s = %d, want %d", status, got, want)
		}
		sumCounts += got
	}
	if sumCounts != summaryInt(sum, "total") {
		t.Errorf("各状态计数之和 = %d, total = %d（必须相等）", sumCounts, summaryInt(sum, "total"))
	}
	if !autoAllDoneVerified(out) {
		t.Fatal("所有 done 都必须 verified=true")
	}
}

// TestAutoAllSummaryExcludesControl 过程条目（批量接受）带 kind=control，
// 不计入 summary（契约澄清：summary 只统计真实任务）。
func TestAutoAllSummaryExcludesControl(t *testing.T) {
	// AcceptStatus 为空 → 阶段 0 会发起 accept，产生一个 control 条目。
	tasks := []upstream.Task{
		{TaskCode: "Expert_Philanthropy", Title: "真实捐款", Target: 1, Current: 0},
	}
	env := newFakeAutoEnv(tasks, nil)
	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())

	var control, real int
	for _, r := range out {
		if kind, _ := r["kind"].(string); kind == "control" {
			control++
			continue
		}
		real++
	}
	if control == 0 {
		t.Fatalf("应产生批量接受控制条目，out=%+v", out)
	}
	sum := autoAllSummary(out)
	if summaryInt(sum, "total") != real {
		t.Fatalf("summary.total = %d, want %d（control 不计入）", summaryInt(sum, "total"), real)
	}
	if summaryInt(sum, AutoStatusManual) != 1 {
		t.Errorf("summary.manual = %d, want 1", summaryInt(sum, AutoStatusManual))
	}
}

// TestAutoAllListFailureReported 两个口径都拉取失败时必须如实报错，
// 不得返回空结果让人以为「没有待办」。
func TestAutoAllListFailureReported(t *testing.T) {
	env := newFakeAutoEnv(nil, nil)
	env.listErr = errors.New("HTTP 401 invalid token")
	env.mpErr = errors.New("HTTP 401 invalid token")
	out := (&Panel{}).runAutoAllWithDeps(&auth.Auth{UID: "u1"}, env.deps())

	if len(out) != 1 {
		t.Fatalf("应返回一条失败说明，out=%+v", out)
	}
	if out[0]["status"] != AutoStatusFailed || out[0]["kind"] != "control" {
		t.Fatalf("应 failed/control，got %+v", out[0])
	}
	if msg, _ := out[0]["message"].(string); !strings.Contains(msg, "invalid token") {
		t.Errorf("message 应带真实错误，got %q", msg)
	}
	if summaryInt(autoAllSummary(out), "total") != 0 {
		t.Error("control 条目不应计入 summary.total")
	}
}
