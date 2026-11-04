package panel

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// TestCodeartsQRExpired 文案识别：二维码失效的几种说法都要认出来。
func TestCodeartsQRExpired(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"欢迎使用华为云 去注册 二维码已失效 刷新 华为云APP", true},
		{"二维码已过期", true},
		{"扫码已失效", true},
		{"欢迎使用华为云 手机号登录 密码登录", false},
		{"", false},
	}
	for _, c := range cases {
		if got := codeartsQRExpired(c.text); got != c.want {
			t.Fatalf("codeartsQRExpired(%q)=%v，期望 %v", c.text, got, c.want)
		}
	}
}

// TestCodeartsNicknameOf 从 Cookie 里取昵称。
func TestCodeartsNicknameOf(t *testing.T) {
	cs := []codearts.Cookie{
		{Name: "SessionID", Value: "x"},
		{Name: "masked_user", Value: "h*********uvj421wxe"},
	}
	got := codeartsNicknameOf(cs)
	if got != "h*********uvj421wxe" {
		t.Fatalf("昵称取错: %q", got)
	}
	if codeartsNicknameOf(nil) != "" {
		t.Fatal("空 Cookie 应返回空昵称")
	}
}

// TestCodeartsLoginMessage 扫码状态 → 能照做的提示。
//
// 真实现场（2026-11-04 11:5x，只读诊断）：页面文案「二维码已失效 刷新」，
// authui 的 /authui/qrcode/status 回 status="expired"，控制台会话实测不通。
// 旧实现只认 new/authorizing/authorized/427 → 这种状态会落到笼统的
// "还没检测到华为云登录态"，用户完全看不到卡在哪。
func TestCodeartsLoginMessage(t *testing.T) {
	cases := []struct {
		name string
		qr   codeartsQRState
		text string
		want string
	}{
		{"expired状态", codeartsQRState{Known: true, Status: "expired"}, "欢迎使用华为云 去注册 二维码已失效 刷新", "失效"},
		{"文案判失效", codeartsQRState{Known: true, Status: "new"}, "二维码已失效 刷新", "失效"},
		{"错误码427", codeartsQRState{Known: true, Status: "new", ErrorCode: "427"}, "", "失效"},
		{"待授权", codeartsQRState{Known: true, Status: "authorizing"}, "", "授权"},
		{"新码未扫", codeartsQRState{Known: true, Status: "new"}, "欢迎使用华为云 扫码登录", "扫描"},
		{"已授权", codeartsQRState{Known: true, Status: "authorized", RedirectURL: "https://x/y"}, "", ""},
		{"不在扫码流程", codeartsQRState{Known: false}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := codeartsLoginMessage(c.qr, c.text)
			if c.want == "" {
				if got != "" {
					t.Fatalf("该给默认文案，却返回了 %q", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("提示应包含 %q，实际 %q", c.want, got)
			}
		})
	}
}
