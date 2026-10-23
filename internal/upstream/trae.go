// trae.go Trae（api.trae.cn）签到：账号读取 + 设备指纹派生 + 状态/签到调用。
//
// 为什么放在网关里：用户此前要同时开「Trae Work 助手」和本面板两个程序、
// 各点一次签到。把 Trae 签到并进面板后只需点一次。
//
// 口径来源：Trae Work 助手内置的 python/auto_checkin.py（同一套端点与请求头），
// 账号数据也直接复用它的 checkin_accounts.json，避免两处维护两份账号。
//
// 端点（POST，body 恒为 {}）：
//   - https://api.trae.cn/trae/api/v2/ug/checkin_credits/status  预检（是否已签、可领积分）
//   - https://api.trae.cn/trae/api/v2/ug/checkin_credits/claim   签到领积分
//
// 鉴权：Authorization: Cloud-IDE-JWT <jwt>（JWT 由 Trae 助手抓取后落盘）。
//
// 设备指纹：device_id / market_user_id / session_id 必须**按 user_id 确定性派生**，
// 与 device_proxy.py / auto_checkin.py 逐字节一致（服务端按设备维度判定，
// 每次随机会导致签到被拒）。派生算法：SHA-256(salt + ":" + seed + uint32BE(i))
// 拼接取前 N 字节（见 traeSeededStream）。
package upstream

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	traeStatusURL = "https://api.trae.cn/trae/api/v2/ug/checkin_credits/status"
	traeClaimURL  = "https://api.trae.cn/trae/api/v2/ug/checkin_credits/claim"
	// traeEntUsageURL 是 Trae IDE 客户端读取「真实积分余额」的接口
	// （客户端 iCubeUsageService.fetchCreditsUsageInfo）：
	//   POST /trae/api/v2/pay/ide_user_ent_usage  body {"require_usage":true}
	// 返回各积分包的 credits_limit 与已用量，客户端据此算标题栏余额。
	traeEntUsageURL = "https://api.trae.cn/trae/api/v2/pay/ide_user_ent_usage"
	// traeEntUsageV1URL 是额度接口的 v1 版本：与 v2（积分包口径）不同，v1 额外下发
	// **模型请求限额**（客户端 ICubeUsageService._extractFastRequestQuota / 请求类目额度）：
	//   premium_model_fast_request_limit  高级模型「快速请求」额度（优先/免排队）
	//   premium_model_slow_request_limit  高级模型「慢速请求」额度（排队请求）
	//   advanced_model_request_limit      高级模型请求额度
	//   auto_completion_limit             自动补全额度
	//   basic_usage_limit / bonus_usage_limit  基础/奖励用额度
	//   solo_agent_parallel_limit         并行（同时进行）上限
	// 对应 usage.premium_model_fast_amount / premium_model_slow_amount 等为已用量。
	// 这是 Trae 唯一暴露的「模型请求排队/限额」数据面。
	traeEntUsageV1URL = "https://api.trae.cn/trae/api/v1/pay/ide_user_ent_usage"
)

// traeAccountsFile 相对 Trae 数据目录的账号文件路径（与 auto_checkin.py 一致）。
const traeAccountsFile = "data/checkin_accounts.json"

// TraeAccount 一个 Trae 账号（来自 checkin_accounts.json）。
//
// 设备三件套优先取文件里的显式字段（Trae 助手已写入），缺失才按 user_id 派生——
// 保证与助手侧写入的值一致，不因算法细节差异导致指纹漂移。
//
// RefreshToken/AddedAt/UpdatedAt 面板不直接使用，但**必须留在结构里**：面板新增
// Trae 账号时要「读-改-写」这个文件，结构缺字段会导致写回时把助手写的值丢掉。
type TraeAccount struct {
	Name         string `json:"name"`
	UserID       string `json:"UserID"`
	JWT          string `json:"jwt"`
	DeviceID     string `json:"device_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	MarketUserID string `json:"market_user_id,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AddedAt      string `json:"added_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// TraeAccountsFile checkin_accounts.json 顶层结构。
type TraeAccountsFile struct {
	Accounts []TraeAccount `json:"accounts"`
}

// LoadTraeAccounts 从 Trae 数据目录读取账号列表。
//
// dataDir 为空时回落 %APPDATA%\TraeWorkAssistant（Trae 助手桌面端默认数据目录）。
// 文件不存在不算错误（返回空列表）：用户可能还没装/还没在 Trae 里加账号。
func LoadTraeAccounts(dataDir string) ([]TraeAccount, error) {
	dir, err := traeDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(traeAccountsFile)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f TraeAccountsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", traeAccountsFile, err)
	}
	out := make([]TraeAccount, 0, len(f.Accounts))
	for _, a := range f.Accounts {
		if strings.TrimSpace(a.JWT) == "" {
			continue // 未配置 jwt 的条目直接跳过（与脚本行为一致）
		}
		if a.UserID == "" {
			a.UserID = TraeUserIDFromJWT(a.JWT)
		}
		out = append(out, a)
	}
	return out, nil
}

