package minimax

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// 协议转换是这条通道最容易出错的地方（OpenAI ⇄ Anthropic），
// 这里锁死：system 归并、tool_calls/tool_result 映射、默认 max_tokens、
// 非流式响应与流式 SSE 的字段形态。

func TestBuildRequest(t *testing.T) {
	openai := []byte(`{
	  "model":"minimax:MiniMax-M3",
	  "stream":true,
	  "messages":[
	    {"role":"system","content":"你是助手"},
	    {"role":"user","content":"北京天气"},
	    {"role":"assistant","content":"我查一下","tool_calls":[
	      {"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"北京\"}"}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"晴 25℃"}
	  ],
	  "tools":[{"type":"function","function":{"name":"weather","description":"查天气",
	    "parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
	  "tool_choice":"auto"
	}`)

	body, stream, err := BuildRequest(openai, "MiniMax-M3")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if !stream {
		t.Fatal("stream 标记丢失")
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "MiniMax-M3" {
		t.Errorf("model 应为裸名，得到 %v", got["model"])
	}
	if got["max_tokens"].(float64) != 4096 {
		t.Errorf("max_tokens 缺省应补 4096，得到 %v", got["max_tokens"])
	}
	if got["system"] != "你是助手" {
		t.Errorf("system 未归并: %v", got["system"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("应合并成 3 条消息（user/assistant/tool_result），得到 %d: %v", len(msgs), msgs)
	}
	// assistant 的 tool_use
	asst := msgs[1].(map[string]any)
	blocks := asst["content"].([]any)
	hasToolUse := false
	for _, b := range blocks {
		bm := b.(map[string]any)
		if bm["type"] == "tool_use" {
			hasToolUse = true
			if bm["id"] != "call_1" || bm["name"] != "weather" {
				t.Errorf("tool_use 映射错误: %v", bm)
			}
			if in, ok := bm["input"].(map[string]any); !ok || in["city"] != "北京" {
				t.Errorf("工具参数应解析成对象: %v", bm["input"])
			}
		}
	}
	if !hasToolUse {
		t.Error("assistant 的 tool_calls 未转成 tool_use")
	}
	// tool → user/tool_result
	third := msgs[2].(map[string]any)
	if third["role"] != "user" {
		t.Errorf("tool 消息应转成 user，得到 %v", third["role"])
	}
	tb := third["content"].([]any)[0].(map[string]any)
	if tb["type"] != "tool_result" || tb["tool_use_id"] != "call_1" || tb["content"] != "晴 25℃" {
		t.Errorf("tool_result 映射错误: %v", tb)
	}
	// tools 定义
	tools := got["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "weather" || tool["input_schema"] == nil {
		t.Errorf("tools 转换错误: %v", tool)
	}
}

func TestConvertResponse(t *testing.T) {
	raw := []byte(`{
	  "id":"msg_1","type":"message","role":"assistant",
	  "content":[{"type":"text","text":"好的"},{"type":"tool_use","id":"tu_1","name":"weather","input":{"city":"北京"}}],
	  "stop_reason":"tool_use",
	  "usage":{"input_tokens":11,"output_tokens":7}
	}`)
	out, err := ConvertResponse(raw, "minimax:MiniMax-M3", 100)
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	choice := got["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "好的" {
		t.Errorf("文本内容错误: %v", msg["content"])
	}
	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls 数量错误: %v", tcs)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason 应为 tool_calls，得到 %v", choice["finish_reason"])
	}
	usage := got["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 11 || usage["total_tokens"].(float64) != 18 {
		t.Errorf("usage 映射错误: %v", usage)
	}
}

func TestWriteOpenAIStream(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"weather"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"北京\"}"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	rec := httptest.NewRecorder()
	if err := WriteOpenAIStream(rec, strings.NewReader(sse), "minimax:MiniMax-M3", 100); err != nil {
		t.Fatalf("WriteOpenAIStream: %v", err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"role":"assistant"`,
		`"content":"你"`,
		`"content":"好"`,
		`"name":"weather"`,
		`{\"city\":\"北京\"}`,
		`"finish_reason":"tool_calls"`,
		`data: [DONE]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE 输出缺少 %q\n实际:\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type 错误: %q", ct)
	}
}

// TestLoadCredentials 只验证解析逻辑（用临时 auth.json），不碰真实登录态。
func TestLoadCredentials(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/auth.json"
	raw := `{"schemaVersion":1,"records":{"com.minimax.mcode.oauth.prod.cn abc":{"accessToken":"tok-123","refreshToken":"rt-123","expiresAtMs":1792561562136}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINIMAX_AUTH_FILE", path)
	c, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if c.AccessToken != "tok-123" || c.RefreshToken != "rt-123" {
		t.Errorf("解析错误: %+v", c)
	}
	if c.ExpiresAt.IsZero() {
		t.Error("expiresAtMs 未解析")
	}
}
