package panel

import (
	"os"
	"strings"
	"testing"
)

// TestCheckinPoolLayoutAligned 签到池 UI 对齐回归测试（2026-11-10）。
//
// 背景：用户反馈六块签到卡片（WorkBuddy / Qoder / MiniMax / CodeArts / 豆包 / Trae）
// 统计行、表头、按钮各不相同 —— WorkBuddy 缺「全部签到」按钮、表头第二列是「版本」
// 而不是「积分」；豆包只有三列；CodeArts 连表头都没有；右下角积分总览四张表
// 列宽各不相同，看着就不齐。
//
// 这里锁死：
//  1. 六个平台的表头统一为「账号 | 积分 | 状态 | 操作」；
//  2. WorkBuddy 卡片有「全部签到」按钮，并显示积分列（不再拿"版本"占位）；
//  3. 积分总览所有表都用 ckfix（固定列宽），且新增 MiniMax 块；
//  4. MiniMax 卡片显示「余额 / 总量」（"总量还剩多少"）；
//  5. Trae 卡片显示的是**真实余额**，签到额度另标。
func TestCheckinPoolLayoutAligned(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	// ① 六个平台的表头统一。
	// Qoder 的渲染体在 renderQoderRows（loadCkQoder 已拆成两阶段）。
	const head4 = "<div class=\"thead\"><div>账号</div><div>积分</div><div>状态</div><div>操作</div></div>"
	for _, fn := range []string{
		"loadCkWorkBuddy", "renderQoderRows", "loadCkMiniMax",
		"loadCkDoubao", "loadCkCodeArts", "loadTraeStatus",
	} {
		body := grabJSFunc(t, src, fn)
		if !strings.Contains(body, head4) {
			t.Errorf("%s 的表头应为统一的「账号|积分|状态|操作」", fn)
		}
	}

	// ② WorkBuddy：卡片上要有「全部签到」按钮（index.html）+ 积分列。
	wb := grabJSFunc(t, src, "loadCkWorkBuddy")
	if !strings.Contains(wb, "data-wb-ck-cred") {
		t.Error("WorkBuddy 卡片应显示积分列（data-wb-ck-cred）")
	}
	if strings.Contains(wb, "<div>版本</div>") {
		t.Error("WorkBuddy 表头第二列应是「积分」而不是「版本」")
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	hs := string(html)
	if !strings.Contains(hs, `id="btnCkWbCheckinAll"`) {
		t.Error("WorkBuddy 卡片应有「全部签到」按钮（btnCkWbCheckinAll）")
	}
	if !strings.Contains(src, "$('btnCkWbCheckinAll').onclick") {
		t.Error("btnCkWbCheckinAll 应绑定点击事件")
	}

	// ③ 积分总览：所有表用 ckfix 固定列宽；MiniMax 块要有。
	if !strings.Contains(hs, `id="mmCrBody"`) {
		t.Error("积分总览应有 MiniMax 块（mmCrBody）")
	}
	if !strings.Contains(src, "async function loadMmCredits") {
		t.Error("应有 loadMmCredits 渲染 MiniMax 积分块")
	}
	// 积分总览里的表统一加 ckfix（数量应为 5：Trae/Qoder/WorkBuddy/MiniMax/CodeArts）。
	if n := strings.Count(hs, `<table class="acc ckfix">`); n != 5 {
		t.Errorf("积分总览应有 5 张 ckfix 固定列宽的表，实际 %d 张", n)
	}

	// ④ MiniMax 显示「总量还剩多少」。
	mm := grabJSFunc(t, src, "loadCkMiniMax")
	if !strings.Contains(mm, "credits_total") {
		t.Error("MiniMax 卡片应显示 credits_total（总量还剩多少）")
	}
	mmc := grabJSFunc(t, src, "loadMmCredits")
	if !strings.Contains(mmc, "credits_total") {
		t.Error("MiniMax 积分总览应显示 credits_total（总量）")
	}

	// ⑤ Trae 卡片显示真实余额（不是签到额度 100）。
	tr := grabJSFunc(t, src, "loadTraeStatus")
	if !strings.Contains(tr, "trae/credits") {
		t.Error("Trae 卡片应并发取 trae/credits 显示真实余额")
	}
	if !strings.Contains(tr, "账号积分余额") {
		t.Error("Trae 卡片积分列应标明是账号余额")
	}
	if !strings.Contains(tr, "今日签到可得") {
		t.Error("Trae 卡片应把签到额度单独标出来")
	}
}