// SaveTraeAccount 把账号 upsert 进 checkin_accounts.json（按 UserID 去重），返回是否新增。
//
// **读-改-写**：保留文件里其它账号与其它字段（device_id / added_at / 助手写的 refresh_token
// 等），绝不整体重写覆盖——这个文件是面板与 Trae 助手**共用**的，覆盖会打掉助手的登录态。
// 原子落盘（tmp + rename），避免半截文件让两侧都读崩。
func SaveTraeAccount(dataDir string, acct TraeAccount) (added bool, err error) {
	if strings.TrimSpace(acct.UserID) == "" {
		return false, fmt.Errorf("缺少 UserID，拒绝写入")
	}
	dir, err := traeDataDir(dataDir)
	if err != nil {
		return false, err
	}
	fp := filepath.Join(dir, filepath.FromSlash(traeAccountsFile))
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		return false, err
	}
	var f TraeAccountsFile
	if raw, rerr := os.ReadFile(fp); rerr == nil {
		_ = json.Unmarshal(raw, &f) // 解析失败按空文件处理：下面会整体重写为合法 JSON
	}
	now := time.Now().Format("2006-01-02T15:04:05")
	for i := range f.Accounts {
		if f.Accounts[i].UserID != acct.UserID {
			continue
		}
		// 已存在：只更新凭证，保留既有 name / added_at（避免每次登录都改展示名与加入时间）
		f.Accounts[i].JWT = acct.JWT
		if acct.RefreshToken != "" {
			f.Accounts[i].RefreshToken = acct.RefreshToken
		}
		if acct.Name != "" {
			f.Accounts[i].Name = acct.Name
		}
		f.Accounts[i].UpdatedAt = now
		return false, writeTraeAccounts(fp, f)
	}
	if acct.AddedAt == "" {
		acct.AddedAt = now
	}
	acct.UpdatedAt = now
	f.Accounts = append(f.Accounts, acct)
	return true, writeTraeAccounts(fp, f)
}

func writeTraeAccounts(fp string, f TraeAccountsFile) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, fp)
}

// traeDataDir 解析 Trae 数据目录：显式配置优先，否则用 %APPDATA%\TraeWorkAssistant。
func traeDataDir(configured string) (string, error) {
	if d := strings.TrimSpace(configured); d != "" {
		return d, nil
	}
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return "", fmt.Errorf("未配置 trae.data_dir 且无法读取 APPDATA 环境变量")
	}
	return filepath.Join(appData, "TraeWorkAssistant"), nil
}

