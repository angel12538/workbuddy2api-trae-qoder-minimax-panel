// tasks.go growth 域「任务」接口：列表查询 / 接受 / 领取奖励。
//
// 来源：上游 scripts/task_common.py 实测口径（list_tasks/accept_tasks/claim_reward），
// 集成进主程序后不再需要外部脚本。
//
// 端点（chatBase，BillingHeaders）：
//   - GET  /v2/activity/growth/tasks                全量任务列表（含 progress/accept_status）
//   - POST /v2/activity/growth/tasks/accept         {"task_codes":[...]} not_accepted → accepted
//   - POST /v2/activity/growth/tasks/reward/claim   {"task_code":"..."} 完成态领奖
//
// 语义要点：
//   - accept 是"报名"，不产生进度；进度由服务端行为事件点亮（如 chat_request_send 上报、
//     真实对话），故 accept 后可幂等重放。
//   - claim 仅在 progress 达标后可领；重复领返回业务错误（安全，不需要前置状态判断）。
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// growth 域任务路径（与 scripts/task_common.py 对齐）。
const (
	tasksListPath   = "/v2/activity/growth/tasks"
	tasksAcceptPath = "/v2/activity/growth/tasks/accept"
	// growthProfilePath 成长路径总览（国际版专有；国服 404）。
	growthProfilePath = "/v2/activity/growth/profile"
)

// mpPlatform 小程序口径头值：小程序限定任务（Sequential_Tasks_1 / school_season）
// 的列表下发、accept、claim 全链路要求 X-Client-Platform: miniprogram。
const mpPlatform = "miniprogram"

// Task 单个任务的对外视图（字段名与上游 JSON 对齐，多余字段不透出）。
type Task struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"` // 操作指引（含跳转说明）
	TaskDesc     string `json:"task_desc,omitempty"`   // 达成条件简述
	Credit       int64  `json:"credit,omitempty"`      // 奖励积分（上游 reward_credit）
	Energy       int64  `json:"energy,omitempty"`      // 奖励能量（上游 reward_energy）
	HasReward    bool   `json:"has_reward,omitempty"`  // 是否带奖励
	RewardBuddy  bool   `json:"reward_buddy,omitempty"`
	TaskType     string `json:"task_type,omitempty"` // single（一次性）/ 累计型
	Tag          string `json:"tag,omitempty"`       // 端标记（PC 等）
	JumpURL      string `json:"jump_url,omitempty"`  // 客户端跳转协议（workbuddy://...）
	Locked       bool   `json:"locked,omitempty"`    // 上游标记未解锁
	UnlockAt     string `json:"unlock_at,omitempty"` // 解锁时刻（RFC3339，上游给；如 2026-09-30T00:00:00+08:00）
	Target       int64  `json:"target"`              // 目标次数（恒输出：0 是有效进度值）
	Current      int64  `json:"current"`             // 当前进度（恒输出：0 是有效进度值）
	AcceptStatus string `json:"accept_status,omitempty"`
	Status       string `json:"status,omitempty"`    // 上游任务状态（complete 等）
	Claimable    bool   `json:"claimable,omitempty"` // 进度达标且未领取（本地推算）
	Claimed      bool   `json:"claimed,omitempty"`   // 已领取（accept_status == claimed）

	// —— 国际版（workbuddy.ai）「成长路径」清单型任务专属字段 ——
	//
	// 国际版 /v2/activity/growth/tasks 返回的是**另一套 schema**：
	//
	//	{"task_id":1,"code":"first_chat","title":"完成一次对话",
	//	 "description":"...","icon_url":"","level_name":"养虾尝试",
	//	 "status":"available","url":""}
	//
	// 与国服的差别：任务码在 code（不是 task_code）；没有 accept_status /
	// progress / reward_* / task_type 等字段；status 取值是 available 等
	// 国际版专属词表（不是国服 not_accepted/accepted/in_progress/completed/claimed）。
	//
	// 语义差别（实测）：**没有 accept（报名）这一步**——向 accept 端点提交这些
	// 任务码会逐条返回 "task not found"。这些任务由桌面客户端行为点亮
	// （真实对话 / 装技能 / 链微信 / 召唤专家 / 用模板），网关侧无法代为完成
	// （已验证：走 /v2/report 上报 chat_request_send 返回 200 但状态不变）。
	//
	// Checklist 标记即由 level_name 非空判定（国服任务不带该字段），
	// 前端据此隐藏「接受」按钮、改按 available 词表渲染状态。
	TaskID    int64  `json:"task_id,omitempty"`
	LevelName string `json:"level_name,omitempty"`
	URL       string `json:"url,omitempty"`
	Checklist bool   `json:"checklist,omitempty"`
}

