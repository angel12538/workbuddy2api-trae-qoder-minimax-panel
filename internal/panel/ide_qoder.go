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
	"log"
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

// qoderCheckinStatus 号池各账号的签到状态。
//
// v2 起逐账号向**上游**核实（并发 2，只查不领、token 过期自动续期写回号池），
// 不再只读本地记录 —— 用户扫出来的「已签」必须是上游确认过的，而不是面板自己
// 记的一笔账（此前 09:00 的假已签能一直挂到中午）。当前登录账号的实时状态放在
// current 字段，号池为空时前端用。
func (p *Panel) qoderCheckinStatus(w http.ResponseWriter, r *http.Request) {
	results := ideauth.QoderPoolScan(nil)
	claimed, claimable, pending, bad := 0, 0, 0, 0
	for _, it := range results {
		switch it.Phase {
		case "claimed":
			claimed++
		case "claimable":
			claimable++
		case "pending":
			pending++
		default:
			bad++
		}
	}
	// 当前登录账号（客户端登录态）的实时状态：供「号池为空但本机已登录」的展示。
	//
	// checked_in_today **只认上游**：phase=claimed 表示上游活动列表里今天的
	// CLAIM_BENEFIT 已是 CLAIMED。此前这里用 CheckedInToday（只比自然日）兜底，
	// 而本地日志里可能残留旧版写的「纯日期」记录 —— 那会让"根本没签到"的号
	// 显示成"今日已签到"（2026-10-24 实测：浏览器加的号显示已签，真正领到
	// 100 Credits 的另一个号反而显示可签，两个号整个反了）。现在只信上游，
	// 外加**可信**的本地记录（10:05 之后写入的完整时间戳）。
	cur := map[string]any{}
	if a, err := ideauth.ReadQoderAuth(); err == nil && a.User.Email != "" {
		oc := ideauth.QoderScanCurrent()
		ph, _ := oc["phase"].(string)
		oc["checked_in_today"] = ph == "claimed" ||
			(ph != "claimable" && ideauth.CheckinRecordTrusted("qoder", a.User.Email))
		cur = oc
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results":   results,
		"claimed":   claimed,
		"claimable": claimable,
		"pending":   pending,
		"bad":       bad,
		"current":   cur,
		// all_settled：今天**所有**号都没有可领的了（聚合口径，与任何单号无关）。
		// 顶层 checked_in_today 是历史字段，保留同值兼容 —— 但它**不是**单号状态：
		// 前端曾把它当成当前登录号的状态用，两个号的签到显示因此整个反了。
		"all_settled":     bad == 0 && claimable == 0,
		"checked_in_today": bad == 0 && claimable == 0,
	})
}

// qoderCheckinSwitched 切号逐号签到：轮流把客户端切到号池每个账号、按当前
// 登录签到，完成后恢复原登录。每日签到活动只对客户端登录过的账号下发（浏览器
// 登录的号扫不到），且每台设备每日限领一个号——切号是让每个号都有资格签的方式。
// 耗时约 N×40 秒（每号一次客户端重启）。
func (p *Panel) qoderCheckinSwitched(w http.ResponseWriter, r *http.Request) {
	results := ideauth.QoderSwitchedCheckinAll(func(s string) {
		log.Printf("panel: [切号签到] %s", s)
	})
	claimed, already, failed := 0, 0, 0
	for _, it := range results {
		switch it.Phase {
		case "claimed":
			claimed++
		case "already":
			already++
		case "error":
			failed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "results": results,
		"claimed_accounts": claimed, "already_accounts": already, "failed_accounts": failed,
		"msg": fmt.Sprintf("切号签到完成：成功 %d · 已领 %d · 失败 %d", claimed, already, failed),
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
