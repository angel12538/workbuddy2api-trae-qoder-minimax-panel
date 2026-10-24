package ideauth

import (
	"strings"
	"time"
)

// Qoder 切号逐号签到。
//
// 为什么需要切号（2026-10-24 与用户确认 + 实测）：每日签到活动只对「在本机
// Qoder 客户端登录过/正在登录」的账号下发——浏览器里登录的号用 API 带真实设备
// 指纹也扫不到活动。号池里的账号轮流切进客户端（写登录态 + 重启），等客户端
// 起来后用真实设备指纹签到，是让每个号都满足「客户端登录」条件的方式。
//
// 服务端另有硬规则：每台物理设备每日限领一个账号（第一个领取后，同设备其余
// 账号的活动被隐藏）。切号逐签能把每个号都带到"有资格签"的状态，但当日积分
// 只有先签的那个号能拿到——扫描结果里会如实说明。
//
// 流程：遍历号池（跳过今日已有可信记录的号）→ 结束客户端 → 写入该号登录态 →
// 重启客户端 → 等 30s（客户端启动 + deviceToken 刷新）→ 按当前登录签到 →
// 记录 → 全部完成后恢复原先登录的号。

// QoderSwitchResult 切号逐签的单号结果。
type QoderSwitchResult struct {
	Email string `json:"email"`
	Phase string `json:"phase"` // claimed / already / pending / error / skipped
	Msg   string `json:"msg"`
}

// QoderSwitchedCheckinAll 切号逐号签到。logf 为进度回调（可 nil）。
func QoderSwitchedCheckinAll(logf func(string)) []QoderSwitchResult {
	log := func(s string) {
		if logf != nil {
			logf(s)
		}
	}
	pool, err := LoadQoderPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		return nil
	}
	if ClientExePath("qoder") == "" {
		log("未找到 Qoder 客户端可执行文件，无法切号签到")
		return nil
	}
	orig := ""
	if a, err := ReadQoderAuth(); err == nil {
		orig = strings.TrimSpace(a.User.Email)
	}

	out := make([]QoderSwitchResult, 0, len(pool.Accounts))
	for _, snap := range pool.Accounts {
		if snap == nil || snap.Auth == nil {
			continue
		}
		email := strings.TrimSpace(snap.Email)
		if email == "" {
			email = strings.TrimSpace(snap.Auth.User.Email)
		}
		if email == "" {
			continue
		}
		if CheckinRecordTrusted("qoder", email) {
			out = append(out, QoderSwitchResult{Email: email, Phase: "skipped", Msg: "今日已有可信签到记录，跳过"})
			continue
		}
		log("切号签到: " + email)
		if note := KillClient("qoder"); strings.Contains(note, "仍未退出") {
			out = append(out, QoderSwitchResult{Email: email, Phase: "error", Msg: "客户端无法结束（" + note + "）"})
			continue
		}
		if _, err := SwitchQoderTo(email, false); err != nil {
			out = append(out, QoderSwitchResult{Email: email, Phase: "error", Msg: "切号失败: " + err.Error()})
			continue
		}
		LaunchClient("qoder")
		// 客户端启动 + deviceToken 刷新 + 活动下发，留足等待；签不到也会如实回报。
		time.Sleep(30 * time.Second)
		res, cerr := QoderCheckin() // 当前登录路径：真实设备指纹
		phase := "error"
		msg := ""
		if cerr != nil {
			msg = cerr.Error()
		} else if res != nil {
			phase = res.Phase
			msg = res.Msg
		} else {
			msg = "签到返回为空"
		}
		if phase == "claimed" || phase == "already" {
			_ = MarkCheckedIn("qoder", email)
		}
		log("  → " + email + ": " + phase + " " + msg)
		out = append(out, QoderSwitchResult{Email: email, Phase: phase, Msg: msg})
	}

	// 恢复原登录号（它一定在号池里：首次切换前 SwitchQoderTo 会先保存当前登录）。
	if orig != "" {
		log("恢复原登录: " + orig)
		if note := KillClient("qoder"); strings.Contains(note, "仍未退出") {
			log("恢复失败：客户端无法结束（" + note + "）")
		}
		if _, err := SwitchQoderTo(orig, false); err != nil {
			log("恢复失败：" + err.Error())
		} else {
			LaunchClient("qoder")
		}
	}
	return out
}
