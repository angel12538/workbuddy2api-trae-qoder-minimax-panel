package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestClaimRewardWebEndpoint 领奖走 Web 域（workbuddy.cn）、任务码在路径里、无 body。
// 这是与 CLI 域（copilot.tencent.com/v2/.../reward/claim，task_code 在 body）的关键区别——
// 后者路径不存在，曾导致长期 400 "task not completed" 误判为"上游不支持领取"。
func TestClaimRewardWebEndpoint(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	var gotPlatform, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotPlatform = r.Header.Get("x-client-platform")
		gotReferer = r.Header.Get("Referer")
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	credit, energy, err := c.ClaimReward(a, "Model_chat_GLM5.2")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if credit != 100 || energy != 5 {
		t.Errorf("credit/energy = %d/%d, want 100/5", credit, energy)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method=%s want POST", gotMethod)
	}
	if want := "/activity/growth/tasks/Model_chat_GLM5.2/claim"; gotPath != want {
		t.Errorf("path=%q want %q（任务码必须在路径里）", gotPath, want)
	}
	if gotBody != "" {
		t.Errorf("claim 不应携带 body，got %q", gotBody)
	}
	if gotPlatform != "web" {
		t.Errorf("x-client-platform=%q want web", gotPlatform)
	}
	if !strings.Contains(gotReferer, "workbuddy.cn") {
		t.Errorf("Referer=%q 应指向 workbuddy.cn", gotReferer)
	}
}

// TestClaimRewardAlreadyClaimed 重复领取：上游返回 already_claimed=true，
// 本地应视为"无新增奖励但不报错"（幂等语义）。
func TestClaimRewardAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":true}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	credit, energy, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5")
	if err != nil {
		t.Fatalf("already_claimed should not error: %v", err)
	}
	if credit != 0 || energy != 0 {
		t.Errorf("already claimed should yield 0/0, got %d/%d", credit, energy)
	}
}

// TestClaimRewardNotCompleted 未达标：上游 400 + task not completed 应作为错误透出。
func TestClaimRewardNotCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{"code": 400, "msg": "task not completed"})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	if _, _, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5"); err == nil {
		t.Fatal("want error for not-completed task")
	}
}

