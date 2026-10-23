// Package traechat 把 Trae（CN）的 IDE/Web 模型通道包装成 OpenAI 兼容能力，
// 让本网关的 /v1/chat/completions 也能转发 trae: 前缀的模型。
//
// 为什么走 Remote 通道（而不是 /api/ide/v2/llm_raw_chat）：
//
//	实测 personal（非企业）账号的 Cloud-IDE-JWT 在 IDE raw 通道上会被上游以
//	「2001 failed to get app config: record not found」固定拒绝——那是企业
//	app config / PAT 才有的绑定，与请求体、模型名无关，无法靠重试绕过。
//	Remote 通道（Trae 网页端同源）则对个人账号完全可用，且模型目录、工具、
//	思考强度都和官方客户端一致。
//
// 协议（全部实测确认）：
//
//	GET  {base}/models?functions=solo_agent_remote,solo_work_remote&show_custom_model=true
//	POST {base}/chat_sessions                       → {chat_session_id, message_id}
//	GET  {base}/chat_sessions/{sid}/events?reply_to_message_id={mid}   （SSE）
//	POST {base}/chat_sessions/{sid}/stop            body {chat_session_id,user_message_id}
//
// 鉴权：Authorization: Cloud-IDE-JWT <jwt>，并带 X-Trae-Client-Type / Origin /
// Referer 等浏览器指纹头（缺了上游会 500 或 401）。
//
// 注意并发：上游对每个账号有 solo_agent_parallel_limit（实测 = 2），超出时
// create_session 直接 429 {"code":991502,"reason":"solo_agent_parallel_limit"}。
// 并发控制由 pool.go 负责。
package traechat

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultRemoteBase Trae CN 网页/Remote 通道基址（product.json bootConfig.remote）。
	DefaultRemoteBase = "https://trae-api-cn.mchost.guru/api/remote/v1"
	// DefaultOrigin 网页端 Origin/Referer（product.json bootConfig.homeUrl 同族）。
	DefaultOrigin = "https://solo.trae.cn"
	// DefaultUserAgent 与 Trae 网页端一致的桌面 Chrome UA。
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// AgentTypeAgent/SoloAgent 是 Agent 执行器（1M 上下文档位，工具能力最全）。
	AgentTypeAgent = "solo_agent_remote"
	// AgentTypeWork 是 Work 执行器（200K 档位，作为回落）。
	AgentTypeWork = "solo_work_remote"

	// ModelsQuery 模型目录的查询串（与网页端一致，含三段 function 分组）。
	ModelsQuery = "functions=solo_agent_remote%2Csolo_work_remote%2Csolo_design_remote&show_custom_model=true"
)

// ModelInfo 是 Trae 模型目录里的一条模型。
//
// 字段类型刻意宽松：上游把 context_window_tokens / icon / max_mode 下发成对象或
// 布尔，features 又是「JSON 字符串」，用强类型 unmarshal 会让除个别模型外的整组
// 解析失败（实测只活下来 1 个）。因此这里统一按 map[string]any 解析后再取值，
// 原始对象保留在 raw 里供创建会话时原样回传 custom_model。
type ModelInfo struct {
	Name            string
	DisplayName     string
	ConfigSource    int
	ContextWindow   int64
	Multimodal      bool
	IsPreset        bool
	IsDefault       bool
	IsBeta          bool
	IsNew           bool
	Features        string
	// ConsumptionRate 消耗倍率（features.consumption_rate.data.rate，如 0.8）。
	// 0 表示上游未下发（不参与倍率过滤）。
	ConsumptionRate float64
	// HasConsumptionRate 上游是否明确下发了倍率。
	HasConsumptionRate bool

	raw map[string]any
}

// Raw 返回该模型的原始 JSON 对象（创建会话时作为 custom_model 回传）。
func (m ModelInfo) Raw() map[string]any { return m.raw }

// ID 网关侧的模型 id（"trae:" 前缀由上层补）。
func (m ModelInfo) ID() string { return m.Name }

// creditsLabel 供 /v1/models 的 credits 字段（"x0.80" 形态，与 WorkBuddy 侧一致）。
func (m ModelInfo) CreditsLabel() string {
	if !m.HasConsumptionRate {
		return ""
	}
	return fmt.Sprintf("x%.2f", m.ConsumptionRate)
}

// Client Trae Remote 通道 HTTP 客户端。
type Client struct {
	HTTP        *http.Client
	Base        string
	Origin      string
	UserAgent   string
	ClientType  string
	Language    string
	UserRegion  string
	AppVersion  string
	AppLanguage string
}

