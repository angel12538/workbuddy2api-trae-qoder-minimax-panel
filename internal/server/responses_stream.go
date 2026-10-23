// Package server 的 Responses 流式事件翻译（chat SSE → Responses 具名事件）。
//
// chat/completions 的流是一串 `data: {chat.completion.chunk}`；Responses 的流是
// 一串具名事件（`event: response.output_text.delta` / `data: {...}`），且每个事件
// 带 response 物品数组（output[]）与 output_index 语义。两者的帧结构完全不同，
// 不能只改字段名——需要维护一个状态机，边收 chunk 边产出 Responses 事件。
//
// 实时性：翻译是**逐帧**的（不等流结束），客户端的流式体验与原生一致。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// respItem 是一个 output 物品（Responses 的 output[] 元素）。
type respItem struct {
	kind string // "message" | "reasoning" | "function_call"
	id   string

	// message / reasoning 的正文
	text *strings.Builder
	// function_call 的参数与标识
	callID string
	name   string
	args   *strings.Builder
	// 文本物品是否已发过 done 事件
	closed bool
}

// responsesStream 是 Responses 事件流的状态机。
type responsesStream struct {
	w    io.Writer
	seq  int
	resp string // response id
	mod  string // 模型名
	crtd int64  // created_at

	items []*respItem // items[i] 的 i 即 output_index

	msgIdx  int // message 物品的 output_index；-1 = 尚未出现
	rsIdx   int // reasoning 物品的 output_index；-1 = 尚未出现
	toolIdx map[int]int

	usage  map[string]any
	finishReason string

	completed bool
	errored   bool
	started   bool
}

// newResponsesStream 创建一个事件流状态机。
func newResponsesStream(w io.Writer, model string) *responsesStream {
	return &responsesStream{
		w:       w,
		resp:    "resp_" + session.NewMessageID(),
		mod:     model,
		crtd:    time.Now().Unix(),
		msgIdx:  -1,
		rsIdx:   -1,
		toolIdx: map[int]int{},
	}
}

// emit 写出一个 Responses 事件（event: + data: 两行）。
func (s *responsesStream) emit(evType string, data map[string]any) error {
	s.seq++
	if data == nil {
		data = map[string]any{}
	}
	data["type"] = evType
	data["sequence_number"] = s.seq
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = io.WriteString(s.w, "event: "+evType+"\ndata: "+string(b)+"\n\n")
	return err
}