// GrowthProfile 国际版成长路径总览（GET /v2/activity/growth/profile）。
// 国服无此端点（404），仅国际版账号可查；用于展示「已完成 x/5 · 等级 n」。
type GrowthProfile struct {
	Completed  int    `json:"completed"`
	Total      int    `json:"total"`
	Level      int    `json:"level"`
	LevelName  string `json:"level_name"`
	MaxLevel   bool   `json:"max_level"`
	FirstVisit bool   `json:"first_visit"`
}

// AcceptResult 单个任务的接受结果（上游 data.results[] 元素）。
//
// status 实测取值：success（本次接受成功）/ already_accepted（此前已接受，幂等）/
// error（message 说明原因，如 "task not found"、"task already completed"）。
type AcceptResult struct {
	TaskCode string `json:"task_code"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
}

// ListTasks 拉取全量任务列表（默认口径，无端标记头）。
// 响应形如 data.tasks[]，元素字段随任务类型变化（progress 可能是 {current,target} 或平铺），
// 这里做宽松解析：两种形状都尝试。
func (c *Client) ListTasks(a *auth.Auth) ([]Task, error) {
	data, err := c.growthJSON(a, http.MethodGet, tasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data)
}

// ListTasksMP 拉取小程序口径的任务列表（X-Client-Platform: miniprogram）。
// 小程序限定任务（Sequential_Tasks_1「小程序首对话」/ school_season「校园日」等）
// 仅在该口径下发——默认列表不出现，accept/claim 同样要求该头（缺头 accept 返回
// task not found，上游 task_runner 实测）。**实测 mp 列表是默认口径的超集**
// （含 RichMeow/Model_chat 等常规任务 + wb_wechat_oa_subscribe_task 等 mp 专属），
// 合并时调用方须按 task_code 去重。
func (c *Client) ListTasksMP(a *auth.Auth) ([]Task, error) {
	data, err := c.growthJSONMP(a, http.MethodGet, tasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data)
}

// growthJSONMP 发 growth 域请求（小程序口径：叠加 X-Client-Platform: miniprogram）
// 并解信封。语义同 growthJSON。
func (c *Client) growthJSONMP(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.chatBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("X-Client-Platform", mpPlatform)
	return c.doJSON(req)
}

// AcceptTasksMP 接受小程序限定任务（mp 头；缺头实测 task not found）。
// 幂等语义同 AcceptTasks。
func (c *Client) AcceptTasksMP(a *auth.Auth, taskCodes []string) error {
	_, err := c.growthJSONMP(a, http.MethodPost, tasksAcceptPath, map[string]any{"task_codes": taskCodes})
	return err
}

// ClaimRewardMP 领取小程序限定任务奖励：chat 域 /activity/growth/tasks/{code}/claim
// + mp 头（上游 task_runner claim_one(mp=True) 同款）；chat 域 400 时降级 Web 域
// 领奖端点（ClaimReward，x-client-platform: web 形态）。返回 (credit, energy, err)。
func (c *Client) ClaimRewardMP(a *auth.Auth, taskCode string) (credit, energy int64, err error) {
	req, err := http.NewRequest(http.MethodPost,
		c.chatBase(a)+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return 0, 0, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("X-Client-Platform", mpPlatform)
	data, err := c.doJSON(req)
	if err != nil {
		// chat 域对该路径 400（部分任务/租户形态）→ Web 域降级（已实测可领）。
		if ue, ok := err.(*Error); ok && ue.Status == http.StatusBadRequest {
			return c.ClaimReward(a, taskCode)
		}
		return 0, 0, err
	}
	return parseClaimReward(data)
}

// parseClaimReward 解析领奖响应 data：{"already_claimed":bool,"credit":n,"energy":n}。
func parseClaimReward(data json.RawMessage) (credit, energy int64, err error) {
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	return resp.Credit, resp.Energy, nil
}

// parseGrowthTasks 解析 growth 任务列表 data.tasks[]（国服 / 国际版两套 schema 共用）。
//
// 两套 schema 的差异见 Task 结构体注释。这里做**双字段兼容**：任务码取
// task_code，缺失时回落 code（国际版）；并读 task_id / level_name / url。
// 判定清单型用 level_name 非空——国服任务不带该字段，不会误判。
func parseGrowthTasks(data json.RawMessage) ([]Task, error) {
	var resp struct {
		Tasks []struct {
			// 国服字段
			TaskCode     string          `json:"task_code"`
			Title        string          `json:"title"`
			Description  string          `json:"description"`
			TaskDesc     string          `json:"task_desc"`
			RewardCredit int64           `json:"reward_credit"` // 上游实际字段名（reward_ 前缀）
			RewardEnergy int64           `json:"reward_energy"`
			HasReward    bool            `json:"has_reward"`
			RewardBuddy  bool            `json:"reward_buddy"`
			TaskType     string          `json:"task_type"`
			Tag          string          `json:"tag"`
			JumpURL      string          `json:"jump_url"`
			Locked       bool            `json:"locked"`
			UnlockAt     string          `json:"unlock_at"`
			AcceptStatus string          `json:"accept_status"`
			Status       string          `json:"status"`
			Target       int64           `json:"target"`
			Current      int64           `json:"current"`
			Progress     json.RawMessage `json:"progress"`
			// 国际版字段（任务码在 code，另有 task_id / level_name / url）
			Code      string `json:"code"`
			TaskID    int64  `json:"task_id"`
			LevelName string `json:"level_name"`
			URL       string `json:"url"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		cur, tgt := t.Current, t.Target
		// progress 可能是 {current,target} 对象（实测口径），覆盖平铺字段。
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var pr struct {
				Current int64 `json:"current"`
				Target  int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &pr) == nil && (pr.Target > 0 || pr.Current > 0) {
				cur, tgt = pr.Current, pr.Target
			}
		}
		// 任务码：国际版放在 code。缺 task_code 时回落，否则前端拿到空码，
		// 接受/领奖请求都会提交空字符串（历史上表现为「点按钮没反应」）。
		code := t.TaskCode
		if code == "" {
			code = t.Code
		}
		claimed := t.AcceptStatus == "claimed"
		// 清单型（国际版成长路径）：level_name 非空是可靠判据（国服无此字段）。
		checklist := t.LevelName != ""
		if checklist && t.Status == "claimed" {
			claimed = true // 国际版把「已领取」放在 status 里
		}
		out = append(out, Task{
			TaskCode:     code,
			Title:        t.Title,
			Description:  t.Description,
			TaskDesc:     t.TaskDesc,
			Credit:       t.RewardCredit,
			Energy:       t.RewardEnergy,
			HasReward:    t.HasReward,
			RewardBuddy:  t.RewardBuddy,
			TaskType:     t.TaskType,
			Tag:          t.Tag,
			JumpURL:      t.JumpURL,
			Locked:       t.Locked,
			UnlockAt:     t.UnlockAt,
			Target:       tgt,
			Current:      cur,
			AcceptStatus: t.AcceptStatus,
			Status:       t.Status,
			Claimable:    !claimed && tgt > 0 && cur >= tgt,
			Claimed:      claimed,
			TaskID:       t.TaskID,
			LevelName:    t.LevelName,
			URL:          t.URL,
			Checklist:    checklist,
		})
	}
	return out, nil
}