// NewClient 构造客户端；零值字段回落到与网页端一致的缺省值。
func NewClient(hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 0}
	}
	return &Client{
		HTTP:        hc,
		Base:        DefaultRemoteBase,
		Origin:      DefaultOrigin,
		UserAgent:   DefaultUserAgent,
		ClientType:  "web",
		Language:    "zh-CN",
		UserRegion:  "CN",
		AppVersion:  "1.0.0.1229",
		AppLanguage: "zh-CN",
	}
}

func (c *Client) base() string {
	if strings.TrimSpace(c.Base) == "" {
		return DefaultRemoteBase
	}
	return strings.TrimRight(c.Base, "/")
}

func (c *Client) origin() string {
	if strings.TrimSpace(c.Origin) == "" {
		return DefaultOrigin
	}
	return strings.TrimRight(c.Origin, "/")
}

// normalizeJWT 统一成 "Cloud-IDE-JWT <token>" 形态。
func normalizeJWT(jwt string) string {
	j := strings.TrimSpace(jwt)
	if j == "" {
		return ""
	}
	if !strings.HasPrefix(j, "Cloud-IDE-JWT ") {
		j = "Cloud-IDE-JWT " + j
	}
	return j
}

// JWTToken 返回不带前缀的裸 token（面板/日志用）。
func JWTToken(jwt string) string { return strings.TrimPrefix(strings.TrimSpace(jwt), "Cloud-IDE-JWT ") }

func (c *Client) headers(jwt, accept string) http.Header {
	o := c.origin()
	lang := c.Language
	if lang == "" {
		lang = "zh-CN"
	}
	region := c.UserRegion
	if region == "" {
		region = "CN"
	}
	ua := c.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	ct := c.ClientType
	if ct == "" {
		ct = "web"
	}
	h := http.Header{}
	h.Set("Authorization", normalizeJWT(jwt))
	h.Set("Content-Type", "application/json")
	h.Set("X-Trae-Client-Type", ct)
	h.Set("X-Preferenced-Language", lang)
	h.Set("x-user-region", region)
	h.Set("Origin", o)
	h.Set("Referer", o+"/")
	h.Set("User-Agent", ua)
	if accept == "" {
		accept = "application/json"
	}
	h.Set("Accept", accept)
	return h
}

// deviceIDFor 在没有账号 device_id 时按 user_id 确定性派生（与 Trae 账号派生口径一致：
// sha256("trae"+uid) 前 32 个 hex 字符取大整数后 % 10^16，左补零到 16 位）。
// 必须确定性——服务端按设备维度判定，随机值会被风控。
func deviceIDFor(userID string) string {
	sum := sha256.Sum256([]byte("trae" + userID))
	hx := hex.EncodeToString(sum[:])
	if len(hx) > 32 {
		hx = hx[:32]
	}
	n := new(big.Int)
	if _, ok := n.SetString(hx, 16); !ok {
		return "0"
	}
	mod := new(big.Int).Exp(big.NewInt(10), big.NewInt(16), nil)
	n.Mod(n, mod)
	return fmt.Sprintf("%016d", n)
}

