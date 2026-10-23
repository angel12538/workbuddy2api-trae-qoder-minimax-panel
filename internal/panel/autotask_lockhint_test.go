package panel

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestUnlockAtText 上游 RFC3339 解锁时刻要转成可读的 MM-DD HH:mm；
// 解析不了时原样返回（宁可显示原始值，也不能显示空）。
func TestUnlockAtText(t *testing.T) {
	cases := map[string]string{
		"2026-09-30T00:00:00+08:00": "09-30 00:00",
		"2026-10-01T09:30:00+08:00": "10-01 09:30",
		"":                          "",
		"garbage":                   "garbage",
	}
	for in, want := range cases {
		if got := unlockAtText(in); got != want {
			t.Errorf("unlockAtText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTaskLockHint 未解锁提示必须带上解锁时刻（用户要知道什么时候轮到他），
// 上游没给时刻时退化为通用文案且仍说明会自动执行。
func TestTaskLockHint(t *testing.T) {
	got := taskLockHint(&upstream.Task{Locked: true, UnlockAt: "2026-09-30T00:00:00+08:00"})
	if !strings.Contains(got, "09-30 00:00") || !strings.Contains(got, "自动执行") {
		t.Errorf("带 unlock_at 的提示应含时刻与「自动执行」，got %q", got)
	}
	got = taskLockHint(&upstream.Task{Locked: true})
	if !strings.Contains(got, "自动执行") {
		t.Errorf("无 unlock_at 时仍应说明会自动执行，got %q", got)
	}
	if got = taskLockHint(nil); got == "" {
		t.Error("nil 任务也必须有可读提示")
	}
}

// TestMpChatTarget 目标条数解析：上游 target 优先；未接受时上游 progress 为 null
// （target=0），必须用兜底表，否则「完成 10 次对话」会被当成 1 次——只补 1 条就
// 收工，而成长队列每天只跑一两个时点，任务永远补不满。
func TestMpChatTarget(t *testing.T) {
	// 上游给了就以上游为准（哪怕与兜底表不同）。
	if got := mpChatTarget("Sequential_Tasks_6", &upstream.Task{Target: 7}); got != 7 {
		t.Errorf("有上游 target 时应取上游值 7，got %d", got)
	}
	// 上游没给（progress=null）→ 用兜底表。
	if got := mpChatTarget("Sequential_Tasks_6", &upstream.Task{}); got != 10 {
		t.Errorf("Tasks_6 无上游 target 时应兜底 10，got %d", got)
	}
	if got := mpChatTarget("Sequential_Tasks_3", nil); got != 5 {
		t.Errorf("Tasks_3 无任务时应兜底 5，got %d", got)
	}
	// 兜底表里没有的码 → 至少 1（不能 0，否则 need<=0 直接跳过上报）。
	if got := mpChatTarget("unknown_code", nil); got != 1 {
		t.Errorf("未知码应兜底 1，got %d", got)
	}
}

// TestGrowthLockedVsPending 未解锁任务与待办任务互斥：
// locked 任务不进待办（跑也只会白等 accept 两轮），但要能被 growthLocked 挑出来
// 单独展示——否则用户看到「没有待办任务」会以为扫描坏了。
func TestGrowthLockedVsPending(t *testing.T) {
	locked := upstream.Task{TaskCode: "Sequential_Tasks_6", Locked: true, AcceptStatus: "not_accepted"}
	if growthPending(locked) {
		t.Error("locked 任务不应进待办")
	}
	if !growthLocked(locked) {
		t.Error("有自动动作的 locked 任务应被 growthLocked 挑出")
	}
	// 已领取的 locked（异常态）不重复展示。
	if growthLocked(upstream.Task{TaskCode: "Sequential_Tasks_6", Locked: true, Claimed: true}) {
		t.Error("已领取的 locked 任务不应展示为未解锁")
	}
	// 无自动动作的 locked 任务（如需要真实捐款的公益专家）不属于「未解锁」桶
	// （那是给有自动动作、只等解锁的任务用的），而应落到 Manual 桶展示。
	if growthLocked(upstream.Task{TaskCode: "Expert_Philanthropy", Locked: true}) {
		t.Error("无自动动作的任务不应展示为「未解锁」")
	}
	if !growthManual(upstream.Task{TaskCode: "Expert_Philanthropy", Locked: true}) {
		t.Error("无自动动作的未完成任务应落 Manual 桶")
	}
	// 普通未完成且未锁定 → 待办，不是「未解锁」。
	pending := upstream.Task{TaskCode: "chat_5", Target: 5, Current: 2, AcceptStatus: "accepted"}
	if !growthPending(pending) {
		t.Error("未完成未锁定应进待办")
	}
	if growthLocked(pending) {
		t.Error("未锁定任务不应展示为未解锁")
	}
}

// TestGrowthManual 扫描必须覆盖**全部**未完成任务：没有自动动作的（关注公众号满 24h /
// 真实捐款 / 国际版成长路径）不能因为「面板帮不上忙」就从扫描结果里消失——否则用户
// 会以为「任务中心扫描不出来任务」。三桶（pending / locked / manual）两两互斥。
func TestGrowthManual(t *testing.T) {
	// 典型 manual：无自动动作、未领取。
	oa := upstream.Task{TaskCode: "wb_wechat_oa_subscribe_task", Target: 1, Current: 0, AcceptStatus: "accepted"}
	if !growthManual(oa) {
		t.Error("关注公众号任务无自动动作，应落 Manual 桶")
	}
	if growthPending(oa) || growthLocked(oa) {
		t.Error("Manual 任务不应同时落进其它两桶")
	}
	// 已领取的不再展示。
	if growthManual(upstream.Task{TaskCode: "wb_wechat_oa_subscribe_task", Claimed: true}) {
		t.Error("已领取任务不应落 Manual 桶")
	}
	// 有自动动作的任务不进 Manual（哪怕未完成）。
	if growthManual(upstream.Task{TaskCode: "chat_5", Target: 5, Current: 2}) {
		t.Error("有自动动作的任务不应落 Manual 桶")
	}
	// 国际版成长路径 5 项：其中 4 项后续补上了自动动作（autoActions 的 Attempt 项），
	// 因此归「可自动化」桶而不再落 Manual；仅 wechat_linked 仍无自动动作 → Manual。
	for _, code := range []string{"first_chat", "skill_installed", "expert_summoned", "template_used"} {
		tk := upstream.Task{TaskCode: code, Checklist: true}
		if growthManual(tk) {
			t.Errorf("国际版清单任务 %s 已有自动动作，不应落 Manual 桶", code)
		}
		if !growthPending(tk) {
			t.Errorf("国际版清单任务 %s 有自动动作且未领取，应落 Pending 桶", code)
		}
	}
	if !growthManual(upstream.Task{TaskCode: "wechat_linked", Checklist: true}) {
		t.Error("wechat_linked 无自动动作，应落 Manual 桶")
	}
	// 互斥性：任一未完成任务恰好落入三桶之一。
	all := []upstream.Task{
		{TaskCode: "chat_5", Target: 5, Current: 2},
		{TaskCode: "Sequential_Tasks_6", Locked: true},
		{TaskCode: "Expert_Philanthropy"},
		{TaskCode: "wb_wechat_oa_subscribe_task", Target: 1},
	}
	for _, tk := range all {
		n := 0
		if growthPending(tk) {
			n++
		}
		if growthLocked(tk) {
			n++
		}
		if growthManual(tk) {
			n++
		}
		if n != 1 {
			t.Errorf("%s 应恰好落入一个桶，实际 %d", tk.TaskCode, n)
		}
	}
}
