// trae.go 把 Trae（CN）模型通道接到统一的 /v1/chat/completions 上。
//
// 路由协议与 cn:/global: 一致：模型名带 "trae:" 前缀即走本文件的中继，
// 底层是 internal/traechat 的 Trae Remote 通道（与 Trae 网页端同源，性能、
// 积分口径与官方客户端一致——中继只做协议转换，不额外发起上游调用）。
//
// 与 WorkBuddy 通道的区别：
//   - 无「积分保底 / 模型冷却 / 会话粘性」概念：Trae 侧只暴露并行上限
//     （solo_agent_parallel_limit，实测 2），由 traechat.Pool 做排队。
//   - 提示词不做改写（透传客户端原文），避免额外的 token 消耗。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/traechat"
)

// SetTrae 注入 Trae 通道（由 main 在启动时调用，覆盖 Config 里的静态字段）。
// 必须在服务开始处理请求之前调用；调用后惰性中继按新配置构建。
func (h *Handler) SetTrae(pool *traechat.Pool, cat *traechat.CatalogCache, firstEvent, idle time.Duration) {
	h.cfg.Trae = pool
	h.cfg.TraeCatalog = cat
	h.traeRelayOnce = sync.Once{}
	h.traeRelayInst = nil
	if pool != nil {
		relay := traechat.NewRelay(pool, cat)
		if firstEvent > 0 {
			relay.FirstEventTimeout = firstEvent
		}
		if idle > 0 {
			relay.IdleTimeout = idle
		}
		h.traeRelayOnce.Do(func() { h.traeRelayInst = relay })
	}
}

// traeChat 处理 realm=="trae" 的 /v1/chat/completions。
func (h *Handler) traeChat(w http.ResponseWriter, r *http.Request, body []byte, bareModel string, st *chatStat) {
	if h.cfg.Trae == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "trae_disabled",
			"Trae 通道未启用（config.json 的 trae.enabled=false）")
		return
	}
	var req traechat.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "解析请求体失败: "+err.Error())
		return
	}
	req.Model = bareModel
	if strings.TrimSpace(req.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少模型名")
		return
	}

	relay := h.traeRelay()
	ctx := r.Context()

	if req.Stream {
		h.traeStream(w, r, relay, req, st)
		return
	}

	t0 := time.Now()
	res, err := relay.Complete(ctx, req)
	st.ttfb = time.Since(t0)
	if err != nil {
		h.traeFail(w, st, err)
		return
	}
	st.status = http.StatusOK
	st.outcome = "ok"
	if res.HasUsage {
		st.promptTokens = res.Usage.PromptTokens
		st.completionTokens = res.Usage.CompletionTokens
		st.totalTokens = res.Usage.TotalTokens
		st.toks = int(res.Usage.TotalTokens)
	} else {
		st.toks = -1
	}
	id := "trae-" + traeIDOf(res.TurnID)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "trae:" + req.Model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":              "assistant",
				"content":           res.Content,
				"reasoning_content": res.Reasoning,
			},
			"finish_reason": "stop",
		}},
		"usage": usageOrNil(res.HasUsage, res.Usage),
	})
}

// traeStream 流式转发。
func (h *Handler) traeStream(w http.ResponseWriter, r *http.Request, relay *traechat.Relay, req traechat.ChatRequest, st *chatStat) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "stream_unsupported", "当前连接不支持流式")
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache, no-transform")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	start := time.Now()
	created := start.Unix()
	id := "trae-" + traeIDOf("")
	model := "trae:" + req.Model

	first := true
	writeDelta := func(d traechat.Delta, finish *string) error {
		if first {
			st.ttfb = time.Since(start)
			d.Role = "assistant"
			first = false
		}
		chunk := traechat.Chunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []traechat.ChoiceDelta{{Index: 0, Delta: d, FinishReason: finish}},
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	em := &sseEmitter{write: writeDelta}
	res, err := relay.Stream(r.Context(), req, em)
	if err != nil {
		// 已开始写响应体，只能以错误帧收尾（客户端据此报错）。
		msg := err.Error()
		raw, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": msg, "type": "upstream_error"},
		})
		fmt.Fprintf(w, "data: %s\n\n", raw)
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		st.status = http.StatusBadGateway
		st.outcome = "trae_upstream_error"
		st.toks = -1
		return
	}
	// 整条流共用一个 completion id（上游 turn_id 只在结束时才拿到，中途换 id 会让
	// 客户端把同一轮拆成两段）。故此处不再回填 res.TurnID。
	stop := "stop"
	_ = writeDelta(traechat.Delta{}, &stop)
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()

	st.status = http.StatusOK
	st.outcome = "ok"
	if res != nil && res.HasUsage {
		st.promptTokens = res.Usage.PromptTokens
		st.completionTokens = res.Usage.CompletionTokens
		st.totalTokens = res.Usage.TotalTokens
		st.toks = int(res.Usage.TotalTokens)
	} else {
		st.toks = -1
	}
}

