package traechat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// ── 入站（OpenAI）请求模型 ────────────────────────────────────────────────

// Message 入站消息（只取转发需要的字段）。
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
}

// ToolCall OpenAI 形态的工具调用（仅用于回填到 query 文本里）。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatRequest 入站请求。
type ChatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	Stream    bool      `json:"stream"`
	MaxTokens int64     `json:"max_tokens,omitempty"`
	// SessionKey 可选：调用方给定的会话键（缺省由账号+模型派生稳定值）。
	SessionKey string `json:"session_key,omitempty"`
}

// ── 请求扁平化 ───────────────────────────────────────────────────────────

// textOfContent 把 OpenAI content（string 或 parts 数组）转成纯文本。
func textOfContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	// 兜底：原样（可能已是文本）
	return strings.Trim(string(raw), "\"")
}

func serializeToolCalls(tcs []ToolCall) string {
	if len(tcs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Tool Calls]")
	for _, tc := range tcs {
		name := tc.Function.Name
		if name == "" {
			name = tc.Type
		}
		b.WriteString("\n- ")
		b.WriteString(name)
		if tc.ID != "" {
			b.WriteString(" (id=" + tc.ID + ")")
		}
		if tc.Function.Arguments != "" {
			b.WriteString(": ")
			b.WriteString(tc.Function.Arguments)
		}
	}
	return b.String()
}

// FlattenQuery 把 OpenAI 消息数组扁平化为 Trae Remote 的 query parts JSON 字符串。
//
// 口径对齐 Trae 网页端 / trae2api 社区实现：system→[System]、assistant→[Assistant]、
// tool→[Client Tool Result]，全部拼成一段文本后再包一层
// `[{"type":"text","data":{"content": "<text>"}}]`。
func FlattenQuery(messages []Message) string {
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		content := textOfContent(m.Content)
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			parts = append(parts, "[System]\n"+content)
		case "assistant":
			s := content
			if tc := serializeToolCalls(m.ToolCalls); tc != "" {
				s += tc
			}
			if strings.TrimSpace(s) != "" {
				parts = append(parts, "[Assistant]\n"+s)
			}
		case "tool":
			label := m.Name
			if label == "" {
				label = "tool"
			}
			id := m.ToolCallID
			if id == "" {
				id = "unknown"
			}
			parts = append(parts, fmt.Sprintf("[Client Tool Result: %s %s]\n%s", id, label, content))
		default:
			if strings.TrimSpace(content) != "" {
				parts = append(parts, content)
			}
		}
	}
	text := strings.Join(parts, "\n\n")
	raw, _ := json.Marshal([]map[string]any{
		{"type": "text", "data": map[string]any{"content": text}},
	})
	return string(raw)
}

// LastUserText 取最后一条 user 消息的文本（供上游 session_title 等场景）。
func LastUserText(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(messages[i].Role, "user") {
			return textOfContent(messages[i].Content)
		}
	}
	return ""
}

// ── 出站（OpenAI）响应模型 ────────────────────────────────────────────────

// Delta OpenAI 流式增量。
type Delta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ChoiceDelta 流式 choice。
type ChoiceDelta struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Chunk 流式帧。
type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChoiceDelta `json:"choices"`
}

// Usage OpenAI usage 对象。
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// ── Trae 事件解析 ────────────────────────────────────────────────────────

type planItem struct {
	ID               string `json:"id"`
	Thought          string `json:"thought"`
	ReasoningContent string `json:"reasoning_content"`
	ToolCallInfo     struct {
		ID     string          `json:"id"`
		Name   string          `json:"name"`
		Params json.RawMessage `json:"params"`
	} `json:"tool_call_info"`
}

type tokenUsageEvent struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
}

type doneEvent struct {
	Status       string `json:"status"`
	AgentRunID   string `json:"agent_run_id"`
	ErrorMessage string `json:"error_message"`
}

type metadataEvent struct {
	TurnID   string `json:"turn_id"`
	MessageID string `json:"message_id"`
	Status   string `json:"status"`
}

type errorEvent struct {
	Code    any    `json:"code"`
	Message string `json:"message"`
	Error   string `json:"error"`
	Detail  string `json:"detail"`
}

// Emitter 消费转换后的内容增量。返回错误即中止。
type Emitter interface {
	// OnReasoning 思考增量（可能为空）。
	OnReasoning(text string) error
	// OnContent 正文增量（可能为空）。
	OnContent(text string) error
	// OnUsage 上游给出的 token 用量。
	OnUsage(u Usage)
}

// contentReplacer 为「可回填」的 Emitter 提供能力：非流式场景下，
// finish 工具的 summary 是权威最终答案，允许直接以它整体替换已聚合的正文。
type contentReplacer interface {
	ReplaceContent(text string) error
}