// GrowthProfile 拉取国际版成长路径总览（GET /v2/activity/growth/profile）。
// 国服该端点 404 → 返回 error，调用方静默降级（不展示等级条）。
func (c *Client) GrowthProfile(a *auth.Auth) (*GrowthProfile, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthProfilePath, nil)
	if err != nil {
		return nil, err
	}
	var p GrowthProfile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// AcceptTasks 接受任务（幂等：已 accepted 时上游返回成功或业务提示，均不视为致命错误）。
func (c *Client) AcceptTasks(a *auth.Auth, taskCodes []string) error {
	_, err := c.AcceptTasksDetailed(a, taskCodes)
	return err
}

// AcceptTasksDetailed 接受任务并返回**逐任务**真实结果。
//
// 为什么必须解析 results[]：上游对 accept 恒返回 HTTP 200 + code=0，把失败
// 藏在 data.results[].status="error" 里（如 "task not found"）。只看 HTTP 状态
// 会把「一条都没接受成功」报成「已接受 N 个」——这正是此前国际版账号点
// 「全部接受」提示成功、但任务状态毫无变化的原因。
func (c *Client) AcceptTasksDetailed(a *auth.Auth, taskCodes []string) ([]AcceptResult, error) {
	data, err := c.growthJSON(a, http.MethodPost, tasksAcceptPath, map[string]any{"task_codes": taskCodes})
	if err != nil {
		return nil, err
	}
	return parseAcceptResults(data, taskCodes), nil
}