// sseEmitter 把中继增量写成 OpenAI 流式帧。
type sseEmitter struct {
	write func(traechat.Delta, *string) error
}

func (e *sseEmitter) OnReasoning(text string) error {
	return e.write(traechat.Delta{ReasoningContent: text}, nil)
}

func (e *sseEmitter) OnContent(text string) error {
	return e.write(traechat.Delta{Content: text}, nil)
}

func (e *sseEmitter) OnUsage(traechat.Usage) {}

// traeFail 把中继错误映射成 OpenAI 错误响应。
func (h *Handler) traeFail(w http.ResponseWriter, st *chatStat, err error) {
	st.toks = -1
	if errors.Is(err, context.Canceled) {
		st.status = 499
		st.outcome = "client_gone"
		return
	}
	switch {
	case traechat.IsParallelLimit(err):
		st.status = http.StatusTooManyRequests
		st.outcome = "trae_parallel_limit"
		writeOpenAIErrorHint(w, http.StatusTooManyRequests, "rate_limit_exceeded",
			"Trae 并行额度已满（上游 solo_agent_parallel_limit）", "稍后重试，或在面板里补充更多 Trae 账号")
	case traechat.IsUnauthorized(err):
		st.status = http.StatusUnauthorized
		st.outcome = "trae_unauthorized"
		writeOpenAIErrorHint(w, http.StatusUnauthorized, "invalid_api_key",
			"Trae 凭证失效", "在面板「Trae IDE 账号」里重新登录，或等待自动续期")
	default:
		st.status = http.StatusBadGateway
		st.outcome = "trae_upstream_error"
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
	}
}

func usageOrNil(has bool, u traechat.Usage) any {
	if !has {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
}

// traeIDOf 由 turn id 生成稳定的 completion id（turn 为空时给随机短 id）。
func traeIDOf(turnID string) string {
	s := strings.TrimSpace(turnID)
	if s == "" {
		return fmt.Sprintf("%d", time.Now().UnixNano()%1e12)
	}
	if len(s) > 24 {
		s = s[:24]
	}
	return s
}

// traeModels 返回 trae: 前缀的模型条目（供 /v1/models）。
// 目录不可用（无账号/上游失败）时返回空切片——不编造静态兜底。
func (h *Handler) traeModels() []map[string]any {
	if h.cfg.Trae == nil || h.cfg.TraeCatalog == nil {
		return nil
	}
	models, err := h.cfg.TraeCatalog.Models(h.baseCtx())
	if err != nil || len(models) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		entry := map[string]any{
			"id":       "trae:" + m.Name,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "trae",
			"name":     firstNonEmptyStr(m.DisplayName, m.Name),
		}
		if m.ContextWindow > 0 {
			entry["context_length"] = m.ContextWindow
		}
		if m.Multimodal {
			entry["supports_images"] = true
		}
		// 消耗倍率（Trae features.consumption_rate.data.rate），与 WorkBuddy 侧的
		// credits 字段同形态，下游过滤器（sync_dsh_models.py 的倍率阈值）可直接复用。
		if lbl := m.CreditsLabel(); lbl != "" {
			entry["credits"] = lbl
			entry["description"] = fmt.Sprintf("[%s credit] Trae %s", lbl, m.Name)
		}
		entry["vendor"] = "trae"
		out = append(out, entry)
	}
	return out
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
