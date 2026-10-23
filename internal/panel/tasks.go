// tasks.go 面板「积分任务」接口：查询任务进度、接受任务、领取奖励。
//
// 上游能力（internal/upstream/tasks.go）的三层薄封装；前端表格展示
// current/target 进度与可领取状态，运维点按钮即可，无需外部 Python 脚本。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// acceptBatchGap 批量接受的批间节流（对齐脚本 1.05s 口径，避免上游风控）。
var acceptBatchGap = 1050 * time.Millisecond

// accountByUID 取账号凭证；不存在时写 404 并返回 nil。
func (p *Panel) accountByUID(w http.ResponseWriter, uid string) *auth.Auth {
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return nil
	}
	return a
}

// accountTasks 查询单账号全量任务（进度/状态/可领取）。
//
// 国际版（workbuddy.ai）返回的是「成长路径」清单型任务，额外附带 profile
// （已完成 x/total · 等级），供前端展示关卡进度条。国服该端点 404 → 静默跳过。
func (p *Panel) accountTasks(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	// 合并小程序口径任务（school_season / Sequential_Tasks_1 等仅在 mp 头列表下发）。
	// mp 列表是默认口径的超集（实测含常规任务），按 task_code 去重；失败静默。
	// 注意：国际版两个口径返回同一份清单型任务（已实测），去重后不会重复。
	if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
		seen := map[string]bool{}
		for _, t := range tasks {
			seen[t.TaskCode] = true
		}
		for _, t := range mpTasks {
			if !seen[t.TaskCode] {
				tasks = append(tasks, t)
				seen[t.TaskCode] = true
			}
		}
	}
	// 清单型（国际版成长路径）：没有「接受」这一步，前端要换一套渲染。
	checklist := false
	for _, t := range tasks {
		if t.Checklist {
			checklist = true
			break
		}
	}
	resp := map[string]any{"ok": true, "tasks": tasks}
	if checklist {
		resp["mode"] = "checklist"
		// profile 失败不影响任务列表返回（国服本来就没有这个端点）。
		if prof, perr := p.cfg.Upstream.GrowthProfile(a); perr == nil {
			resp["profile"] = prof
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// accountTaskAccept 接受任务（报名；幂等）。
//
// 返回逐任务真实结果：上游对 accept 恒返回 HTTP 200，失败藏在 results[].status
// 里，只看 HTTP 状态会谎报成功（见 upstream.AcceptTasksDetailed 注释）。
func (p *Panel) accountTaskAccept(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	var body struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.TaskCodes) == 0 {
		writeErr(w, http.StatusBadRequest, "task_codes required")
		return
	}
	// 丢掉空码：国际版旧版解析曾产出空 task_code，提交空串只会换来
	// 一条 "task not found"，还会被当成成功。
	codes := nonEmptyCodes(body.TaskCodes)
	if len(codes) == 0 {
		writeErr(w, http.StatusBadRequest, "task_codes 为空（任务码缺失，请先重新查询）")
		return
	}
	results, err := p.cfg.Upstream.AcceptTasksDetailed(a, codes)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "accept: "+err.Error())
		return
	}
	ok, failed := countAcceptResults(results)
	log.Printf("panel: 接受任务 uid=%s 成功=%d 失败=%d codes=%v", uid, ok, failed, codes)
	resp := map[string]any{"ok": true, "accepted": ok, "results": results}
	if failed > 0 {
		resp["failed"] = failed
		resp["message"] = "有 " + strconv.Itoa(failed) + " 个任务上游拒绝（详见结果）"
	}
	writeJSON(w, http.StatusOK, resp)
}

// nonEmptyCodes 过滤空/纯空白任务码（保持顺序）。
func nonEmptyCodes(in []string) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

