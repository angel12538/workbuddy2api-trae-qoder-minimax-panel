package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// responsesSSE 是上游 chat/completions 的标准 SSE 片段（文本 + 收尾）。
const responsesSSE = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
	"data: [DONE]\n\n"

// responsesToolSSE 是上游返回一次函数调用的 SSE 片段。
const responsesToolSSE = "data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_abc\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"SH\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

// capturingUpstream 记录出站请求体（供 previous_response_id 续接断言）。
type capturingUpstream struct {
	mu    sync.Mutex
	bodies [][]byte
}

func (c *capturingUpstream) lastBody() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return nil
	}
	var obj map[string]any
	_ = json.Unmarshal(c.bodies[len(c.bodies)-1], &obj)
	return obj
}

func newCapturingUpstream(t *testing.T, cap *capturingUpstream, status int, body string, isStream bool) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Body != nil {
				b, _ := io.ReadAll(r.Body)
				cap.mu.Lock()
				cap.bodies = append(cap.bodies, b)
				cap.mu.Unlock()
			}
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func responsesTestHandler(t *testing.T, up *upstream.Client) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	return NewHandler(Config{Pool: p, Upstream: up})
}

// TestResponsesNonStreamShape 非流式：Responses 请求 → Responses 响应对象。
func TestResponsesNonStreamShape(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, responsesSSE, true
	})
	h := responsesTestHandler(t, up)

	body := `{"model":"glm-5.2","input":"你好"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(body))))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "response" {
		t.Errorf("object=%v want response", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Errorf("status=%v want completed", resp["status"])
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("model=%v want glm-5.2", resp["model"])
	}
	id, _ := resp["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Errorf("id=%q want prefix resp_", id)
	}
	// output 必须是物品数组：message 物品 + output_text 内容。
	out, _ := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output len=%d want 1 (body=%s)", len(out), rec.Body)
	}
	item, _ := out[0].(map[string]any)
	if item["type"] != "message" {
		t.Fatalf("output[0].type=%v want message", item["type"])
	}
	if item["role"] != "assistant" {
		t.Errorf("output[0].role=%v want assistant", item["role"])
	}
	contents, _ := item["content"].([]any)
	if len(contents) != 1 {
		t.Fatalf("content len=%d want 1", len(contents))
	}
	part, _ := contents[0].(map[string]any)
	if part["type"] != "output_text" {
		t.Errorf("content[0].type=%v want output_text", part["type"])
	}
	if part["text"] != "你好" {
		t.Errorf("content[0].text=%v want 你好", part["text"])
	}
	// usage：chat 的 prompt/completion → Responses 的 input/output。
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("usage missing (body=%s)", rec.Body)
	}
	if usage["input_tokens"] != float64(3) {
		t.Errorf("input_tokens=%v want 3", usage["input_tokens"])
	}
	if usage["output_tokens"] != float64(2) {
		t.Errorf("output_tokens=%v want 2", usage["output_tokens"])
	}
	if usage["total_tokens"] != float64(5) {
		t.Errorf("total_tokens=%v want 5", usage["total_tokens"])
	}
}

// TestResponsesStreamEvents 流式：chat SSE → Responses 具名事件。
func TestResponsesStreamEvents(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, responsesSSE, true
	})
	h := responsesTestHandler(t, up)

	body := `{"model":"glm-5.2","input":"你好","stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(body))))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	text := rec.Body.String()

	// 事件顺序：created → in_progress → output_item.added → content_part.added
	// → output_text.delta → … → completed。
	for _, want := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in stream:\n%s", want, text)
		}
	}
	// delta 必须带上正文。
	if !strings.Contains(text, `"delta":"你好"`) {
		t.Errorf("delta payload missing text:\n%s", text)
	}
	// 每个事件都要带 sequence_number（Responses 协议要求，客户端据此排序）。
	var seqSeen int
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("bad event json: %v line=%s", err, line)
		}
		if _, ok := ev["sequence_number"]; !ok {
			t.Errorf("event %v missing sequence_number", ev["type"])
		}
		seqSeen++
	}
	if seqSeen == 0 {
		t.Fatalf("no events emitted:\n%s", text)
	}
	// completed 事件里的 response 必须带完整 output。
	idx := strings.Index(text, "event: response.completed")
	if idx < 0 {
		t.Fatalf("no completed event:\n%s", text)
	}
	rest := text[idx:]
	line := ""
	for _, l := range strings.Split(rest, "\n") {
		if strings.HasPrefix(l, "data: ") {
			line = strings.TrimPrefix(l, "data: ")
			break
		}
	}
	var done map[string]any
	if err := json.Unmarshal([]byte(line), &done); err != nil {
		t.Fatalf("completed payload: %v", err)
	}
	inner, _ := done["response"].(map[string]any)
	if inner == nil {
		t.Fatalf("completed event missing response: %s", line)
	}
	if inner["status"] != "completed" {
		t.Errorf("completed response status=%v", inner["status"])
	}
	if out, _ := inner["output"].([]any); len(out) != 1 {
		t.Errorf("completed output len=%d want 1", len(out))
	}
}