// stableSessionID 按 (账号, 模型, 变体) 派生一个稳定的 UUID 形式会话键。
func stableSessionID(userID, model, variant string) string {
	seed := strings.Join([]string{userID, model, variant}, "\x1f")
	sum := sha256.Sum256([]byte(seed))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (c *Client) commonParams(userID, deviceID, sessionKey, chatMode string) string {
	if deviceID == "" {
		deviceID = deviceIDFor(userID)
	}
	appVer := c.AppVersion
	if appVer == "" {
		appVer = "1.0.0.1229"
	}
	appLang := c.AppLanguage
	if appLang == "" {
		appLang = "zh-CN"
	}
	if chatMode == "" {
		chatMode = "code"
	}
	params := map[string]any{
		"language":        "zh-cn",
		"app_language":    appLang,
		"quality":         "stable",
		"app_version":     appVer,
		"web_id":          "",
		"user_identity":   "Free",
		"is_freshman":     "0",
		"biz_user_id":     "",
		"user_unique_id":  "",
		"scope":           "marscode-cn",
		"tenant":          "marscode",
		"region":          "cn",
		"aiRegion":        "cn",
		"is_privacy_mode": 0,
		"privacy_mode":    "off",
		"solo_chat_mode":  chatMode,
		"device_id":       deviceID,
	}
	if sessionKey != "" {
		params["biz_session_id"] = sessionKey
	}
	raw, _ := json.Marshal(params)
	return string(raw)
}

// ── 模型目录 ─────────────────────────────────────────────────────────────

type modelsResponse struct {
	Code int `json:"code"`
	Data struct {
		List []struct {
			Function string          `json:"function"`
			Models   []json.RawMessage `json:"models"`
		} `json:"list"`
	} `json:"data"`
	Message string `json:"message"`
}

// Models 拉取模型目录，返回 (agent 档位模型, work 档位模型)。
func (c *Client) Models(jwt string) (agent []ModelInfo, work []ModelInfo, err error) {
	url := c.base() + "/models?" + ModelsQuery
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header = c.headers(jwt, "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, nil, &HTTPError{Status: resp.StatusCode, Body: snippet(body)}
	}
	var out modelsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil, fmt.Errorf("解析 Trae 模型目录失败: %w (body=%s)", err, snippet(body))
	}
	if out.Code != 0 && out.Code != 200 {
		return nil, nil, fmt.Errorf("Trae 模型目录返回 code=%d: %s", out.Code, out.Message)
	}
	for _, grp := range out.Data.List {
		for _, raw := range grp.Models {
			obj := map[string]any{}
			if err := json.Unmarshal(raw, &obj); err != nil {
				continue
			}
			name, _ := obj["name"].(string)
			if strings.TrimSpace(name) == "" {
				continue
			}
			m := ModelInfo{
				Name:         name,
				DisplayName:  strOr(obj["display_name"], name),
				ConfigSource: intOr(obj["config_source"], 1),
				Multimodal:   boolOf(obj["multimodal"]),
				IsPreset:     boolOf(obj["is_preset"]),
				IsDefault:    boolOf(obj["is_default"]),
				IsBeta:       boolOf(obj["is_beta"]),
				IsNew:        boolOf(obj["is_new"]),
				Features:     strOr(obj["features"], ""),
				ContextWindow: ctxTokensOf(obj["context_window_tokens"]),
				raw:          obj,
			}
			if r, ok := consumptionRateOf(m.Features); ok {
				m.ConsumptionRate = r
				m.HasConsumptionRate = true
			}
			switch strings.ToLower(strings.TrimSpace(grp.Function)) {
			case AgentTypeAgent:
				agent = append(agent, m)
			case AgentTypeWork:
				work = append(work, m)
			}
		}
	}
	return agent, work, nil
}

// ctxTokensOf 解析 context_window_tokens：可能是对象 {"dev":N,"max":M} 或数字。
func ctxTokensOf(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case map[string]any:
		// 优先 max，其次 dev（1M 档位取 max 更贴近能力上限）
		if n := intOr(t["max"], 0); n > 0 {
			return int64(n)
		}
		return int64(intOr(t["dev"], 0))
	}
	return 0
}

// consumptionRateOf 从 features（JSON 字符串）里取 consumption_rate.data.rate。
// 这是 Trae 客户端用来算积分消耗的倍率，本网关据此做「太贵的模型不暴露」过滤。
func consumptionRateOf(features string) (float64, bool) {
	s := strings.TrimSpace(features)
	if s == "" {
		return 0, false
	}
	var f struct {
		ConsumptionRate struct {
			Enable bool `json:"enable"`
			Data   struct {
				Rate float64 `json:"rate"`
			} `json:"data"`
		} `json:"consumption_rate"`
	}
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return 0, false
	}
	if !f.ConsumptionRate.Enable {
		return 0, false
	}
	return f.ConsumptionRate.Data.Rate, true
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// ── 会话创建 / 事件流 / 停止 ───────────────────────────────────────────────

// CreateRequest 创建一次 Trae 会话所需的全部入参。
type CreateRequest struct {
	JWT         string
	UserID      string
	DeviceID    string
	Model       ModelInfo
	AgentType   string // 空 = solo_agent_remote
	Query       string // 已 flatten 的 parts JSON
	ChatMode    string // "code" / "work"
	SessionKey  string // 稳定会话键（biz_session_id）
	MaxTokens   int64  // >0 写入 max_tokens
}

type createResponse struct {
	Code int `json:"code"`
	Data struct {
		ChatSessionID string `json:"chat_session_id"`
		MessageID     string `json:"message_id"`
		Status        int    `json:"status"`
	} `json:"data"`
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
}

