package minimax

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// WriteOpenAIStream 把 Anthropic 的 SSE 流实时转成 OpenAI 的 chunk 流。
//
// Anthropic 事件：message_start / content_block_start / content_block_delta /
// content_block_stop / message_delta / message_stop；
// OpenAI 侧：每行 `data: {"choices":[{"delta":…}]}`，最后 `data: [DONE]`。
func WriteOpenAIStream(w http.ResponseWriter, body io.Reader, model string, created int64) error {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	chunkID := fmt.Sprintf("chatcmpl-minimax-%d", created)
	write := func(delta map[string]any, finish any) error {
		payload := map[string]any{
			"id": chunkID, "object": "chat.completion.chunk",
			"created": created, "model": model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		buf, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	if err := write(map[string]any{"role": "assistant", "content": ""}, nil); err != nil {
		return err
	}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)

	toolIdx := map[int]int{} // Anthropic content block index → OpenAI tool_calls index
	nextTool := 0
	stopReason := ""
	sawTool := false

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, ":") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				toolIdx[ev.Index] = nextTool
				sawTool = true
				delta := map[string]any{
					"tool_calls": []map[string]any{{
						"index": nextTool, "id": ev.ContentBlock.ID, "type": "function",
						"function": map[string]any{"name": ev.ContentBlock.Name, "arguments": ""},
					}},
				}
				nextTool++
				if err := write(delta, nil); err != nil {
					return err
				}
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				if err := write(map[string]any{"content": ev.Delta.Text}, nil); err != nil {
					return err
				}
			case "input_json_delta":
				idx := toolIdx[ev.Index]
				delta := map[string]any{
					"tool_calls": []map[string]any{{
						"index":    idx,
						"function": map[string]any{"arguments": ev.Delta.PartialJSON},
					}},
				}
				if err := write(delta, nil); err != nil {
					return err
				}
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
		case "error":
			if ev.Error.Message != "" {
				_ = write(map[string]any{"content": ""}, nil)
				return fmt.Errorf("minimax 流式错误: %s", ev.Error.Message)
			}
		}
	}

	if err := write(map[string]any{}, mapStopReason(stopReason, sawTool)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return sc.Err()
}
