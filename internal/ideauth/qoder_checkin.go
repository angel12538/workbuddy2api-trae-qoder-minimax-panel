package ideauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"github.com/google/uuid"
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
	// Derived 表示本设备由账号确定性派生（device_mode=derived），不是物理设备。
	// 与 Synthetic 的区别：Derived 设备的 Token/Code 也必须真实发送，否则上游不下发活动。
	Derived bool `json:"-"`
	// ProxyURL 该账号的出口代理（http/https://host:port）；空 = 进程默认网络。
	ProxyURL string `json:"-"`
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

// NewHTTPClient 按设备的出口代理构造 HTTP 客户端。
//
//   - ProxyURL 为空：http.DefaultTransport 克隆 —— 与现网 &http.Client{} 完全等价
//     （同样受进程级 HTTP_PROXY/HTTPS_PROXY 影响）；
//   - ProxyURL 为 http(s)://host:port：克隆默认 Transport 后换成 http.ProxyURL；
//   - socks5 等不支持的 scheme：返回错误（不新增依赖），绝不静默直连 ——
//     静默直连会让请求从本机真实出口发出去，而调用方以为已经做了隔离。
func (d *QoderDevice) NewHTTPClient(timeout time.Duration) (*http.Client, error) {
	tr := cloneQoderDefaultTransport()
	proxy := ""
	if d != nil {
		proxy = strings.TrimSpace(d.ProxyURL)
	}
	if proxy != "" {
		u, err := parseQoderProxyURL(proxy)
		if err != nil {
			return nil, err
		}
		if u != nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: timeout, Transport: tr}, nil
}

// cloneQoderDefaultTransport 克隆进程默认 Transport（保留代理环境变量、TLS、HTTP/2 设置）。
func cloneQoderDefaultTransport() *http.Transport {
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		return tr.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}

// qoderMachineOS 返回 "x86_64_windows" 形态的 machineOS。
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

// qoderCST Qoder 活动体系用的时区（每日 10:00 (UTC+8) 刷新当天 campaignId）。
var qoderCST = time.FixedZone("UTC+8", 8*60*60)

// qoderTodayStartCST 返回「今天」（UTC+8 自然日）的零点时刻。
func qoderTodayStartCST() time.Time {
	now := time.Now().In(qoderCST)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, qoderCST)
}

// qoderCampaignIsToday 判断活动是否属于「今天」下发的（startAt 在今天零点之后）。
//
// 为什么必须判这个：Qoder 的每日签到（CLAIM_BENEFIT）campaignId 每天换新，但服务端
// 会把**昨天已领过的活动**留在列表里直到当天刷新。此前代码只要看到 CLAIMED 的
// CLAIM_BENEFIT 就报「今日已领取」，于是 09:00 的自动签到天天被昨天的陈旧活动
// 骗成「已签」，写进本地记录后 30 分钟重试循环也不再补签 —— 10:00 真活动下发时
// 什么都不会发生。这就是「每天都告诉我已经签了，但实际上没签上」的完整链条。
func qoderCampaignIsToday(c QoderCampaign) bool {
	if c.StartAt <= 0 {
		return true // 上游没给 startAt：无从判断，按今天处理（宁误报可签不误报已签）
	}
	return time.Unix(c.StartAt, 0).After(qoderTodayStartCST())
}

// qoderClassifyCampaigns 把活动列表分成「今日可领 / 今日已领 / 陈旧已领」，其余忽略。
func qoderClassifyCampaigns(campaigns []QoderCampaign) (claimable []QoderCampaign, claimedToday, staleClaimed *QoderCampaign) {
	for _, c := range campaigns {
		if c.ActionType != qoderClaimAction {
			continue
		}
		switch c.ClaimStatus {
		case "CLAIMABLE":
			claimable = append(claimable, c)
		case "CLAIMED":
			if qoderCampaignIsToday(c) {
				if claimedToday == nil {
					cp := c
					claimedToday = &cp
				}
			} else if staleClaimed == nil {
				cp := c
				staleClaimed = &cp
			}
		}
	}
	return claimable, claimedToday, staleClaimed
}

