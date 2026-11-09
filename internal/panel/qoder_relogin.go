package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// qoderRelogin POST /panel/api/ide/qoder/relogin  {"email":"..."}
//
// 干什么：让号池里**指定的某个号**在 Qoder 客户端里真登录一次。
//
// 为什么需要它（2026-11-09 实测）：每日 100 Credits 的活动是上游按
// 「账号 + 服务端给这个号发过的 agent 租户位(scope)」下发的。一个号如果长期
// 没在客户端里登录过，它的 scope 就过期了 —— 上游不再给它下发当天的每日活动，
// 它的活动列表里只剩早先的促销。此时：
//
//   - 面板的纯 API 签到拿不到活动（claim_campaigns=0 → pending）；
//   - 换设备模式（real/derived）也解��了 —— 活动压根没下发；
//   - 换出口代理同样没用 —— 问题不在请求怎么发，在服务端没给这个号发活动。
//
// 唯一的解法是让这个号在客户端里**真登录一次**（扫码），服务端会重新给它下发
// scope 与当天的活动，之后面板的自动签到就能领到。
//
// 流程：结束客户端 → 把该号自己的 machine-id 恢复回去（设备身份要和当初一致，
// 否则等于换了一台新设备）→ 清掉登录态让客户端回到登录页 → 启动客户端 →
// 进入等待登录状态（复用 qoderLoginSession，登录成功由 qoderLoginPoll 收号池）。
func (p *Panel) qoderRelogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if r != nil && r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	email := strings.TrimSpace(req.Email)
	if email == "" {
		writeErr(w, http.StatusBadRequest, "缺少 email")
		return
	}
	pool, err := ideauth.LoadQoderPool()
	if err != nil || pool == nil {
		writeErr(w, http.StatusInternalServerError, "读号池失败: "+errStr(err))
		return
	}
	found := false
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(a.Email), email) ||
			(a.Auth != nil && strings.EqualFold(strings.TrimSpace(a.Auth.User.Email), email)) {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "号池里没有该账号")
		return
	}

	// 1) 结束客户端（退出时它会把内存里的登录态写回 auth.v1.dat）。
	killNote := ""
	if ideauth.ClientRunning("qoder") {
		killNote = ideauth.KillClient("qoder")
		if strings.Contains(killNote, "仍未退出") {
			writeErr(w, http.StatusBadGateway, "Qoder 客户端无法结束（"+killNote+"），请手动关闭后重试")
			return
		}
		time.Sleep(2 * time.Second)
	}

	// 2) 把该号自己的 machine-id 恢复回去（设备身份必须和当初一致）。
	if _, err := ideauth.SwitchQoderTo(email, true); err != nil {
		writeErr(w, http.StatusBadRequest, "切换到该号失败: "+err.Error())
		return
	}

	// 3) 清掉登录态 → 客户端启动即登录页（用户扫码即可）。
	clearNote, err := qoderClearAuthFile(ideauth.QoderDataDir())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "清理登录态失败: "+err.Error())
		return
	}

	// 4) 进入等待登录状态：登录完成由 qoderLoginPoll 收号池并回读额度。
	qoderLoginSession.mu.Lock()
	before := ""
	if a, err := ideauth.ReadQoderAuth(); err == nil {
		before = strings.TrimSpace(a.User.Email)
	}
	qoderLoginSession.before = before
	qoderLoginSession.startedAt = time.Now()
	qoderLoginSession.done = false
	qoderLoginSession.ok = false
	qoderLoginSession.message = ""
	qoderLoginSession.scope = ""
	qoderLoginSession.independent = false
	qoderLoginSession.detail = ""
	qoderLoginSession.mu.Unlock()

	// 5) 启动客户端。
	launchNote := ideauth.LaunchClient("qoder")
	log.Printf("panel: Qoder 重新登录 %s：%s；%s；%s", email, killNote, clearNote, launchNote)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "email": email, "running": true,
		"restart_note": killNote + "；" + clearNote + "；" + launchNote,
		"message": "已启动 Qoder 客户端 —— 请在客户端里用「" + email + "」扫码登录一次；" +
			"服务端会重新给这个号下发当天的每日活动，登录完面板自动收录并补签",
	})
}
