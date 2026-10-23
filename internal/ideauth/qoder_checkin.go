package ideauth

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Qoder 每日签到（Campaign 体系）
//
// 协议（逆向自客户端 + 交叉验证公开实现）：
//   1. GET  {base}/sash/api/v1/me/campaigns
//        → {campaigns:[{campaignId,name,actionType,claimStatus,benefit:{amount},startAt,endAt}]}
//        只关心 actionType=CLAIM_BENEFIT 且 claimStatus=CLAIMABLE
//   2. POST {base}/sash/api/v1/me/campaigns/{cid}/claim   body {}
//        → {status:"CLAIMED",benefit:{amount}}（幂等，重复领不重复发币）
//   3. POST {base}/api/v1/deviceToken/refresh  body {"refresh_token":...}
//
// 所有请求必须带 Authorization: Bearer <accessToken> 以及 Cosy-Machine* 机器指纹头，
// 指纹由客户端自带的 resources/umid/runtime-info.exe 生成（--account-stdin）。
// base：国内 https://openapi.qoder.com.cn，国际 https://openapi.qoder.sh
// 每日 10:00 (UTC+8) 刷新当天 campaignId。
// ---------------------------------------------------------------------------

const (
	qoderOpenAPICN   = "https://openapi.qoder.com.cn"
	qoderOpenAPIINTL = "https://openapi.qoder.sh"

	qoderPathCampaigns     = "/sash/api/v1/me/campaigns"
	qoderPathClaim         = "/sash/api/v1/me/campaigns/%s/claim"
	qoderPathDeviceRefresh = "/api/v1/deviceToken/refresh"
	qoderPathUsage         = "/api/v1/me/usage"

	qoderClaimAction = "CLAIM_BENEFIT"
)

// QoderDevice 是 Cosy-* 机器指纹集合。
type QoderDevice struct {
	ClientType      string `json:"clientType"`
	MachineOS       string `json:"machineOS"`
	MachineHostname string `json:"machineHostname"`
	MachineID       string `json:"machineId"`
	MachineToken    string `json:"machineToken"`
	MachineCode     string `json:"machineCode"`
	MachineType     string `json:"machineType"`
	Version         string `json:"version"`
	// Synthetic 表示这是"按账号派生"的设备标识（不是物理设备）：
	// 设备指纹照常参与判定，但 MachineToken/MachineCode 是物理设备绑定值，不能发出去。
	Synthetic bool `json:"-"`
}

// Apply 写入 Cosy-* 请求头。
func (d *QoderDevice) Apply(h http.Header) {
	set := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			h.Set(k, v)
		}
	}
	set("Cosy-ClientType", firstNonEmpty(d.ClientType, "10"))
	set("Cosy-MachineOS", d.MachineOS)
	set("Cosy-MachineHostname", d.MachineHostname)
	set("Cosy-MachineId", d.MachineID)
	if !d.Synthetic {
		set("Cosy-MachineToken", d.MachineToken)
		set("Cosy-MachineCode", d.MachineCode)
	}
	set("Cosy-MachineType", d.MachineType)
	set("Cosy-Version", d.Version)
}

// qoderUSTags 返回 "x86_64_windows" 形态的 machineOS。
func qoderMachineOS() string {
	arch := "x86_64"
	switch runtime.GOARCH {
	case "arm64":
		arch = "aarch64"
	}
	return arch + "_windows"
}

