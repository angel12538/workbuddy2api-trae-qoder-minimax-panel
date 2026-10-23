// Package server 的 OpenAI Responses API 适配层（POST /v1/responses）。
//
// 背景：Codex CLI、OpenAI 官方 SDK 的 responses.create、以及新一代 agent 客户端
// 走的是 /v1/responses 而非 /v1/chat/completions。本网关此前只提供后者，这类
// 客户端拿到的是 404 ——「协议不通」比「功能没做」更致命：客户端根本连不上。
//
// 实现取向：**协议适配，而不是另起一条管线**。请求体翻译成 chat/completions 形态后
// 交回既有的 chatCompletions 处理——选号、会话粘性、熔断冷却、模型级限流、错误
// 分类与降级、请求指标与脱敏日志全部原样复用，不新增第二套状态机，也不出现两套
// 调度口径漂移。响应再从 chat/completions 形态翻译回 Responses 形态。
//
// 翻译只做协议层的事，不发明语义：
//   - 请求：input/instructions/tools 形态差异 → chat 的 messages/tools；
//   - 响应：chat 的 choices[].message → Responses 的 output[] 物品数组；
//   - 流式：chat 的 SSE chunk → Responses 的具名事件（response.*）。
// 凡是协议里没有对应物的字段（如 previous_response_id 对应的上游无状态），
// 用进程内短期存储补齐，让多轮续接可用。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// responsesRequest 是 Responses API 请求的承载结构。
//
// 只声明适配层真正要读的字段；未声明字段不透传（Responses 与 chat 的字段集差异
// 太大，整体透传会把 Responses 专有字段塞进上游请求体）。
type responsesRequest struct {
	Model              string           `json:"model"`
	Input              json.RawMessage  `json:"input"`
	Instructions       string           `json:"instructions"`
	Stream             bool             `json:"stream"`
	MaxOutputTokens    int              `json:"max_output_tokens"`
	Temperature        *float64         `json:"temperature"`
	TopP               *float64         `json:"top_p"`
	Reasoning          json.RawMessage  `json:"reasoning"`
	Tools              []map[string]any `json:"tools"`
	ToolChoice         json.RawMessage  `json:"tool_choice"`
	PreviousResponseID string           `json:"previous_response_id"`
	Conversation       json.RawMessage  `json:"conversation"`
	Metadata           map[string]any   `json:"metadata"`
	Store              *bool            `json:"store"`
	User               string           `json:"user"`
}

// responsesReasoning 是 reasoning 字段的承载（effort → chat 的 reasoning_effort）。
type responsesReasoning struct {
	Effort string `json:"effort"`
}

// responses 处理 POST /v1/responses。
//
// 入口与 /v1/chat/completions 共用鉴权（withAuth）与请求指标口径（ServeHTTP 的
// requestTrace），差别只在本文件内部的协议翻译。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}

	// 上下文装配：previous_response_id 续接的历史 → instructions(system) → 本轮 input。
	msgs := make([]any, 0, 8)
	if req.PreviousResponseID != "" {
		msgs = append(msgs, h.responseStore().get(req.PreviousResponseID)...)
	}
	if s := strings.TrimSpace(req.Instructions); s != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": s})
	}
	inputMsgs, err := responsesInputToMessages(req.Input)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	msgs = append(msgs, inputMsgs...)

	chatBody, err := responsesToChatBody(&req, msgs)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chatBytes, err := json.Marshal(chatBody)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "marshal chat body: "+err.Error())
		return
	}

	// 交给既有 chat 管线处理（鉴权、选号、粘性、熔断、错误分类、指标全部复用）。
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chatBytes))
	r2.ContentLength = int64(len(chatBytes))
	r2.Header.Set("Content-Type", "application/json")

	if req.Stream {
		h.responsesStream(w, r2, &req, msgs)
		return
	}
	h.responsesNonStream(w, r2, &req, msgs)
}