// responseObject 构造当前时刻的 response 对象。
// 每个携带 response 的事件都用它，保证客户端看到的状态与流进度一致。
func (s *responsesStream) responseObject(status string) map[string]any {
	output := make([]any, 0, len(s.items))
	for _, it := range s.items {
		output = append(output, s.itemObject(it, status))
	}
	obj := map[string]any{
		"id":                   s.resp,
		"object":               "response",
		"created_at":           s.crtd,
		"status":               status,
		"model":                s.mod,
		"output":               output,
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
	if s.finishReason == "length" {
		obj["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if u := s.usageObject(); u != nil {
		obj["usage"] = u
	}
	return obj
}

// itemObject 把内部物品转成对外形态。
func (s *responsesStream) itemObject(it *respItem, status string) map[string]any {
	switch it.kind {
	case "message":
		text := ""
		if it.text != nil {
			text = it.text.String()
		}
		return map[string]any{
			"type":   "message",
			"id":     it.id,
			"status": status,
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		}
	case "reasoning":
		text := ""
		if it.text != nil {
			text = it.text.String()
		}
		summary := []any{}
		if text != "" {
			summary = append(summary, map[string]any{"type": "summary_text", "text": text})
		}
		return map[string]any{
			"type":    "reasoning",
			"id":      it.id,
			"summary": summary,
			"status":  status,
		}
	case "function_call":
		args := ""
		if it.args != nil {
			args = it.args.String()
		}
		return map[string]any{
			"type":      "function_call",
			"id":        it.id,
			"call_id":   it.callID,
			"name":      it.name,
			"arguments": args,
			"status":    status,
		}
	}
	return map[string]any{}
}

// usageObject 把 chat 的 usage 转成 Responses 形态。
func (s *responsesStream) usageObject() map[string]any {
	if s.usage == nil {
		return nil
	}
	in := numberOf(s.usage["prompt_tokens"])
	out := numberOf(s.usage["completion_tokens"])
	total := numberOf(s.usage["total_tokens"])
	if total == 0 {
		total = in + out
	}
	return map[string]any{
		"input_tokens":         in,
		"output_tokens":        out,
		"total_tokens":         total,
		"input_tokens_details": map[string]any{"cached_tokens": 0},
	}
}

// start 发出 response.created / response.in_progress。
func (s *responsesStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	if err := s.emit("response.created", map[string]any{
		"response": s.responseObject("in_progress"),
	}); err != nil {
		return err
	}
	return s.emit("response.in_progress", map[string]any{
		"response": s.responseObject("in_progress"),
	})
}

// ensureMessage 惰性创建 message 物品（首个文本增量时）。
func (s *responsesStream) ensureMessage() error {
	if s.msgIdx >= 0 {
		return nil
	}
	it := &respItem{kind: "message", id: "msg_" + session.NewMessageID(), text: &strings.Builder{}}
	s.items = append(s.items, it)
	s.msgIdx = len(s.items) - 1
	if err := s.emit("response.output_item.added", map[string]any{
		"output_index": s.msgIdx,
		"item": map[string]any{
			"type":    "message",
			"id":      it.id,
			"status":  "in_progress",
			"role":    "assistant",
			"content": []any{},
		},
	}); err != nil {
		return err
	}
	return s.emit("response.content_part.added", map[string]any{
		"item_id":       it.id,
		"output_index":  s.msgIdx,
		"content_index": 0,
		"part": map[string]any{
			"type":        "output_text",
			"text":        "",
			"annotations": []any{},
		},
	})
}

// ensureReasoning 惰性创建 reasoning 物品（首个思维链增量时）。
func (s *responsesStream) ensureReasoning() error {
	if s.rsIdx >= 0 {
		return nil
	}
	it := &respItem{kind: "reasoning", id: "rs_" + session.NewMessageID(), text: &strings.Builder{}}
	s.items = append(s.items, it)
	s.rsIdx = len(s.items) - 1
	if err := s.emit("response.output_item.added", map[string]any{
		"output_index": s.rsIdx,
		"item": map[string]any{
			"type":    "reasoning",
			"id":      it.id,
			"status":  "in_progress",
			"summary": []any{},
		},
	}); err != nil {
		return err
	}
	return s.emit("response.reasoning_summary_part.added", map[string]any{
		"item_id":       it.id,
		"output_index":  s.rsIdx,
		"summary_index": 0,
		"part": map[string]any{
			"type": "summary_text",
			"text": "",
		},
	})
}

// ensureTool 惰性创建 function_call 物品（chat 侧 tool_call index → output_index）。
func (s *responsesStream) ensureTool(chatIdx int, callID, name string) error {
	if _, ok := s.toolIdx[chatIdx]; ok {
		return nil
	}
	it := &respItem{
		kind:   "function_call",
		id:     "fc_" + session.NewMessageID(),
		callID: callID,
		name:   name,
		args:   &strings.Builder{},
	}
	s.items = append(s.items, it)
	idx := len(s.items) - 1
	s.toolIdx[chatIdx] = idx
	return s.emit("response.output_item.added", map[string]any{
		"output_index": idx,
		"item": map[string]any{
			"type":      "function_call",
			"id":        it.id,
			"call_id":   callID,
			"name":      name,
			"arguments": "",
			"status":    "in_progress",
		},
	})
}

// handleChunk 处理一个 chat.completion.chunk。
func (s *responsesStream) handleChunk(obj map[string]any) error {
	if errObj, ok := obj["error"]; ok && errObj != nil {
		return s.emitError(errObj)
	}
	if err := s.start(); err != nil {
		return err
	}
	if u, ok := obj["usage"].(map[string]any); ok && u != nil {
		s.usage = u
	}
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			s.finishReason = fr
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		// 思维链 → reasoning 物品。
		if rc, _ := delta["reasoning_content"].(string); rc != "" {
			if err := s.ensureReasoning(); err != nil {
				return err
			}
			it := s.items[s.rsIdx]
			it.text.WriteString(rc)
			if err := s.emit("response.reasoning_summary_text.delta", map[string]any{
				"item_id":       it.id,
				"output_index":  s.rsIdx,
				"summary_index": 0,
				"delta":         rc,
			}); err != nil {
				return err
			}
		}
		// 正文 → message 物品。
		if txt, _ := delta["content"].(string); txt != "" {
			if err := s.ensureMessage(); err != nil {
				return err
			}
			it := s.items[s.msgIdx]
			it.text.WriteString(txt)
			if err := s.emit("response.output_text.delta", map[string]any{
				"item_id":       it.id,
				"output_index":  s.msgIdx,
				"content_index": 0,
				"delta":         txt,
			}); err != nil {
				return err
			}
		}
		// 工具调用 → function_call 物品。
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				if err := s.handleToolDelta(tc); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// handleToolDelta 处理一个 delta.tool_calls 条目。
func (s *responsesStream) handleToolDelta(tc any) error {
	tcm, _ := tc.(map[string]any)
	if tcm == nil {
		return nil
	}
	idx := int(numberOf(tcm["index"]))
	callID, _ := tcm["id"].(string)
	fn, _ := tcm["function"].(map[string]any)
	name := ""
	args := ""
	if fn != nil {
		name, _ = fn["name"].(string)
		args, _ = fn["arguments"].(string)
	}
	if err := s.ensureTool(idx, callID, name); err != nil {
		return err
	}
	outIdx := s.toolIdx[idx]
	it := s.items[outIdx]
	// 首片带 id/name，后续片只有参数片段——已存在的字段不覆盖。
	if it.callID == "" {
		it.callID = callID
	}
	if it.name == "" {
		it.name = name
	}
	if args == "" {
		return nil
	}
	it.args.WriteString(args)
	return s.emit("response.function_call_arguments.delta", map[string]any{
		"item_id":      it.id,
		"output_index": outIdx,
		"delta":        args,
	})
}

// emitError 把上游 error 帧转成 Responses 的 error 事件。
func (s *responsesStream) emitError(errObj any) error {
	s.errored = true
	em, _ := errObj.(map[string]any)
	code := "upstream_error"
	msg := ""
	if em != nil {
		if v, ok := em["code"].(string); ok && v != "" {
			code = v
		}
		if v, ok := em["message"].(string); ok {
			msg = v
		}
	}
	return s.emit("error", map[string]any{
		"code":    code,
		"message": msg,
		"param":   nil,
	})
}

// finish 收尾：关闭各物品并发出 response.completed。
func (s *responsesStream) finish() error {
	if s.completed {
		return nil
	}
	s.completed = true
	// 一个物品都没有（上游空回合）：补一个空的 message 物品，保持 output 非空。
	if len(s.items) == 0 {
		if err := s.ensureMessage(); err != nil {
			return err
		}
	}
	// 关闭 message 物品。
	if s.msgIdx >= 0 {
		it := s.items[s.msgIdx]
		if !it.closed {
			it.closed = true
			text := it.text.String()
			if err := s.emit("response.output_text.done", map[string]any{
				"item_id":       it.id,
				"output_index":  s.msgIdx,
				"content_index": 0,
				"text":          text,
			}); err != nil {
				return err
			}
			if err := s.emit("response.content_part.done", map[string]any{
				"item_id":       it.id,
				"output_index":  s.msgIdx,
				"content_index": 0,
				"part": map[string]any{
					"type":        "output_text",
					"text":        text,
					"annotations": []any{},
				},
			}); err != nil {
				return err
			}
			if err := s.emit("response.output_item.done", map[string]any{
				"output_index": s.msgIdx,
				"item":         s.itemObject(it, "completed"),
			}); err != nil {
				return err
			}
		}
	}
	// 关闭 reasoning 物品。
	if s.rsIdx >= 0 {
		it := s.items[s.rsIdx]
		if !it.closed {
			it.closed = true
			text := it.text.String()
			if err := s.emit("response.reasoning_summary_text.done", map[string]any{
				"item_id":       it.id,
				"output_index":  s.rsIdx,
				"summary_index": 0,
				"text":          text,
			}); err != nil {
				return err
			}
			if err := s.emit("response.reasoning_summary_part.done", map[string]any{
				"item_id":       it.id,
				"output_index":  s.rsIdx,
				"summary_index": 0,
				"part": map[string]any{
					"type": "summary_text",
					"text": text,
				},
			}); err != nil {
				return err
			}
			if err := s.emit("response.output_item.done", map[string]any{
				"output_index": s.rsIdx,
				"item":         s.itemObject(it, "completed"),
			}); err != nil {
				return err
			}
		}
	}
	// 关闭 function_call 物品。
	for _, it := range s.items {
		if it.kind != "function_call" || it.closed {
			continue
		}
		it.closed = true
		idx := s.indexOf(it)
		if err := s.emit("response.function_call_arguments.done", map[string]any{
			"item_id":      it.id,
			"output_index": idx,
			"arguments":    it.args.String(),
		}); err != nil {
			return err
		}
		if err := s.emit("response.output_item.done", map[string]any{
			"output_index": idx,
			"item":         s.itemObject(it, "completed"),
		}); err != nil {
			return err
		}
	}
	// 上游以 error 帧收尾：按 Responses 语义给 failed，不再发 completed。
	if s.errored {
		return s.emit("response.failed", map[string]any{
			"response": s.responseObject("failed"),
		})
	}
	status := "completed"
	if s.finishReason == "length" {
		status = "incomplete"
	}
	return s.emit("response.completed", map[string]any{
		"response": s.responseObject(status),
	})
}

// indexOf 查物品的 output_index。
func (s *responsesStream) indexOf(target *respItem) int {
	for i, it := range s.items {
		if it == target {
			return i
		}
	}
	return -1
}

// responsesStreamWriter 把 chat/completions 的 SSE 输出实时翻译成 Responses 事件流。
//
// 两种写出模式：
//   - SSE 模式：首帧形如 "data: ..."，逐帧翻译成具名事件后写出；
//   - 直通模式：首帧是 JSON（错误路径），原样透传——错误体在两套 API 里形状一致，
//     重写只会丢掉上游原文与 hint。
type responsesStreamWriter struct {
	w  http.ResponseWriter
	fl http.Flusher
	st *responsesStream

	buf   []byte
	sse   bool
	raw   bool
	ended bool
}

func newResponsesStreamWriter(w http.ResponseWriter, st *responsesStream) *responsesStreamWriter {
	fl, _ := w.(http.Flusher)
	return &responsesStreamWriter{w: w, fl: fl, st: st}
}

func (t *responsesStreamWriter) Header() http.Header { return t.w.Header() }

func (t *responsesStreamWriter) WriteHeader(code int) { t.w.WriteHeader(code) }

// Write 接收 chat 管线的输出。
func (t *responsesStreamWriter) Write(p []byte) (int, error) {
	if t.raw {
		return t.w.Write(p)
	}
	t.buf = append(t.buf, p...)
	// 模式判定：首个非空字节决定是 SSE 还是直通。
	if !t.sse && !t.raw {
		trimmed := strings.TrimLeft(string(t.buf), " \t\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data:"):
			t.sse = true
		case trimmed != "":
			t.raw = true
			// 直通：把已缓冲的内容原样写出，后续不再翻译。
			if _, err := t.w.Write(t.buf); err != nil {
				return len(p), err
			}
			t.buf = nil
		}
	}
	if !t.sse {
		return len(p), nil
	}
	for {
		idx := bytes.Index(t.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		chunk := t.buf[:idx]
		t.buf = t.buf[idx+2:]
		if err := t.handleChunk(chunk); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// handleChunk 处理一个完整的 SSE 事件块。
func (t *responsesStreamWriter) handleChunk(chunk []byte) error {
	for _, line := range strings.Split(string(chunk), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			t.ended = true
			return t.st.finish()
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			// 非 JSON 帧：原样透传（不吞掉未知内容）。
			if _, werr := io.WriteString(t.w, "data: "+payload+"\n\n"); werr != nil {
				return werr
			}
			continue
		}
		if err := t.st.handleChunk(obj); err != nil {
			return err
		}
	}
	return nil
}

// Flush 冲刷底层 writer。
//
// 这里**绝不能**收尾：upstream.StreamHint 每写出一帧就调一次 Flush（逐帧 flush 是
// 流式体验的来源），若把 Flush 当成「流结束」信号，第一帧之后就会发出
// response.completed——随后到达的工具调用参数分片只能落在 completed 之后，
// 客户端拿到的 arguments 是被截断的半个 JSON。
//
// 收尾由 Close() 负责（chat 管线返回后调用），或由 [DONE] 帧触发。
func (t *responsesStreamWriter) Flush() {
	if t.fl != nil {
		t.fl.Flush()
	}
}

// Close 收尾：处理残留字节（最后一帧可能没带 "\n\n"）并发出 response.completed。
func (t *responsesStreamWriter) Close() error {
	if !t.sse {
		return nil
	}
	if len(t.buf) > 0 {
		rest := t.buf
		t.buf = nil
		if err := t.handleChunk(rest); err != nil {
			return err
		}
	}
	if !t.ended {
		t.ended = true
		return t.st.finish()
	}
	return nil
}

// responsesStream 处理流式请求。
func (h *Handler) responsesStream(w http.ResponseWriter, r *http.Request, req *responsesRequest, msgs []any) {
	st := newResponsesStream(w, req.Model)
	tw := newResponsesStreamWriter(w, st)
	h.chatCompletions(tw, r)
	_ = tw.Close()
}