// converter 维护「累积快照 → 增量」的状态。
type converter struct {
	em            Emitter
	lastThought   map[string]string
	lastReasoning map[string]string
	usage         Usage
	hasUsage      bool
	contentSeen   bool
	contentTotal  string
	turnID        string
	done          bool
	upstreamErr   error
}

func newConverter(em Emitter) *converter {
	return &converter{
		em:            em,
		lastThought:   map[string]string{},
		lastReasoning: map[string]string{},
	}
}

// delta 计算累积值的增量：新值以旧值为前缀时只吐尾部；否则整段重发。
func deltaOf(last, cur string) string {
	if cur == last {
		return ""
	}
	if strings.HasPrefix(cur, last) {
		return cur[len(last):]
	}
	return cur
}

// emitContent 统一出口：记录已累计正文，再向 Emitter 吐增量。
func (c *converter) emitContent(d string) error {
	if d == "" {
		return nil
	}
	c.contentTotal += d
	c.contentSeen = true
	return c.em.OnContent(d)
}

// emitFinal 处理 finish 工具的权威 summary。
// 非流式（Emitter 支持 ReplaceContent）：整体替换，保证正文完整不截断。
// 流式：只能追加——summary 以已发正文为前缀时补发剩余部分；已发正文为空时整段发；
// 两者都不满足（脏数据）时不动，避免重复吐出。
func (c *converter) emitFinal(summary string) error {
	if summary == "" || summary == c.contentTotal {
		return nil
	}
	if r, ok := c.em.(contentReplacer); ok {
		c.contentTotal = summary
		c.contentSeen = true
		return r.ReplaceContent(summary)
	}
	switch {
	case c.contentTotal == "":
		return c.emitContent(summary)
	case strings.HasPrefix(summary, c.contentTotal):
		return c.emitContent(summary[len(c.contentTotal):])
	default:
		return nil
	}
}

