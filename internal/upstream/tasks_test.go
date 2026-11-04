package upstream

import (
	"encoding/json"
	"errors"
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

// —— C-WB-TASKLIST@1 验收（T7）——

// newTaskListServer 启动只回固定状态/body 的假上游（禁止打真实外网）。
func newTaskListServer(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
}

// TestListTasksDetailedEmptyArrayIsLegal 验收点①：data.tasks=[] 是合法空——
// 必须返回非 nil 空列表 + nil error，且 Items 序列化为 []（不是 null）。
func TestListTasksDetailedEmptyArrayIsLegal(t *testing.T) {
	c := newTaskListServer(t, http.StatusOK, `{"code":0,"msg":"OK","data":{"tasks":[]}}`)
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	tl, err := c.ListTasksDetailed(a)
	if err != nil {
		t.Fatalf("合法空数组不应报错: %v", err)
	}
	if tl == nil {
		t.Fatal("TaskList 不应为 nil")
	}
	if tl.Items == nil {
		t.Error("Items 必须是非 nil 空切片（否则 JSON 序列化成 null）")
	}
	if len(tl.Items) != 0 || tl.TaskCount != 0 {
		t.Errorf("len/TaskCount = %d/%d, want 0/0", len(tl.Items), tl.TaskCount)
	}
	if tl.Source != "cn" {
		t.Errorf("Source = %q, want cn", tl.Source)
	}
	if len(tl.RawKeys) != 1 || tl.RawKeys[0] != "tasks" {
		t.Errorf("RawKeys = %v, want [tasks]", tl.RawKeys)
	}
	raw, err := json.Marshal(tl.Items)
	if err != nil {
		t.Fatalf("marshal items: %v", err)
	}
	if string(raw) != "[]" {
		t.Errorf("Items 序列化 = %s, want []（不得是 null）", raw)
	}

	// 旧接口签名不变；合法空仍返回空列表 + nil。
	items, err := c.ListTasks(a)
	if err != nil {
		t.Fatalf("ListTasks 合法空不应报错: %v", err)
	}
	if items == nil || len(items) != 0 {
		t.Errorf("ListTasks = %#v, want 非 nil 空切片", items)
	}
}

// TestListTasksDetailedMissingPayload 验收点②③：data 缺失/null、tasks 缺失/null/
// 非数组必须返回 ErrTasksPayloadMissing，错误文本含账号标识与 data 顶层键，
// 且绝不能被当成「0 任务」返回空列表 + nil。
func TestListTasksDetailedMissingPayload(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		wants []string // 错误文本必须同时包含的片段
	}{
		{"data 缺失", `{"code":0,"msg":"OK"}`, []string{"data 缺失"}},
		{"data 为 null", `{"code":0,"data":null}`, []string{"data 缺失"}},
		{"data 为空对象（tasks 缺失）", `{"code":0,"data":{}}`, []string{"顶层键=[]", "缺少 tasks"}},
		{"tasks 缺失但 data 有其他键", `{"code":0,"data":{"progress":1,"tasks_v2":[]}}`, []string{"顶层键=[progress tasks_v2]", "缺少 tasks"}},
		{"tasks 为 null", `{"code":0,"data":{"tasks":null}}`, []string{"顶层键=[tasks]", "tasks 为 null"}},
		{"tasks 为字符串", `{"code":0,"data":{"tasks":"oops"}}`, []string{"tasks 类型=string"}},
		{"tasks 为对象", `{"code":0,"data":{"tasks":{}}}`, []string{"tasks 类型=object"}},
		{"tasks 为数字", `{"code":0,"data":{"tasks":3}}`, []string{"tasks 类型=number"}},
		{"data 为数组", `{"code":0,"data":[]}`, []string{"data 类型=array"}},
		{"data 为字符串", `{"code":0,"data":"oops"}`, []string{"data 类型=string"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTaskListServer(t, http.StatusOK, tc.body)
			a := &auth.Auth{AccessToken: "at", UID: "u1"}

			tl, err := c.ListTasksDetailed(a)
			if err == nil {
				t.Fatalf("want ErrTasksPayloadMissing，got nil（tl=%#v）", tl)
			}
			if tl != nil {
				t.Errorf("错误时必须返回 nil TaskList，got %#v", tl)
			}
			if !errors.Is(err, ErrTasksPayloadMissing) {
				t.Fatalf("errors.Is(err, ErrTasksPayloadMissing) = false，err=%v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "u1") {
				t.Errorf("错误文本缺少账号标识 u1: %q", msg)
			}
			if !strings.Contains(msg, "tasks") {
				t.Errorf("错误文本缺少缺失/异常的顶层键 tasks: %q", msg)
			}
			for _, want := range tc.wants {
				if !strings.Contains(msg, want) {
					t.Errorf("错误文本缺少 %q: %q", want, msg)
				}
			}

			// 旧接口兼容收紧：缺失 payload 必须报错，不得返回空列表。
			if items, err := c.ListTasks(a); err == nil || items != nil {
				t.Errorf("ListTasks 应返回错误与 nil，got items=%#v err=%v", items, err)
			}
			// mp 口径同样不得吞错。
			if _, err := c.ListTasksMPDetailed(a); !errors.Is(err, ErrTasksPayloadMissing) {
				t.Errorf("ListTasksMPDetailed 应返回同一哨兵，got %v", err)
			}
		})
	}
}

// TestListTasksDetailedHTTPStatusKept 验收点④：401/403/5xx 必须原样保留
// *Error（状态码 + 上游 body 摘要），不得被包装成 payload 缺失。
func TestListTasksDetailedHTTPStatusKept(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := newTaskListServer(t, status, `{"error":"token expired or forbidden"}`)
			tl, err := c.ListTasksDetailed(&auth.Auth{AccessToken: "at", UID: "u1"})
			if err == nil {
				t.Fatalf("HTTP %d 必须报错", status)
			}
			if tl != nil {
				t.Errorf("错误时 TaskList 应为 nil，got %#v", tl)
			}
			var ue *Error
			if !errors.As(err, &ue) {
				t.Fatalf("HTTP %d 应返回 *Error，got %T: %v", status, err, err)
			}
			if ue.Status != status {
				t.Errorf("Status = %d, want %d", ue.Status, status)
			}
			if !strings.Contains(ue.Msg, "token expired") {
				t.Errorf("Msg 应保留上游 body 摘要，got %q", ue.Msg)
			}
			if errors.Is(err, ErrTasksPayloadMissing) {
				t.Error("HTTP 错误不得被误报成 payload 缺失")
			}
		})
	}
}

