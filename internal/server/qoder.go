package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/qoderchat"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// ── Qoder 模型通道（qoder2api Bridge 移植） ─────────────────────────────
//
// 复用 qoder2api（仓库内 qoder2api/ 子模块）的 Qoder 协议实现：
//   - 账号来源：面板「IDE 账号池 → Qoder」已登录的账号（dt- 访问令牌 + scope）
//   - 请求出口：qoder2api 的 OpenAI 兼容 Bridge（流式/非流式、工具调用）
//   - 模型名：网关侧统一加 "qoder:" 前缀，转发前剥掉前缀
//
// 账号令牌由面板的 Qoder 刷新任务维护；这里按令牌变化重建 Bridge（不重复登录）。

// qoderChat 把 qoder: 前缀请求交给 qoder2api Bridge（它直接写 OpenAI 响应）。
func (h *Handler) qoderChat(w http.ResponseWriter, r *http.Request, body []byte, bareModel string, st *chatStat) {
	b, err := qoderchat.Bridge()
	if err != nil {
		log.Printf("qoder: 聊天不可用: %v", err)
		writeOpenAIError(w, http.StatusServiceUnavailable, "qoder_unavailable", err.Error())
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "解析请求体失败: "+err.Error())
		return
	}
	payload["model"] = bareModel
	nb, err := json.Marshal(payload)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "重写请求体失败: "+err.Error())
		return
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(nb))
	r2.ContentLength = int64(len(nb))
	r2.Header = r.Header.Clone()
	r2.Header.Set("Content-Length", strconv.Itoa(len(nb)))
	cw := &qoderCaptureWriter{ResponseWriter: w, st: st, start: time.Now()}
	b.HandleChatCompletions(cw, r2)
	if cw.status > 0 {
		st.status = cw.status
	} else {
		st.status = http.StatusOK
	}
	st.outcome = reqlog.OutcomeSuccess
	if st.status >= 400 {
		st.outcome = reqlog.OutcomeHTTPError
	} else if strings.Contains(cw.buf.String(), `"error"`) && !strings.Contains(cw.buf.String(), `"usage"`) {
		st.outcome = reqlog.OutcomeStreamError
	}
	u := qoderUsageFrom(cw.buf.String())
	if u.total > 0 {
		st.toks = u.total
		st.promptTokens = int64(u.prompt)
		st.completionTokens = int64(u.completion)
		st.totalTokens = int64(u.total)
	} else {
		st.toks = -1
	}
}

// qoderCaptureWriter 透传响应，同时缓存一份用于解析 usage（Bridge 直接写响应）。
type qoderCaptureWriter struct {
	http.ResponseWriter
	buf    bytes.Buffer
	st     *chatStat
	start  time.Time
	first  bool
	status int
}

func (c *qoderCaptureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *qoderCaptureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.buf.Len() < 1<<20 {
		c.buf.Write(p)
	}
	if !c.first {
		c.first = true
		if c.st != nil && c.st.mode == "stream" {
			c.st.ttfb = time.Since(c.start)
		}
	}
	return c.ResponseWriter.Write(p)
}

func (c *qoderCaptureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type qoderUsage struct{ prompt, completion, total int }

// qoderUsageFrom 从非流式 JSON 或 SSE 分块中取 usage 明细（取最后一帧）。
func qoderUsageFrom(body string) qoderUsage {
	var out qoderUsage
	apply := func(raw []byte) {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			return
		}
		u, ok := obj["usage"].(map[string]any)
		if !ok {
			return
		}
		p := intOf(u["prompt_tokens"])
		c := intOf(u["completion_tokens"])
		t := intOf(u["total_tokens"])
		if t <= 0 {
			t = p + c
		}
		if t > 0 {
			out = qoderUsage{prompt: p, completion: c, total: t}
		}
	}
	trim := strings.TrimSpace(body)
	if strings.HasPrefix(trim, "{") {
		apply([]byte(trim))
		return out
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		apply([]byte(data))
	}
	return out
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case int:
		return n
	}
	return 0
}

// qoderModels 返回 /v1/models 的 qoder: 条目（目录缓存 10min，失败返回空）。
func (h *Handler) qoderModels() []map[string]any {
	ms, err := qoderchat.Models()
	if err != nil {
		log.Printf("qoder: 模型目录不可用: %v", err)
		return nil
	}
	out := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		if m.Key == "" || !m.Enable {
			continue
		}
		entry := map[string]any{
			"id":       "qoder:" + m.Key,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "qoder",
			"name":     firstNonEmptyStr(m.DisplayName, m.Key),
			"vendor":   "Qoder",
			"tags":     []string{"qoder"},
		}
		// 上下文长度取该模型所有档位里的最大值（200K/400K/1M），
		// 与网关其它域“透出能力上限”的口径一致。
		ctx := m.ContextWindow
		for _, t := range m.ContextTiers {
			if t.Tokens > ctx {
				ctx = t.Tokens
			}
		}
		if ctx > 0 {
			entry["context_length"] = ctx
		}
		// 上游 model/list 不提供最大输出字段（已核对原始响应），这里不编造。
		if m.IsReasoning || m.SupportsThinking {
			entry["supports_reasoning"] = true
		}
		if m.SupportsThinking {
			if len(m.ThinkingEfforts) > 0 {
				entry["reasoning_supported_efforts"] = m.ThinkingEfforts
			}
			if m.DefaultEffort != "" {
				entry["reasoning_default_effort"] = m.DefaultEffort
			}
		}
		if m.IsDefault {
			entry["is_default"] = true
		}
		if m.PriceFactor > 0 {
			entry["credits"] = fmt.Sprintf("x%.2f", m.PriceFactor)
		}
		out = append(out, entry)
	}
	return out
}