// QoderCheckinResult 签到结果。
type QoderCheckinResult struct {
	Email     string  `json:"email"`
	Name      string  `json:"name"`
	UID       string  `json:"uid"`
	Phase     string  `json:"phase"` // claimed / already / pending / login_required / error
	Msg       string  `json:"msg"`
	Credits   float64 `json:"credits"`
	Claimed   int     `json:"claimed"`
	Total     int     `json:"total_campaigns"`
	DeviceOK  bool    `json:"device_ok"`
	Refreshed bool    `json:"refreshed"`
	Exe       string  `json:"umid_exe,omitempty"`
	Base      string  `json:"base,omitempty"`
	// ClaimCID 今天这条 CLAIM_BENEFIT 活动的实例 id（没有则空）。
	//
	// 用途：服务端的每日额度按**人**给，同一人的两个账号共用一份活动实例。
	// 某个号的列表里看不到活动时，用**另一个号**的实例 id 去试一次就能拿到
	// 服务端的真实判定（SAME_PERSON_ALREADY_CLAIMED），而不是含糊地说"暂无活动"。
	ClaimCID string `json:"claim_cid,omitempty"`
	// ClaimedAt 服务端给的领取时刻（RFC3339，**UTC**）。空 = 这次没领到/上游没给。
	// 本地签到记录用它落时间戳，这样"今天签没签"不再依赖本机时钟。
	ClaimedAt string `json:"claimed_at,omitempty"`
	// ClaimedToday 本次领到的那条活动是**今天下发**的（qoderCampaignIsToday）。
	// 昨天那波如果还挂着 CLAIMABLE，领它同样给积分，但那是补领，不是今天的每日
	// 额度 —— 不能据此把"今天已签"写进记录（否则今天真活动下发时反而被跳过）。
	ClaimedToday bool `json:"claimed_today,omitempty"`
	// CheckedInToday 面板观察到该账号今天已完成领取。
	CheckedInToday bool `json:"checked_in_today,omitempty"`
	// Scope 该账号的 agentAccountScope（agent 租户位标识）。
	//
	// ⚠️ 它不是每日额度的分组键：2026-11-02 实测两个号的 scope 其实**不同**；
	// 2026-11-04 实测两号各自拿到不同的 grantId、各自 +100 —— 额度不是无条件
	// 按人共享的，服务端判同人才只发一份。
	Scope string `json:"scope,omitempty"`
	// QuotaGroup 本账号所在**同一人组**里的账号数（含自己）。>1 = 与别的号同一人。
	QuotaGroup int `json:"quota_group,omitempty"`
	// QuotaTakenBy 同组里今天领到每日额度的账号（空 = 组里还没人领）。
	QuotaTakenBy string `json:"quota_taken_by,omitempty"`
}

// qoderOpenAPIBaseList 返回候选 base（按优先级）。默认国际服优先、国内兜底；
// 可用 QODER_OPENAPI_BASE 固定单一 base。
func qoderOpenAPIBaseList() []string {
	if v := strings.TrimSpace(os.Getenv("QODER_OPENAPI_BASE")); v != "" {
		return []string{strings.TrimRight(v, "/")}
	}
	return []string{qoderOpenAPIINTL, qoderOpenAPICN}
}

// qoderOpenAPIBaseListForScope 按账号归属的 scope 排 base 顺序：
// global 账号先打国际服（openapi.qoder.sh），其余先打国内服。
//
// 排错顺序的代价是实打出来的：global 账号先打国内服会拿到 401 TOKEN_EXPIRE
// （"token is not active"），而它其实有**完全有效**的凭证 —— 401 极易被误读成
// "token 失效"，面板于是显示"需要重新登录"，用户白折腾。
func qoderOpenAPIBaseListForScope(scope string) []string {
	if v := strings.TrimSpace(os.Getenv("QODER_OPENAPI_BASE")); v != "" {
		return []string{strings.TrimRight(v, "/")}
	}
	if strings.Contains(strings.ToLower(scope), ":global") {
		return []string{qoderOpenAPIINTL, qoderOpenAPICN}
	}
	return []string{qoderOpenAPICN, qoderOpenAPIINTL}
}

