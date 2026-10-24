// qoder_login.go Qoder「浏览器登录并添加账号」。
//
// 为什么不是 WorkBuddy/MiniMax 那种"授权链接"：
// Qoder 的可用登录态**只存在于桌面客户端**（auth.v1.dat，DPAPI + AES-256-GCM 加密），
// 网页端登录拿不到 IDE 能用的令牌 —— 实测网页端只有
//   POST /api/v1/sessions            （邮箱 + 密码 + CSRF）
//   POST /api/v1/auth/check-login-type（邮箱 + oauth_callback）
//   /sso/login/…                     （网页版自己的 SSO，组织级）
// 而 IDE 用的是 openapi.qoder.sh 上的 `dt-` 开头 access token，与网页 session 不是
// 同一套凭据，无法直接换。
//
// 所以这里的「浏览器登录」= **启动 Qoder 客户端 → 在客户端里点浏览器登录/扫码**
// （Qoder 自己的登录界面会跳浏览器完成鉴权）→ **面板盯着客户端的登录态**，一旦
// 登录成功（auth.v1.dat 里的账号发生变化）就自动收进号池。
//
// 好处：面板不需要知道客户端的登录页 URL、不需要切号、也不需要用户手工粘贴任何
// 东西；用户全程在浏览器里完成鉴权。
package panel

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// qoderLoginTTL 等待登录的最长时间（客户端启动 + 用户在浏览器里鉴权可能要一会儿）。
const qoderLoginTTL = 15 * time.Minute

// qoderLoginSession 一次「等客户端登录完成」的进行态。
var qoderLoginSession struct {
	mu        sync.Mutex
	before    string // 开始时的登录账号（空 = 当时未登录）
	startedAt time.Time
	done      bool
	ok        bool
	message   string
}

// qoderLoginStart 启动 Qoder 客户端，开始等待登录完成。
func (p *Panel) qoderLoginStart(w http.ResponseWriter, r *http.Request) {
	qoderLoginSession.mu.Lock()
	defer qoderLoginSession.mu.Unlock()
	if qoderLoginSession.startedAt.IsZero() || qoderLoginSession.done {
		qoderLoginSession.before = ""
		if a, err := ideauth.ReadQoderAuth(); err == nil {
			qoderLoginSession.before = strings.TrimSpace(a.User.Email)
		}
		qoderLoginSession.startedAt = time.Now()
		qoderLoginSession.done = false
		qoderLoginSession.ok = false
		qoderLoginSession.message = ""
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

// qoderLoginPoll 轮询客户端登录态；账号变了就收进号池。
func (p *Panel) qoderLoginPoll(w http.ResponseWriter, r *http.Request) {
	qoderLoginSession.mu.Lock()
	if qoderLoginSession.startedAt.IsZero() {
		qoderLoginSession.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "还没开始，请在客户端里点「浏览器登录并添加」"})
		return
	}
	if qoderLoginSession.done {
		res := map[string]any{"done": true, "ok": qoderLoginSession.ok, "message": qoderLoginSession.message}
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
	snap, added, err := ideauth.UpsertQoderAccount()
	verb := "已添加"
	if err == nil && !added {
		verb = "已更新"
	}
	msg := verb + "：" + email
	if err != nil {
		msg = "登录已成功，但写入号池失败：" + err.Error()
	}
	qoderLoginSession.mu.Lock()
	qoderLoginSession.done = true
	qoderLoginSession.ok = err == nil
	qoderLoginSession.message = msg
	qoderLoginSession.mu.Unlock()
	if err != nil {
		log.Printf("panel: Qoder 浏览器登录后写入号池失败: %v", err)
	} else {
		log.Printf("panel: Qoder 浏览器登录成功：%s（%s）", msg, snap.Email)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true, "ok": err == nil, "message": msg,
		"email": email, "added": added,
	})
}
