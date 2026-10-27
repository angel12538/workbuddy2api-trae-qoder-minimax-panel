// Package mmapi 把 MiniMax 开放平台的 API Key 接进网关。
//
// 与 internal/minimax 的区别：
//   - minimax：MiniMax Code 客户端内的 OAuth 通道，走 Anthropic Messages 协议；
//   - mmapi  ：开放平台 api.minimaxi.com/v1，**本身就是 OpenAI 兼容格式**，
//     所以这里只做「带密钥转发 + 多 key 轮换」，不需要协议翻译。
//
// 支持按用途分组（例如「按量」与「订阅」分开），每组是一个独立模型前缀：
//
//	{"base_url":"https://api.minimaxi.com/v1","enabled":true,
//	 "groups":[{"id":"mm","name":"MiniMax 按量","keys":["sk-…"]},
//	           {"id":"mms","name":"MiniMax 订阅","keys":["sk-…"]}]}
//
// 兼容老格式 {"keys":[...]}：会当成一个 id=mm 的组。
package mmapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBase 开放平台国内站（国际站为 api.minimax.io/v1，key 不通用）。
const DefaultBase = "https://api.minimaxi.com/v1"

// Client 一个分组的转发器（组内多 key 轮询）。
type Client struct {
	ID   string // 模型前缀：<ID>:<model>
	Name string // 展示名
	Base string
	Keys []string
	HTTP *http.Client

	cur atomic.Uint64

	mu     sync.Mutex
	models []string
	at     time.Time
}

// Registry 全部通道分组（按配置顺序）。
type Registry struct {
	Groups []*Client
	byID   map[string]*Client
}

// Match 按模型名前缀找分组：形如 "mms:MiniMax-M3" → (组, "MiniMax-M3", true)。
func (r *Registry) Match(model string) (*Client, string, bool) {
	if r == nil {
		return nil, "", false
	}
	i := strings.IndexByte(model, ':')
	if i <= 0 {
		return nil, "", false
	}
	if g, ok := r.byID[model[:i]]; ok {
		return g, model[i+1:], true
	}
	return nil, "", false
}

// Load 读取配置；文件不存在/未启用返回 nil（通道关闭）。
func Load(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f struct {
		BaseURL string   `json:"base_url"`
		Enabled bool     `json:"enabled"`
		Keys    []string `json:"keys"`
		Groups  []struct {
			ID   string   `json:"id"`
			Name string   `json:"name"`
			Keys []string `json:"keys"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("密钥文件解析失败: %w", err)
	}
	if !f.Enabled {
		return nil, nil
	}
	base := strings.TrimRight(strings.TrimSpace(f.BaseURL), "/")
	if base == "" {
		base = DefaultBase
	}

	reg := &Registry{byID: map[string]*Client{}}
	add := func(id, name string, keys []string) {
		id = strings.TrimSpace(id)
		var clean []string
		for _, k := range keys {
			if k = strings.TrimSpace(k); k != "" {
				clean = append(clean, k)
			}
		}
		if id == "" || len(clean) == 0 || reg.byID[id] != nil {
			return
		}
		c := &Client{ID: id, Name: name, Base: base, Keys: clean,
			HTTP: &http.Client{Timeout: 300 * time.Second}}
		if c.Name == "" {
			c.Name = id
		}
		reg.Groups = append(reg.Groups, c)
		reg.byID[id] = c
	}

	for _, g := range f.Groups {
		add(g.ID, g.Name, g.Keys)
	}
	if len(f.Groups) == 0 {
		add("mm", "MiniMax", f.Keys) // 兼容老格式
	}
	if len(reg.Groups) == 0 {
		return nil, nil
	}
	return reg, nil
}

// nextKey 轮询挑一把 key。
func (c *Client) nextKey() string {
	n := c.cur.Add(1)
	return c.Keys[int(n-1)%len(c.Keys)]
}

// Models 拉该组的模型列表（缓存 30 分钟）。
func (c *Client) Models(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	if len(c.models) > 0 && time.Since(c.at) < 30*time.Minute {
		out := append([]string(nil), c.models...)
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.nextKey())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("minimax /models HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	c.mu.Lock()
	c.models, c.at = ids, time.Now()
	c.mu.Unlock()
	return ids, nil
}

// Forward 把请求体原样转发到上游（OpenAI 兼容）。
//
// 组内多 key 时，欠费（402）/失效（401）/限流（429）会自动换下一把重试，
// 避免用户看到"余额不足"其实是轮到了坏 key。
func (c *Client) Forward(ctx context.Context, path string, body []byte) (*http.Response, error) {
	var lastErr error
	n := len(c.Keys)
	start := int(c.cur.Add(1) - 1)
	for i := 0; i < n; i++ {
		key := c.Keys[(start+i)%n]
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream, application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusTooManyRequests:
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("该分组的所有 API Key 均不可用（欠费/限流/失效）")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
