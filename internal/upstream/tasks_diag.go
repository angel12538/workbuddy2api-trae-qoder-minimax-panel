// tasks_diag.go 任务列表只读诊断（C-WB-DIAG@1）。
//
// 背景：面板「扫描不到任务」时，前端只能看到「没有待办」或一句笼统的错误，
// 无法区分「账号真的没有任务 / 401 / 空 body / data 形态变化 / mp 合并失败」。
// 本文件提供**与正式列表完全相同**的 base / headers / 信封解析路径的一次探针：
// 记录 HTTP 状态、响应字节数、data 顶层键、任务数与原样错误，且**不消耗、
// 不修改账号状态**（纯 GET，无 accept/claim，无本地写回）。
//
// 契约冻结 @1：字段名与语义不得改动（见 C-WB-DIAG@1 2.2.14）。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// diagBodyLimit 单次诊断读取的最大响应字节数。
// 与 doJSON 的上限保持一致（1 MiB）：任务列表正常远小于该值，超限只截断
// BodyBytes，不会把超大响应读进内存。
const diagBodyLimit = 1 << 20

// TaskListDiag 一次任务列表诊断的结果（C-WB-DIAG@1）。
//
// 语义：
//   - HTTP：上游状态码；网络错误时为 0；
//   - DurationMS：从发起请求到读完 body 的耗时；
//   - BodyBytes：实际读取到的响应字节数（最多 diagBodyLimit）；
//   - TopKeys：data 对象顶层键的稳定排序（data 非对象时为空并写 Error）；
//   - TaskCount：data.tasks 数组元素数（形态异常时为 0 并写 Error）；
//   - Error：首个错误原因（HTTP/业务码/形态/网络），已脱敏，不含 token。
//
// 注意：本结构**不携带原始 body**——诊断只需要可区分形态的信息，原始响应可能
// 含账号标识或凭据片段，不能进日志/前端。
type TaskListDiag struct {
	HTTP       int      `json:"http"`
	DurationMS int64    `json:"duration_ms"`
	BodyBytes  int      `json:"body_bytes"`
	TopKeys    []string `json:"top_keys"`
	TaskCount  int      `json:"task_count"`
	Error      string   `json:"error,omitempty"`
}

// TasksListDiag 对指定账号执行一次任务列表只读诊断。
//
// mp=false 走默认口径（GET /v2/activity/growth/tasks，BillingHeaders）；
// mp=true 额外带 X-Client-Platform: miniprogram（与 ListTasksMPDetailed 同口径）。
// base 由 chatBase(a) 决定，与正式列表完全一致（global 账号走 workbuddy.ai）。
func (c *Client) TasksListDiag(a *auth.Auth, mp bool) TaskListDiag {
	out := TaskListDiag{TopKeys: []string{}}
	start := time.Now()
	finish := func() TaskListDiag {
		out.DurationMS = time.Since(start).Milliseconds()
		return out
	}

	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+tasksListPath, nil)
	if err != nil {
		out.Error = err.Error()
		return finish()
	}
	c.BillingHeaders(req, a)
	if mp {
		req.Header.Set("X-Client-Platform", mpPlatform)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// 网络错误：HTTP 保持 0，方便前端与「401/空 body」区分。
		out.Error = err.Error()
		return finish()
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, diagBodyLimit))
	out.HTTP = resp.StatusCode
	out.BodyBytes = len(raw)
	if readErr != nil {
		out.Error = redactDiagText("read body: " + readErr.Error())
		return finish()
	}
	if resp.StatusCode >= 400 {
		// 保留真实状态与 body 摘要（脱敏），供把 401/403/5xx 与空列表区分开。
		out.Error = redactDiagText(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200)))
		return finish()
	}

	// —— 以下信封解析与归一口径与 ListTasksDetailed 保持一致 ——
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		out.Error = redactDiagText(fmt.Sprintf("parse failed: %v", err))
		return finish()
	}
	if env.Code != 0 {
		out.Error = redactDiagText(fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160)))
		return finish()
	}

	data := bytes.TrimSpace(env.Data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		// 契约：data 非对象时把 data_type 记录到 Error。
		out.Error = "data_type=null（data 缺失或为 null）"
		return finish()
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		out.Error = "data_type=" + jsonValueType(data) + "（data 非对象）"
		return finish()
	}
	out.TopKeys = sortedRawKeys(obj)

	tasksRaw, ok := obj["tasks"]
	if !ok {
		out.Error = fmt.Sprintf("缺少 tasks 字段（data 顶层键=%v）", out.TopKeys)
		return finish()
	}
	trimmed := bytes.TrimSpace(tasksRaw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		out.Error = fmt.Sprintf("tasks 为 null（data 顶层键=%v）", out.TopKeys)
		return finish()
	}
	if trimmed[0] != '[' {
		out.Error = fmt.Sprintf("tasks 类型=%s（期望数组，data 顶层键=%v）",
			jsonValueType(trimmed), out.TopKeys)
		return finish()
	}
	var wires []taskWire
	if err := json.Unmarshal(trimmed, &wires); err != nil {
		out.Error = fmt.Sprintf("tasks 数组解析失败: %v（data 顶层键=%v）", err, out.TopKeys)
		return finish()
	}
	out.TaskCount = len(wires)
	return finish()
}

// diagBearerRE 匹配 Authorization 类文本里的 Bearer 凭据。
var diagBearerRE = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)

// diagTokenFieldRE 匹配 JSON 里常见的凭据字段值。
var diagTokenFieldRE = regexp.MustCompile(
	`(?i)("(?:access_token|refresh_token|token|authorization|api_key)"\s*:\s*")[^"]*(")`)

// redactDiagText 从诊断错误文本里抹掉凭据片段。
//
// 诊断会透出上游错误 body 的摘要；上游/中间层偶尔会把请求头或 token 回显到
// body 里（如 "Bearer xxx invalid"）。这里统一替换成 <redacted>，保证
// C-WB-DIAG@1「raw 摘要脱敏、不含 token」的约束在任何响应形态下都成立。
func redactDiagText(s string) string {
	s = diagBearerRE.ReplaceAllString(s, "Bearer <redacted>")
	s = diagTokenFieldRE.ReplaceAllString(s, "${1}<redacted>${2}")
	return s
}
