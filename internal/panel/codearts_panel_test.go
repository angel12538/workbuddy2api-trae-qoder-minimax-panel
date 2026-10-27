package panel

import "testing"

// 实测（未登录时点开「活动福利」）：面板底部出现
//
//	解锁更多福利 / 登录即可解锁更多限时福利。/ 立即登录
//
// 而首页的「立即参与」会变成「登录参与」。登录后才解锁「每日领取」。
//
// 这个判据很重要：登录态失效时，面板**照样能点开**，只是里面没有签到按钮。
// 如果不识别这个状态，签到会报「没找到入口」，把「登录过期」说成「页面改版」，
// 用户根本不知道该干什么。
func TestCaPanelNeedsLogin(t *testing.T) {
	cases := []struct {
		name  string
		hints []string
		want  bool
	}{
		{"未登录面板", []string{"立即登录", "解锁更多福利", "登录即可解锁更多限时福利。"}, true},
		{"未登录参与按钮", []string{"登录参与", "活动福利"}, true},
		{"已登录面板", []string{"每日领取", "今日已领", "做任务赚积分"}, false},
		{"空", nil, false},
	}
	for _, c := range cases {
		if got := caPanelNeedsLogin(c.hints); got != c.want {
			t.Errorf("%s: caPanelNeedsLogin = %v, want %v", c.name, got, c.want)
		}
	}
}
