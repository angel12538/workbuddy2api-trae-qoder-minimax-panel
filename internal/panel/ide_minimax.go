// ide_minimax.go 面板「MiniMax Code」：号池（多账号）+ 每日签到 + 积分。
//
// 与其他 IDE 不同，MiniMax 账号没有邮箱等身份字段，号池用 token 指纹做 id；
// 签到的鉴权就是各账号自己的 OAuth token，所以不需要切号、不需要客户端在线。
//
// 接口：
//   - GET  /panel/api/ide/minimax/accounts   号池账号 + 今日签到/积分状态
//   - POST /panel/api/ide/minimax/add        把当前客户端登录态存入号池
//   - POST /panel/api/ide/minimax/remove     从号池移除 {"id":"..."}
//   - POST /panel/api/ide/minimax/checkin    逐个签到 {"ids":[...]}（缺省=全部）
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/minimax"
)

// minimaxAccounts 返回号池账号与实时状态（签到/积分）。
func (p *Panel) minimaxAccounts(w http.ResponseWriter, r *http.Request) {
	st, err := minimax.StatusOfAccounts(r.Context(), p.minimaxClient)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "minimax_pool_read_failed: "+err.Error())
		return
	}
	current := ""
	if cred, err := minimax.LoadCredentials(); err == nil {
		current = minimax.Fingerprint(cred.AccessToken)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"accounts":     st,
		"current_id":   current,
		"pool_path":    minimax.PoolPath(),
		"has_client":   current != "",
	})
}

// minimaxAdd 把当前 MiniMax Code 登录态存进号池。
func (p *Panel) minimaxAdd(w http.ResponseWriter, r *http.Request) {
	acc, added, err := minimax.CaptureCurrent(r.Context(), p.minimaxClient)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "minimax_add_failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "added": added, "id": acc.ID, "label": acc.Label,
	})
}

// minimaxRemove 从号池移除账号（不影响客户端当前登录态）。
func (p *Panel) minimaxRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	if err := minimax.RemoveAccount(req.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "minimax_remove_failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// minimaxCheckin 对号池账号逐个签到（缺省=全部），各自用各自的 token。
func (p *Panel) minimaxCheckin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	results, err := minimax.SigninAccounts(r.Context(), p.minimaxClient, req.IDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "minimax_checkin_failed: "+err.Error())
		return
	}
	claimed, already, failed, credits := 0, 0, 0, 0.0
	for _, res := range results {
		switch res.Phase {
		case "claimed":
			claimed++
			credits += res.Points
		case "already":
			already++
		default:
			failed++
		}
	}
	msg := fmt.Sprintf("本次签到 %d 个 · 今日已领 %d 个 · 失败 %d 个", claimed, already, failed)
	if credits > 0 {
		msg += fmt.Sprintf("，新增 +%.0f 积分", credits)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "results": results,
		"claimed_accounts": claimed, "already_accounts": already, "failed_accounts": failed,
		"credits": credits, "msg": msg,
	})
}

// minimaxRenew 续期 MiniMax 凭证。
//
// MiniMax 的 access token 只有 1 小时、refresh token 每次刷新都轮换，号池副本
// 不续就死（这就是「一小时后全过期」的根因）。面板支持三种续法：
//   - {"force":true}            → 全部账号强制续期
//   - {}                        → 只续临期（30 分钟内过期）账号 —— 定时任务同款
//   - {"id":"u569..."}          → 单账号续期（前端「续期」按钮）
//
// 与客户端相同账号的续期会自动把新 token 同步回客户端 auth.json（refresh token
// 是单次有效的轮换令牌，不同步回写客户端下次刷新必挂）。
func (p *Panel) minimaxRenew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Force bool   `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ID != "" {
		res := p.minimaxClient.RenewAccount(r.Context(), req.ID, true)
		writeJSON(w, http.StatusOK, map[string]any{"ok": res.Phase != "error", "results": []minimax.RenewResult{res}})
		return
	}
	results := p.minimaxClient.RenewAll(r.Context(), req.Force)
	okN, errN := 0, 0
	for _, res := range results {
		switch res.Phase {
		case "renewed", "adopted":
			okN++
		case "error":
			errN++
		}
	}
	log.Printf("panel: MiniMax 续期完成：%d 成功 / %d 失败", okN, errN)
	writeJSON(w, http.StatusOK, map[string]any{"ok": errN == 0, "results": results})
}

// minimaxAddManual 手动添加账号（粘贴 access_token / refresh_token）。
// 「添加账号」弹窗里 MiniMax 页签的手动通道：没有客户端也能加号。
func (p *Panel) minimaxAddManual(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAtMs  int64  `json:"expires_at_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request: "+err.Error())
		return
	}
	acc, added, err := p.minimaxClient.AddManual(r.Context(), req.AccessToken, req.RefreshToken, req.ExpiresAtMs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "minimax_add_failed: "+err.Error())
		return
	}
	verb := "已添加"
	if !added {
		verb = "已更新"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "added": added, "id": acc.ID, "label": acc.Label, "msg": verb + "：" + acc.Label,
	})
}