func (c *converter) handle(ev EventTrae) error {
	switch ev.Name {
	case "plan_item":
		var pi planItem
		if err := json.Unmarshal(ev.Data, &pi); err != nil {
			return nil
		}
		// 思考内容（无论哪种 plan item 都可能有）
		if d := deltaOf(c.lastReasoning[pi.ID], pi.ReasoningContent); d != "" {
			c.lastReasoning[pi.ID] = pi.ReasoningContent
			if err := c.em.OnReasoning(d); err != nil {
				return err
			}
		} else {
			c.lastReasoning[pi.ID] = pi.ReasoningContent
		}
		// 正文：只取「非工具调用」的 plan item 的 thought，避免把工具执行叙述当正文。
		// finish 工具例外——它的 params.summary 就是最终答案。
		if pi.ToolCallInfo.Name == "" {
			if d := deltaOf(c.lastThought[pi.ID], pi.Thought); d != "" {
				c.lastThought[pi.ID] = pi.Thought
				if err := c.emitContent(d); err != nil {
					return err
				}
			} else {
				c.lastThought[pi.ID] = pi.Thought
			}
		} else if pi.ToolCallInfo.Name == "finish" {
			var p struct {
				Summary string `json:"summary"`
			}
			raw := pi.ToolCallInfo.Params
			// params 可能是对象，也可能被序列化成 JSON 字符串，两种都尝试。
			if err := json.Unmarshal(raw, &p); err != nil {
				var s string
				if json.Unmarshal(raw, &s) == nil {
					_ = json.Unmarshal([]byte(s), &p)
				}
			}
			if p.Summary != "" {
				c.lastThought[pi.ID] = p.Summary
				if err := c.emitFinal(p.Summary); err != nil {
					return err
				}
			} else if pi.Thought != "" {
				// 有些轮次 finish 不带 summary，退化用 thought 差分。
				if d := deltaOf(c.lastThought[pi.ID], pi.Thought); d != "" {
					c.lastThought[pi.ID] = pi.Thought
					if err := c.emitContent(d); err != nil {
						return err
					}
				}
			}
		}
	case "token_usage":
		var tu tokenUsageEvent
		if err := json.Unmarshal(ev.Data, &tu); err == nil && tu.TotalTokens > 0 {
			c.usage = Usage{
				PromptTokens:     tu.PromptTokens,
				CompletionTokens: tu.CompletionTokens,
				TotalTokens:      tu.TotalTokens,
			}
			c.hasUsage = true
			c.em.OnUsage(c.usage)
		}
	case "metadata":
		var me metadataEvent
		if err := json.Unmarshal(ev.Data, &me); err == nil && me.TurnID != "" {
			c.turnID = me.TurnID
		}
	case "done":
		c.done = true
		var de doneEvent
		if json.Unmarshal(ev.Data, &de) == nil {
			if de.Status != "" && de.Status != "completed" && de.ErrorMessage != "" {
				c.upstreamErr = fmt.Errorf("Trae 会话以 %s 结束: %s", de.Status, de.ErrorMessage)
			}
		}
	case "error":
		var ee errorEvent
		_ = json.Unmarshal(ev.Data, &ee)
		msg := firstNonEmpty(ee.Message, ee.Error, ee.Detail)
		c.upstreamErr = fmt.Errorf("Trae 上游错误: %s", msg)
		return c.upstreamErr
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ── 顶层中继 ─────────────────────────────────────────────────────────────

// DebugDump 非空时，把中继内部收到的每个上游原始事件追加写入该文件。
// 通过环境变量 TRAE_RELAY_DUMP 设定，仅用于排查，不参与正常转发。
var DebugDump = os.Getenv("TRAE_RELAY_DUMP")

func dumpEvent(ev EventTrae) {
	if DebugDump == "" {
		return
	}
	f, err := os.OpenFile(DebugDump, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "=== %s ===\n%s\n\n", ev.Name, ev.Data)
}

// Relay 把一次 OpenAI 请求转成 Trae Remote 会话并回放事件。
type Relay struct {
	Pool    *Pool
	Catalog *CatalogCache
	// FirstEventTimeout 首帧（含排队）等待上限，缺省 120s。
	FirstEventTimeout time.Duration
	// IdleTimeout 两次事件之间的空闲上限，缺省 180s。
	IdleTimeout time.Duration
	// MaxRetry 并行超限时的换号重试次数，缺省 3。
	MaxRetry int
}

// NewRelay 构造中继。
func NewRelay(p *Pool, cat *CatalogCache) *Relay {
	return &Relay{Pool: p, Catalog: cat, MaxRetry: 3}
}

func (r *Relay) firstTimeout() time.Duration {
	if r.FirstEventTimeout <= 0 {
		return 120 * time.Second
	}
	return r.FirstEventTimeout
}

func (r *Relay) idleTimeout() time.Duration {
	if r.IdleTimeout <= 0 {
		return 180 * time.Second
	}
	return r.IdleTimeout
}

func (r *Relay) maxRetry() int {
	if r.MaxRetry <= 0 {
		return 3
	}
	return r.MaxRetry
}

// RunResult 非流式结果。
type RunResult struct {
	Content   string
	Reasoning string
	Usage     Usage
	HasUsage  bool
	TurnID    string
	Model     string
}

type bufferEmitter struct {
	content   strings.Builder
	reasoning strings.Builder
	relay     *Relay
}

func (b *bufferEmitter) OnReasoning(t string) error { b.reasoning.WriteString(t); return nil }
func (b *bufferEmitter) OnContent(t string) error   { b.content.WriteString(t); return nil }
func (b *bufferEmitter) OnUsage(u Usage)            {}

// ReplaceContent 非流式：以 finish 的权威 summary 整体替换已聚合正文。
func (b *bufferEmitter) ReplaceContent(t string) error {
	b.content.Reset()
	b.content.WriteString(t)
	return nil
}

// Complete 非流式执行一次。
//
// 上游 Trae Agent 偶发「status=completed 但 thought/finish.summary 全空」的空答案，
// 此时再跑一次（换新会话）通常能拿到正常回答。空答案重试上限 1 次，且只在
// 首次确实什么都没产出时才发生，不影响正常请求的积分消耗。
func (r *Relay) Complete(ctx context.Context, req ChatRequest) (*RunResult, error) {
	res, err := r.completeOnce(ctx, req)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(res.Content) == "" && ctx.Err() == nil {
		if retry, err2 := r.completeOnce(ctx, req); err2 == nil && strings.TrimSpace(retry.Content) != "" {
			retry.Reasoning = firstNonEmpty(retry.Reasoning, res.Reasoning)
			return retry, nil
		}
	}
	return res, nil
}

func (r *Relay) completeOnce(ctx context.Context, req ChatRequest) (*RunResult, error) {
	be := &bufferEmitter{relay: r}
	conv, err := r.run(ctx, req, be)
	if err != nil {
		return nil, err
	}
	return &RunResult{
		Content:   be.content.String(),
		Reasoning: be.reasoning.String(),
		Usage:     conv.usage,
		HasUsage:  conv.hasUsage,
		TurnID:    conv.turnID,
		Model:     req.Model,
	}, nil
}

// Stream 流式执行一次（逐增量回调 emitter）。
func (r *Relay) Stream(ctx context.Context, req ChatRequest, em Emitter) (*RunResult, error) {
	conv, err := r.run(ctx, req, em)
	if err != nil {
		return nil, err
	}
	return &RunResult{
		Usage:    conv.usage,
		HasUsage: conv.hasUsage,
		TurnID:   conv.turnID,
		Model:    req.Model,
	}, nil
}

// run 执行「选号 → 建会话 → 拉事件流」并对可恢复错误换号重试。
func (r *Relay) run(ctx context.Context, req ChatRequest, em Emitter) (*converter, error) {
	if r.Pool == nil {
		return nil, fmt.Errorf("Trae 通道未启用")
	}
	models := []ModelInfo(nil)
	if r.Catalog != nil {
		models, _ = r.Catalog.Models(ctx)
	}
	mi, ok := Find(models, req.Model)
	if !ok {
		// 目录不可用时仍允许透传模型名（上游会自行判定）
		mi = ModelInfo{Name: strings.TrimPrefix(strings.TrimSpace(req.Model), "trae:")}
		if mi.Name == "" {
			return nil, fmt.Errorf("缺少模型名")
		}
	}

	query := FlattenQuery(req.Messages)
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("messages 为空")
	}

	var lastErr error
	for attempt := 0; attempt <= r.maxRetry(); attempt++ {
		acct, err := r.Pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		conv, err := r.attempt(ctx, acct, mi, query, req, em)
		r.Pool.Release(acct)
		if err == nil {
			r.Pool.MarkSuccess(acct)
			return conv, nil
		}
		lastErr = err
		r.Pool.MarkFailure(acct, err)
		// 仅「并行超限 / 凭证失效」值得换号重试；其他错误（含上游语义错误）直接抛出。
		if !IsParallelLimit(err) && !IsUnauthorized(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return nil, lastErr
}

func (r *Relay) attempt(ctx context.Context, acct *Account, mi ModelInfo, query string, req ChatRequest, em Emitter) (*converter, error) {
	sessKey := strings.TrimSpace(req.SessionKey)
	if sessKey == "" {
		sessKey = stableSessionID(acct.UserID, mi.Name, "")
	}
	// 目录里 agent 组与 work 组同名模型只保留 agent 组条目（merge 顺序在前），
	// 因此统一走 Agent 执行器（1M 档位，能力最全）。
	sid, mid, _, err := r.Pool.Client().CreateSession(CreateRequest{
		JWT:        acct.JWT,
		UserID:     acct.UserID,
		DeviceID:   acct.DeviceID,
		Model:      mi,
		AgentType:  AgentTypeAgent,
		Query:      query,
		ChatMode:   "code",
		SessionKey: sessKey,
		MaxTokens:  req.MaxTokens,
	})
	if err != nil {
		return nil, err
	}
	// 无论成败都尽力释放上游并发名额（后台执行，不阻塞返回）。
	defer func() {
		go func() { _ = r.Pool.Client().Stop(acct.JWT, sid, mid) }()
	}()

	conv := newConverter(em)

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 看门狗：首帧未到按 firstTimeout 计时；首帧到达后改为「相邻两帧间隔」的 idle 计时。
	timer := time.NewTimer(r.firstTimeout())
	defer timer.Stop()
	go func() {
		select {
		case <-timer.C:
			cancel()
		case <-streamCtx.Done():
		}
	}()

	done := make(chan error, 1)
	go func() {
		firstSeen := false
		err := r.Pool.Client().StreamEvents(acct.JWT, sid, mid, func(ev EventTrae) error {
			if !firstSeen {
				firstSeen = true
				if timer.Stop() {
					timer.Reset(r.idleTimeout())
				}
			} else {
				timer.Reset(r.idleTimeout())
			}
			select {
			case <-streamCtx.Done():
				return streamCtx.Err()
			default:
			}
			if err := conv.handle(ev); err != nil {
				return err
			}
			dumpEvent(ev)
			if conv.done {
				return errStopStream
			}
			return nil
		})
		if err == errStopStream {
			err = nil
		}
		done <- err
	}()

	select {
	case <-ctx.Done():
		cancel()
		<-done
		if conv.upstreamErr != nil {
			return conv, conv.upstreamErr
		}
		return conv, ctx.Err()
	case res := <-done:
		if res != nil && res != errStopStream {
			if conv.upstreamErr != nil {
				return conv, conv.upstreamErr
			}
			return conv, res
		}
		if conv.upstreamErr != nil {
			return conv, conv.upstreamErr
		}
		return conv, nil
	}
}

// errStopStream 由事件回调返回以正常结束流（事件流不会自己发 [DONE]，靠 done 事件终止）。
var errStopStream = fmt.Errorf("trae stream finished")
