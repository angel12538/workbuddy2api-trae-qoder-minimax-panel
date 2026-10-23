// ide_qoder.go 面板「Qoder 账号」：读取/保存/切换本地 Qoder IDE 的登录账号。
//
// 背景：Qoder 的登录态存在 %APPDATA%\com.qoder.app.stable\auth.v1.dat，
// 格式为 "v10" + AES-256-GCM(iv12|ct|tag16)，密钥来自 Local State 的
// DPAPI 加密 key。本模块把它解密成明文 JSON，就能做到「添加账号 / 一键切号」。
//
// 接口：
//   - GET  /panel/api/ide/qoder/accounts       号池账号 + 当前登录账号
//   - POST /panel/api/ide/qoder/add            把当前登录态存进号池
//   - POST /panel/api/ide/qoder/switch         切到指定账号 {"email":"..."}
//   - POST /panel/api/ide/qoder/remove         从号池移除 {"email":"..."}
//   - POST /panel/api/ide/qoder/checkin        每日签到（Campaign 领取 Credits）
//   - GET  /panel/api/ide/qoder/device         设备指纹（runtime-info.exe）状态
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// qoderAccounts 返回号池账号列表与当前登录账号。
func (p *Panel) qoderAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := ideauth.ListQoderAccounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "qoder_pool_read_failed: "+err.Error())
		return
	}
	cur := ""
	curName := ""
	curExpires := ""
	if a, err := ideauth.ReadQoderAuth(); err == nil {
		cur = a.User.Email
		curName = a.User.Name
		curExpires = a.ExpiresAt
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       list,
		"current_email":  cur,
		"current_name":   curName,
		"current_expires": curExpires,
	})
}

// qoderAdd 把当前 Qoder 登录态保存进号池。
func (p *Panel) qoderAdd(w http.ResponseWriter, r *http.Request) {
	snap, added, err := ideauth.UpsertQoderAccount()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "qoder_add_failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"added":   added,
		"email":   snap.Email,
		"name":    snap.Name,
		"user_id": snap.UserID,
	})
}

type qoderSwitchReq struct {
	Email          string `json:"email"`
	RestoreMachine bool   `json:"restore_machine_id"`
	Restart        bool   `json:"restart"`
}

// qoderSwitch 把号池指定账号写回 Qoder（切号）。
// restart=true：先结束 Qoder 进程 → 写入目标账号凭据 → 重新启动 Qoder。
func (p *Panel) qoderSwitch(w http.ResponseWriter, r *http.Request) {
	var req qoderSwitchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Email) == "" {
		writeErr(w, http.StatusBadRequest, "email_required")
		return
	}
	killNote := ""
	if req.Restart {
		killNote = ideauth.KillClient("qoder")
	}
	snap, err := ideauth.SwitchQoderTo(req.Email, req.RestoreMachine)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "qoder_switch_failed: " + err.Error()})
		return
	}
	launchNote := ""
	if req.Restart {
		launchNote = ideauth.LaunchClient("qoder")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"email":        snap.Email,
		"name":         snap.Name,
		"restarted":    req.Restart,
		"restart_note": killNote + "；" + launchNote,
	})
}

type qoderRemoveReq struct {
	Email string `json:"email"`
}