// traeUserIDFromJWT 从 JWT payload 取 user id（不校验签名，与脚本同口径）。
// 依次尝试 data.id / auth_id / sub。
func TraeUserIDFromJWT(jwt string) string {
	tok := strings.TrimPrefix(jwt, "Cloud-IDE-JWT ")
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	seg := parts[1]
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		return ""
	}
	var payload struct {
		Data struct {
			ID json.Number `json:"id"`
		} `json:"data"`
		AuthID json.Number `json:"auth_id"`
		Sub    string      `json:"sub"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	if s := payload.Data.ID.String(); s != "" && s != "0" {
		return s
	}
	if s := payload.AuthID.String(); s != "" && s != "0" {
		return s
	}
	return payload.Sub
}

// TraeJWTExpiry 解析 JWT payload 的 exp 声明（不校验签名，与 traeUserIDFromJWT 同口径）。
//
// 用途：把「上游 code=1001 鉴权失败」翻译成可操作的中文提示。JWT 由 Trae 助手经
// 浏览器登录态提取（payload.source=refresh_token），有效期 14 天，**面板无法自行
// 续期** —— 过期后只能回 Trae 助手重新登录。返回 ok=false 表示 payload 无法解析
// （调用方应回落展示上游原始错误）。
func TraeJWTExpiry(jwt string) (time.Time, bool) {
	return JWTExpiry(jwt)
}

// JWTExpiry 解析任意 JWT 的 exp 声明（秒级时间戳）。
// 兼容带 "Cloud-IDE-JWT " 前缀的形态（Trae 凭证）；WorkBuddy 的 accessToken 是
// 裸 JWT，同样适用。返回 ok=false 表示 payload 无法解析或没有有效 exp。
//
// 用途：当上游 expiresIn 缺失/不可信时，JWT 的 exp 是**唯一权威**的过期时刻——
// 登录落盘与刷新都应回落到它，而不是瞎填一个值。
func JWTExpiry(jwt string) (time.Time, bool) {
	tok := strings.TrimPrefix(jwt, "Cloud-IDE-JWT ")
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	seg := parts[1]
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		return time.Time{}, false
	}
	var payload struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return time.Time{}, false
	}
	n, err := payload.Exp.Int64()
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// TraeAuthHint 上游报鉴权失败时，结合本地 JWT 的 exp 给出可操作的中文提示。
// 返回空串 = 不是「本地凭证已过期」这种可识别情形，调用方应回落展示上游原始错误。
func TraeAuthHint(a TraeAccount) string {
	exp, ok := TraeJWTExpiry(a.JWT)
	if !ok || !time.Now().After(exp) {
		return ""
	}
	// 引导走面板自己的「添加账号 → Trae 登录」：登录一次后会把浏览器会话归档，
	// 之后面板每 4 小时自动续期，不必再手动重登（见 panel/trae_refresh.go）。
	return fmt.Sprintf("凭证已过期（%s），请在面板「添加账号 → Trae 登录」重新登录该账号（登录一次后即可自动续期）",
		exp.Local().Format("2006-01-02 15:04"))
}

// traeDevice 设备指纹三件套。
type traeDevice struct {
	DeviceID     string
	MarketUserID string
	SessionID    string
}

// traeSeededStream 确定性派生 nbytes 字节：SHA-256(salt:seed || uint32BE(i)) 串接。
// 与 auto_checkin.py 的 _seeded_stream 逐字节等价（含 i 从 0 起、大端 4 字节）。
func traeSeededStream(seed, salt string, nbytes int) []byte {
	base := []byte(salt + ":" + seed)
	out := make([]byte, 0, nbytes+32)
	var idx [4]byte
	for i := uint32(0); len(out) < nbytes; i++ {
		binary.BigEndian.PutUint32(idx[:], i)
		h := sha256.New()
		h.Write(base)
		h.Write(idx[:])
		out = append(out, h.Sum(nil)...)
	}
	return out[:nbytes]
}

// traeDeviceFor 取账号设备指纹：优先账号文件里的显式值，缺失才派生。
func traeDeviceFor(a TraeAccount) traeDevice {
	d := traeDevice{
		DeviceID:     a.DeviceID,
		MarketUserID: a.MarketUserID,
		SessionID:    a.SessionID,
	}
	if d.DeviceID == "" {
		d.DeviceID = traeRandDigits(15, a.UserID)
	}
	if d.MarketUserID == "" {
		d.MarketUserID = traeMarketUUID(a.UserID)
	}
	if d.SessionID == "" {
		d.SessionID = traeRandHex(64, a.UserID)
	}
	return d
}

// traeRandDigits 派生 n 位数字串（byte % 10）。
func traeRandDigits(n int, seed string) string {
	bs := traeSeededStream(seed, "devid", n+1)
	var sb strings.Builder
	for _, b := range bs[:n] {
		sb.WriteByte(byte('0' + b%10))
	}
	return sb.String()
}

// traeRandHex 派生 n 位十六进制串（n 为奇数时取够 (n+1)/2 字节再截断）。
func traeRandHex(n int, seed string) string {
	bs := traeSeededStream(seed, "sess", (n+1)/2)
	const hexdig = "0123456789abcdef"
	var sb strings.Builder
	for _, b := range bs {
		sb.WriteByte(hexdig[b>>4])
		sb.WriteByte(hexdig[b&0x0f])
	}
	s := sb.String()
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// traeMarketUUID 派生标准 UUID v4（版本位/变体位按 RFC 4122 置位）。
func traeMarketUUID(seed string) string {
	b := traeSeededStream(seed, "market", 16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// traeClient 专用 HTTP 客户端：**禁用代理**。
//
// 与脚本里的 NO_PROXY=* 同因：Trae 助手开启代理时会把系统代理指向本地 MITM
// 端口，进程退出后该端口成死端口，签到会报 WinError 10061 连不上。
// Go 的 DefaultTransport 会读 HTTP_PROXY/HTTPS_PROXY 环境变量，这里显式清空。
var traeClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:               nil,
		MaxIdleConnsPerHost: 4,
	},
}

// traePost 发一次 POST（body 恒为 {}），返回 (httpStatus, 响应体)。
func traePost(url string, a TraeAccount) (int, string) {
	return traePostJSON(url, a, "{}")
}

// traePostJSON 与 traePost 相同的请求头/设备指纹，唯独 body 可自定义。
func traePostJSON(url string, a TraeAccount, payload string) (int, string) {
	dev := traeDeviceFor(a)
	jwt := a.JWT
	if !strings.HasPrefix(jwt, "Cloud-IDE-JWT ") {
		jwt = "Cloud-IDE-JWT " + jwt
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(payload)))
	if err != nil {
		return -1, err.Error()
	}
	// 请求头与 auto_checkin.py 的 _build_headers 保持一致（服务端按这套指纹判设备）。
	h := req.Header
	h.Set("accept", "*/*")
	h.Set("accept-encoding", "gzip, deflate")
	h.Set("accept-language", "zh-CN")
	h.Set("authorization", jwt)
	h.Set("content-type", "application/json")
	h.Set("user-agent", "VSCode 1.107.1 (TRAE SOLO CN)")
	h.Set("x-market-client-id", "VSCode 1.107.1")
	h.Set("x-market-user-id", dev.MarketUserID)
	h.Set("x-user-region", "CN")
	h.Set("x-device-id", dev.DeviceID)
	h.Set("x-lgw-req-sdk-type", "3")
	h.Set("package-type", "stable_cn")
	h.Set("x-request-id", traeRandomUUID())
	h.Set("x-lscbd-aid", "787976")
	h.Set("x-lscbd-platform", "windows")
	h.Set("app-version", "0.1.45")
	h.Set("x-tt-trace-id", "00-"+traeRandHex(16, traeRandomUUID())+"-01")
	h.Set("vscode-sessionid", dev.SessionID)
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "no-cors")
	h.Set("sec-fetch-site", "none")

	resp, err := traeClient.Do(req)
	if err != nil {
		return -1, err.Error()
	}
	defer resp.Body.Close()
	// 请求头里显式声明了 accept-encoding，Go 不会自动解压，需手动处理。
	var rdr io.Reader = resp.Body
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Encoding")), "gzip") {
		gr, gerr := gzip.NewReader(resp.Body)
		if gerr != nil {
			return -1, "解压响应失败: " + gerr.Error()
		}
		defer gr.Close()
		rdr = gr
	}
	body, _ := io.ReadAll(io.LimitReader(rdr, 4<<20))
	return resp.StatusCode, string(body)
}

// traeRandomUUID 生成随机 UUID v4（仅用于 x-request-id / trace-id 这类一次性字段）。
func traeRandomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 随机源不可用时退回时间派生，仍保证唯一性足够
		binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixNano()))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// TraeStatus 预检账号签到状态。
// 返回 checkedIn（今日是否已签）、credits（可领/现有积分）、message、error。
func TraeStatus(a TraeAccount) (checkedIn bool, credits float64, message string, err error) {
	code, body := traePost(traeStatusURL, a)
	if code < 0 {
		return false, 0, "", fmt.Errorf("网络异常: %s", body)
	}
	var resp struct {
		Code      int             `json:"code"`
		CheckedIn bool            `json:"checked_in"`
		Credits   json.RawMessage `json:"credits"`
		Message   string          `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return false, 0, "", fmt.Errorf("HTTP %d 非 JSON 响应: %s", code, traeTruncate(body, 200))
	}
	if resp.Code != 0 {
		// 鉴权失败且本地凭证确已过期 → 给可操作的中文提示，别把上游英文错误透给用户。
		if hint := TraeAuthHint(a); hint != "" {
			return resp.CheckedIn, 0, hint, fmt.Errorf("%s", hint)
		}
		msg := resp.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", code)
		}
		return resp.CheckedIn, 0, msg, fmt.Errorf("上游返回 code=%d: %s", resp.Code, msg)
	}
	// credits 可能是数字或字符串，两种都收
	if len(resp.Credits) > 0 {
		var f float64
		if json.Unmarshal(resp.Credits, &f) == nil {
			credits = f
		} else {
			var s string
			if json.Unmarshal(resp.Credits, &s) == nil {
				fmt.Sscanf(s, "%f", &credits)
			}
		}
	}
	return resp.CheckedIn, credits, resp.Message, nil
}

