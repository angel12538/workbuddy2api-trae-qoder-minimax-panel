// qoder_login.go Qoder「浏览器登录并添加账号」。
//
// 为什么不是 WorkBuddy/MiniMax 那种"授权链接"：
// Qoder 的可用登录态**只存在于桌面客户端**（auth.v1.dat，DPAPI + AES-256-GCM 加密），
// 网页端登录拿不到 IDE 能用的令牌 —— 实测网页端只有
//
//	POST /api/v1/sessions            （邮箱 + 密码 + CSRF）
//	POST /api/v1/auth/check-login-type（邮箱 + oauth_callback）
//	/sso/login/…                     （网页版自己的 SSO，组织级）
//
// 而 IDE 用的是 openapi.qoder.sh 上 `dt-` 开头的 access token，两者不通。
//
// 所以这里的「浏览器登录」= **启动客户端 → 在客户端里点浏览器登录/扫码完成鉴权 →
// 面板盯着客户端登录态，一变就自动收进号池**。全程在浏览器里鉴权。
//
// ⚠️ 还有一个更关键的点：**必须走一次真正的登录**，不能用面板直接写登录态
// （切号）代替。原因：服务端在**登录时**给账号发一个 `agentAccountScope`
// （"这个人"的身份标识）。面板写登录态不会触发重新签发，账号就沿用上一个
// 账号的 scope —— 于是两个号被服务端算成**同一个人**，每天只能领一份
// 100 积分（实测另一个号去领会被回 SAME_PERSON_ALREADY_CLAIMED）。
// 走一次真登录，服务端会按这个账号自己的身份重新发 scope，它才有机会成为
// 独立的"人"、独立领自己那份。
//
// 因此本文件提供两个动作：
//   - start（logout=false）：客户端已登录时，直接在客户端里登录/切换即可
//   - start（logout=true） ：先退出当前账号（备份登录态），让客户端回到登录页，
//     你再用浏览器登录**另一个**账号 —— 服务端会给它发属于它自己的 scope。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// qoderLoginTTL 等待登录的最长时间（客户端启动 + 用户在浏览器里鉴权可能要一会儿）。
const qoderLoginTTL = 20 * time.Minute

// qoderLoginSession 一次「等客户端登录完成」的进行态。
var qoderLoginSession struct {
	mu        sync.Mutex
	before    string // 开始时的登录账号（空 = 当时未登录）
	startedAt time.Time
	done      bool
	ok        bool
	message   string
	// identity 结果：该号登录后服务端给的"人"标识与是否独立。
	scope       string
	independent bool
	detail      string
}