// AcceptTasksMPDetailed 小程序口径的接受（mp 头），结果语义同 AcceptTasksDetailed。
func (c *Client) AcceptTasksMPDetailed(a *auth.Auth, taskCodes []string) ([]AcceptResult, error) {
	data, err := c.growthJSONMP(a, http.MethodPost, tasksAcceptPath, map[string]any{"task_codes": taskCodes})
	if err != nil {
		return nil, err
	}
	return parseAcceptResults(data, taskCodes), nil
}

// parseAcceptResults 解析 accept 响应的 data.results[]。
//
// 上游未返回 results（形态变化）时，为请求中的每个码回填 status="unknown"——
// 不假装成功，也不当成失败，让调用方按「结果未知」提示。
func parseAcceptResults(data json.RawMessage, requested []string) []AcceptResult {
	var resp struct {
		Results []AcceptResult `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil || len(resp.Results) == 0 {
		out := make([]AcceptResult, 0, len(requested))
		for _, code := range requested {
			out = append(out, AcceptResult{TaskCode: code, Status: "unknown", Message: "上游未返回逐任务结果"})
		}
		return out
	}
	return resp.Results
}

// Failed 判断一条接受结果是否算失败。
//
// 语义：
//   - success / already_accepted → 成功（后者是幂等重放的正常回执）
//   - error + "already completed" → 不算失败：任务早已完成（甚至已领奖），
//     把它计为失败会让「全部接受」在成熟账号上永远报红
//   - error 其他 / unknown 等未知状态 → 失败。unknown 来自上游未返回
//     results 的形态变化，宁可报「结果未知」也不能假装成功。
func (r AcceptResult) Failed() bool {
	switch r.Status {
	case "success", "already_accepted":
		return false
	case "error":
		return !strings.Contains(r.Message, "already completed")
	default:
		return true
	}
}

// ClaimReward 领取单个任务奖励。
//
// 端点来源（实测）：Web 成长中心的领奖请求 ——
//
//	POST https://www.workbuddy.cn/activity/growth/tasks/<task_code>/claim
//	（任务码在**路径**里，无 body；带 x-client-platform: web 头，Bearer 鉴权）
//
// 关键区别：此前误用 CLI 域 copilot.tencent.com 的
// /v2/activity/growth/tasks/reward/claim（task_code 放 body），该路径**不存在**，
// 一直返回 400 "task not completed"，是此前领奖失败的真实原因。
// 本实现返回 (credit, energy, err)：credit/energy 为本次到账奖励（已领取过时为 0）。
func (c *Client) ClaimReward(a *auth.Auth, taskCode string) (credit, energy int64, err error) {
	req, err := http.NewRequest(http.MethodPost,
		c.webBase(a)+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return 0, 0, err
	}
	// Web 端请求头形状（对照浏览器实际请求）：Origin/Referer 指向 workbuddy.cn 成长中心，
	// 带 x-client-platform: web 标记来源端。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://www.workbuddy.cn")
	req.Header.Set("Referer", "https://www.workbuddy.cn/profile/growth-center")
	req.Header.Set("x-client-platform", "web")
	if ua := c.userAgent(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}

	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, err
	}
	// 响应 data：{"already_claimed":bool,"credit":100,"energy":5,...}
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	if resp.AlreadyClaimed {
		return 0, 0, nil // 幂等：重复领取不算错误，但无新增奖励
	}
	return resp.Credit, resp.Energy, nil
}