// TraeCreditPack 单个积分包的展示信息。
type TraeCreditPack struct {
	Desc        string  `json:"desc"`
	Limit       float64 `json:"limit"`
	Used        float64 `json:"used"`
	Remaining   float64 `json:"remaining"`
	Unlimited   bool    `json:"unlimited"`
	EndTime     int64   `json:"end_time"`
	ProductType int     `json:"product_type"`
}

// TraeCreditsUsage 查询账号真实积分余额（来自 IDE 客户端同款接口）。
// remaining = Σ max(credits_limit - used, 0)；limit/used 为各包合计；expireUnix 为最近到期时刻（秒）。
// packs 返回逐包明细，供「积分构成」展示。
func TraeCreditsUsage(a TraeAccount) (remaining, limit, used float64, expireUnix int64, packs []TraeCreditPack, err error) {
	code, body := traePostJSON(traeEntUsageURL, a, `{"require_usage":true}`)
	if code < 0 {
		return 0, 0, 0, 0, nil, fmt.Errorf("网络异常: %s", body)
	}
	var resp struct {
		Code         int    `json:"code"`
		Message      string `json:"message"`
		UsageSummary struct {
			ConsumedAmount float64 `json:"consumed_amount"`
			TotalAmount    float64 `json:"total_amount"`
		} `json:"usage_summary"`
		Packs []struct {
			DisplayDesc string `json:"display_desc"`
			BaseInfo    struct {
				ProductType int   `json:"product_type"`
				EndTime     int64 `json:"end_time"`
				Quota       struct {
					CreditsLimit *float64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return 0, 0, 0, 0, nil, fmt.Errorf("HTTP %d 非 JSON 响应: %s", code, traeTruncate(body, 200))
	}
	if resp.Code != 0 {
		if hint := TraeAuthHint(a); hint != "" {
			return 0, 0, 0, 0, nil, fmt.Errorf("%s", hint)
		}
		msg := resp.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", code)
		}
		return 0, 0, 0, 0, nil, fmt.Errorf("上游返回 code=%d: %s", resp.Code, msg)
	}
	unlimited := false
	now := time.Now().Unix()
	var soonest int64
	for _, p := range resp.Packs {
		l := p.BaseInfo.Quota.CreditsLimit
		cp := TraeCreditPack{
			Desc:        p.DisplayDesc,
			EndTime:     p.BaseInfo.EndTime,
			ProductType: p.BaseInfo.ProductType,
		}
		if l == nil {
			packs = append(packs, cp)
			continue
		}
		cp.Limit = *l
		cp.Used = p.Usage.CreditsAmount
		if *l < 0 {
			// -1 表示不限量（客户端同样按 Infinity 处理）
			cp.Unlimited = true
			unlimited = true
		} else if *l > 0 {
			cp.Remaining = *l - cp.Used
			if cp.Remaining < 0 {
				cp.Remaining = 0
			}
			limit += *l
			used += cp.Used
			remaining += cp.Remaining
			if cp.EndTime > now && (soonest == 0 || cp.EndTime < soonest) {
				soonest = cp.EndTime
			}
		}
		packs = append(packs, cp)
	}
	if unlimited {
		limit = -1
	}
	// usage_summary 更权威（客户端 total_amount 口径），有值就采用。
	if resp.UsageSummary.TotalAmount > 0 {
		limit = resp.UsageSummary.TotalAmount
		remaining = resp.UsageSummary.TotalAmount - resp.UsageSummary.ConsumedAmount
		if remaining < 0 {
			remaining = 0
		}
		used = resp.UsageSummary.ConsumedAmount
	}
	if soonest == 0 {
		// 无未来到期批次时退化为最晚 end_time
		for _, p := range resp.Packs {
			if p.BaseInfo.EndTime > expireUnix {
				expireUnix = p.BaseInfo.EndTime
			}
		}
	} else {
		expireUnix = soonest
	}
	return remaining, limit, used, expireUnix, packs, nil
}

// TraeRequestLimit 一个「模型请求类目」的限额与用量（来自 Trae v1 额度接口）。
type TraeRequestLimit struct {
	Key       string  `json:"key"`       // 类目标识
	Label     string  `json:"label"`     // 中文名
	Limit     float64 `json:"limit"`     // 额度上限（-1 = 不限）
	Used      float64 `json:"used"`      // 已用
	Remaining float64 `json:"remaining"` // 剩余（不限时为 -1）
	Unlimited bool    `json:"unlimited"` // limit == -1
	Enabled   bool    `json:"enabled"`   // 该类目是否启用（配额 > 0 或 usage > 0）
	Parallel  bool    `json:"parallel"`  // true = 并行上限（不是用量类目）
}

// TraeRequestLimits 查账号的**模型请求限额**（v1 额度接口）。
// 返回按 快速请求 / 慢速请求 / 高级模型 / 自动补全 / 基础 / 奖励 / 并行上限 排序的类目表，
// 以及并行上限值 parallelLimit（无则 0）。凭证过期或上游失败返回 error。
//
// 与 TraeCreditsUsage（v2 积分包）互补：v2 给「积分余额」，v1 给「模型请求额度」。
func TraeRequestLimits(a TraeAccount) (limits []TraeRequestLimit, parallelLimit int, err error) {
	code, body := traePostJSON(traeEntUsageV1URL, a, `{"require_usage":true}`)
	if code < 0 {
		return nil, 0, fmt.Errorf("网络异常: %s", body)
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Packs   []struct {
			DisplayDesc string `json:"display_desc"`
			BaseInfo    struct {
				Quota map[string]json.RawMessage `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage map[string]json.RawMessage `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, 0, fmt.Errorf("HTTP %d 非 JSON 响应: %s", code, traeTruncate(body, 200))
	}
	if resp.Code != 0 {
		if hint := TraeAuthHint(a); hint != "" {
			return nil, 0, fmt.Errorf("%s", hint)
		}
		msg := resp.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", code)
		}
		return nil, 0, fmt.Errorf("上游返回 code=%d: %s", resp.Code, msg)
	}
	num := func(m map[string]json.RawMessage, key string) (float64, bool) {
		raw, ok := m[key]
		if !ok {
			return 0, false
		}
		var f float64
		if json.Unmarshal(raw, &f) == nil {
			return f, true
		}
		return 0, false
	}
	// 汇总所有 pack 里的类目额度（通常只有「免费」pack 带这些键）。
	quota := map[string]json.RawMessage{}
	usage := map[string]json.RawMessage{}
	for _, p := range resp.Packs {
		for k, v := range p.BaseInfo.Quota {
			if _, ok := quota[k]; !ok {
				quota[k] = v
			}
		}
		for k, v := range p.Usage {
			if _, ok := usage[k]; !ok {
				usage[k] = v
			}
		}
	}
	type spec struct {
		key, label, usageKey, usageReqKey string
		parallel                          bool
	}
	specs := []spec{
		{"premium_model_fast_request_limit", "高级模型·快速请求", "premium_model_fast_amount", "premium_model_fast_request_usage", false},
		{"premium_model_slow_request_limit", "高级模型·慢速请求", "premium_model_slow_amount", "premium_model_slow_request_usage", false},
		{"advanced_model_request_limit", "高级模型请求", "advanced_model_amount", "advanced_model_request_usage", false},
		{"auto_completion_limit", "自动补全", "auto_completion_amount", "auto_completion_usage", false},
		{"basic_usage_limit", "基础用量", "basic_usage_amount", "", false},
		{"bonus_usage_limit", "奖励用量", "bonus_usage_amount", "", false},
		{"solo_agent_parallel_limit", "并行上限", "", "", true},
	}
	for _, s := range specs {
		lv, lok := num(quota, s.key)
		if !lok {
			continue // 上游没下发该类目 → 不展示（不同账号/套餐类目集合不同）
		}
		item := TraeRequestLimit{Key: s.key, Label: s.label, Parallel: s.parallel}
		if s.parallel {
			parallelLimit = int(lv)
			item.Limit = lv
			item.Enabled = lv > 0
			limits = append(limits, item)
			continue
		}
		used, _ := num(usage, s.usageKey)
		if used == 0 && s.usageReqKey != "" {
			if u2, ok := num(usage, s.usageReqKey); ok {
				used = u2
			}
		}
		item.Limit = lv
		item.Used = used
		if lv < 0 {
			item.Unlimited = true
			item.Remaining = -1
			item.Enabled = true
		} else if lv > 0 {
			item.Remaining = lv - used
			if item.Remaining < 0 {
				item.Remaining = 0
			}
			item.Enabled = true
		} else {
			item.Enabled = used > 0
		}
		limits = append(limits, item)
	}
	return limits, parallelLimit, nil
}

// TraeClaim 执行签到领积分。返回 message 与 error（code != 0 视为业务失败）。
func TraeClaim(a TraeAccount) (message string, err error) {
	code, body := traePost(traeClaimURL, a)
	if code < 0 {
		return "", fmt.Errorf("网络异常: %s", body)
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", fmt.Errorf("HTTP %d 非 JSON 响应: %s", code, traeTruncate(body, 200))
	}
	if resp.Code != 0 {
		if hint := TraeAuthHint(a); hint != "" {
			return hint, fmt.Errorf("%s", hint)
		}
		msg := resp.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", code)
		}
		return msg, fmt.Errorf("上游返回 code=%d: %s", resp.Code, msg)
	}
	return resp.Message, nil
}

// traeTruncate 截断过长响应体用于错误信息（避免把整页 HTML 塞进日志）。
//
// 复用同包 truncate（内部 logfmt.Truncate，切点回退到 UTF-8 rune 边界）——
// 错误 body 多为中文，按字节切会留半截序列、日志里出乱码。截断时补省略号。
func traeTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return truncate(s, n) + "…"
}

// ── Trae 令牌续期（OAuth ExchangeToken）────────────────────────────────────
//
// 背景：Trae 的 access token 就是 Cloud-IDE-JWT，**有效期只有 8 小时**（实测
// iat→exp 恒为 28800s），过期后签到接口一律 401。这是 Trae 服务器签发的，
// 客户端改不了 exp（改了签名即失效）。
//
// 唯一的「延长」手段是**续期**：用长效的 refresh_token 去换一张新的 JWT。
// 面板把这一步自动化（每 4 小时一次），用户就不必反复重新登录。
//
// 口径逐字对齐 Trae Work 助手 src-tauri/src/commands/accounts.rs::refresh_jwt：
//
//	POST https://api.trae.com.cn/cloudide/api/v3/trae/oauth/ExchangeToken
//	{"ClientID":"en1oxy7wnw8j9n","RefreshToken":"<rt>","ClientSecret":"-","UserID":""}
//	→ {"code":0,"data":{"access_token":"...","refresh_token":"..."}}
//
// refresh_token 可能轮换（响应里给新值就替换），所以调用方务必把返回值写回账号。

const (
	traeOAuthExchangeURL = "https://api.trae.com.cn/cloudide/api/v3/trae/oauth/ExchangeToken"
	traeOAuthClientID    = "en1oxy7wnw8j9n"
	traeOAuthClientSecret = "-"
)

// TraeExchangeToken 用 refresh_token 换新的 access token（JWT）。
// 返回的 newRefresh 非空时表示服务端轮换了 refresh_token，调用方需落盘。
func TraeExchangeToken(refreshToken string) (accessToken, newRefresh string, err error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return "", "", fmt.Errorf("该账号没有 refresh_token，无法自动续期（需用浏览器重新登录一次以获取）")
	}
	body, _ := json.Marshal(map[string]string{
		"ClientID":     traeOAuthClientID,
		"RefreshToken": refreshToken,
		"ClientSecret": traeOAuthClientSecret,
		"UserID":       "",
	})
	req, err := http.NewRequest(http.MethodPost, traeOAuthExchangeURL, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "*/*")
	resp, err := traeClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("ExchangeToken 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AccessToken  string `json:"access_token"`
			Token        string `json:"token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("ExchangeToken 响应解析失败: %w（body=%s）", err, traeTruncate(string(raw), 200))
	}
	if out.Code != 0 {
		return "", "", fmt.Errorf("ExchangeToken 失败 (code=%d): %s", out.Code, out.Message)
	}
	at := out.Data.AccessToken
	if at == "" {
		at = out.Data.Token
	}
	if at == "" {
		return "", "", fmt.Errorf("ExchangeToken 响应缺少 access_token（body=%s）", traeTruncate(string(raw), 200))
	}
	if !strings.HasPrefix(at, "Cloud-IDE-JWT ") {
		at = "Cloud-IDE-JWT " + at
	}
	return at, out.Data.RefreshToken, nil
}

// TraeNeedsRefresh 判断账号 JWT 是否「该续期了」：无法解析 exp、或剩余不足 margin。
// 无法解析 exp 的（老账号）一律视为需要续期——续期失败也无害，只是日志里多一条。
func TraeNeedsRefresh(a TraeAccount, margin time.Duration) bool {
	exp, ok := JWTExpiry(a.JWT)
	if !ok {
		return true
	}
	return time.Now().Add(margin).After(exp)
}

// ── Trae 积分（本地快照 + 每日趋势）──────────────────────────────────────────
//
// 上游的积分接口要带有效 JWT，凭证一过期就查不到（面板原本的积分列直接显示 "—"）。
// Trae 助手会在本地留两份快照，凭证不可用时用它们兜底展示：
//
//	data/remaining_credits.json  {"credits":{uid:数值},"expire_times":{uid:unix},"updated_at":…}
//	data/credits_daily.json      {"snapshots":[{date,total,earned,consumed}]}
//
// 注意：这些文件由**助手**写；签到搬到面板后助手不再运行，快照会停在最后一次运行。
// 所以本地值只作兜底（前端标「本地快照」），实时值仍以面板查上游为准。

// TraeCreditsLocal 本地积分快照（每账号当前积分 + 到期时刻）。
type TraeCreditsLocal struct {
	Credits     map[string]float64 `json:"credits"`
	ExpireTimes map[string]int64   `json:"expire_times"`
	UpdatedAt   string             `json:"updated_at"`
}

// TraeCreditsDailySnapshot 某一天的积分汇总（全账号合计，助手口径）。
type TraeCreditsDailySnapshot struct {
	Date     string  `json:"date"`
	Total    float64 `json:"total"`
	Earned   float64 `json:"earned"`
	Consumed float64 `json:"consumed"`
}

// TraeCreditsDaily 每日积分快照序列。
type TraeCreditsDaily struct {
	Snapshots []TraeCreditsDailySnapshot `json:"snapshots"`
}

// readTraeJSONFile 读 Trae 数据目录下的一个 JSON 文件。
// 文件不存在返回 (nil, nil)——用户可能没装助手或从未跑过签到，不是错误。
func readTraeJSONFile(dataDir, name string, out any) error {
	dir, err := traeDataDir(dataDir)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash("data/"+name)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(raw, out)
}

// LoadTraeCreditsLocal 读本地积分快照；文件不存在返回 nil（调用方按「无数据」处理）。
func LoadTraeCreditsLocal(dataDir string) *TraeCreditsLocal {
	var out TraeCreditsLocal
	if err := readTraeJSONFile(dataDir, "remaining_credits.json", &out); err != nil {
		return nil
	}
	if len(out.Credits) == 0 && len(out.ExpireTimes) == 0 {
		return nil
	}
	return &out
}

// LoadTraeCreditsDaily 读每日积分快照；文件不存在返回 nil。
func LoadTraeCreditsDaily(dataDir string) *TraeCreditsDaily {
	var out TraeCreditsDaily
	if err := readTraeJSONFile(dataDir, "credits_daily.json", &out); err != nil {
		return nil
	}
	if len(out.Snapshots) == 0 {
		return nil
	}
	return &out
}
