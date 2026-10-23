// ide_trae.go 面板「Trae IDE 账号」：读取/保存/切换本机 Trae IDE 的登录态。
//
// 与 trae.go 的区别：trae.go 管的是「签到账号」（checkin_accounts.json，用于
// 调 api.trae.cn 签到）；本文件管的是「Trae IDE 自身的登录态」
// （%APPDATA%\Trae CN\User\globalStorage\storage.json），用于一键切号。
package panel

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// traeIdeAccounts 返回 Trae IDE 号池。
// Trae IDE 与「Trae 签到」是同一套 Trae 账号体系，故这里把签到侧账号也并入展示：
// 以 IDE 登录态为主，用签到账号补全 name/email，并补上只在签到侧存在的账号。
func (p *Panel) traeIdeAccounts(w http.ResponseWriter, r *http.Request) {
	avail := ideauth.TraeAvailable()
	list, err := ideauth.ListTraeAccounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "trae_pool_read_failed: "+err.Error())
		return
	}
	curEmail, curName, curID := "", "", ""
	if a, err := ideauth.CaptureTraeSnapshot(); err == nil {
		curEmail, curName, curID = a.Email, a.Name, a.UserID
	}

	// 合并签到侧账号：user_id → name
	type checkinInfo struct{ Name string }
	checkin := map[string]checkinInfo{}
	traeList, _ := p.traeLoadAccounts()
	for _, a := range traeList {
		checkin[a.UserID] = checkinInfo{Name: a.Name}
	}
	seen := map[string]bool{}
	for i := range list {
		seen[list[i].UserID] = true
		if info, ok := checkin[list[i].UserID]; ok && list[i].Name == "" {
			list[i].Name = info.Name
		}
	}
	for _, a := range traeList {
		if a.UserID == "" || seen[a.UserID] {
			continue
		}
		seen[a.UserID] = true
		list = append(list, ideauth.TraeAccountView{
			UserID:  a.UserID,
			Name:    a.Name,
			Current: a.UserID == curID && curID != "",
			// 只在签到侧存在：没有本地 IDE 登录态可切换。
			CheckinOnly: true,
		})
	}
	// 兜底：当前登录账号若既不在号池、也不在签到侧，仍要展示为「当前登录（未入库）」，
	// 否则会出现「客户端明明登录了，面板却看不到是哪个账号」的情况。
	if curID != "" && !seen[curID] {
		list = append([]ideauth.TraeAccountView{{
			UserID:      curID,
			Current:     true,
			CheckinOnly: true,
		}}, list...)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"available":       avail,
		"data_dir":        ideauth.TraeDataDir(),
		"storage_path":    ideauth.TraeStoragePath(),
		"accounts":        list,
		"current_email":   curEmail,
		"current_name":    curName,
		"current_user_id": curID,
	})
}

func (p *Panel) traeIdeAdd(w http.ResponseWriter, r *http.Request) {
	snap, added, err := ideauth.UpsertTraeAccount()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "trae_ide_add_failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"added":   added,
		"email":   snap.Email,
		"user_id": snap.UserID,
	})
}

type traeIdeSwitchReq struct {
	ID             string `json:"id"`
	RestoreMachine bool   `json:"restore_machine_id"`
	Restart        bool   `json:"restart"`
}

func (p *Panel) traeIdeSwitch(w http.ResponseWriter, r *http.Request) {
	var req traeIdeSwitchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeErr(w, http.StatusBadRequest, "id_required")
		return
	}
	killNote := ""
	if req.Restart {
		killNote = ideauth.KillClient("trae")
	}
	snap, err := ideauth.SwitchTraeTo(req.ID, req.RestoreMachine)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "trae_ide_switch_failed: "+err.Error())
		return
	}
	launchNote := ""
	if req.Restart {
		launchNote = ideauth.LaunchClient("trae")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"email":        snap.Email,
		"name":         snap.Name,
		"restarted":    req.Restart,
		"restart_note": killNote + "；" + launchNote,
	})
}

type traeIdeRemoveReq struct {
	ID string `json:"id"`
}

func (p *Panel) traeIdeRemove(w http.ResponseWriter, r *http.Request) {
	var req traeIdeRemoveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeErr(w, http.StatusBadRequest, "id_required")
		return
	}
	if err := ideauth.RemoveTraeAccount(req.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "trae_ide_remove_failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
