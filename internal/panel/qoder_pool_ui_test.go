package panel

import (
	"os"
	"strings"
	"testing"
)

// Qoder 多账号签到 UI 回归测试。
//
// 背景：签到池曾只渲染「当前登录」一个 Qoder 账号，号池里第二个号永远没有签到入口。
// 这里锁死三件事，防止以后改回去：
//   1) 签到池按号池全量渲染（每行一个签到按钮，data-ck-qoder 带 email）；
//   2) 存在「全部签到」按钮，且调用时不带 email（= 号池全部）；
//   3) 每行额度按账号查询（usage?email=），不再只查当前号。
func TestQoderCheckinUIListsAllAccounts(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	body := grabJSFunc(t, src, "loadCkQoder")
	if !strings.Contains(body, "rows.map(") {
		t.Error("loadCkQoder 应按号池全量渲染（rows.map）")
	}
	// 属性名由 UI.act(...) 生成（渲染结果仍是 data-ck-qoder="…"），
	// 所以这里断言属性名本身，而不是字面量写法。
	if !strings.Contains(body, "data-ck-qoder") {
		t.Error("每行应有独立签到按钮（data-ck-qoder 带账号）")
	}
	if !strings.Contains(body, "usage?email=") {
		t.Error("积分应按账号查询（usage?email=）")
	}
	if strings.Contains(body, `$('ckQoderSummary').textContent = cur ?`) {
		t.Error("汇总文案又退回成「只显示当前账号」了")
	}

	// 签到函数必须支持按账号，且默认=全部
	ck := grabJSFunc(t, src, "doQoderCheckin")
	if !strings.Contains(ck, "emails") {
		t.Error("doQoderCheckin 应支持传 emails（按账号/全部）")
	}

	// 全部签到按钮
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="btnCkQoderCheckinAll"`) {
		t.Error("index.html 缺少「全部签到」按钮")
	}

	// 积分总览也应列全部账号
	qk := grabJSFunc(t, src, "loadQkCredits")
	if !strings.Contains(qk, "rows.map(") || !strings.Contains(qk, "usage?email=") {
		t.Error("积分总览应列全部 Qoder 账号并逐号查额度")
	}
}