// TestListTasksDetailedBusinessCodeKept 业务 code != 0 必须保留既有业务错误。
func TestListTasksDetailedBusinessCodeKept(t *testing.T) {
	c := newTaskListServer(t, http.StatusOK, `{"code":11140,"msg":"request illegal"}`)
	_, err := c.ListTasksDetailed(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err == nil {
		t.Fatal("业务 code != 0 必须报错")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("应返回 *Error，got %T: %v", err, err)
	}
	if !strings.Contains(ue.Msg, "code=11140") {
		t.Errorf("Msg 应包含 code=11140，got %q", ue.Msg)
	}
	if errors.Is(err, ErrTasksPayloadMissing) {
		t.Error("业务错误不得被误报成 payload 缺失")
	}
}

// TestListTasksMPDetailedSourceAndHeader mp 口径：Source=mp，且必须带
// X-Client-Platform: miniprogram（缺头上游不下发 mp 专属任务）。
func TestListTasksMPDetailedSourceAndHeader(t *testing.T) {
	var gotPlatform string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlatform = r.Header.Get("X-Client-Platform")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
			{"task_code":"Sequential_Tasks_1","title":"小程序首对话","progress":{"current":1,"target":1}}
		]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}

	tl, err := c.ListTasksMPDetailed(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("mp list: %v", err)
	}
	if gotPlatform != mpPlatform {
		t.Errorf("X-Client-Platform = %q, want %q", gotPlatform, mpPlatform)
	}
	if tl.Source != "mp" {
		t.Errorf("Source = %q, want mp", tl.Source)
	}
	if tl.TaskCount != 1 || len(tl.Items) != 1 {
		t.Errorf("TaskCount/len = %d/%d, want 1/1", tl.TaskCount, len(tl.Items))
	}
	if tl.Items[0].TaskCode != "Sequential_Tasks_1" || !tl.Items[0].Claimable {
		t.Errorf("mp 任务解析异常: %#v", tl.Items[0])
	}
}

// TestListTasksDetailedIntlSchema 验收点⑥：国际版 code/level_name 解析不回归
// （任务码取 code、level_name 非空即清单型、status=claimed 即已领取）。
func TestListTasksDetailedIntlSchema(t *testing.T) {
	body := `{"code":0,"msg":"OK","data":{"tasks":[
		{"task_id":1,"code":"first_chat","title":"完成一次对话","description":"...","level_name":"养虾尝试","status":"available","url":""},
		{"task_id":3,"code":"wechat_linked","title":"链接微信","description":"...","level_name":"养虾熟手","status":"claimed","url":""}
	]}}`
	c := newTaskListServer(t, http.StatusOK, body)

	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
		t.Fatalf("设置 realm: %v", err)
	}
	tl, err := c.ListTasksDetailed(a)
	if err != nil {
		t.Fatalf("国际版列表解析: %v", err)
	}
	if tl.Source != "global" {
		t.Errorf("Source = %q, want global", tl.Source)
	}
	if tl.TaskCount != 2 || len(tl.Items) != 2 {
		t.Fatalf("TaskCount/len = %d/%d, want 2/2", tl.TaskCount, len(tl.Items))
	}
	t0 := tl.Items[0]
	if t0.TaskCode != "first_chat" {
		t.Errorf("TaskCode = %q, want first_chat（国际版任务码在 code）", t0.TaskCode)
	}
	if !t0.Checklist || t0.TaskID != 1 || t0.LevelName != "养虾尝试" {
		t.Errorf("Checklist/TaskID/LevelName = %v/%d/%q，want true/1/养虾尝试",
			t0.Checklist, t0.TaskID, t0.LevelName)
	}
	if t0.Claimed {
		t.Error("status=available 不应判定为已领取")
	}
	if !tl.Items[1].Claimed {
		t.Error("国际版 status=claimed 应判定为已领取")
	}
}