// responsesToChatBody 把 Responses 请求翻译成 chat/completions 请求体。
func responsesToChatBody(req *responsesRequest, msgs []any) (map[string]any, error) {
	out := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if req.MaxOutputTokens > 0 {
		out["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Reasoning) > 0 {
		var rr responsesReasoning
		if err := json.Unmarshal(req.Reasoning, &rr); err == nil && rr.Effort != "" {
			out["reasoning_effort"] = rr.Effort
		}
	}
	if tools := responsesToolsToChat(req.Tools); len(tools) > 0 {
		out["tools"] = tools
	}
	if len(req.ToolChoice) > 0 {
		out["tool_choice"] = responsesToolChoiceToChat(req.ToolChoice)
	}
	// 会话键：Responses 的 conversation 形态与 chat 的 conversation_id 对应。
	// 有 conversation 就带上，让粘性路由能认出同一会话（否则多轮会随机换号）。
	if conv := responsesConversationID(req.Conversation); conv != "" {
		out["conversation_id"] = conv
	}
	return out, nil
}

// responsesConversationID 从 conversation 字段取会话标识。
// 支持字符串形态（"conv_xxx"）与对象形态（{"id":"conv_xxx"}）。
func responsesConversationID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return strings.TrimSpace(obj.ID)
	}
	return ""
}

// responsesToolsToChat 把 Responses 的扁平工具定义转成 chat 的嵌套形态。
//
// Responses: {"type":"function","name":"f","parameters":{...}}
// Chat:      {"type":"function","function":{"name":"f","parameters":{...}}}
//
// 非 function 类型的工具（web_search 等）上游不认，直接丢弃——透传会让上游
// 报 11101 之类的参数错误，比「少一个工具」更难排查。
func responsesToolsToChat(tools []map[string]any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		typ, _ := t["type"].(string)
		if typ != "" && typ != "function" {
			continue
		}
		fn := map[string]any{}
		for _, k := range []string{"name", "description", "parameters", "strict"} {
			if v, ok := t[k]; ok && v != nil {
				fn[k] = v
			}
		}
		if _, ok := fn["name"]; !ok {
			continue // 无名工具上游无法调用
		}
		out = append(out, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	return out
}

// responsesToolChoiceToChat 把 tool_choice 转成 chat 形态。
// 对象形态 {"type":"function","name":"f"} → {"type":"function","function":{"name":"f"}}。
func responsesToolChoiceToChat(raw json.RawMessage) any {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "auto"
	}
	typ, _ := obj["type"].(string)
	if typ != "function" {
		return "auto"
	}
	name, _ := obj["name"].(string)
	if name == "" {
		return "auto"
	}
	return map[string]any{
		"type":     "function",
		"function": map[string]any{"name": name},
	}
}

// responsesInputToMessages 把 Responses 的 input 字段转成 chat 的 messages 数组。
//
// input 有两种形态：
//   - 字符串：等价于一条 user 消息；
//   - 数组：每项是一个 item，常见 type 有 message / function_call /
//     function_call_output / reasoning。
func responsesInputToMessages(raw json.RawMessage) ([]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []any{map[string]any{"role": "user", "content": s}}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or an array of input items")
	}
	msgs := make([]any, 0, len(items))
	for _, it := range items {
		typ, _ := it["type"].(string)
		switch typ {
		case "", "message":
			role, _ := it["role"].(string)
			if role == "" {
				role = "user"
			}
			content, ok := responseItemContent(it["content"])
			if !ok {
				continue
			}
			msgs = append(msgs, map[string]any{"role": role, "content": content})
		case "function_call":
			// Responses 的 function_call 项 → chat 的 assistant.tool_calls。
			callID, _ := it["call_id"].(string)
			name, _ := it["name"].(string)
			args := responseItemText(it["arguments"])
			if name == "" {
				continue
			}
			msgs = append(msgs, map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				}},
			})
		case "function_call_output":
			// Responses 的 function_call_output 项 → chat 的 tool 消息。
			callID, _ := it["call_id"].(string)
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      responseItemText(it["output"]),
			})
		case "reasoning":
			// 推理项：上游无状态可还原，summary 文本降级成一条 assistant 消息保留上下文。
			text := responseItemText(it["summary"])
			if text == "" {
				text = responseItemText(it["content"])
			}
			if text != "" {
				msgs = append(msgs, map[string]any{"role": "assistant", "content": text})
			}
		case "item_reference":
			// 引用既有 item：进程内无该 item 的存储，跳过（不编造内容）。
		default:
			// 未知 type：跳过而不是报错——Responses 的 item 类型在演进，
			// 硬失败会让新客户端完全不可用。
		}
	}
	return msgs, nil
}