// TestParseGrowthTasksIntlSchema 国际版（workbuddy.ai）任务 schema：
// 任务码在 code（不是 task_code），另有 task_id / level_name / url，
// 且**没有** accept_status / progress / reward_* —— 解析后必须：
//   - TaskCode 取到 code（此前解析成空串，前端提交空码 → 点按钮没反应）
//   - Checklist=true（前端据此隐藏「接受」按钮）
//   - AcceptStatus 为空、Target/Current 为 0（国际版不提供）
func TestParseGrowthTasksIntlSchema(t *testing.T) {
	raw := json.RawMessage(`{"tasks":[
		{"task_id":1,"code":"first_chat","title":"完成一次对话","description":"使用 WorkBuddy 完成至少一次对话交互","icon_url":"","level_name":"养虾尝试","status":"available","url":""},
		{"task_id":3,"code":"wechat_linked","title":"链接微信","description":"完成 WorkBuddy 设置，成功链接到微信","icon_url":"","level_name":"养虾熟手","status":"claimed","url":""}
	]}`)
	tasks, err := parseGrowthTasks(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	t0 := tasks[0]
	if t0.TaskCode != "first_chat" {
		t.Errorf("TaskCode = %q, want first_chat（国际版任务码在 code 字段）", t0.TaskCode)
	}
	if !t0.Checklist {
		t.Error("Checklist = false, want true（level_name 非空即清单型）")
	}
	if t0.TaskID != 1 || t0.LevelName != "养虾尝试" {
		t.Errorf("TaskID/LevelName = %d/%q, want 1/养虾尝试", t0.TaskID, t0.LevelName)
	}
	if t0.AcceptStatus != "" || t0.Target != 0 || t0.Current != 0 {
		t.Errorf("国际版不应有 accept_status/progress，got %q/%d/%d", t0.AcceptStatus, t0.Target, t0.Current)
	}
	if t0.Claimed {
		t.Error("status=available 不应判定为已领取")
	}
	// 国际版把「已领取」放在 status 里（不是 accept_status）。
	if !tasks[1].Claimed {
		t.Error("status=claimed 应判定为已领取")
	}
}

// TestParseGrowthTasksCNSchemaUnchanged 国服 schema 解析行为不变（回归保护）。
func TestParseGrowthTasksCNSchemaUnchanged(t *testing.T) {
	raw := json.RawMessage(`{"tasks":[
		{"task_code":"chat_5","title":"对话 5 次","task_desc":"完成 5 次对话","task_type":"single","jump_url":"workbuddy://chat","reward_credit":100,"reward_energy":5,"accept_status":"accepted","progress":{"current":2,"target":5},"tag":"PC"}
	]}`)
	tasks, err := parseGrowthTasks(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	t0 := tasks[0]
	if t0.TaskCode != "chat_5" || t0.Checklist {
		t.Errorf("国服任务 TaskCode/Checklist = %q/%v, want chat_5/false", t0.TaskCode, t0.Checklist)
	}
	if t0.Current != 2 || t0.Target != 5 || t0.Credit != 100 {
		t.Errorf("progress/reward = %d/%d/%d, want 2/5/100", t0.Current, t0.Target, t0.Credit)
	}
	if t0.AcceptStatus != "accepted" {
		t.Errorf("AcceptStatus = %q, want accepted", t0.AcceptStatus)
	}
}

// TestAcceptTasksDetailedParsesResults accept 恒返回 HTTP 200，失败藏在
// data.results[].status 里 —— 必须逐条解析，否则会把「一条都没成功」报成成功。
func TestAcceptTasksDetailedParsesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[
			{"task_code":"first_chat","status":"error","message":"task not found"},
			{"task_code":"black_cat","status":"already_accepted"},
			{"task_code":"old_task","status":"error","message":"task already completed"}
		]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	res, err := c.AcceptTasksDetailed(&auth.Auth{AccessToken: "at", UID: "u1"}, []string{"first_chat", "black_cat", "old_task"})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %d", len(res))
	}
	if !res[0].Failed() {
		t.Error("task not found 应判为失败")
	}
	if res[1].Failed() {
		t.Error("already_accepted 是幂等成功，不应判为失败")
	}
	if res[2].Failed() {
		t.Error("task already completed 是已完成回执，不应判为失败")
	}
}

// TestParseGrowthTasksLockedUnlockAt 未解锁任务必须把 locked + unlock_at 带出来。
//
// 2026-09-29 实测：Sequential_Tasks_6 在列表里带
// {"locked":true,"unlock_at":"2026-09-30T00:00:00+08:00"}，accept 逐条回
// "task locked until 2026-09-30" —— 面板要靠 unlock_at 如实告诉用户什么时候轮到他，
// 否则只能给一个没有时间点的「未解锁」，用户会以为面板坏了。
func TestParseGrowthTasksLockedUnlockAt(t *testing.T) {
	raw := json.RawMessage(`{"tasks":[
		{"task_code":"Sequential_Tasks_6","title":"完成 10次对话","task_type":"single","reward_credit":100,"reward_energy":5,"accept_status":"not_accepted","progress":null,"locked":true,"unlock_at":"2026-09-30T00:00:00+08:00","tag":"小程序限定"}
	]}`)
	tasks, err := parseGrowthTasks(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	t0 := tasks[0]
	if !t0.Locked {
		t.Error("Locked 应为 true")
	}
	if t0.UnlockAt != "2026-09-30T00:00:00+08:00" {
		t.Errorf("UnlockAt = %q, want 2026-09-30T00:00:00+08:00", t0.UnlockAt)
	}
	if t0.Claimable {
		t.Error("locked 且 progress=null 不应判为可领取")
	}
}

// TestParseAcceptResultsFallback 上游未返回 results 时不得假装成功。
func TestParseAcceptResultsFallback(t *testing.T) {
	res := parseAcceptResults(json.RawMessage(`{"foo":"bar"}`), []string{"a", "b"})
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	for _, r := range res {
		if r.Status != "unknown" || !r.Failed() {
			t.Errorf("缺失 results 应回填 unknown 且算失败，got %q failed=%v", r.Status, r.Failed())
		}
	}
}
