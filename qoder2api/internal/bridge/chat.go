package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"qoder2api/internal/cosy"
	"strings"
	"time"

	"qoder2api/logger"
)

func (b *Bridge) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		WriteErr(w, err)
		return
	}

	reqID := cosy.NewUUID()[:8]
	logger.Info("[Chat][%s] 收到请求 %s %s body_size=%d", reqID, r.Method, r.URL.Path, len(body))
	logger.Debug("[Chat][%s] 请求体（敏感字段已脱敏）: %s", reqID, RedactRequestBodyJSON(body))

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		WriteErr(w, err)
		return
	}
	stream, _ := req["stream"].(bool)
	model := StrValDefault(req, "model", "auto")
	incomingMsgs, _ := req["messages"].([]interface{})
	tools := req["tools"]
	toolsEnabled := tools != nil

	logger.Info("[Chat][%s] model=%s stream=%v tools=%v msgs=%d", reqID, model, stream, toolsEnabled, len(incomingMsgs))

	prompt := ExtractLatestUserPrompt(incomingMsgs)
	messages := BuildQoderMessages(b.templateMessages(), incomingMsgs, prompt, toolsEnabled)

	reqId := "chatcmpl-" + cosy.NewRequestID()
	created := cosy.UnixSec()
	ctx := r.Context()

	agentType := InferAgent(model)
	mappedModel := MapModel(agentType, model)
	effort := b.clampEffortForModel(mappedModel, requestedEffort(req))

	if stream {
		// 延迟写 SSE 响应头：只有真正产出了首段内容才写 200。
		// 上游排队/鉴权等错误若发生在首个 delta 之前，仍能按真实 HTTP 状态
		// （503/403/502）返回，客户端不会把失败误当成 200 流式响应。
		var wroteStream bool
		flusher, _ := w.(http.Flusher)
		startSSE := func() {
			if wroteStream {
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			wroteStream = true
		}

		var toolCallBuf []interface{}
		var totalInputTokens, totalOutputTokens int

		err = b.CallQoderWithOpts(ctx, agentType, messages, model, tools, CallOpts{Effort: effort}, func(d Delta) {
			if d.Err != nil {
				logger.Error("[Chat][%s] upstream error in stream callback: %v", reqID, d.Err)
				return
			}
			if d.InputTokens > 0 || d.OutputTokens > 0 {
				totalInputTokens = d.InputTokens
				totalOutputTokens = d.OutputTokens
			}
			// 无实际内容的帧（如仅 usage）不触发响应头，也不写出空 chunk。
			if d.Content == "" && d.Reasoning == "" && d.ToolCalls == nil {
				return
			}
			startSSE()
			chunk := MakeChatChunk(reqId, created, model)
			choices := chunk["choices"].([]interface{})
			delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
			if d.Content != "" {
				delta["role"] = "assistant"
				delta["content"] = d.Content
			}
			if d.Reasoning != "" {
				delta["reasoning_content"] = d.Reasoning
			}
			if d.ToolCalls != nil {
				delta["tool_calls"] = d.ToolCalls
				toolCallBuf = append(toolCallBuf, d.ToolCalls...)
			}
			data, err := json.Marshal(chunk)
			if err != nil {
				logger.Error("[Chat][%s] marshal chunk failed: %v", reqID, err)
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			if flusher != nil {
				flusher.Flush()
			}
		})
		if err != nil {
			logger.Error("[Chat][%s] stream 请求失败: %v (耗时 %dms)", reqID, err, time.Since(startTime).Milliseconds())
			if !wroteStream {
				// 尚未写出任何内容：直接按错误分类返回真实 HTTP 状态，
				// 避免客户端收到「200 + error 帧」的伪成功响应。
				WriteErr(w, err)
				return
			}
			errMsg, errType := FriendlyError(err)
			errData, _ := json.Marshal(map[string]interface{}{
				"error": map[string]interface{}{"message": errMsg, "type": errType},
			})
			fmt.Fprintf(w, "data: %s\n\n", string(errData))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		startSSE()
		finishReason := "stop"
		if len(toolCallBuf) > 0 {
			finishReason = "tool_calls"
		}
		done := MakeChatChunk(reqId, created, model)
		choices := done["choices"].([]interface{})
		ch := choices[0].(map[string]interface{})
		ch["finish_reason"] = finishReason
		ch["delta"] = map[string]interface{}{}
		if totalInputTokens > 0 || totalOutputTokens > 0 {
			done["usage"] = map[string]interface{}{
				"prompt_tokens":     totalInputTokens,
				"completion_tokens": totalOutputTokens,
				"total_tokens":      totalInputTokens + totalOutputTokens,
			}
		}
		data, _ := json.Marshal(done)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", string(data))
		if flusher != nil {
			flusher.Flush()
		}
		logger.Info("[Chat][%s] stream 完成 finish=%s tool_calls=%d 耗时=%dms", reqID, finishReason, len(toolCallBuf), time.Since(startTime).Milliseconds())
	} else {
		var full strings.Builder
		var reasoning strings.Builder
		var toolCallBuf []interface{}
		var totalInputTokens, totalOutputTokens int
		err = b.CallQoderWithOpts(ctx, agentType, messages, model, tools, CallOpts{Effort: effort}, func(d Delta) {
			if d.InputTokens > 0 || d.OutputTokens > 0 {
				totalInputTokens = d.InputTokens
				totalOutputTokens = d.OutputTokens
			}
			if d.Content != "" {
				full.WriteString(d.Content)
			}
			if d.Reasoning != "" {
				reasoning.WriteString(d.Reasoning)
			}
			if d.ToolCalls != nil {
				toolCallBuf = append(toolCallBuf, d.ToolCalls...)
			}
		})
		if err != nil {
			logger.Error("[Chat][%s] 请求失败: %v (耗时 %dms)", reqID, err, time.Since(startTime).Milliseconds())
			WriteErr(w, err)
			return
		}
		finishReason := "stop"
		msg := map[string]interface{}{"role": "assistant", "content": full.String()}
		if reasoning.Len() > 0 {
			msg["reasoning_content"] = reasoning.String()
		}
		if len(toolCallBuf) > 0 {
			finishReason = "tool_calls"
			msg["tool_calls"] = toolCallBuf
			if full.Len() == 0 {
				msg["content"] = nil
			}
		}
		resp := map[string]interface{}{
			"id": reqId, "object": "chat.completion",
			"created": created, "model": model,
			"choices": []interface{}{
				map[string]interface{}{"index": 0, "message": msg, "finish_reason": finishReason},
			},
			"usage": map[string]interface{}{"prompt_tokens": totalInputTokens, "completion_tokens": totalOutputTokens, "total_tokens": totalInputTokens + totalOutputTokens},
		}
		logger.Info("[Chat][%s] 完成 finish=%s content_len=%d tool_calls=%d 耗时=%dms", reqID, finishReason, full.Len(), len(toolCallBuf), time.Since(startTime).Milliseconds())
		logger.Debug("[Chat][%s] 响应体: %s", reqID, func() string { d, _ := json.Marshal(resp); return string(d) }())
		WriteJSON(w, resp)
	}
}

// requestedEffort 从 OpenAI / Responses / Anthropic 三种请求形态提取思考档位。
func requestedEffort(req map[string]interface{}) string {
	for _, k := range []string{"reasoning_effort", "reasoningEffort", "thinking_effort"} {
		if s, ok := req[k].(string); ok && strings.TrimSpace(s) != "" {
			return normalizeEffortName(s)
		}
	}
	if r, ok := req["reasoning"].(map[string]interface{}); ok {
		if s, ok := r["effort"].(string); ok && strings.TrimSpace(s) != "" {
			return normalizeEffortName(s)
		}
	}
	if t, ok := req["thinking"].(map[string]interface{}); ok {
		if s, ok := t["effort"].(string); ok && strings.TrimSpace(s) != "" {
			return normalizeEffortName(s)
		}
		if s, ok := t["level"].(string); ok && strings.TrimSpace(s) != "" {
			return normalizeEffortName(s)
		}
	}
	return ""
}

// normalizeEffortName 统一档位别名。
func normalizeEffortName(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "minimal", "min", "light":
		return "low"
	case "mid", "normal":
		return "medium"
	case "extra_high", "extra-high", "very_high", "very-high", "x-high":
		return "xhigh"
	case "maximum", "ultra":
		return "max"
	case "off", "none":
		return "none"
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// clampEffortForModel 按模型 thinking_config 声明的档位钳制请求档位。
// 模型不支持思考 → 忽略；目录不可用 → 原样透传（交给上游判定）。
func (b *Bridge) clampEffortForModel(model, effort string) string {
	if effort == "" {
		return ""
	}
	var found *QoderModel
	for _, m := range b.modelCatalog() {
		if strings.EqualFold(m.Key, model) {
			mm := m
			found = &mm
			break
		}
	}
	if found == nil {
		return effort
	}
	if !found.SupportsThinking {
		return ""
	}
	if effort == "none" {
		return "none"
	}
	levels := found.ThinkingEfforts
	if len(levels) == 0 {
		return effort
	}
	r := effortRank(effort)
	if r < 0 {
		return ""
	}
	best := ""
	for _, lv := range levels {
		if effortRank(lv) <= r && (best == "" || effortRank(lv) > effortRank(best)) {
			best = lv
		}
	}
	if best == "" {
		best = levels[0]
	}
	return best
}

func ExtractLatestUserPrompt(msgs []interface{}) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]interface{})
		if m["role"] == "user" {
			return NormalizeMessageContent(m)
		}
	}
	return ""
}

func MakeChatChunk(id string, created int64, model string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "object": "chat.completion.chunk",
		"created": created, "model": model,
		"choices": []interface{}{
			map[string]interface{}{"index": 0, "delta": map[string]interface{}{}, "finish_reason": nil},
		},
	}
}

func (b *Bridge) templateMessages() []interface{} {
	if msgs, ok := b.templateBase["messages"].([]interface{}); ok {
		return msgs
	}
	return nil
}

func WriteJSON(w http.ResponseWriter, v interface{}) {
	data, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(data)
}

func WriteErr(w http.ResponseWriter, err error) {
	// 结构化上游错误 → 友好中文消息 + 分类类型（内容审核/瞬时/普通）
	errMsg, errType := FriendlyError(err)
	if errType == "" {
		errType = "qoder_error"
	}
	body, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{"message": errMsg, "type": errType},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ErrorStatus(err))
	w.Write(body)
}
