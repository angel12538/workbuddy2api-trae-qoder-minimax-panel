package panel

import (
	"os"
	"strings"
	"testing"
)

// 回归：签到按钮的「已签到」外观必须全站一致。
//
// 用户连着两次反馈同一个问题：
//   "已签到为什么有的是灰色按钮，有的是蓝色按钮？"
//   "签完到的就变成已签到，后面那个按钮应该变成灰色，而不是现在的蓝色"
//
// 根因：同一个动作（签到）在不同平台渲染成了不同的样子 —— 有的用
// `class="xs" disabled` + 文案「已签到」，有的用 `class="xs primary"` +
// 文案「已签到」（蓝色却写着已签到），还有的用 primary + 文案「签到」。
//
// 规则（UI 工厂注释里也写了）：
//   · 还没签到 → 蓝底 `xs primary`「签到」，可点
//   · 已经签到 → 灰底 `xs` + disabled「已签到」
//   · 状态徽章另说（徽章讲上游核实的细节，按钮管能不能点）
func TestCheckinButtonStyleIsUniform(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	// 1) 不能再有「primary + 已签到」这种自相矛盾的组合。
	for _, bad := range []string{
		`class="xs primary" data-ck-wb`,
		`class="xs primary" data-ck-qoder`,
		`class="xs primary" data-mm-ck`,
		`class="xs primary" data-db-ck`,
		`class="xs primary" data-trae-uid`,
	} {
		if strings.Contains(src, bad) {
			t.Errorf("签到按钮不应直接写死 class=\"xs primary\"：%s（已签到必须是灰底禁用）", bad)
		}
	}
	// 2) 也不能再用「同一个按钮两种文案」的三元写法（'已签到' : '签到'）。
	if strings.Contains(src, "'已签到' : '签到'") || strings.Contains(src, "isDone ? '已签到'") {
		t.Error("签到按钮文案不应再用三元切换（已签到/签到）—— 已签到走灰底禁用分支，未签走蓝底分支")
	}
	// 3) 每个签到池都要有「灰底已签到」分支：UI.off('已签到', …)。
	//    WorkBuddy / Qoder / MiniMax / 豆包 / Trae 共 5 处。
	if n := strings.Count(src, "UI.off('已签到'"); n < 5 {
		t.Errorf("应有 5 处灰底「已签到」分支（WorkBuddy/Qoder/MiniMax/豆包/Trae），实际 %d 处", n)
	}
	// 4) 每个签到池都要有「蓝底签到」分支：UI.act(…, '签到', …)。
	if n := strings.Count(src, "'签到',"); n < 5 {
		t.Errorf("应有 5 处蓝底「签到」分支（每个签到池一个），实际 %d 处", n)
	}
	// 5) 工厂本身：主操作蓝底、次操作灰底、禁用灰底。
	for _, want := range []string{
		`class="xs primary"`,
		`class="xs" `,
		`disabled`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("UI 工厂缺少 %q", want)
		}
	}
}