// qoderRemove 从号池移除账号。
func (p *Panel) qoderRemove(w http.ResponseWriter, r *http.Request) {
	var req qoderRemoveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Email) == "" {
		writeErr(w, http.StatusBadRequest, "email_required")
		return
	}
	if err := ideauth.RemoveQoderAccount(req.Email); err != nil {
		writeErr(w, http.StatusInternalServerError, "qoder_remove_failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// qoderCheckin 按号池逐个账号执行每日签到（领取 Campaign Credits）。
//
// 请求体可选：{"emails":["a@x","b@y"]}，缺省 = 号池全部账号。
// 为什么改成批量：签到只需要各账号自己的 token + 设备级指纹，不需要切号；
// 旧实现只签「当前登录」那一个，导致号池有两个号时另一个永远签不到。
// 号池为空时回退到旧行为（签当前登录账号）。
func (p *Panel) qoderCheckin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Emails []string `json:"emails"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	out, err := ideauth.QoderCheckinPool(req.Emails)
	if err != nil || out == nil || len(out.Results) == 0 {
		// 号池为空/读取失败：回退到「当前登录账号」这条老路径，保证单号用户不受影响。
		res, err2 := ideauth.QoderCheckin()
		if err2 != nil {
			writeErr(w, http.StatusBadGateway, "qoder_checkin_failed: "+err2.Error())
			return
		}
		if res.Phase == "claimed" || res.Phase == "already" {
			_ = ideauth.MarkCheckedIn("qoder", res.Email)
			res.CheckedInToday = true
		}
		code := http.StatusOK
		if res.Phase == "error" {
			code = http.StatusBadGateway
		}
		writeJSON(w, code, map[string]any{
			"ok": true, "results": []any{res},
			"claimed_accounts": btoi(res.Phase == "claimed"), "already_accounts": btoi(res.Phase == "already"),
			"failed_accounts": btoi(res.Phase == "error" || res.Phase == "login_required"),
			"credits": res.Credits, "phase": res.Phase, "msg": res.Msg,
			"checked_in_today": res.CheckedInToday,
		})
		return
	}

	msg := fmt.Sprintf("本次签到 %d 个 · 今日已领 %d 个 · 失败 %d 个", out.Claimed, out.Already, out.Failed)
	if out.Credits > 0 {
		msg += fmt.Sprintf("，新增 +%.0f Credits", out.Credits)
	} else if out.AlreadyCredits > 0 {
		msg += fmt.Sprintf("（今日已入账 %.0f Credits）", out.AlreadyCredits)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "results": out.Results,
		"claimed_accounts": out.Claimed, "already_accounts": out.Already, "failed_accounts": out.Failed,
		"credits": out.Credits, "credits_already": out.AlreadyCredits, "msg": msg,
		"no_device":        out.NoDevice,
		"checked_in_today": out.Failed == 0,
	})
}

// btoi 布尔转 0/1（JSON 汇总字段用）。
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// qoderCheckinStatus 当前账号的上游签到状态 + 号池各账号的本地签到记录。
// 号池状态只读本地日志，不打上游（进页面不该发 N 个请求）。
func (p *Panel) qoderCheckinStatus(w http.ResponseWriter, r *http.Request) {
	res := ideauth.QoderCheckinStatus()
	res.CheckedInToday = ideauth.CheckedInToday("qoder", res.Email) || res.Phase == "claimed"
	writeJSON(w, http.StatusOK, map[string]any{
		"email":            res.Email,
		"phase":            res.Phase,
		"msg":              res.Msg,
		"checked_in_today": res.CheckedInToday,
		"results":          ideauth.QoderPoolStatuses(),
	})
}

// qoderUsage 返回指定账号的积分额度（?email=，缺省当前登录账号）。
func (p *Panel) qoderUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ideauth.QoderUsageFor(r.URL.Query().Get("email")))
}

// qoderRenew 续期 Qoder 凭证（refresh_token 换新 token 并写回）。
func (p *Panel) qoderRenew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	writeJSON(w, http.StatusOK, ideauth.QoderRenew(req.Force))
}

// qoderDevice 返回设备指纹状态（runtime-info.exe 是否可用、machineToken 是否拿到）。
func (p *Panel) qoderDevice(w http.ResponseWriter, r *http.Request) {
	dev := ideauth.QoderDeviceInfo()
	exe := ideauth.QoderRuntimeInfoExe()
	writeJSON(w, http.StatusOK, map[string]any{
		"umid_exe":      exe,
		"exe_found":     exe != "",
		"machine_id":    dev.MachineID,
		"machine_token": maskIfAny(dev.MachineToken),
		"machine_code":  dev.MachineCode,
		"machine_type":  dev.MachineType,
		"version":       dev.Version,
		"ready":         exe != "" && dev.MachineToken != "",
	})
}

func maskIfAny(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "…" + s[len(s)-4:]
}