// qoderLoginStart 启动 Qoder 客户端，开始等待登录完成。
//
// body: {"logout":true} —— 先退出当前账号（备份登录态），让客户端回到登录页。
func (p *Panel) qoderLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Logout bool `json:"logout"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	qoderLoginSession.mu.Lock()
	if qoderLoginSession.startedAt.IsZero() || qoderLoginSession.done {
		qoderLoginSession.before = ""
		if a, err := ideauth.ReadQoderAuth(); err == nil {
			qoderLoginSession.before = strings.TrimSpace(a.User.Email)
		}
		qoderLoginSession.startedAt = time.Now()
		qoderLoginSession.done = false
		qoderLoginSession.ok = false
		qoderLoginSession.message = ""
		qoderLoginSession.scope = ""
		qoderLoginSession.independent = false
		qoderLoginSession.detail = ""
	}
	qoderLoginSession.mu.Unlock()

	if req.Logout {
		if note, err := qoderLogoutClient(); err != nil {
			writeErr(w, http.StatusBadGateway, "退出当前账号失败: "+err.Error())
			return
		} else {
			log.Printf("panel: Qoder 浏览器登录：已退出当前账号（%s）", note)
		}
	}

	note := ""
	if ideauth.ClientRunning("qoder") {
		note = "Qoder 客户端已在运行"
	} else {
		note = ideauth.LaunchClient("qoder")
	}
	log.Printf("panel: Qoder 浏览器登录：等待客户端登录完成（%s）", note)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "running": true, "note": note,
		"message": "已启动 Qoder 客户端 —— 请在客户端里点「浏览器登录 / 扫码登录」完成鉴权，登录成功后面板自动收录这个号",
	})
}

// qoderLogoutClient 退出客户端当前账号，让客户端回到登录页。
//
// 三个坑（都踩过）：
//  1. **必须先结束客户端**：它退出时会把内存里的登录态写回 auth.v1.dat，
//     不先杀掉的话我们的"登出"会被它覆盖回去。
//  2. **不能写占位符**：此前往 auth.v1.dat 写了个 3 字节的 "v10"，结果解密直接
//     报 "body too short: 0"，面板和客户端都读不了这个文件。正确做法是**删掉它**
//     （= 全新安装的状态），客户端启动自然回到登录界面。
//  3. **只备份有效的登录态**：坏文件再备份一遍只会攒一串垃圾。
func qoderLogoutClient() (string, error) {
	// 1) 先结束客户端（它退出时可能把登录态写回来）。
	if ideauth.ClientRunning("qoder") {
		_ = ideauth.KillClient("qoder")
		time.Sleep(2 * time.Second)
	}
	return qoderClearAuthFile(ideauth.QoderDataDir())
}

// qoderClearAuthFile 清掉（并按需备份）Qoder 的登录态文件。
//
// 单独拆出来是为了能单测：它只动文件、不碰进程。
func qoderClearAuthFile(dir string) (string, error) {
	authPath := filepath.Join(dir, "auth.v1.dat")

	// 只在当前登录态**有效**时备份一份（坏文件再备份一遍只会攒垃圾）。
	note := "已退出当前账号"
	if raw, err := os.ReadFile(authPath); err == nil {
		if _, derr := ideauth.ReadQoderAuth(); derr == nil {
			bakDir := ideauth.PoolDir()
			if err := os.MkdirAll(bakDir, 0o700); err == nil {
				bak := filepath.Join(bakDir, "qoder_auth_backup_"+time.Now().Format("20060102-150405")+".dat")
				if err := os.WriteFile(bak, raw, 0o600); err == nil {
					note = "已退出当前账号（登录态备份到 " + filepath.Base(bak) + "）"
				}
			}
		} else {
			note = "已清理损坏的登录态（原文件本来就无法解密）"
		}
	}

	// 删掉登录态文件（= 全新安装状态）→ 客户端启动即登录界面。
	// ⚠️ 绝不能写 3 字节的 "v10" 占位符：那样解密会报 "body too short: 0"，
	// 面板和客户端都读不了这个文件（2026-10-24 实测踩过）。
	if err := os.Remove(authPath); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return note, nil
}

// qoderLoginPoll 轮询客户端登录态；账号变了就收进号池，并立刻回读它
// 在服务端是不是一个**独立的"人"**（有没有自己的 scope / 自己的每日额度）。
func (p *Panel) qoderLoginPoll(w http.ResponseWriter, r *http.Request) {
	qoderLoginSession.mu.Lock()
	if qoderLoginSession.startedAt.IsZero() {
		qoderLoginSession.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "还没开始，请点「浏览器登录并添加」"})
		return
	}
	if qoderLoginSession.done {
		res := map[string]any{
			"done": qoderLoginSession.done, "ok": qoderLoginSession.ok,
			"message": qoderLoginSession.message, "scope": qoderLoginSession.scope,
			"independent": qoderLoginSession.independent, "detail": qoderLoginSession.detail,
		}
		qoderLoginSession.mu.Unlock()
		writeJSON(w, http.StatusOK, res)
		return
	}
	if time.Since(qoderLoginSession.startedAt) > qoderLoginTTL {
		qoderLoginSession.done = true
		qoderLoginSession.ok = false
		qoderLoginSession.message = "等待登录超时，请重新开始"
		qoderLoginSession.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "expired": true, "message": qoderLoginSession.message})
		return
	}
	before := qoderLoginSession.before
	qoderLoginSession.mu.Unlock()

	a, err := ideauth.ReadQoderAuth()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待 Qoder 客户端完成登录…"})
		return
	}
	email := strings.TrimSpace(a.User.Email)
	if email == "" || email == before {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "等待 Qoder 客户端完成登录…"})
		return
	}
	_, added, err := ideauth.UpsertQoderAccount()
	verb := "已添加"
	if err == nil && !added {
		verb = "已更新"
	}
	msg := verb + "：" + email
	if err != nil {
		msg = "登录已成功，但写入号池失败：" + err.Error()
	}

	// 立刻回读这个号在上游的身份与今日额度状态：
	//   phase=claimable → 它已经是独立的"人"，今天这份 100 积分在它名下
	//   phase=claimed   → 它今天已经领过（自己领的）
	//   phase=pending + device_quota → 仍与另一个号共用同一个"人"
	scope := ""
	if s, err := ideauth.QoderScope(); err == nil {
		scope = s
	}
	independent := false
	detail := ""
	if err == nil {
		for _, row := range ideauth.QoderPoolScan([]string{email}) {
			switch row.Phase {
			case "claimable":
				independent = true
				detail = "这个号现在是独立身份，今天的 100 积分在它名下（回上游核实：可领取）"
			case "claimed":
				independent = true
				detail = "这个号已是独立身份，且今天已经领过"
			default:
				detail = row.Msg
				if row.DeviceQuotaUsed {
					detail = "仍与号池里另一个号被服务端算作同一个人，今天这份额度已被对方领走"
				}
			}
		}
	}

	qoderLoginSession.mu.Lock()
	qoderLoginSession.done = true
	qoderLoginSession.ok = err == nil
	qoderLoginSession.message = msg
	qoderLoginSession.scope = scope
	qoderLoginSession.independent = independent
	qoderLoginSession.detail = detail
	qoderLoginSession.mu.Unlock()
	if err != nil {
		log.Printf("panel: Qoder 浏览器登录后写入号池失败: %v", err)
	} else {
		log.Printf("panel: Qoder 浏览器登录成功：%s（scope=%s 独立身份=%v）%s", msg, scope, independent, detail)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true, "ok": err == nil, "message": msg,
		"email": email, "added": added,
		"scope": scope, "independent": independent, "detail": detail,
	})
}