// CreateSession 创建会话并返回 (chat_session_id, message_id)。
//
// strategy=manual：把模型目录里的原始对象作为 custom_model 回传，让上游精确绑定
// 我们请求的模型（不传会被服务端静默回落到默认模型）。
func (c *Client) CreateSession(req CreateRequest) (sessionID, messageID string, status int, err error) {
	if normalizeJWT(req.JWT) == "" {
		return "", "", 0, fmt.Errorf("缺少 Trae 凭证（JWT）")
	}
	agent := req.AgentType
	if agent == "" {
		agent = AgentTypeAgent
	}
	cm := map[string]any{}
	for k, v := range req.Model.Raw() {
		cm[k] = v
	}
	name := req.Model.Name
	if _, ok := cm["config_name"]; !ok || cm["config_name"] == "" {
		cm["config_name"] = name
	}
	if _, ok := cm["model_name"]; !ok || cm["model_name"] == "" {
		cm["model_name"] = name
	}
	if cs, ok := cm["config_source"]; !ok || cs == nil {
		cm["config_source"] = 1
	}
	if _, ok := cm["provider"]; !ok {
		cm["provider"] = ""
	}
	cm["multimodal"] = req.Model.Multimodal
	cm["is_preset"] = true
	if _, ok := cm["use_remote_service"]; !ok {
		cm["use_remote_service"] = true
	}

	initial := map[string]any{
		"chat_session_id":         "",
		"content":                 []any{},
		"query":                   req.Query,
		"model_name":              name,
		"agent_type":              agent,
		"agent_id":                agent,
		"model_selection_strategy": "manual",
		"model_config_source":     intOr(cm["config_source"], 1),
		"model_is_preset":         true,
		"model_provider":          strOr(cm["provider"], ""),
		"custom_model":            cm,
		"common_params": c.commonParams(
			req.UserID, req.DeviceID, req.SessionKey, req.ChatMode),
	}
	if req.MaxTokens > 0 {
		initial["max_tokens"] = req.MaxTokens
	}
	body := map[string]any{
		"mode":                "code",
		"environment_id":      "default",
		"initial_message":     initial,
		"env":                 "remote",
		"auto_create_project": false,
		"origin":              "web",
	}
	if strings.EqualFold(req.ChatMode, "work") {
		body["mode"] = "work"
	}

	raw, _ := json.Marshal(body)
	hr, err := http.NewRequest(http.MethodPost, c.base()+"/chat_sessions", bytes.NewReader(raw))
	if err != nil {
		return "", "", 0, err
	}
	hr.Header = c.headers(req.JWT, "application/json")
	resp, err := c.HTTP.Do(hr)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, &HTTPError{Status: resp.StatusCode, Body: snippet(rb)}
	}
	var out createResponse
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", "", 0, fmt.Errorf("解析创建会话响应失败: %w (body=%s)", err, snippet(rb))
	}
	if out.Data.ChatSessionID == "" || out.Data.MessageID == "" {
		return "", "", 0, fmt.Errorf("创建会话失败 code=%d %s %s", out.Code, out.Message, snippet(out.Error))
	}
	return out.Data.ChatSessionID, out.Data.MessageID, out.Data.Status, nil
}

// EventTrae 是 Trae 事件流里的一条事件。
type EventTrae struct {
	Name string
	Data []byte
}

// StreamEvents 连接事件流并把每条事件回调出去；ctx 取消即断开。
//
// 注意：Trae 的 plan_item 是**累积快照**（每帧都带完整 thought / reasoning_content），
// 增量差分由 relay.go 负责。
func (c *Client) StreamEvents(jwt, sessionID, messageID string, onEvent func(EventTrae) error) error {
	url := fmt.Sprintf("%s/chat_sessions/%s/events?reply_to_message_id=%s",
		c.base(), sessionID, messageID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header = c.headers(jwt, "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return &HTTPError{Status: resp.StatusCode, Body: snippet(b)}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var event string
	var data bytes.Buffer
	flush := func() error {
		if event == "" && data.Len() == 0 {
			return nil
		}
		ev := EventTrae{Name: event, Data: append([]byte(nil), data.Bytes()...)}
		if ev.Name == "" {
			ev.Name = "message"
		}
		event = ""
		data.Reset()
		return onEvent(ev)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(line[len("data:"):]))
		default:
			// id: / 注释行忽略
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

// Stop 尽力停止一次会话（释放上游并发名额）。失败只返回错误，调用方通常忽略。
func (c *Client) Stop(jwt, sessionID, messageID string) error {
	if sessionID == "" || messageID == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{
		"chat_session_id": sessionID,
		"user_message_id": messageID,
	})
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/chat_sessions/%s/stop", c.base(), sessionID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = c.headers(jwt, "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}

// ── 小工具 ───────────────────────────────────────────────────────────────

// HTTPError 带状态的传输层错误。
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Trae 上游 HTTP %d: %s", e.Status, e.Body)
}

// IsParallelLimit 判定错误是否为「并行/排队超限」（上游 429 + 991502）。
func IsParallelLimit(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "solo_agent_parallel_limit") ||
		strings.Contains(s, "991502") ||
		strings.Contains(s, "parallel limit")
}

// IsUnauthorized 判定错误是否为凭证失效。
func IsUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	if he, ok := err.(*HTTPError); ok && (he.Status == 401 || he.Status == 403) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "HTTP 401") || strings.Contains(s, "HTTP 403") ||
		strings.Contains(s, "unauthorized")
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func intOr(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	}
	return def
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

// timeoutHint 暴露给上层的建议超时（Remote 首帧可能排队数十秒）。
const timeoutHint = 120 * time.Second
