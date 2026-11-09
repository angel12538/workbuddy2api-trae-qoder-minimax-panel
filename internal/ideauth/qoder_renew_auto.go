package ideauth

import (
	"fmt"
	"time"
)

// QoderRenewSnapshotAuth 给号池里的某个账号续期凭证（refresh_token 换新 token）。
//
// 【为什么要有它】
//   以前 Qoder 只有「手动续期」按钮，而且 QoderRenew 只续**当前登录**那一个号。
//   号池里其它号的 token 没人管 —— 过期了就只能等签到时撞上 401 才现续，
//   那一次签到就废了（表现为「某些号老是签不上」）。
//   面板的后台续期循环（StartQoderRenewAuto）会定期调这里，把临期的号提前续上。
//
// 【返回值】
//   renewed  = 真的换了新 token
//   expiresAt= 服务端下发的新到期时间（RFC3339，续期失败时是原值）
//   err      = 失败原因（调用方如实记日志即可）
//
// ⚠ 调用方负责把续出来的新 token 写回号池（走 UpdateQoderPool），
//   本函数只改传入的快照指针 —— 避免在包内再拿一次号池锁（不可重入会死锁）。
func QoderRenewSnapshotAuth(snap *QoderSnapshot) (renewed bool, expiresAt string, err error) {
	if snap == nil || snap.Auth == nil {
		return false, "", fmt.Errorf("账号快照为空")
	}
	auth := snap.Auth
	if auth.RefreshToken == "" {
		return false, auth.ExpiresAt, fmt.Errorf("缺少 refresh token，需要重新登录 Qoder")
	}
	dev := qoderDeviceForAccount(auth)

	scope := snap.Scope
	if scope == "" {
		scope = currentQoderScope()
	}
	lastErr := ""
	for _, base := range qoderOpenAPIBaseListForScope(scope) {
		at, rt, exp, rtexp, ok := qoderRefreshTokenFull(auth, base, dev)
		if !ok {
			lastErr = "刷新失败 @ " + base
			continue
		}
		// refresh_token 会被轮换，必须把返回值写回，否则下次续期会失败。
		auth.Token = at
		auth.RefreshToken = rt
		if exp != "" {
			auth.ExpiresAt = exp
		}
		if rtexp != "" {
			auth.RefreshTokenExpiresAt = rtexp
		}
		return true, auth.ExpiresAt, nil
	}
	if lastErr == "" {
		lastErr = "无可用的 openapi base"
	}
	return false, auth.ExpiresAt, fmt.Errorf("%s", lastErr)
}

// QoderSnapshotNeedsRenew 判断号池某个账号是否该续期。
//
// 判据：没有到期时间（= 还没观测过）或者剩余有效期不足 margin。
// margin 默认 24 小时 —— token 通常是几天到 15 天有效期，
// 留 24 小时余量足够扛住一次网络抖动或一次进程重启。
func QoderSnapshotNeedsRenew(snap *QoderSnapshot, margin time.Duration) bool {
	if snap == nil || snap.Auth == nil {
		return false
	}
	if snap.Auth.RefreshToken == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, snap.Auth.ExpiresAt)
	if err != nil {
		// 没有到期信息 = 不知道，只能按"该续"处理（续期本身很便宜）。
		return true
	}
	return time.Now().Add(margin).After(t)
}
