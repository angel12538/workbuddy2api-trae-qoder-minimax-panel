package minimax

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OpenAI /v1/chat/completions 请求 → Anthropic /messages 请求。
//
// 覆盖：system、user/assistant 文本、assistant 的 tool_calls、role=tool 的工具结果、
// tools 定义、tool_choice、max_tokens/temperature/top_p/stop、stream。
// 不支持的多模态（图片/视频）会被忽略并保留文本，避免整请求失败。
func BuildRequest(raw []byte, bareModel string) (body []byte, stream bool, err error) {
	var in struct {
		Model       string           `json:"model"`
		Messages    []map[string]any `json:"messages"`
		MaxTokens   int              `json:"max_tokens"`
		Temperature *float64         `json:"temperature"`
		TopP        *float64         `json:"top_p"`
		Stop        any              `json:"stop"`
		Stream      bool             `json:"stream"`
		Tools       []map[string]any `json:"tools"`
		ToolChoice  any              `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, false, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	model := strings.TrimSpace(bareModel)
	if model == "" {
		model = in.Model
	}

	maxTokens := in.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096 // Anthropic 必填；OpenAI 客户端常省略
	}

	var systemParts []string
	out := make([]map[string]any, 0, len(in.Messages))
	appendMsg := func(role string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		// Anthropic 要求 user/assistant 交替：相邻同角色合并成一条，块按序拼接。
		if n := len(out); n > 0 && out[n-1]["role"] == role {
			prev := out[n-1]["content"].([]map[string]any)
			out[n-1]["content"] = append(prev, blocks...)
			return
		}
		out = append(out, map[string]any{"role": role, "content": blocks})
	}

	for _, m := range in.Messages {
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			systemParts = append(systemParts, textOf(m["content"]))
		case "user":
			appendMsg("user", []map[string]any{{"type": "text", "text": textOf(m["content"])}})
		case "assistant":
			var blocks []map[string]any
			if txt := textOf(m["content"]); txt != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": txt})
			}
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, t := range tcs {
					tc, _ := t.(map[string]any)
					fn, _ := tc["function"].(map[string]any)
					id, _ := tc["id"].(string)
					name, _ := fn["name"].(string)
					args := parseArgs(fn["arguments"])
					blocks = append(blocks, map[string]any{
						"type": "tool_use", "id": id, "name": name, "input": args,
					})
				}
			}
			appendMsg("assistant", blocks)
		case "tool":
			id, _ := m["tool_call_id"].(string)
			appendMsg("user", []map[string]any{{
				"type": "tool_result", "tool_use_id": id, "content": textOf(m["content"]),
			}})
		}
	}

	req := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages":   out,
		"stream":     in.Stream,
	}
	if len(systemParts) > 0 {
		req["system"] = strings.Join(systemParts, "\n\n")
	}
	if in.Temperature != nil {
		req["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		req["top_p"] = *in.TopP
	}
	if stops := stopList(in.Stop); len(stops) > 0 {
		req["stop_sequences"] = stops
	}
	if tools := convertTools(in.Tools); len(tools) > 0 {
		req["tools"] = tools
		if tc := convertToolChoice(in.ToolChoice); tc != nil {
			req["tool_choice"] = tc
		}
	}
	buf, err := json.Marshal(req)
	if err != nil {
		return nil, false, err
	}
	return buf, in.Stream, nil
}

// textOf 把 OpenAI 的 content（字符串或分块数组）压成纯文本。
func textOf(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, part := range c {
			p, _ := part.(map[string]any)
			if p == nil {
				continue
			}
			if t, _ := p["text"].(string); t != "" {
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	return ""
}

// parseArgs 工具参数：OpenAI 是 JSON 字符串，Anthropic 要对象。
func parseArgs(v any) map[string]any {
	switch a := v.(type) {
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(a), &m) == nil && m != nil {
			return m
		}
	case map[string]any:
		return a
	}
	return map[string]any{}
}

// convertTools OpenAI function tools → Anthropic tools。
func convertTools(tools []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		schema := fn["parameters"]
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		item := map[string]any{"name": name, "input_schema": schema}
		if d, _ := fn["description"].(string); d != "" {
			item["description"] = d
		}
		out = append(out, item)
	}
	return out
}

// convertToolChoice OpenAI tool_choice → Anthropic tool_choice。
func convertToolChoice(v any) map[string]any {
	switch c := v.(type) {
	case string:
		switch c {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return nil
		}
	case map[string]any:
		if fn, ok := c["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

func stopList(v any) []string {
	switch s := v.(type) {
	case string:
		if s != "" {
			return []string{s}
		}
	case []any:
		out := make([]string, 0, len(s))
		for _, x := range s {
			if str, ok := x.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// ConvertResponse 非流式：Anthropic message → OpenAI chat.completion。
func ConvertResponse(raw []byte, model string, created int64) ([]byte, error) {
	var in struct {
		ID      string `json:"id"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("上游响应解析失败: %w", err)
	}
	var text strings.Builder
	var toolCalls []map[string]any
	for _, c := range in.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			args := "{}"
			if len(c.Input) > 0 {
				args = string(c.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": args},
			})
		}
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id":      "chatcmpl-" + firstNonEmpty(in.ID, "minimax"),
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0, "message": msg, "finish_reason": mapStopReason(in.StopReason, len(toolCalls) > 0),
		}},
		"usage": map[string]any{
			"prompt_tokens":     in.Usage.InputTokens,
			"completion_tokens": in.Usage.OutputTokens,
			"total_tokens":      in.Usage.InputTokens + in.Usage.OutputTokens,
		},
	}
	return json.Marshal(out)
}

// mapStopReason Anthropic stop_reason → OpenAI finish_reason。
func mapStopReason(r string, hasTools bool) string {
	switch r {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "end_turn", "stop_sequence", "":
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	default:
		return "stop"
	}
}