// responseItemContent 把 message item 的 content 转成 chat 的 content 形态。
// 返回 (content, 是否产出内容)。
func responseItemContent(raw any) (any, bool) {
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil, false
		}
		return v, true
	case []any:
		texts := strings.Builder{}
		parts := make([]any, 0, len(v))
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			ptyp, _ := pm["type"].(string)
			switch ptyp {
			case "input_text", "output_text", "text":
				if t, _ := pm["text"].(string); t != "" {
					texts.WriteString(t)
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "refusal":
				if t, _ := pm["refusal"].(string); t != "" {
					texts.WriteString(t)
				}
			case "input_image", "image_url":
				// Responses 的 image_url 是字符串；chat 要对象形态。
				url := ""
				if s, ok := pm["image_url"].(string); ok {
					url = s
				} else if om, ok := pm["image_url"].(map[string]any); ok {
					url, _ = om["url"].(string)
				}
				if url == "" {
					// file_id 形态需要先调上传接口才能拿到 URL，适配层不做，丢弃该图。
					continue
				}
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		}
		if len(parts) == 0 {
			return nil, false
		}
		// 纯文本走字符串形态（与上游既有内容形态一致）；含图片走多模态数组形态。
		hasImage := false
		for _, p := range parts {
			if pm, _ := p.(map[string]any); pm["type"] == "image_url" {
				hasImage = true
				break
			}
		}
		if !hasImage {
			return texts.String(), true
		}
		return parts, true
	}
	return nil, false
}

// responseItemText 把可能是字符串/对象数组的字段转成纯文本。
func responseItemText(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := pm["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

// ---------- 非流式 ----------

// captureWriter 收集 handler 写出的响应体（非流式路径用）。
type captureWriter struct {
	h      http.Header
	buf    bytes.Buffer
	status int
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{h: http.Header{}}
}

func (c *captureWriter) Header() http.Header { return c.h }
func (c *captureWriter) Write(p []byte) (int, error) {
	return c.buf.Write(p)
}
func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

// responsesNonStream 处理非流式请求：跑完既有 chat 管线后，把 chat 响应翻成
// Responses 形态再写回客户端。
func (h *Handler) responsesNonStream(w http.ResponseWriter, r *http.Request, req *responsesRequest, msgs []any) {
	cw := newCaptureWriter()
	h.chatCompletions(cw, r)

	body := cw.buf.Bytes()
	status := cw.status
	if status == 0 {
		status = http.StatusOK
	}
	// 错误路径：chat 管线写出的就是 OpenAI 错误体（两套 API 的 error 形状一致），
	// 原样透传——不重写成 Responses 形状，避免丢掉上游原文与 hint。
	if status >= 400 || !json.Valid(body) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", "decode chat response: "+err.Error())
		return
	}
	respObj := chatResponseToResponses(chat, req.Model)
	// 记录到 store：供后续 previous_response_id 续接上下文。
	if id, _ := respObj["id"].(string); id != "" {
		h.responseStore().put(id, append(msgs, responsesAssistantMessage(chat)...))
	}
	out, err := json.Marshal(respObj)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "marshal response: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// responsesAssistantMessage 从 chat 响应还原出一条 assistant 消息（存进 store，
// 供下一轮 previous_response_id 续接）。
func responsesAssistantMessage(chat map[string]any) []any {
	choices, _ := chat["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return nil
	}
	msg, _ := c["message"].(map[string]any)
	if msg == nil {
		return nil
	}
	out := map[string]any{"role": "assistant"}
	if content, ok := msg["content"].(string); ok && content != "" {
		out["content"] = content
	} else {
		out["content"] = nil
	}
	if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
		out["tool_calls"] = tcs
	}
	return []any{out}
}

// chatResponseToResponses 把 chat/completions 响应翻成 Responses 形态。
func chatResponseToResponses(chat map[string]any, model string) map[string]any {
	respID := "resp_" + session.NewMessageID()
	created := time.Now().Unix()

	output := make([]any, 0, 2)
	var text string
	var finish string

	choices, _ := chat["choices"].([]any)
	if len(choices) > 0 {
		c, _ := choices[0].(map[string]any)
		if c != nil {
			finish, _ = c["finish_reason"].(string)
			msg, _ := c["message"].(map[string]any)
			if msg != nil {
				text, _ = msg["content"].(string)
				// 思维链作为独立的 reasoning 物品（Responses 的原生形态）。
				if rc, _ := msg["reasoning_content"].(string); rc != "" {
					output = append(output, map[string]any{
						"type":   "reasoning",
						"id":     "rs_" + session.NewMessageID(),
						"summary": []any{map[string]any{
							"type": "summary_text",
							"text": rc,
						}},
						"status": "completed",
					})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						tcm, _ := tc.(map[string]any)
						if tcm == nil {
							continue
						}
						fn, _ := tcm["function"].(map[string]any)
						callID, _ := tcm["id"].(string)
						name, _ := fn["name"].(string)
						args, _ := fn["arguments"].(string)
						output = append(output, map[string]any{
							"type":      "function_call",
							"id":        "fc_" + session.NewMessageID(),
							"call_id":   callID,
							"name":      name,
							"arguments": args,
							"status":    "completed",
						})
					}
				}
			}
		}
	}
	// message 物品恒在（Responses 语义：助手回合是一个 message 物品）。
	output = append(output, map[string]any{
		"type":   "message",
		"id":     "msg_" + session.NewMessageID(),
		"status": "completed",
		"role":   "assistant",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}},
	})

	status := "completed"
	if finish == "length" {
		status = "incomplete"
	}

	obj := map[string]any{
		"id":                   respID,
		"object":               "response",
		"created_at":           created,
		"status":               status,
		"model":                model,
		"output":               output,
		"output_text":          text,
		"error":                nil,
		"incomplete_details":    nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"metadata":             map[string]any{},
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{"effort": nil, "summary": nil},
		"store":                false,
		"temperature":          nil,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                nil,
		"truncation":           "disabled",
		"user":                 nil,
	}
	if finish == "length" {
		obj["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if u := chatUsageToResponses(chat); u != nil {
		obj["usage"] = u
	}
	return obj
}