// QoderRuntimeInfoExe 定位客户端自带的 runtime-info.exe（找不到返回空串）。
func QoderRuntimeInfoExe() string {
	if v := strings.TrimSpace(os.Getenv("QODER_UMID_EXE")); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	local := os.Getenv("LOCALAPPDATA")
	var roots []string
	// 版本目录（最新优先）
	for _, base := range qoderInstallBases() {
		vers, _ := filepath.Glob(filepath.Join(base, ".qoder-versions", "*", "resources", "umid", "runtime-info.exe"))
		sort.Slice(vers, func(i, j int) bool {
			fi, _ := os.Stat(vers[i])
			fj, _ := os.Stat(vers[j])
			if fi == nil || fj == nil {
				return false
			}
			return fi.ModTime().After(fj.ModTime())
		})
		roots = append(roots, vers...)
		roots = append(roots, filepath.Join(base, "resources", "umid", "runtime-info.exe"))
	}
	_ = local
	for _, p := range roots {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// qoderInstallBases 返回 Qoder 可能的安装根目录。
func qoderInstallBases() []string {
	var out []string
	add := func(p string) {
		if p != "" {
			out = append(out, p)
		}
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		add(filepath.Join(local, "Programs", "Qoder"))
		add(filepath.Join(local, "Programs", "Qoder CN"))
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		add(filepath.Join(pf, "Qoder"))
		add(filepath.Join(pf, "Qoder CN"))
	}
	return out
}

// qoderRunRuntimeInfo 运行 runtime-info.exe 取机器指纹。
func qoderRunRuntimeInfo(exe string) map[string]any {
	if exe == "" {
		return nil
	}
	cmd := exec.Command(exe, "--account-stdin")
	cmd.Dir = filepath.Dir(exe)
	cmd.Stdin = bytes.NewReader(nil)
	hideWindow(cmd)
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := cmd.Output()
		ch <- res{b, err}
	}()
	select {
	case r := <-ch:
		out := strings.TrimSpace(string(r.b))
		lines := strings.Split(out, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil {
				return m
			}
		}
	case <-time.After(40 * time.Second):
		return nil
	}
	return nil
}

// QoderDeviceInfo 组装完整设备指纹。
func QoderDeviceInfo() *QoderDevice {
	exe := QoderRuntimeInfoExe()
	ri := qoderRunRuntimeInfo(exe)
	str := func(k string) string {
		if ri == nil {
			return ""
		}
		if v, ok := ri[k].(string); ok {
			return v
		}
		return ""
	}
	d := &QoderDevice{
		ClientType:      "10",
		MachineOS:       qoderMachineOS(),
		MachineHostname: hostname(),
		MachineToken:    str("machineToken"),
		MachineCode:     str("machineCode"),
		MachineType:     str("machineType"),
	}
	if mid, err := QoderMachineID(); err == nil {
		d.MachineID = strings.TrimSpace(mid)
	}
	if exe != "" {
		mf := filepath.Join(filepath.Dir(filepath.Dir(exe)), "build-manifest.json")
		if raw, err := os.ReadFile(mf); err == nil {
			var m struct {
				ProductVersion string `json:"productVersion"`
			}
			if json.Unmarshal(raw, &m) == nil {
				d.Version = m.ProductVersion
			}
		}
	}
	return d
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// ---------- Campaign 类型 ----------

// QoderBenefit 活动权益。
type QoderBenefit struct {
	Amount float64 `json:"amount"`
}

// QoderCampaign 单个活动。
type QoderCampaign struct {
	CampaignID  string       `json:"campaignId"`
	Name        string       `json:"name"`
	ActionType  string       `json:"actionType"`
	ClaimStatus string       `json:"claimStatus"`
	Benefit     QoderBenefit `json:"benefit"`
	StartAt     int64        `json:"startAt"`
	EndAt       int64        `json:"endAt"`
}

type qoderCampaignsData struct {
	Campaigns []QoderCampaign `json:"campaigns"`
}

// QoderCheckinResult 签到结果。
type QoderCheckinResult struct {
	Email      string  `json:"email"`
	Name       string  `json:"name"`
	UID        string  `json:"uid"`
	Phase      string  `json:"phase"` // claimed / already / pending / login_required / error
	Msg        string  `json:"msg"`
	Credits    float64 `json:"credits"`
	Claimed    int     `json:"claimed"`
	Total      int     `json:"total_campaigns"`
	DeviceOK  bool    `json:"device_ok"`
	Refreshed bool    `json:"refreshed"`
	Exe       string  `json:"umid_exe,omitempty"`
	Base      string  `json:"base,omitempty"`
	// CheckedInToday 面板观察到该账号今天已完成领取。
	CheckedInToday bool `json:"checked_in_today,omitempty"`
}

// qoderOpenAPIBaseList 返回候选 base（按优先级）。默认国际服优先、国内兜底；
// 可用 QODER_OPENAPI_BASE 固定单一 base。
func qoderOpenAPIBaseList() []string {
	if v := strings.TrimSpace(os.Getenv("QODER_OPENAPI_BASE")); v != "" {
		return []string{strings.TrimRight(v, "/")}
	}
	return []string{qoderOpenAPIINTL, qoderOpenAPICN}
}

// qoderDo 发一次带鉴权与设备头的请求。
func qoderDo(base, path, method, token string, body []byte, dev *QoderDevice) (int, []byte, error) {
	url := strings.TrimRight(base, "/") + path
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Qoder/claim")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if dev != nil {
		dev.Apply(req.Header)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

type qoderAPIResp struct {
	Data json.RawMessage `json:"data"`
}

// QoderCheckin 对当前登录的 Qoder 账号执行每日签到。
func QoderCheckin() (*QoderCheckinResult, error) {
	auth, err := ReadQoderAuth()
	if err != nil {
		return &QoderCheckinResult{Phase: "login_required", Msg: "读取 Qoder 登录态失败: " + err.Error()}, nil
	}
	dev := qoderDeviceForAccount(auth)
	res, at, rt := qoderCheckinAuth(auth, dev)
	if at != "" || rt != "" {
		// 当前登录号：token 轮换必须写回客户端登录态，否则下次续期会失败。
		_ = writeBackQoderTokens(auth, at, rt)
	}
	return res, nil
}

// qoderCheckinAuth 用「指定登录态」签一次（不读盘、不写盘），返回续期后的 token。
//
// 为什么要抽出来：号池里可能有多个账号，签到必须按每个账号各自的 token 逐个执行，
// 而不是只签「当前登录」的那一个。设备指纹（machineToken/machineCode）是设备级的，
// 一次生成即可给所有账号共用，不需要切号、不需要重启客户端。
//
// 返回的 newAccess/newRefresh 非空表示本次 401 后续期成功，由调用方负责持久化：
// 当前登录号写回客户端登录态，号池账号写回号池文件（绝不能互相串写）。
func qoderCheckinAuth(auth *QoderAuth, dev *QoderDevice) (res *QoderCheckinResult, newAccess, newRefresh string) {
	if auth == nil || (auth.Token == "" && auth.RefreshToken == "") {
		return &QoderCheckinResult{Phase: "login_required", Msg: "该账号没有可用凭证（token/refresh_token 均为空）"}, "", ""
	}
	res = &QoderCheckinResult{
		Email: auth.User.Email,
		Name:  auth.User.Name,
		UID:   auth.User.ID,
	}
	if dev == nil {
		dev = &QoderDevice{}
	}
	res.DeviceOK = dev.MachineToken != "" && dev.MachineID != ""
	res.Exe = QoderRuntimeInfoExe()

	token := auth.Token
	var (
		status  int
		raw     []byte
		base    string
		lastErr string
	)
	// 先看本地记录的过期时间：过期就直接续期，别等 401。
	// 关键原因：国际服 base 对「过期/无效 token」返回的是 200 + 空活动列表，
	// 不是 401——以前循环在第一个 200 就 break，于是永远"暂无可领取活动"，
	// 而且永远不会触发续期。表现就是：明明有活动，面板却一直签不了。
	if qoderAuthExpired(auth) {
		if nt, nrt, ok := qoderRefreshToken(auth, qoderOpenAPIBaseList()[0], dev); ok {
			token = nt
			res.Refreshed = true
			newAccess, newRefresh = nt, nrt
			auth.Token = nt
		}
	}

	authorized := false
	emptyOK := false // 拿到过滤 200 但没有活动：作为"确实没有活动"的兜底证据
	for _, b := range qoderOpenAPIBaseList() {
		st, rw, err := qoderDo(b, qoderPathCampaigns, http.MethodGet, token, nil, dev)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		if st == http.StatusUnauthorized {
			// 换 base 无效时，尝试用 refreshToken 续期后再打一次
			if nt, nrt, ok := qoderRefreshToken(auth, b, dev); ok {
				st2, rw2, err2 := qoderDo(b, qoderPathCampaigns, http.MethodGet, nt, nil, dev)
				if err2 == nil && st2 != http.StatusUnauthorized {
					token, status, raw, base = nt, st2, rw2, b
					res.Refreshed = true
					newAccess, newRefresh = nt, nrt
					// 续期后拿到的也可能还是空列表，交给下面的空列表判断
					if base != "" && len(qoderCampaignsOf(raw)) > 0 {
						authorized = true
						break
					}
					emptyOK = true
					continue
				}
			}
			lastErr = "401 @ " + b
			continue
		}
		// 200：必须真的解析出活动才算命中；空列表继续试下一个 base。
		if n := len(qoderCampaignsOf(rw)); n > 0 {
			status, raw, base = st, rw, b
			authorized = true
			break
		}
		if !emptyOK {
			status, raw, base = st, rw, b
			emptyOK = true
		}
	}
	if !authorized && emptyOK {
		// 所有 base 都返回空活动列表：按"当前没有活动"处理（不是错误）。
		authorized = true
	}
	if !authorized {
		if lastErr == "" {
			lastErr = "无可用的 openapi base"
		}
		res.Phase = "login_required"
		res.Msg = "token 无效且刷新失败，请在 Qoder 重新登录（" + lastErr + "）"
		return res, newAccess, newRefresh
	}
	if status != http.StatusOK {
		res.Phase = "error"
		res.Msg = fmt.Sprintf("查询活动失败 HTTP %d: %s", status, truncateStr(string(raw), 160))
		return res, newAccess, newRefresh
	}
	res.Base = base

	// 解析（活动列表可能在 data 或顶层）
	var top qoderCampaignsData
	if json.Unmarshal(raw, &top) != nil || len(top.Campaigns) == 0 {
		var wrap struct {
			Data qoderCampaignsData `json:"data"`
		}
		if json.Unmarshal(raw, &wrap) == nil {
			top = wrap.Data
		}
	}
	res.Total = len(top.Campaigns)

	var claimable []QoderCampaign
	for _, c := range top.Campaigns {
		if c.ActionType == qoderClaimAction && c.ClaimStatus == "CLAIMABLE" {
			claimable = append(claimable, c)
		}
	}
	if len(claimable) == 0 {
		for _, c := range top.Campaigns {
			if c.ActionType == qoderClaimAction && c.ClaimStatus == "CLAIMED" {
				res.Phase = "already"
				res.Msg = "今日已领取"
				if c.Benefit.Amount > 0 {
					res.Credits = c.Benefit.Amount
				}
				return res, newAccess, newRefresh
			}
		}
		res.Phase = "pending"
		res.Msg = "活动尚未下发（Qoder 每天 10:00 后陆续放出；后台每 30 分钟自动重试）"
		return res, newAccess, newRefresh
	}

	var got float64
	var claimed int
	var fails []string
	for _, c := range claimable {
		p := fmt.Sprintf(qoderPathClaim, c.CampaignID)
		s2, r2, err := qoderDo(base, p, http.MethodPost, token, []byte("{}"), dev)
		if err != nil {
			fails = append(fails, c.CampaignID+":"+err.Error())
			continue
		}
		amount, ok := parseClaimResult(s2, r2)
		if ok {
			claimed++
			if amount > 0 {
				got += amount
			} else {
				got += c.Benefit.Amount
			}
		} else {
			fails = append(fails, fmt.Sprintf("%s:HTTP%d", c.CampaignID, s2))
		}
	}
	res.Claimed = claimed
	res.Credits = got
	if len(fails) > 0 && claimed == 0 {
		res.Phase = "error"
		res.Msg = "领取失败: " + strings.Join(fails, "; ")
	} else {
		res.Phase = "claimed"
		res.Msg = fmt.Sprintf("签到成功 +%.0f Credits", got)
		if dev.MachineToken == "" {
			res.Msg += "（缺少机器指纹，可能被风控）"
		}
	}
	return res, newAccess, newRefresh
}

// QoderCheckinStatus 只查询当前账号的签到状态（不领取）。phase：
// claimed（今日已领）/ claimable（今日可领）/ pending（无活动）/ login_required / error。
func QoderCheckinStatus() *QoderCheckinResult {
	auth, err := ReadQoderAuth()
	if err != nil {
		return &QoderCheckinResult{Phase: "login_required", Msg: "未登录或无法读取登录态"}
	}
	res := &QoderCheckinResult{Email: auth.User.Email, Name: auth.User.Name, UID: auth.User.ID}
	dev := qoderDeviceForAccount(auth)
	res.DeviceOK = dev.MachineToken != "" && dev.MachineID != ""
	lastErr := ""
	for _, b := range qoderOpenAPIBaseList() {
		st, raw, err := qoderDo(b, qoderPathCampaigns, http.MethodGet, auth.Token, nil, dev)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		if st == http.StatusUnauthorized {
			lastErr = "401"
			continue
		}
		if st != http.StatusOK {
			res.Phase = "error"
			res.Msg = fmt.Sprintf("查询活动失败 HTTP %d", st)
			return res
		}
		res.Base = b
		var top qoderCampaignsData
		if json.Unmarshal(raw, &top) != nil || len(top.Campaigns) == 0 {
			var wrap struct {
				Data qoderCampaignsData `json:"data"`
			}
			if json.Unmarshal(raw, &wrap) == nil {
				top = wrap.Data
			}
		}
		res.Total = len(top.Campaigns)
		claimable, claimed := 0, 0
		for _, c := range top.Campaigns {
			if c.ActionType != qoderClaimAction {
				continue
			}
			switch c.ClaimStatus {
			case "CLAIMABLE":
				claimable++
				if c.Benefit.Amount > 0 {
					res.Credits = c.Benefit.Amount
				}
			case "CLAIMED":
				claimed++
				if res.Credits == 0 && c.Benefit.Amount > 0 {
					res.Credits = c.Benefit.Amount
				}
			}
		}
		switch {
		case claimable > 0:
			res.Phase = "claimable"
			res.Msg = "今日可领取"
		case claimed > 0:
			res.Phase = "claimed"
			res.Msg = "今日已领取"
		default:
			res.Phase = "pending"
			res.Msg = "暂无签到活动"
		}
		return res
	}
	res.Phase = "login_required"
	res.Msg = "token 无效或无法连接（" + lastErr + "）"
	return res
}

// parseClaimResult 解析 claim 响应，判断是否 CLAIMED。
func parseClaimResult(status int, raw []byte) (float64, bool) {
	if status != http.StatusOK {
		return 0, false
	}
	var wrap struct {
		Status  string       `json:"status"`
		Benefit QoderBenefit `json:"benefit"`
		Data    *struct {
			Status  string       `json:"status"`
			Benefit QoderBenefit `json:"benefit"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) != nil {
		return 0, false
	}
	if wrap.Data != nil && wrap.Data.Status == "CLAIMED" {
		return wrap.Data.Benefit.Amount, true
	}
	if wrap.Status == "CLAIMED" {
		return wrap.Benefit.Amount, true
	}
	// 有些返回把 status 放在 data 里但结构不同，退而求其次
	if bytes.Contains(raw, []byte(`"CLAIMED"`)) {
		return 0, true
	}
	return 0, false
}

// qoderRefreshToken 刷新 accessToken，返回 (accessToken, refreshToken, ok)。
func qoderRefreshToken(auth *QoderAuth, base string, dev *QoderDevice) (string, string, bool) {
	at, rt, _, _, ok := qoderRefreshTokenFull(auth, base, dev)
	return at, rt, ok
}

// qoderRefreshTokenFull 刷新 token，并返回服务端下发的到期时间。
// 注意：refresh_token 会被轮换，必须把返回值写回，否则下次续期会失败。
func qoderRefreshTokenFull(auth *QoderAuth, base string, dev *QoderDevice) (access, refresh, expiresAt, refreshExpiresAt string, ok bool) {
	if auth.RefreshToken == "" {
		return "", "", "", "", false
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": auth.RefreshToken})
	status, raw, err := qoderDo(base, qoderPathDeviceRefresh, http.MethodPost, "", body, dev)
	if err != nil || status != http.StatusOK {
		return "", "", "", "", false
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", "", "", "", false
	}
	if d, ok := m["data"].(map[string]any); ok {
		m = d
	}
	at := firstString(m, "device_token", "accessToken", "access_token", "token")
	rt := firstString(m, "refreshToken", "refresh_token")
	if at == "" {
		return "", "", "", "", false
	}
	if rt == "" {
		rt = auth.RefreshToken
	}
	return at, rt, firstString(m, "expires_at"), firstString(m, "refresh_token_expires_at"), true
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// writeBackQoderTokens 把刷新后的 token 写回号池与 IDE（best-effort）。
func writeBackQoderTokens(auth *QoderAuth, access, refresh string) error {
	if access != "" {
		auth.Token = access
	}
	if refresh != "" {
		auth.RefreshToken = refresh
	}
	return WriteQoderAuth(auth)
}

// ---------- Qoder 用量 / 续期 ----------

// QoderQuota 一组额度（套餐内 / 附加包）。
type QoderQuota struct {
	Used       float64 `json:"used"`
	Remaining  float64 `json:"remaining"`
	Total      float64 `json:"total"`
	Percentage float64 `json:"percentage"`
	Unit       string  `json:"unit"`
}

// QoderUsageResult 当前账号的额度与套餐信息。
type QoderUsageResult struct {
	Email        string      `json:"email"`
	Name         string      `json:"name"`
	UID          string      `json:"uid"`
	PlanTier     string      `json:"plan_tier"`
	PlanTierName string      `json:"plan_tier_name"`
	PackageQuota *QoderQuota `json:"user_quota,omitempty"`   // 套餐内 Credits
	AddOnQuota   *QoderQuota `json:"add_on_quota,omitempty"` // 附加 / 签到 Credits
	Base         string      `json:"base,omitempty"`
	Phase        string      `json:"phase,omitempty"` // ok / login_required / error
	Msg          string      `json:"msg,omitempty"`
}

// QoderUsage 查询当前登录账号的积分额度（GET {base}/api/v1/me/usage）。
// 该端点由 Qoder IDE 自身的额度面板使用，返回套餐内 + 附加包两组 credits。
func QoderUsage() *QoderUsageResult {
	auth, err := ReadQoderAuth()
	if err != nil {
		return &QoderUsageResult{Phase: "login_required", Msg: "未登录或无法读取登录态"}
	}
	return qoderUsageAuth(auth, QoderDeviceInfo())
}

// qoderUsageAuth 用指定登录态查额度（号池多号查询复用，见 QoderUsageFor）。
func qoderUsageAuth(auth *QoderAuth, dev *QoderDevice) *QoderUsageResult {
	if auth == nil {
		return &QoderUsageResult{Phase: "login_required", Msg: "账号登录态为空"}
	}
	if dev == nil {
		dev = &QoderDevice{}
	}
	res := &QoderUsageResult{Email: auth.User.Email, Name: auth.User.Name, UID: auth.User.ID}
	lastErr := ""
	for _, base := range qoderOpenAPIBaseList() {
		st, raw, derr := qoderDo(base, qoderPathUsage, http.MethodGet, auth.Token, nil, dev)
		if derr != nil {
			lastErr = derr.Error()
			continue
		}
		if st == http.StatusUnauthorized {
			lastErr = "401"
			continue
		}
		if st != http.StatusOK {
			res.Phase = "error"
			res.Msg = fmt.Sprintf("查询用量失败 HTTP %d", st)
			return res
		}
		var m struct {
			PlanTier     string      `json:"plan_tier"`
			PlanTierName string      `json:"plan_tier_name"`
			UserQuota    *QoderQuota `json:"user_quota"`
			AddOnQuota   *QoderQuota `json:"add_on_quota"`
		}
		if json.Unmarshal(raw, &m) != nil {
			// 可能包在 data 里
			var wrap struct {
				Data struct {
					PlanTier     string      `json:"plan_tier"`
					PlanTierName string      `json:"plan_tier_name"`
					UserQuota    *QoderQuota `json:"user_quota"`
					AddOnQuota   *QoderQuota `json:"add_on_quota"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &wrap) == nil {
				m.PlanTier, m.PlanTierName, m.UserQuota, m.AddOnQuota = wrap.Data.PlanTier, wrap.Data.PlanTierName, wrap.Data.UserQuota, wrap.Data.AddOnQuota
			}
		}
		res.Base = base
		res.PlanTier, res.PlanTierName = m.PlanTier, m.PlanTierName
		res.PackageQuota, res.AddOnQuota = m.UserQuota, m.AddOnQuota
		res.Phase = "ok"
		return res
	}
	res.Phase = "error"
	if lastErr == "" {
		lastErr = "无可用的 openapi base"
	}
	res.Msg = lastErr
	return res
}

// QoderRenewResult 续期结果。
type QoderRenewResult struct {
	Email                 string `json:"email"`
	Name                  string `json:"name"`
	Phase                 string `json:"phase"` // renewed / skipped / login_required / error
	Msg                   string `json:"msg"`
	ExpiresAt             string `json:"expires_at,omitempty"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at,omitempty"`
	Base                  string `json:"base,omitempty"`
}

// QoderRenew 续期 Qoder 凭证（用 refresh_token 换新 token 并写回本地）。
// force=false 时，凭证剩余有效期 > 72 小时则跳过，避免无谓轮换。
func QoderRenew(force bool) *QoderRenewResult {
	auth, err := ReadQoderAuth()
	if err != nil {
		return &QoderRenewResult{Phase: "login_required", Msg: "未登录或无法读取登录态"}
	}
	res := &QoderRenewResult{Email: auth.User.Email, Name: auth.User.Name}
	if !force {
		if t, perr := time.Parse(time.RFC3339, auth.ExpiresAt); perr == nil && time.Now().Add(72*time.Hour).Before(t) {
			res.Phase = "skipped"
			res.Msg = "凭证仍在有效期内（>72h），无需续期"
			res.ExpiresAt, res.RefreshTokenExpiresAt = auth.ExpiresAt, auth.RefreshTokenExpiresAt
			return res
		}
	}
	if auth.RefreshToken == "" {
		res.Phase = "login_required"
		res.Msg = "缺少 refresh token，请重新登录 Qoder"
		return res
	}
	dev := QoderDeviceInfo()
	lastErr := ""
	for _, base := range qoderOpenAPIBaseList() {
		at, rt, exp, rtexp, ok := qoderRefreshTokenFull(auth, base, dev)
		if !ok {
			lastErr = "刷新失败 @ " + base
			continue
		}
		auth.Token, auth.RefreshToken = at, rt
		if exp != "" {
			auth.ExpiresAt = exp
		}
		if rtexp != "" {
			auth.RefreshTokenExpiresAt = rtexp
		}
		if werr := WriteQoderAuth(auth); werr != nil {
			res.Phase = "error"
			res.Msg = "刷新成功但写回失败：" + werr.Error()
			return res
		}
		res.Phase = "renewed"
		res.Msg = "凭证已续期"
		res.Base = base
		res.ExpiresAt, res.RefreshTokenExpiresAt = auth.ExpiresAt, auth.RefreshTokenExpiresAt
		return res
	}
	res.Phase = "error"
	res.Msg = lastErr
	return res
}

// qoderAuthExpired 判断本地记录的凭证是否已过期（ExpiresAt 为 RFC3339）。
func qoderAuthExpired(auth *QoderAuth) bool {
	if auth == nil || strings.TrimSpace(auth.ExpiresAt) == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(auth.ExpiresAt))
	if err != nil {
		return false
	}
	return time.Now().Add(2 * time.Minute).After(t)
}

// qoderCampaignsOf 解析活动列表（兼容 data 包裹与顶层两种形态）。
func qoderCampaignsOf(raw []byte) []QoderCampaign {
	var top qoderCampaignsData
	if json.Unmarshal(raw, &top) == nil && len(top.Campaigns) > 0 {
		return top.Campaigns
	}
	var wrap struct {
		Data qoderCampaignsData `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) == nil {
		return wrap.Data.Campaigns
	}
	return nil
}

// qoderDeviceForAccount 为每个账号派生出**独立且稳定**的设备标识。
//
// 为什么必须这样做：Qoder 的活动判定是按设备（Cosy-MachineId）维度的。
// 号池里所有账号如果共用客户端那台设备的指纹，第一个号领完之后，
// 服务端会认为「这台设备今天已领」，其余账号直接返回空列表（showCampaign=false），
// 表现就是"第二个号提示已签过 / 识别不到签到任务"。
//
// 这里用账号自身 id 派生一个确定性 UUID（同一账号永远同一值），并清空
// MachineToken / MachineCode（它们绑定真实物理设备，留着会串回设备维度）。
func qoderDeviceForAccount(auth *QoderAuth) *QoderDevice {
	base := QoderDeviceInfo()
	if base == nil {
		base = &QoderDevice{}
	}
	key := ""
	if auth != nil {
		key = strings.TrimSpace(auth.User.ID)
		if key == "" {
			key = strings.TrimSpace(auth.User.Email)
		}
	}
	if key == "" {
		return base
	}
	sum := sha1.Sum([]byte("qoder-account-device:" + key))
	h := hex.EncodeToString(sum[:16])
	machineID := h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
	dev := *base
	dev.MachineID = machineID
	dev.Synthetic = true // 保留 MachineToken 仅用于 DeviceOK 判断，Apply 时不会发送
	return &dev
}
