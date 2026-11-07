// ide_qoder.go 面板「Qoder 账号」：读取/保存/切换本地 Qoder IDE 的登录账号。
//
// 背景：Qoder 的登录态存在 %APPDATA%\com.qoder.app.stable\auth.v1.dat，
// 格式为 "v10" + AES-256-GCM(iv12|ct|tag16)，密钥来自 Local State 的
// DPAPI 加密 key。本模块把它解密成明文 JSON，就能做到「添加账号 / 一键切号」。
//
// 接口（T11 负责注册路由，本文件只提供 handler）：
//   - GET  /panel/api/ide/qoder/accounts       号池账号 + 当前登录账号
//   - POST /panel/api/ide/qoder/add            把当前登录态存进号池
//   - POST /panel/api/ide/qoder/switch         切到指定账号 {"email":"..."}
//   - POST /panel/api/ide/qoder/remove         从号池移除 {"email":"..."}
//   - POST /panel/api/ide/qoder/checkin        每日签到（C-QD-API@1 A）
//   - GET  /panel/api/ide/qoder/checkin-status 签到状态（C-QD-API@1 B）
//   - GET  /panel/api/ide/qoder/claim_history  逐日领取历史（C-QD-API@1 C）
//   - GET  /panel/api/ide/qoder/egress         设备/出口配置（C-QD-API@1 D）
//   - POST /panel/api/ide/qoder/egress         写设备/出口配置（C-QD-API@1 E）
//   - GET  /panel/api/ide/qoder/device         设备指纹（runtime-info.exe）状态
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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
		cur = ideauth.QoderIdentityKey(a)
		curName = a.User.Name
		curExpires = a.ExpiresAt
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        list,
		"current_email":   cur,
		"current_name":    curName,
		"current_expires": curExpires,
		"switch_mode":     qoderSwitchMode(),
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
	if ideauth.QoderServerMode() {
		snap, err := ideauth.SwitchQoderTo(req.Email, false)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "qoder_switch_failed: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "email": snap.Email, "name": snap.Name,
			"switch_mode": "server", "restarted": false,
			"message": "已切换服务端面板当前 Qoder 账号",
		})
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