// chatUsageToResponses 把 chat 的 usage 转成 Responses 的 usage 形态。
func chatUsageToResponses(chat map[string]any) map[string]any {
	u, _ := chat["usage"].(map[string]any)
	if u == nil {
		return nil
	}
	in := numberOf(u["prompt_tokens"])
	out := numberOf(u["completion_tokens"])
	total := numberOf(u["total_tokens"])
	if total == 0 {
		total = in + out
	}
	res := map[string]any{
		"input_tokens":  in,
		"output_tokens": out,
		"total_tokens":  total,
		"input_tokens_details": map[string]any{
			"cached_tokens": 0,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": 0,
		},
	}
	if hit, ok := upstream.UsageCacheHitTokens(u); ok {
		res["input_tokens_details"] = map[string]any{"cached_tokens": hit}
	}
	return res
}

// numberOf 把 JSON 里的数值（可能是 float64 / json.Number / int）转成 int64。
func numberOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return i
	}
	return 0
}

// ---------- previous_response_id 存储 ----------

// respStore 保存近期响应，供 previous_response_id 续接上下文。
//
// Responses 协议本身有服务端状态（previous_response_id 指回上一轮）；上游
// CodeBuddy 是无状态的，所以适配层在网关侧补这一层。条目有 TTL 与条数上限，
// 不做持久化——重启后 previous_response_id 失效，退化成单轮请求（不报错）。
type respStore struct {
	mu   sync.Mutex
	byID map[string]*storedResp
	ring []string
}

type storedResp struct {
	messages []any
	at       time.Time
}

const (
	respStoreMax  = 128
	respStoreTTL  = time.Hour
	respStoreTrim = 32 // 每次写入顺手裁掉的最老条目数
)

func (s *respStore) put(id string, msgs []any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = make(map[string]*storedResp)
	}
	s.byID[id] = &storedResp{messages: msgs, at: time.Now()}
	s.ring = append(s.ring, id)
	// 裁剪：超上限时从最老开始删（保留最近的连续对话链）。
	for len(s.ring) > respStoreMax {
		oldest := s.ring[0]
		s.ring = s.ring[1:]
		delete(s.byID, oldest)
	}
}

func (s *respStore) get(id string) []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return nil
	}
	if time.Since(rec.at) > respStoreTTL {
		delete(s.byID, id)
		return nil
	}
	return rec.messages
}

// responseStore 返回处理器的响应存储（惰性创建，挂在 Handler 上）。
func (h *Handler) responseStore() *respStore {
	h.respStoreOnce.Do(func() {
		h.respStoreInst = &respStore{byID: make(map[string]*storedResp)}
	})
	return h.respStoreInst
}
