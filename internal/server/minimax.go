package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/minimax"
)

// MiniMax 通道：模型名带 "minimax:" 前缀时整条链路走 Anthropic 协议，
// 与 cn/global 的 WorkBuddy 号池、trae 通道互不影响。

// minimaxModels 返回 MiniMax 侧模型名单（纯动态；未登录/接口不可用 → 不列出）。
func (h *Handler) minimaxModels() []map[string]any {
	if h.cfg.Minimax == nil {
		return nil
	}
	models, err := h.cfg.Minimax.Models(h.baseCtx())
	if err != nil || len(models) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		entry := map[string]any{
			"id":       "minimax:" + m.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "minimax",
			"name":     firstNonEmptyStr(m.Name, m.ID),
		}
		if m.Context > 0 {
			entry["context_length"] = m.Context
		}
		if m.Output > 0 {
			entry["max_output_tokens"] = m.Output
		}
		if m.ToolCall {
			entry["supports_tools"] = true
		}
		out = append(out, entry)
	}
	return out
}

// minimaxChat 处理 minimax: 前缀的对话请求（流式/非流式）。
func (h *Handler) minimaxChat(w http.ResponseWriter, r *http.Request, body []byte, bareModel string, st *chatStat) {
	if h.cfg.Minimax == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "minimax_disabled",
			"MiniMax 通道未启用")
		return
	}
	conv, stream, err := minimax.BuildRequest(body, bareModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	t0 := time.Now()
	resp, err := h.cfg.Minimax.Do(r.Context(), conv)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "minimax_error", err.Error())
		return
	}
	defer resp.Body.Close()
	if st != nil {
		st.ttfb = time.Since(t0)
	}

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeOpenAIError(w, http.StatusBadGateway, "minimax_upstream",
			fmt.Sprintf("MiniMax 上游 HTTP %d: %s", resp.StatusCode, truncateForErr(string(raw), 300)))
		return
	}

	created := time.Now().Unix()
	modelName := "minimax:" + bareModel
	if stream {
		if err := minimax.WriteOpenAIStream(w, resp.Body, modelName, created); err != nil {
			log.Printf("[minimax] 流式转发中断: %v", err)
		}
		return
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "minimax_read", "读取上游响应失败: "+err.Error())
		return
	}
	out, err := minimax.ConvertResponse(raw, modelName, created)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "minimax_convert", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// truncateForErr 截断错误信息，避免把整段 HTML 塞进响应。
func truncateForErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