// QoderDo 是 Qoder 上游请求的唯一入口：带鉴权头、设备头，并按 dev.ProxyURL 走
// 该账号的出口。
//
// 网络层失败会**原地重试一次**：本机默认走 HTTP_PROXY（127.0.0.1:10808 的本地
// 代理），那个代理偶尔拒连，一次网络错误就判「token 失效」是冤枉。实测：同一秒
// 探针脚本能通、服务进程却报连接失败，重试一次就稳了。
//
// 代理配置错误（socks5 / 格式非法）属于配置问题，不重试，直接返回错误：
// 重试只会把 1.5s 的等待加在必然失败的请求上，还会掩盖真正的原因。
func QoderDo(base, path, method, token string, body []byte, dev *QoderDevice) (int, []byte, error) {
	client, err := dev.NewHTTPClient(30 * time.Second)
	if err != nil {
		return 0, nil, err
	}
	url := strings.TrimRight(base, "/") + path
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(1500 * time.Millisecond)
		}
		st, raw, err := qoderDoOnce(client, url, method, token, body, dev)
		if err == nil {
			return st, raw, nil
		}
		lastErr = err
	}
	return 0, nil, lastErr
}

// qoderDo 是 QoderDo 的薄封装（保留包内既有调用点的签名）。
//
// 所有签到/探测/usage 调用都经此进入 QoderDo，从而按账号出口走代理。
func qoderDo(base, path, method, token string, body []byte, dev *QoderDevice) (int, []byte, error) {
	return QoderDo(base, path, method, token, body, dev)
}

// QoderDoAny 按候选 base 顺序尝试，返回第一个拿到 HTTP 响应的 base。
//
// 给不掌握 qoderOpenAPIBaseListForScope 的调用方（cmd/qoderprobe）使用：
// scope 决定候选顺序（global 账号国际服优先，其余国内服优先），
// QODER_OPENAPI_BASE 可固定单一 base。设备指纹与出口复用 QoderDo。
//
// 语义约定：
//   - 网络层错误（连不上/DNS/TLS）继续试下一个 base；
//   - 任何 HTTP 响应（含 401/403/404）都算命中并立即返回 —— 探测需要区分
//     "没网"与"没权限"，把 401 当成失败会让结论失真；
//   - 全部 base 网络失败时返回最后一个错误，base 为空。
func QoderDoAny(scope, path, method, token string, body []byte, dev *QoderDevice) (status int, raw []byte, base string, err error) {
	bases := qoderOpenAPIBaseListForScope(scope)
	if len(bases) == 0 {
		return 0, nil, "", fmt.Errorf("没有可用的 openapi base")
	}
	var lastErr error
	for _, b := range bases {
		st, rw, derr := QoderDo(b, path, method, token, body, dev)
		if derr != nil {
			// 代理配置错误对所有 base 都一样，继续试只会把同一个错误重复一遍。
			if errors.Is(derr, ErrInvalidProxyURL) {
				return 0, nil, "", derr
			}
			lastErr = derr
			continue
		}
		return st, rw, b, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("所有 openapi base 均不可达")
	}
	return 0, nil, "", lastErr
}

