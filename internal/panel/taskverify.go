// taskverify.go 「一键完成」的真实回读判定与汇总（C-WB-AUTO@1）。
//
// 背景（W4/W5）：「一键完成」曾经在动作函数返回 nil 后**无条件**写
// status=done——动作不报错就标完成，哪怕上游进度一点没动。用户看到的是
// 「全部已完成」，转头在任务中心却仍有待办，也就是所谓的「一键完成幻觉」。
//
// 本文件把「如何判定完成」收敛为唯一实现：
//   - classifyAutoOutcome：动作执行后必须回读上游任务状态，只有达标才 done；
//   - autoAllSummary：汇总只统计真实任务（kind=control 的过程条目不计入）。
//
// 契约冻结 @1：状态词表、字段名、verified 语义不得改动。
package panel

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 一键完成/队列共用的状态词表（C-WB-AUTO@1 / C-WB-QUEUE@1 冻结）。
const (
	AutoStatusDone       = "done"       // 动作成功且回读确认达标（verified=true）
	AutoStatusPartial    = "partial"    // 动作成功、进度有变化但未达标
	AutoStatusUnverified = "unverified" // 动作无错误，但进度未变化或回读失败——未证实完成
	AutoStatusFailed     = "failed"     // 动作本身报错（或领奖失败）
	AutoStatusSkipped    = "skipped"    // 未执行（已领取/该账号无此任务等）
	AutoStatusLocked     = "locked"     // 上游锁定，未执行
	AutoStatusManual     = "manual"     // 面板无自动动作，需真实操作
)

// autoSummaryStatuses summary 的固定键（顺序仅用于构造/测试，不影响 JSON）。
var autoSummaryStatuses = []string{
	AutoStatusDone,
	AutoStatusPartial,
	AutoStatusUnverified,
	AutoStatusSkipped,
	AutoStatusLocked,
	AutoStatusManual,
	AutoStatusFailed,
}

// classifyAutoOutcome 一键完成/执行队列的唯一完成判定。
//
// 参数：
//   - before：动作前读取到的任务快照（可为 nil —— 读取失败或任务当时不存在）；
//   - after：动作后**重新拉取**的任务状态；回读失败或任务消失时为 nil；
//   - runErr：动作函数返回的错误。
//
// 规则（与 C-WB-AUTO@1 2.2.16 表逐条对应，顺序不可变）：
//  1. runErr != nil                    → failed,     verified=false
//  2. after == nil                     → unverified, verified=false
//  3. after.Claimed                    → done,       verified=true
//  4. after.Target>0 && Current>=Target→ done,       verified=true
//  5. after.Target==0 且 status 建成   → done,       verified=true
//  6. 进度键较 before 变化但未达标      → partial,    verified=false
//  7. 进度键无变化且未达标              → unverified, verified=false
//
// 关键约束：**done 必须 verified=true**；没有成功回读时绝不允许写 done。
// before==nil 且 after 未达标时归入 unverified —— 没有基线就谈不上「进度变化」，
// 不能凭任务存在就宣称部分完成。
func classifyAutoOutcome(before, after *upstream.Task, runErr error) (status string, verified bool) {
	if runErr != nil {
		return AutoStatusFailed, false
	}
	if after == nil {
		// 回读失败或任务从列表消失：动作可能执行了，但没有任何证据证明达标。
		return AutoStatusUnverified, false
	}
	if after.Claimed {
		return AutoStatusDone, true
	}
	if after.Target > 0 && after.Current >= after.Target {
		return AutoStatusDone, true
	}
	if after.Target == 0 && autoTaskCompletedStatus(after) {
		return AutoStatusDone, true
	}
	if autoProgressChanged(before, after) {
		return AutoStatusPartial, false
	}
	return AutoStatusUnverified, false
}

// autoTaskCompletedStatus 目标条数为 0 的任务是否已由上游状态字面宣布完成。
// 只认契约列出的三个取值，避免把任意 "success" 之类词当成完成。
func autoTaskCompletedStatus(t *upstream.Task) bool {
	switch t.AcceptStatus {
	case "completed":
		return true
	}
	switch t.Status {
	case "complete", "claimed":
		return true
	}
	return false
}

// autoProgressChanged 进度键（current/target/accept_status/status）是否变化。
// before 为空时返回 false：没有基线就无法证明「动了」。
func autoProgressChanged(before, after *upstream.Task) bool {
	if before == nil || after == nil {
		return false
	}
	if before.Current != after.Current || before.Target != after.Target {
		return true
	}
	return before.AcceptStatus != after.AcceptStatus || before.Status != after.Status
}

// autoAllSummary 汇总一键完成结果（C-WB-AUTO@1）。
//
// 语义：
//   - 只统计**真实任务**：带 kind="control" 的过程条目（「(批量接受)」/
//     「(批量接受-mp)」/「(任务列表)」等）不计入 total 与各状态计数；
//   - total == done+partial+unverified+skipped+locked+manual+failed；
//   - 未知状态（历史遗留 "error" 等）归入 failed，保证等式恒成立。
func autoAllSummary(results []map[string]any) map[string]any {
	summary := map[string]any{"total": 0}
	counts := make(map[string]int, len(autoSummaryStatuses))
	for _, s := range autoSummaryStatuses {
		counts[s] = 0
	}
	for _, r := range results {
		if kind, _ := r["kind"].(string); kind == "control" {
			continue
		}
		status, _ := r["status"].(string)
		if _, known := counts[status]; !known {
			status = AutoStatusFailed
		}
		counts[status]++
	}
	total := 0
	for _, s := range autoSummaryStatuses {
		summary[s] = counts[s]
		total += counts[s]
	}
	summary["total"] = total
	return summary
}

// autoAllDoneVerified 结果集中每个 done 是否都带 verified=true。
// 供测试与自检使用：任何「未证实却被标完成」的条目都必须被揪出来。
func autoAllDoneVerified(results []map[string]any) bool {
	for _, r := range results {
		if status, _ := r["status"].(string); status != AutoStatusDone {
			continue
		}
		if verified, _ := r["verified"].(bool); !verified {
			return false
		}
	}
	return true
}