func qoderSwitchMode() string {
	if ideauth.QoderServerMode() {
		return "server"
	}
	return "desktop"
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
// 请求体可选：
//   - {"emails":["a@x","b@y"]}  缺省 = 号池全部账号
//   - {"priority":"a@x"}        让这个号**先打**（今天想把那 100 积分给谁就传谁）
//
// 响应按契约 C-QD-API@1 A 返回：七相计数（claimed/already/blocked/pending/
// failed/login_required/error）、rotation_order（本轮实际尝试顺序）、
// quota_groups（同一人分桶）与 results[]（每号带 strategy / rotation_rank /
// quota_group / quota_taken_by / last_claimed_at）。
//
// checked_in_today 只在**所有**账号都真领到/已领时才为 true；blocked/pending/
// failed/login_required/error 一律 false —— 旧实现只看 Failed/Blocked，
// 会把「上游没下发」显示成已签到。
func (p *Panel) qoderCheckin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Emails   []string `json:"emails"`
		Priority string   `json:"priority"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	out, err := ideauth.QoderCheckinPoolOrdered(req.Emails, req.Priority)
	if err != nil || out == nil || len(out.Results) == 0 {
		// 号池为空/读取失败：回退到「当前登录账号」这条老路径，保证单号用户不受影响。
		res, err2 := ideauth.QoderCheckin()
		if err2 != nil {
			writeErr(w, http.StatusBadGateway, "qoder_checkin_failed: "+err2.Error())
			return
		}
		item := qoderLegacyCheckinItem(res)
		// 账本同样逐次记录（C-QD-LEDGER@1：每次尝试都写，blocked/pending/failed 也写）。
		_ = ideauth.AppendQoderClaim(ideauth.QoderClaimRecord{
			Email:      item.Email,
			Phase:      item.Phase,
			Credits:    item.Credits,
			CampaignID: item.ClaimCID,
			Strategy:   item.Strategy,
		})

		code := http.StatusOK
		if res.Phase == ideauth.QoderPhaseError {
			code = http.StatusBadGateway
		}
		claimed, already, pending, failed, needLogin, errored := 0, 0, 0, 0, 0, 0
		switch item.Phase {
		case ideauth.QoderPhaseClaimed:
			claimed = 1
		case ideauth.QoderPhaseAlready:
			already = 1
		case ideauth.QoderPhasePending:
			pending = 1
		case ideauth.QoderPhaseLoginRequired:
			needLogin = 1
		case ideauth.QoderPhaseFailed:
			failed = 1
		default:
			errored = 1
		}
		writeJSON(w, code, map[string]any{
			"ok":                      true,
			"results":                 []ideauth.QoderCheckinItem{item},
			"claimed_accounts":        claimed,
			"already_accounts":        already,
			"blocked_accounts":        0,
			"pending_accounts":        pending,
			"failed_accounts":         failed,
			"login_required_accounts": needLogin,
			"error_accounts":          errored,
			"credits":                 item.Credits,
			"credits_already":         0,
			"no_device":               !item.DeviceOK,
			"rotation_order":          []string{item.Email},
			"quota_groups":            []ideauth.QoderQuotaGroupView{},
			"checked_in_today":        item.CheckedInToday,
			"msg":                     item.Msg,
		})
		return
	}

	msg := fmt.Sprintf("本次签到 %d 个 · 今日已领 %d 个", out.Claimed, out.Already)
	if out.Blocked > 0 {
		// 这档不是失败（重试没用），也不是"已签到"（本号根本没领到）——
		// 服务端判定这两个账号是**同一个人**，今天这一份额度只给了其中一个。
		msg += fmt.Sprintf(" · %d 个今天没签上（服务端判定与另一个号是同一个人，那份额度已被对方领走）", out.Blocked)
	}
	if out.Pending > 0 {
		// 这档是"服务端今天没给这个号下发活动实例"（claim_campaigns=0），
		// 不是被别人抢的，单列避免被当成失败或已签到。
		msg += fmt.Sprintf(" · %d 个上游未下发活动（claim_campaigns=0）", out.Pending)
	}
	if out.Failed > 0 {
		msg += fmt.Sprintf(" · %d 个失败", out.Failed)
	}
	if out.LoginRequired > 0 {
		msg += fmt.Sprintf(" · %d 个需重新登录", out.LoginRequired)
	}
	if out.Error > 0 {
		msg += fmt.Sprintf(" · %d 个错误", out.Error)
	}
	if out.Credits > 0 {
		msg += fmt.Sprintf(" · 新增 +%.0f Credits", out.Credits)
	} else if out.AlreadyCredits > 0 {
		msg += fmt.Sprintf("（今日已入账 %.0f Credits）", out.AlreadyCredits)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                      true,
		"results":                 out.Results,
		"claimed_accounts":        out.Claimed,
		"already_accounts":        out.Already,
		"blocked_accounts":        out.Blocked,
		"pending_accounts":        out.Pending,
		"failed_accounts":         out.Failed,
		"login_required_accounts": out.LoginRequired,
		"error_accounts":          out.Error,
		"credits":                 out.Credits,
		"credits_already":         out.AlreadyCredits,
		"no_device":               out.NoDevice,
		"rotation_order":          out.RotationOrder,
		"quota_groups":            out.QuotaGroups,
		"checked_in_today":        out.CheckedInToday,
		"msg":                     msg,
	})
}

// qoderLegacyCheckinItem 把「当前登录账号」单号签到结果封装成契约结果项。
//
// 号池为空时走这条老路径（单号用户），但字段仍必须满足 C-QD-API@1：
// strategy/rotation_rank/quota_group 一个都不能少，非 claimed/already 的
// credits 必须为 0（活动面值放 benefit_amount）。
func qoderLegacyCheckinItem(res *ideauth.QoderCheckinResult) ideauth.QoderCheckinItem {
	if res == nil {
		res = &ideauth.QoderCheckinResult{Phase: ideauth.QoderPhaseError, Msg: "签到未返回结果"}
	}
	phase := res.Phase
	switch phase {
	case ideauth.QoderPhaseClaimed, ideauth.QoderPhaseAlready, ideauth.QoderPhasePending, ideauth.QoderPhaseLoginRequired:
		// 保持不变。
	case ideauth.QoderPhaseError:
		// 单号路径的 error = 上游请求失败 → failed（本地结构性错误这里分不出来）。
		phase = ideauth.QoderPhaseFailed
	default:
		phase = ideauth.QoderPhaseFailed
	}
	// strategy 由 ideauth 的统一函数推导（面板不自己判断）：
	// 真领到 / 本地账本可信 ⇒ 本号独占一份（owner）；否则证据不足，如实留 unknown，
	// 绝不默认扣上「服务端判定同人」的帽子（2026-11-04 实测：服务端并未判同人）。
	verdict := ideauth.QoderVerdictUnknown
	switch phase {
	case ideauth.QoderPhaseClaimed:
		verdict = ideauth.QoderVerdictOwner
	case ideauth.QoderPhaseAlready:
		if ideauth.CheckinRecordTrusted("qoder", res.Email) {
			verdict = ideauth.QoderVerdictOwner
		}
	}
	item := ideauth.QoderCheckinItem{
		Email:        res.Email,
		Name:         res.Name,
		UID:          res.UID,
		Phase:        phase,
		Msg:          res.Msg,
		DeviceOK:     res.DeviceOK,
		Refreshed:    res.Refreshed,
		Exe:          res.Exe,
		Base:         res.Base,
		Scope:        res.Scope,
		ClaimCID:     res.ClaimCID,
		QuotaGroup:   1,
		Strategy:     ideauth.QoderStrategyFor(res.Email, phase, "", 1, verdict),
		RotationRank: 1,
	}
	if phase == ideauth.QoderPhaseClaimed || phase == ideauth.QoderPhaseAlready {
		item.Credits = res.Credits
		item.CheckedInToday = true
		item.ClaimedAt = res.ClaimedAt
	} else {
		item.BenefitAmount = res.Credits
		item.CheckedInToday = false
	}
	if t, ok := ideauth.LastClaimedAt("qoder", res.Email); ok {
		item.LastClaimedAt = t.Format(time.RFC3339)
	}
	if item.ClaimedAt != "" && item.CheckedInToday {
		item.LastClaimedAt = item.ClaimedAt
	}
	return item
}

// qoderCheckinStatus 号池各账号的签到状态（契约 C-QD-API@1 B）。
//
// v2 起逐账号向**上游**核实（并发 2，只查不领、token 过期自动续期写回号池），
// 不再只读本地记录 —— 用户扫出来的「已签」必须是上游确认过的，而不是面板自己
// 记的一笔账（此前 09:00 的假已签能一直挂到中午）。当前登录账号的实时状态放在
// current 字段，号池为空时前端用。
func (p *Panel) qoderCheckinStatus(w http.ResponseWriter, r *http.Request) {
	results := ideauth.QoderPoolScan(nil)
	if results == nil {
		results = []ideauth.QoderScanAccount{}
	}
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
	if a, err := ideauth.ReadQoderAuth(); err == nil && ideauth.QoderIdentityKey(a) != "" {
		oc := ideauth.QoderScanCurrent()
		ph, _ := oc["phase"].(string)
		oc["checked_in_today"] = ph == "claimed" ||
			(ph != "claimable" && ideauth.CheckinRecordTrusted("qoder", ideauth.QoderIdentityKey(a)))
		cur = oc
	}
	// 顶层 checked_in_today 与 POST checkin **同义**：只有**所有**账号今天都
	// 真领到/已领才为 true（扫描结果没有 already 相，claimed 即已领）。
	// 存在 pending / claimable / 登录失效 / 错误 → false。
	//
	// 旧语义「今天所有号都没有可领的了」不再借用 checked_in_today 这个名字：
	//   - all_settled       保留旧语义（契约 C-QD-API@1 B 的字段，不改名）；
	//   - all_settled_legacy 是给还没迁移的旧前端的兼容别名（omitempty，仅在
	//     true 时出现）。⚠️ 两个字段名语义相同，checked_in_today 则与它们正交。
	allClaimed := len(results) > 0 && claimed == len(results)
	allSettled := bad == 0 && claimable == 0
	type checkinStatusTop struct {
		Results          []ideauth.QoderScanAccount `json:"results"`
		Claimed          int                        `json:"claimed"`
		Claimable        int                        `json:"claimable"`
		Pending          int                        `json:"pending"`
		Bad              int                        `json:"bad"`
		Current          map[string]any             `json:"current"`
		AllSettled       bool                       `json:"all_settled"`
		CheckedInToday   bool                       `json:"checked_in_today"`
		AllSettledLegacy bool                       `json:"all_settled_legacy,omitempty"`
	}
	writeJSON(w, http.StatusOK, checkinStatusTop{
		Results:          results,
		Claimed:          claimed,
		Claimable:        claimable,
		Pending:          pending,
		Bad:              bad,
		Current:          cur,
		AllSettled:       allSettled,
		CheckedInToday:   allClaimed,
		AllSettledLegacy: allSettled,
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

// qoderClaimHistory 返回逐日领取历史（契约 C-QD-API@1 C）。
//
// GET /panel/api/ide/qoder/claim_history?email=&days=7
//   - days<=0 视为 30，上限 180；
//   - email 为空 = 全部账号，按 at 降序最多 500 条；
//   - 记录包含 blocked/pending/failed 等**没领到**的尝试，用于审计与轮换解释。
func (p *Panel) qoderClaimHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	email := strings.TrimSpace(q.Get("email"))
	days, _ := strconv.Atoi(strings.TrimSpace(q.Get("days")))
	if days <= 0 {
		days = 30
	}
	if days > 180 {
		days = 180
	}
	records := ideauth.QoderClaimHistory(email, days)
	if email == "" {
		// 契约：全部账号按 at 降序，最多 500 条。
		sort.SliceStable(records, func(i, j int) bool {
			if records[i].At != records[j].At {
				return records[i].At > records[j].At
			}
			return records[i].Email < records[j].Email
		})
		if len(records) > 500 {
			records = records[:500]
		}
	}
	if records == nil {
		records = []ideauth.QoderClaimRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "records": records})
}

// qoderEgressGet 返回号池各账号的设备/出口配置（契约 C-QD-API@1 D）。
//
// 未配置的账号按默认值返回（device_mode=real、proxy_url 空 = 进程默认网络），
// 这样前端能直接对每个号做配置，而不是"没配置就看不到"。last_probe 来自
// C-QD-PROBE@1 的结论摘要（文件不存在时为 null，不编造结论）。
func (p *Panel) qoderEgressGet(w http.ResponseWriter, r *http.Request) {
	egress, err := ideauth.LoadQoderEgress()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "qoder_egress_read_failed: "+err.Error())
		return
	}
	accounts, _ := ideauth.ListQoderAccounts()
	probe := ideauth.QoderProbeSummary()

	seen := map[string]bool{}
	profiles := make([]map[string]any, 0, len(accounts)+len(egress.Profiles))
	add := func(email string, prof ideauth.QoderEgressProfile) {
		key := strings.ToLower(strings.TrimSpace(email))
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		if strings.TrimSpace(prof.Email) == "" {
			prof.Email = key
		}
		if strings.TrimSpace(prof.DeviceMode) == "" {
			prof.DeviceMode = ideauth.QoderDeviceModeReal
		}
		dir := strings.TrimSpace(prof.ProfileDir)
		if dir == "" {
			dir = qoderWebProfileDir(p, prof.Email)
		}
		profiles = append(profiles, map[string]any{
			"email":       prof.Email,
			"device_mode": prof.DeviceMode,
			"proxy_url":   prof.ProxyURL,
			"profile_dir": dir,
			"updated_at":  prof.UpdatedAt,
			"last_probe":  probe,
		})
	}
	for _, a := range accounts {
		email := strings.TrimSpace(a.Email)
		add(email, egress.Profiles[strings.ToLower(email)])
	}
	// 配置里存在、但号池里已被移除的账号也要如实展示（否则用户无法清理）。
	for _, prof := range egress.Profiles {
		add(prof.Email, prof)
	}
	sort.SliceStable(profiles, func(i, j int) bool {
		return profiles[i]["email"].(string) < profiles[j]["email"].(string)
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "profiles": profiles})
}

// qoderEgressSet 写入某账号的设备/出口配置（契约 C-QD-API@1 E）。
//
// 请求：{"email":"a@x","device_mode":"real|derived","proxy_url":"http://host:port",
// "profile_dir":"..."}。校验失败返回 400 + invalid_device_mode / invalid_proxy_url，
// 且**不落盘**（校验在 ideauth.SetQoderEgress 内完成）。
func (p *Panel) qoderEgressSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email      string `json:"email"`
		DeviceMode string `json:"device_mode"`
		ProxyURL   string `json:"proxy_url"`
		ProfileDir string `json:"profile_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Email) == "" {
		writeErr(w, http.StatusBadRequest, "email_required")
		return
	}
	prof, err := ideauth.SetQoderEgress(req.Email, req.DeviceMode, req.ProxyURL, req.ProfileDir)
	if err != nil {
		code := "invalid_egress_config"
		if errors.Is(err, ideauth.ErrInvalidDeviceMode) {
			code = "invalid_device_mode"
		}
		if errors.Is(err, ideauth.ErrInvalidProxyURL) {
			code = "invalid_proxy_url"
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": code, "msg": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "profile": prof})
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
