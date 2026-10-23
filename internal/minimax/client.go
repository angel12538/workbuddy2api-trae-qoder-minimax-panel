// Package minimax 把 MiniMax Code 客户端内置的模型接口接进网关。
//
// 为什么要单独一层：MiniMax Code 用的是 **Anthropic Messages 协议**
// （POST {base}/llm/v1/messages，SSE 事件为 message_start/content_block_delta/…），
// 而本网关对外统一是 OpenAI 的 /v1/chat/completions。两者必须转换。
//
// 鉴权：客户端的登录态保存在
//
//	~/.minimax/auth/prod/<region>/<clientId>/auth.json
//
// 里（明文 JSON，OAuth accessToken/refreshToken）。请求头直接用
// `Authorization: Bearer <accessToken>` 即可，config.yaml 里的 apiKey 是
// 运行时占位符（实测为 "sk-xxx"），不能拿来用。
//
// 每次请求都会重新读一次 auth.json（带 30s 缓存）：客户端自己续期后，
// 网关这边不用重启就能拿到新 token。
package minimax

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBase 国内站 mavis 网关（模型接口与客户端一致）。
	DefaultBase = "https://agent.minimax.cn/mavis/api/v1"
	llmPath     = "/llm/v1/messages"
	modelsPath  = "/models"
)

// Credentials 一份登录态。
type Credentials struct {
	AccessToken  string    `json:"-"`
	RefreshToken string    `json:"-"`
	ExpiresAt    time.Time `json:"-"`
	Source       string    `json:"-"`
}

// Expired 判断 access token 是否已过期（留 60s 冗余）。
func (c Credentials) Expired() bool {
	return !c.ExpiresAt.IsZero() && time.Now().Add(60*time.Second).After(c.ExpiresAt)
}

// AuthCandidates 返回可能的 auth.json 路径（按修改时间新→旧）。
//
// 目录布局：~/.minimax/auth/prod/{cn,global}/{clientId}/auth.json
// 可用 MINIMAX_AUTH_FILE 显式指定（分发场景/多账号并存时有用）。
func AuthCandidates() []string {
	if v := strings.TrimSpace(os.Getenv("MINIMAX_AUTH_FILE")); v != "" {
		return []string{v}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	root := filepath.Join(home, ".minimax", "auth")
	patterns := []string{
		filepath.Join(root, "prod", "*", "*", "auth.json"),
		filepath.Join(root, "*", "*", "*", "auth.json"),
	}
	var out []string
	for _, p := range patterns {
		ms, _ := filepath.Glob(p)
		out = append(out, ms...)
	}
	sort.Slice(out, func(i, j int) bool {
		fi, _ := os.Stat(out[i])
		fj, _ := os.Stat(out[j])
		if fi == nil || fj == nil {
			return false
		}
		return fi.ModTime().After(fj.ModTime())
	})
	return out
}

// authFile 是 auth.json 的最小结构：records 的键带随机后缀，只能遍历取值。
type authFile struct {
	Records map[string]struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		TokenType    string `json:"tokenType"`
		ExpiresAtMs  int64  `json:"expiresAtMs"`
		ClientID     string `json:"clientId"`
	} `json:"records"`
}

// LoadCredentials 读取最新一份可用登录态。
func LoadCredentials() (Credentials, error) {
	var lastErr error
	for _, p := range AuthCandidates() {
		raw, err := os.ReadFile(p)
		if err != nil {
			lastErr = err
			continue
		}
		var f authFile
		if err := json.Unmarshal(raw, &f); err != nil {
			lastErr = fmt.Errorf("%s: %w", p, err)
			continue
		}
		for _, rec := range f.Records {
			if strings.TrimSpace(rec.AccessToken) == "" {
				continue
			}
			c := Credentials{AccessToken: rec.AccessToken, RefreshToken: rec.RefreshToken, Source: p}
			if rec.ExpiresAtMs > 0 {
				c.ExpiresAt = time.UnixMilli(rec.ExpiresAtMs)
			}
			return c, nil
		}
		lastErr = fmt.Errorf("%s: records 里没有 accessToken", p)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("找不到 MiniMax Code 登录态（%s 下没有 auth.json）", "~/.minimax/auth")
	}
	return Credentials{}, lastErr
}

// Model 一个可路由模型。
type Model struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Context  int    `json:"context"`
	Output   int    `json:"output"`
	ToolCall bool   `json:"tool_call"`
	Reasoning bool  `json:"reasoning"`
}

// Client MiniMax 通道客户端（并发安全）。
type Client struct {
	BaseURL string
	HTTP    *http.Client

	mu       sync.Mutex
	models   []Model
	modelsAt time.Time
	cred     Credentials
	credAt   time.Time
}

// New 构造客户端（默认国内站，30s 超时由调用方覆盖）。
func New() *Client {
	return &Client{BaseURL: DefaultBase, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

// credentials 带 30 秒缓存：既避免每请求读盘，又能在客户端续期后及时跟进。
func (c *Client) credentials() (Credentials, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.credAt) < 30*time.Second && c.cred.AccessToken != "" {
		return c.cred, nil
	}
	cred, err := LoadCredentials()
	if err != nil {
		return Credentials{}, err
	}
	c.cred, c.credAt = cred, time.Now()
	return cred, nil
}

// Models 拉模型目录（缓存 30 分钟）。纯动态，不编造兜底。
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	c.mu.Lock()
	if len(c.models) > 0 && time.Since(c.modelsAt) < 30*time.Minute {
		out := append([]Model(nil), c.models...)
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	cred, err := c.credentials()
	if err != nil {
		return nil, err
	}
	// 目录查询单独限时：/v1/models 不该被上游卡住 120 秒。
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+modelsPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("minimax models HTTP %d: %s", resp.StatusCode, truncate(string(body), 160))
	}
	models := parseModels(body)
	if len(models) == 0 {
		return nil, fmt.Errorf("minimax models 返回空目录")
	}
	c.mu.Lock()
	c.models, c.modelsAt = models, time.Now()
	c.mu.Unlock()
	return models, nil
}

// parseModels 兼容两种形态：{providers:[{config:{models:{id:...}}}]} 与 {models:[...]}。
func parseModels(raw []byte) []Model {
	var top struct {
		Providers []struct {
			ProviderID string `json:"providerId"`
			Config     struct {
				Models map[string]struct {
					Name      string `json:"name"`
					ToolCall  bool   `json:"tool_call"`
					Reasoning bool   `json:"reasoning"`
					Limit     struct {
						Context int `json:"context"`
						Output  int `json:"output"`
					} `json:"limit"`
				} `json:"models"`
			} `json:"config"`
		} `json:"providers"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Model
	for _, p := range top.Providers {
		ids := make([]string, 0, len(p.Config.Models))
		for id := range p.Config.Models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			m := p.Config.Models[id]
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, Model{
				ID: id, Name: firstNonEmpty(m.Name, id),
				Context: m.Limit.Context, Output: m.Limit.Output,
				ToolCall: m.ToolCall, Reasoning: m.Reasoning,
			})
		}
	}
	for _, m := range top.Models {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, Model{ID: m.ID, Name: m.ID})
	}
	return out
}

// Do 把已转换好的 Anthropic 请求发出去，返回原始响应。
func (c *Client) Do(ctx context.Context, body []byte) (*http.Response, error) {
	cred, err := c.credentials()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+llmPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, fmt.Errorf("minimax 登录态失效（401）：请打开 MiniMax Code 重新登录后重试")
	}
	return resp, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
