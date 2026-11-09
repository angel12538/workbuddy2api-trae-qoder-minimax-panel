package ideauth

import "strings"

// QoderClientAuthFor 返回**客户端当前登录态**（当客户端登录的正是指定账号时）。
//
// 为什么需要它（2026-11-09 用户反馈：「软件里有签到任务、我已经在软件里领了，
// 面板却显示今日未下发」）：
//
//	用户在 Qoder 软件里登录/领额度时，客户端会自己刷新出一份**新的会话**
//	（新的 device_token + 新的 scope）。这份新会话只存在客户端的
//	auth.v1.dat 里；号池里那份快照还停在旧 token 上。
//	面板拿旧 token 去问上游活动列表 —— 上游对「认不出来的旧会话」一律回
//	200 + 空 campaigns（不是 401！），于是面板得出「今天没给这个号下发活动」，
//	而软件里的同一个号明明有任务、也已经领了。
//
//	所以签到前先看一眼客户端：如果客户端登录的就是这个号，就用客户端那份
//	（更新的）凭证，并把新 token 写回号池，让两边对齐。
//
// 语义要点：
//   - 只在**账号完全相同**时返回（大小写不敏感）；
//   - 客户端没登录 / 解不开登录态 / 账号不同 → 返回 nil，调用方按原逻辑走；
//   - 读不到不是错误，不抛异常 —— 这只是"锦上添花"的加速路径。
func QoderClientAuthFor(email string) *QoderAuth {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil
	}
	a, err := ReadQoderAuth()
	if err != nil || a == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(a.User.Email), email) {
		return nil
	}
	if strings.TrimSpace(a.Token) == "" {
		return nil
	}
	return a
}