// taskAcceptAll 接受该账号全部尚未接受的任务（跳过已 accepted/claimed 的）。
//
// 为什么值得做：accept 不产生进度（进度靠行为事件点亮），但让状态机规范
// （not_accepted → accepted → completed），也便于后续筛选"我报过名的任务"。
// 上游 scripts 的注释同样建议"先 accept"。
// 批量提交会分片（上游对 task_codes 数组长度无公开上限，保守每批 20 个）。
//
// 国际版清单型任务（Checklist）直接跳过：它们没有 accept 这一步，提交只会
// 逐条换来 "task not found"。
func (p *Panel) taskAcceptAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	codes, skippedChecklist := acceptCandidates(tasks)
	if len(codes) == 0 {
		msg := "所有任务均已接受"
		if skippedChecklist > 0 {
			msg = "该账号的 " + strconv.Itoa(skippedChecklist) + " 个任务属于「成长路径」清单，" +
				"没有报名环节——需在官方客户端完成对应操作后回来领取"
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": 0, "message": msg})
		return
	}
	const batch = 20
	accepted := 0
	var failed []string
	for i := 0; i < len(codes); i += batch {
		end := i + batch
		if end > len(codes) {
			end = len(codes)
		}
		results, err := p.cfg.Upstream.AcceptTasksDetailed(a, codes[i:end])
		if err != nil {
			log.Printf("panel: 批量接受失败 uid=%s codes=%v err=%v", uid, codes[i:end], err)
			failed = append(failed, codes[i:end]...)
			continue
		}
		for _, res := range results {
			if res.Failed() {
				failed = append(failed, res.TaskCode)
			} else {
				accepted++
			}
		}
		time.Sleep(acceptBatchGap) // 批间节流（对齐脚本 1.05s 口径）
	}
	// 小程序口径任务单独批量接受（默认列表不含 mp 码，accept 也要求 mp 头）。
	if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
		mpCodes, _ := acceptCandidates(mpTasks)
		if len(mpCodes) > 0 {
			results, err := p.cfg.Upstream.AcceptTasksMPDetailed(a, mpCodes)
			if err != nil {
				log.Printf("panel: mp 批量接受失败 uid=%s err=%v", uid, err)
				failed = append(failed, mpCodes...)
			} else {
				for _, res := range results {
					if res.Failed() {
						failed = append(failed, res.TaskCode)
					} else {
						accepted++
					}
				}
				time.Sleep(acceptBatchGap)
			}
		}
	}
	log.Printf("panel: 全部接受 uid=%s 接受=%d 失败=%d 跳过清单=%d", uid, accepted, len(failed), skippedChecklist)
	resp := map[string]any{"ok": true, "accepted": accepted, "failed": failed}
	if len(failed) > 0 {
		resp["message"] = "部分任务接受失败（上游拒绝），可重试"
	}
	writeJSON(w, http.StatusOK, resp)
}

// acceptCandidates 从任务列表挑出可接受的任务码，并统计被跳过的清单型任务数。
//
// 跳过规则：已领取/已完成/已接受、上游锁定、以及国际版清单型（无 accept 步骤）。
func acceptCandidates(tasks []upstream.Task) (codes []string, skippedChecklist int) {
	for _, t := range tasks {
		if t.Checklist {
			skippedChecklist++
			continue
		}
		if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		if strings.TrimSpace(t.TaskCode) == "" {
			continue // 任务码缺失（解析异常）时不提交空串
		}
		codes = append(codes, t.TaskCode)
	}
	return codes, skippedChecklist
}

// checklistSummary 判断任务列表是否为国际版「成长路径」清单型，并数出未完成项。
// 返回 (是否清单型, 未完成数)。
func checklistSummary(tasks []upstream.Task) (bool, int) {
	checklist, pending := false, 0
	for _, t := range tasks {
		if !t.Checklist {
			continue
		}
		checklist = true
		if !t.Claimed && t.Status != "claimed" && t.Status != "completed" {
			pending++
		}
	}
	return checklist, pending
}

// countAcceptResults 统计接受结果：返回 (成功数, 失败数)。
// "already_accepted"（幂等重放）与 "task already completed" 都计入成功。
func countAcceptResults(results []upstream.AcceptResult) (ok, failed int) {
	for _, res := range results {
		if res.Failed() {
			failed++
		} else {
			ok++
		}
	}
	return ok, failed
}

// accountTaskClaim 领取任务奖励（未达标时上游返回业务错误，原样透出给前端提示）。
func (p *Panel) accountTaskClaim(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TaskCode == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	// 小程序口径任务走 chat 域 mp 头领奖（缺头实测不可领）；其余 Web 端接口。
	var credit, energy int64
	var err error
	if isMPTaskCode(body.TaskCode) {
		credit, energy, err = p.cfg.Upstream.ClaimRewardMP(a, body.TaskCode)
	} else {
		credit, energy, err = p.cfg.Upstream.ClaimReward(a, body.TaskCode)
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "claim: "+err.Error())
		return
	}
	if credit == 0 && energy == 0 {
		log.Printf("panel: 领取任务奖励 uid=%s code=%s（已领取过，无新增）", uid, body.TaskCode)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already_claimed": true, "message": "该奖励此前已领取"})
		return
	}
	log.Printf("panel: 领取任务奖励 uid=%s code=%s +%d分 +%d能", uid, body.TaskCode, credit, energy)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credit": credit, "energy": energy})
}