// TestResponsesToolCalls 工具调用：chat 的 tool_calls → Responses 的 function_call 物品。
func TestResponsesToolCalls(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, responsesToolSSE, true
	})
	h := responsesTestHandler(t, up)

	body := `{"model":"glm-5.2","input":"北京天气？","stream":true,` +
		`"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(body))))

	text := rec.Body.String()
	for _, want := range []string{
		"event: response.output_item.added",
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in stream:\n%s", want, text)
		}
	}
	// 完整参数必须拼接正确（两个分片 → {"city":"SH"}）。
	if !strings.Contains(text, `{\"city\":\"SH\"}`) {
		t.Errorf("merged arguments missing:\n%s", text)
	}
	if !strings.Contains(text, `"name":"get_weather"`) {
		t.Errorf("function name missing:\n%s", text)
	}
}

// TestResponsesInputToMessages input 形态翻译（纯函数级）。
func TestResponsesInputToMessages(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int
	}{
		{"纯字符串", `"你好"`, 1},
		{"空数组", `[]`, 0},
		{"message 数组", `[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`, 1},
		{"函数调用与结果", `[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
			`{"type":"function_call_output","call_id":"c1","output":"ok"}]`, 2},
		{"未知类型跳过", `[{"type":"weird_thing"}]`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs, err := responsesInputToMessages(json.RawMessage(tc.input))
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if len(msgs) != tc.want {
				t.Errorf("msgs len=%d want %d (%+v)", len(msgs), tc.want, msgs)
			}
		})
	}
}

// TestResponsesToolsToChat 工具定义形态翻译：扁平 → 嵌套。
func TestResponsesToolsToChat(t *testing.T) {
	in := []map[string]any{
		{"type": "function", "name": "f", "description": "d", "parameters": map[string]any{"type": "object"}},
		{"type": "web_search"},
	}
	out := responsesToolsToChat(in)
	if len(out) != 1 {
		t.Fatalf("tools len=%d want 1 (非 function 工具应丢弃）", len(out))
	}
	m, _ := out[0].(map[string]any)
	if m["type"] != "function" {
		t.Errorf("type=%v", m["type"])
	}
	fn, _ := m["function"].(map[string]any)
	if fn == nil || fn["name"] != "f" {
		t.Errorf("function=%v want name f", fn)
	}
}

// TestResponsesPreviousResponseID previous_response_id 续接：第二轮请求带上第一轮历史。
func TestResponsesPreviousResponseID(t *testing.T) {
	cap := &capturingUpstream{}
	up := newCapturingUpstream(t, cap, 200, responsesSSE, true)
	h := responsesTestHandler(t, up)

	// 第一轮
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest("POST", "/v1/responses",
		bytes.NewReader([]byte(`{"model":"glm-5.2","input":"第一句"}`))))
	var r1 map[string]any
	if err := json.Unmarshal(rec1.Body.Bytes(), &r1); err != nil {
		t.Fatalf("first unmarshal: %v", err)
	}
	prevID, _ := r1["id"].(string)
	if prevID == "" {
		t.Fatalf("no response id: %s", rec1.Body)
	}

	// 第二轮：带 previous_response_id
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/responses",
		bytes.NewReader([]byte(`{"model":"glm-5.2","input":"第二句","previous_response_id":"`+prevID+`"}`))))

	// 出站 chat body 的 messages 必须是：第一轮 user + 第一轮 assistant + 第二轮 user。
	out := cap.lastBody()
	if out == nil {
		t.Fatalf("no outbound body captured")
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len=%d want 3 (%+v)", len(msgs), msgs)
	}
	m0, _ := msgs[0].(map[string]any)
	m1, _ := msgs[1].(map[string]any)
	m2, _ := msgs[2].(map[string]any)
	if m0["role"] != "user" || m0["content"] != "第一句" {
		t.Errorf("msg[0]=%v", m0)
	}
	if m1["role"] != "assistant" {
		t.Errorf("msg[1].role=%v want assistant", m1["role"])
	}
	if m2["role"] != "user" || m2["content"] != "第二句" {
		t.Errorf("msg[2]=%v", m2)
	}
}

// TestResponsesRequiresAuth 鉴权：配置了 api_key 时必须带 Bearer。
func TestResponsesRequiresAuth(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, responsesSSE, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "secret"})

	// 无 key
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		bytes.NewReader([]byte(`{"model":"glm-5.2","input":"hi"}`))))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no-key code=%d want 401", rec.Code)
	}

	// 错误 key
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"glm-5.2","input":"hi"}`)))
	req2.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("wrong-key code=%d want 401", rec2.Code)
	}

	// 正确 key
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"glm-5.2","input":"hi"}`)))
	req3.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Errorf("right-key code=%d want 200 body=%s", rec3.Code, rec3.Body)
	}
}

// TestResponsesMissingModel 缺 model 直接 400，不打上游。
func TestResponsesMissingModel(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, responsesSSE, true
	})
	h := responsesTestHandler(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		bytes.NewReader([]byte(`{"input":"hi"}`))))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
	if calls != 0 {
		t.Errorf("upstream calls=%d want 0", calls)
	}
}

// TestResponsesErrorPassthrough 上游错误原样透传（不重写成 Responses 形状）。
func TestResponsesErrorPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed"}`, false
	})
	h := responsesTestHandler(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		bytes.NewReader([]byte(`{"model":"glm-5.2","input":"hi"}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	// 上游原文必须保留（不能被网关固定文案覆盖）。
	if !strings.Contains(rec.Body.String(), "Unmarshal chat params failed") {
		t.Errorf("upstream message lost: %s", rec.Body)
	}
}

// TestResponsesUsesSharedChatPipeline 确认走的是既有 chat 管线：
// 会话粘性键（conversation_id）必须被透传到出站 body。
func TestResponsesUsesSharedChatPipeline(t *testing.T) {
	cap := &capturingUpstream{}
	up := newCapturingUpstream(t, cap, 200, responsesSSE, true)
	h := responsesTestHandler(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		bytes.NewReader([]byte(`{"model":"glm-5.2","input":"hi","conversation":"conv_abc"}`))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := cap.lastBody()
	if out == nil {
		t.Fatalf("no outbound body")
	}
	if out["conversation_id"] != "conv_abc" {
		t.Errorf("conversation_id=%v want conv_abc (粘性路由依赖它)", out["conversation_id"])
	}
}

// TestResponsesPoolRotationResponses 确认与 chat 共享账号池语义：
// 坏号被打到时轮转到好号（两端点同一套选号逻辑）。
//
// 用的必须是「账号级」错误（此处 402 余额不足，与 TestChatRotatesOnHardCredit 同款）。
// 请求级错误（11101 参数错、内容拦截、prompt 超长、图片非法）按设计**不换号**——
// 同一 body 换任何号都是同样结果，换号只是把请求放大到所有号都白挨一次。
func TestResponsesPoolRotationResponses(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, responsesSSE, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中（选号按快过期加权 + 成本分层）。
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		bytes.NewReader([]byte(`{"model":"glm-5.2","input":"hi"}`))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-good"] == 0 {
		t.Errorf("good account never called (calls=%v)", calls)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "completed" {
		t.Errorf("status=%v want completed", resp["status"])
	}
}

// 确认 pool.Pool 的类型签名在测试里可用（防止 import 漂移）。
var _ = pool.New
var _ = upstream.Client{}