// qoderDoOnce 用已构造好的客户端发一次请求。
func qoderDoOnce(client *http.Client, url, method, token string, body []byte, dev *QoderDevice) (int, []byte, error) {
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
	res, at, rt := qoderCheckinAuth(auth, dev, currentQoderScope())
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
func qoderCheckinAuth(auth *QoderAuth, dev *QoderDevice, scope string) (res *QoderCheckinResult, newAccess, newRefresh string) {
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
		if nt, nrt, ok := qoderRefreshToken(auth, qoderOpenAPIBaseListForScope(scope)[0], dev); ok {
			token = nt
			res.Refreshed = true
			newAccess, newRefresh = nt, nrt
			auth.Token = nt
		}
	}

	authorized := false
	emptyOK := false // 拿到过滤 200 但没有活动：作为"确实没有活动"的兜底证据
	for _, b := range qoderOpenAPIBaseListForScope(scope) {
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
	var claimedToday, staleClaimed *QoderCampaign
	claimable, claimedToday, staleClaimed = qoderClassifyCampaigns(top.Campaigns)
	if len(claimable) == 0 {
		if claimedToday != nil {
			// 只有「今天下发的活动」已领取才算是真·已签。昨天的陈旧活动
			// （staleClaimed）不算 —— 见 qoderCampaignIsToday 的注释。
			res.Phase = "already"
			res.Msg = "今日已领取"
			res.ClaimCID = claimedToday.CampaignID
			if claimedToday.Benefit.Amount > 0 {
				res.Credits = claimedToday.Benefit.Amount
			}
			return res, newAccess, newRefresh
		}
		if staleClaimed != nil {
			// 列表里只有昨天已领的签到活动：今天的还没下发（10:00 刷新）。
			// 绝不能把它当成「今日已领取」写进本地记录 —— 那正是每天误报已签的根因。
			res.Phase = "pending"
			res.Msg = "今日签到活动尚未下发（Qoder 每天 10:00 后刷新；后台每 30 分钟自动重试）"
			return res, newAccess, newRefresh
		}
		res.Phase = "pending"
		res.Msg = "活动尚未下发（Qoder 每天 10:00 后陆续放出；后台每 30 分钟自动重试）"
		return res, newAccess, newRefresh
	}

	var got float64
	var claimed int
	var fails []string
	res.ClaimCID = claimable[0].CampaignID
	for _, c := range claimable {
		p := fmt.Sprintf(qoderPathClaim, c.CampaignID)
		s2, r2, err := qoderDo(base, p, http.MethodPost, token, []byte("{}"), dev)
		if err != nil {
			fails = append(fails, c.CampaignID+":"+err.Error())
			continue
		}
		amount, claimedAt, ok := parseClaimResult(s2, r2)
		if ok {
			claimed++
			// 只有「今天下发的活动」才算今天的签到；补领昨天那波也给积分，但不能
			// 写成「今日已领取」—— 那会让今天真活动下发时被可信线跳过。
			if qoderCampaignIsToday(c) {
				res.ClaimedToday = true
				if claimedAt != "" {
					res.ClaimedAt = claimedAt
				}
			}
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

// qoderClaimInstance 用指定的活动实例 id 领一次。
//
// 用途：**探测**「服务端把同一个人算作一个人」这件事。
//
// 实测（2026-10-24）：Qoder 的每日 100 Credits 按**人**给，一个人的多个账号
// 共用同一条活动实例；第一个账号领过之后，另一个账号的 campaigns 列表里就
// 看不到这条活动了。此时用**另一个账号**的实例 id 去领，服务端会如实回答：
//
//	{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED", …}
//
// 拿到这个真实判定，面板才能说「今天已签到（同一人额度已由另一账号领取）」，
// 而不是含糊的"暂无签到活动" —— 前者是事实，后者会让人以为没签上。
//
// 返回：phase ∈ {"claimed"（真领到了）, "blocked"（同一人已领）, ""（无结论）}；
// detail 是服务端的 failureCode（blocked 时）；claimedAt 是服务端给的领取时刻。
func qoderClaimInstance(auth *QoderAuth, dev *QoderDevice, cid, scope string) (phase string, amount float64, detail string, claimedAt string) {
	if auth == nil || cid == "" {
		return "", 0, "", ""
	}
	if dev == nil {
		dev = &QoderDevice{}
	}
	token := auth.Token
	if qoderAuthExpired(auth) {
		if nt, _, ok := qoderRefreshToken(auth, qoderOpenAPIBaseListForScope(scope)[0], dev); ok {
			token = nt
		}
	}
	for _, b := range qoderOpenAPIBaseListForScope(scope) {
		st, raw, err := qoderDo(b, fmt.Sprintf(qoderPathClaim, cid), http.MethodPost, token, []byte("{}"), dev)
		if err != nil || st != http.StatusOK {
			continue
		}
		var r struct {
			Status      string `json:"status"`
			FailureCode string `json:"failureCode"`
			ErrorCode   string `json:"errorCode"`
			ClaimedAt   string `json:"claimedAt"`
			Benefit     struct {
				Amount float64 `json:"amount"`
			} `json:"benefit"`
		}
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		switch r.Status {
		case "CLAIMED", "SUCCESS":
			return "claimed", r.Benefit.Amount, "", r.ClaimedAt
		case "BLOCKED":
			code := r.FailureCode
			if code == "" {
				code = r.ErrorCode
			}
			return "blocked", 0, code, ""
		}
	}
	return "", 0, "", ""
}

// QoderCheckinStatus 查询当前账号的签到状态（不领取）。phase：
// claimed（今日已领）/ claimable（今日可领）/ pending（活动未下发或只有陈旧活动）/
// login_required / error。
//
// 与签到路径同一套判定（qoderClassifyCampaigns）：只有「今天下发的活动」CLAIMED
// 才算已签，昨天的陈旧活动一律报 pending。过期 token 先续期再查（国际服对过期
// token 返回 200 + 空列表，不刷新会永远"暂无活动"）。
func QoderCheckinStatus() *QoderCheckinResult {
	auth, err := ReadQoderAuth()
	if err != nil {
		return &QoderCheckinResult{Phase: "login_required", Msg: "未登录或无法读取登录态"}
	}
	dev := qoderDeviceForAccount(auth)
	res := qoderScanAuth(auth, dev, currentQoderScope())
	if at, rt := res.newAccess, res.newRefresh; at != "" || rt != "" {
		// 当前登录号：续期结果必须写回客户端登录态。
		_ = writeBackQoderTokens(auth, at, rt)
	}
	return res.result
}

// QoderScanCurrent 对「当前客户端登录账号」做一次上游核实（只查不领），
// 续期结果写回客户端登录态。返回给面板直接渲染的 map。
func QoderScanCurrent() map[string]any {
	auth, err := ReadQoderAuth()
	if err != nil {
		return map[string]any{"phase": "login_required", "msg": "未登录或无法读取登录态"}
	}
	oc := qoderScanAuth(auth, qoderDeviceForAccount(auth), currentQoderScope())
	if oc.newAccess != "" || oc.newRefresh != "" {
		_ = writeBackQoderTokens(auth, oc.newAccess, oc.newRefresh)
	}
	r := oc.result
	return map[string]any{
		"email":     r.Email,
		"name":      r.Name,
		"phase":     r.Phase,
		"msg":       r.Msg,
		"refreshed": r.Refreshed,
		"total":     r.Total,
	}
}

// qoderScanOutcome 一次「只查不领」的产出：结果 + 续期出的新 token（调用方持久化）。
type qoderScanOutcome struct {
	result     *QoderCheckinResult
	newAccess  string
	newRefresh string
}

// qoderScanAuth 用指定登录态做一次「只查不领」的上游核实（多 base + 续期重试）。
func qoderScanAuth(auth *QoderAuth, dev *QoderDevice, scope string) qoderScanOutcome {
	out := qoderScanOutcome{result: &QoderCheckinResult{
		Email: auth.User.Email, Name: auth.User.Name, UID: auth.User.ID,
	}}
	if auth == nil || (auth.Token == "" && auth.RefreshToken == "") {
		out.result.Phase = "login_required"
		out.result.Msg = "该账号没有可用凭证"
		return out
	}
	if dev == nil {
		dev = &QoderDevice{}
	}
	out.result.DeviceOK = dev.MachineToken != "" && dev.MachineID != ""

	token := auth.Token
	// 过期先续期（别等 401：国际服对过期 token 返回 200 + 空列表）。
	if qoderAuthExpired(auth) {
		if nt, nrt, ok := qoderRefreshToken(auth, qoderOpenAPIBaseListForScope(scope)[0], dev); ok {
			token = nt
			out.result.Refreshed = true
			out.newAccess, out.newRefresh = nt, nrt
			auth.Token = nt
		}
	}

	lastErr := ""
	authorized := false
	emptyOK := false
	var campaigns []QoderCampaign
	// 每个 base 的失败原因都要留档：只留最后一个会把「国际服网络失败」显示成
	// 「国内服 401 token 失效」，把用户引到完全错误的方向上。
	var baseErrs []string
	for _, b := range qoderOpenAPIBaseListForScope(scope) {
		st, raw, err := qoderDo(b, qoderPathCampaigns, http.MethodGet, token, nil, dev)
		if err != nil {
			lastErr = err.Error()
			baseErrs = append(baseErrs, b+" 连接失败: "+err.Error())
			continue
		}
		if st == http.StatusUnauthorized {
			if nt, nrt, ok := qoderRefreshToken(auth, b, dev); ok {
				st2, rw2, err2 := qoderDo(b, qoderPathCampaigns, http.MethodGet, nt, nil, dev)
				if err2 == nil && st2 == http.StatusOK {
					token = nt
					out.result.Refreshed = true
					out.newAccess, out.newRefresh = nt, nrt
					campaigns = qoderCampaignsOf(rw2)
					out.result.Base = b
					authorized = true
					break
				}
			}
			lastErr = "401 @ " + b
			baseErrs = append(baseErrs, b+" HTTP 401（token 失效）")
			continue
		}
		if st != http.StatusOK {
			baseErrs = append(baseErrs, fmt.Sprintf("%s HTTP %d: %s", b, st, truncateStr(string(raw), 120)))
			continue
		}
		// 200：空列表也接受（可能领完即删 / 未下发），但要继续试下一个 base 是否有货。
		cs := qoderCampaignsOf(raw)
		if len(cs) > 0 {
			campaigns = cs
			out.result.Base = b
			authorized = true
			break
		}
		if !emptyOK {
			out.result.Base = b
			emptyOK = true
		}
	}
	if !authorized && emptyOK {
		authorized = true
	}
	if !authorized {
		out.result.Phase = "login_required"
		if len(baseErrs) > 0 {
			out.result.Msg = "上游不可达或凭证失效：" + strings.Join(baseErrs, "；")
		} else {
			out.result.Msg = "token 无效或无法连接（" + lastErr + "）"
		}
		return out
	}

	out.result.Total = len(campaigns)
	claimable, claimedToday, staleClaimed := qoderClassifyCampaigns(campaigns)
	switch {
	case len(claimable) > 0:
		out.result.Phase = "claimable"
		out.result.Msg = "今日可领取"
		out.result.ClaimCID = claimable[0].CampaignID
		for _, c := range claimable {
			if c.Benefit.Amount > 0 {
				out.result.Credits = c.Benefit.Amount
				break
			}
		}
	case claimedToday != nil:
		out.result.Phase = "claimed"
		out.result.Msg = "今日已领取"
		out.result.ClaimCID = claimedToday.CampaignID
		if claimedToday.Benefit.Amount > 0 {
			out.result.Credits = claimedToday.Benefit.Amount
		}
	case staleClaimed != nil:
		// 只有昨天的已领活动：今天的还没下发。这不是「已签」。
		out.result.Phase = "pending"
		out.result.Msg = "今日签到活动尚未下发（Qoder 每天 10:00 后刷新）"
	default:
		out.result.Phase = "pending"
		out.result.Msg = "未观察到每日签到活动（已领完即删或尚未下发）"
	}
	return out
}

// parseClaimResult 解析 claim 响应，判断是否 CLAIMED，并取回**服务端给的**领取时刻。
//
// 为什么要把 claimedAt 带回去：本地签到记录的可信判定要看"这条记录是不是今天
// 活动下发之后写的"。用本机时钟判，本机时钟一错（这台机器实测钟慢 8 小时）就把
// 真领到的记录当成旧版误报，面板整天显示"没签上"。服务端给的时刻没有这个问题。
func parseClaimResult(status int, raw []byte) (float64, string, bool) {
	if status != http.StatusOK {
		return 0, "", false
	}
	var wrap struct {
		Status    string       `json:"status"`
		ClaimedAt string       `json:"claimedAt"`
		Benefit   QoderBenefit `json:"benefit"`
		Data      *struct {
			Status    string       `json:"status"`
			ClaimedAt string       `json:"claimedAt"`
			Benefit   QoderBenefit `json:"benefit"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) != nil {
		return 0, "", false
	}
	if wrap.Data != nil && wrap.Data.Status == "CLAIMED" {
		return wrap.Data.Benefit.Amount, wrap.Data.ClaimedAt, true
	}
	if wrap.Status == "CLAIMED" {
		return wrap.Benefit.Amount, wrap.ClaimedAt, true
	}
	// 有些返回把 status 放在 data 里但结构不同，退而求其次
	if bytes.Contains(raw, []byte(`"CLAIMED"`)) {
		return 0, "", true
	}
	return 0, "", false
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
	return qoderUsageAuth(auth, qoderDeviceForAccount(auth), currentQoderScope())
}

// qoderUsageAuth 用指定登录态查额度（号池多号查询复用，见 QoderUsageFor）。
func qoderUsageAuth(auth *QoderAuth, dev *QoderDevice, scope string) *QoderUsageResult {
	if auth == nil {
		return &QoderUsageResult{Phase: "login_required", Msg: "账号登录态为空"}
	}
	if dev == nil {
		dev = &QoderDevice{}
	}
	res := &QoderUsageResult{Email: auth.User.Email, Name: auth.User.Name, UID: auth.User.ID}
	lastErr := ""
	for _, base := range qoderOpenAPIBaseListForScope(scope) {
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
	dev := qoderDeviceForAccount(auth)
	lastErr := ""
	for _, base := range qoderOpenAPIBaseListForScope(currentQoderScope()) {
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

// QoderDeviceForEmail 按账号解析设备指纹与出口（契约 C-QD-DEVICE@1）。
//
// 读取 qoder_egress.json：
//   - real（无配置时的默认值）：返回 QoderDeviceInfo() 的真实设备，字段与现网完全一致；
//   - derived：按 lower(email) 确定性派生 MachineID/MachineToken/MachineCode/
//     MachineHostname，ClientType/MachineOS/MachineType/Version 复制真实设备；
//     派生设备 Derived=true、Synthetic=false —— Token/Code 必须真实发送，
//     否则上游不下发活动（2026-10-24 实测）。
//
// 出口代理只写入 dev.ProxyURL，由 NewHTTPClient/QoderDo 应用；本函数不发起网络请求。
// 配置文件损坏时返回错误（由调用方决定是否降级），不静默改配置。
func QoderDeviceForEmail(email string) (*QoderDevice, error) {
	pool, err := LoadQoderEgress()
	if err != nil {
		return nil, err
	}
	prof := qoderEgressProfileOf(pool, email)
	proxy := strings.TrimSpace(prof.ProxyURL)
	base := QoderDeviceInfo()
	if base == nil {
		base = &QoderDevice{}
	}
	if strings.ToLower(strings.TrimSpace(prof.DeviceMode)) == QoderDeviceModeDerived {
		dev := qoderDeriveDevice(prof.Email, base)
		dev.ProxyURL = proxy
		return dev, nil
	}
	base.Synthetic = false
	base.Derived = false
	base.ProxyURL = proxy
	return base, nil
}

// qoderDeriveDevice 按邮箱确定性派生设备指纹（同一邮箱跨进程稳定）。
//
// 规则见契约 2.2.6：
//
//	MachineID       = uuid.NewSHA1(NameSpaceOID, "qoder-device:"+lower(email))
//	MachineToken    = base64.RawURLEncoding(sha256("qoder-token:"+lower(email)+":"+realMachineID))
//	MachineCode     = hex(sha256("qoder-code:"+lower(email)))[:32]
//	MachineHostname = "DESKTOP-" + upper(hex(sha256("qoder-host:"+lower(email)))[:7])
//
// 真实 MachineID 参与 Token 派生：不同物理机器的同一邮箱会得到不同 Token，
// 但同一台机器上同一邮箱始终一致。
func qoderDeriveDevice(email string, base *QoderDevice) *QoderDevice {
	key := normalizeQoderEmail(email)
	if base == nil {
		base = &QoderDevice{}
	}
	realMachineID := strings.TrimSpace(base.MachineID)
	d := &QoderDevice{
		ClientType:  base.ClientType,
		MachineOS:   base.MachineOS,
		MachineType: base.MachineType,
		Version:     base.Version,
		Derived:     true,
		Synthetic:   false,
	}
	d.MachineID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("qoder-device:"+key)).String()
	d.MachineToken = base64.RawURLEncoding.EncodeToString(qoderSHA256("qoder-token:" + key + ":" + realMachineID))
	d.MachineCode = hex.EncodeToString(qoderSHA256("qoder-code:" + key))[:32]
	d.MachineHostname = "DESKTOP-" + strings.ToUpper(hex.EncodeToString(qoderSHA256("qoder-host:" + key))[:7])
	return d
}

// qoderSHA256 返回字符串的 SHA-256 摘要。
func qoderSHA256(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// qoderDeviceForAccount 返回该账号的设备指纹与出口。
//
// 2026-10-24 实测修正：此前为多号同签给每个账号派生「合成 MachineId + 不发
// MachineToken」，结果服务端**只对真实设备指纹下发每日签到活动**（合成设备
// 的活动列表永远没有 CLAIM_BENEFIT，表现为"扫描不到签到活动"）。用真实
// runtime-info 指纹（machineToken/machineCode/machineId 三件套）后活动正常
// 出现并可领取（+100 Credits 实测到账）。
//
// 2026-11-02 实测再修正：设备指纹只是「有没有设备」的开关，**不是每日额度的归属键**。
// 伪造整套 Cosy-* 头（machineToken/machineCode/machineId 全换）服务端照样返回同一
// 条活动；刷新 device token 的响应里更是没有任何 scope/设备字段。真正的归属键是
// **人**（服务端判定码 SAME_PERSON_ALREADY_CLAIMED），与账号、scope 都无关。
// 详见 QoderQuotaGroupKey 与 qoder_person.go。
//
// 现在按 qoder_egress.json 的 device_mode 决定：默认 real（真实指纹，现网行为），
// 显式配置 derived 时才使用派生设备。读取配置失败一律回落到真实设备 ——
// 签到是无人值守路径，不能因为一个坏掉的配置文件全部失败。
func qoderDeviceForAccount(auth *QoderAuth) *QoderDevice {
	email := QoderIdentityKey(auth)
	if dev, err := QoderDeviceForEmail(email); err == nil && dev != nil {
		return dev
	}
	dev := QoderDeviceInfo()
	if dev == nil {
		dev = &QoderDevice{}
	}
	dev.Synthetic = false
	return dev
}
