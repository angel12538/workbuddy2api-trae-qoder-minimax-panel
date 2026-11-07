// Package qoderchat 提供 Qoder 通道的 Bridge 与模型目录（网关/面板共用）。
package qoderchat

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
	"qoder2api/qoderapi"
)

var (
	bridgeMu   sync.Mutex
	bridgeInst *qoderapi.Bridge
	bridgeKey  string

	modelsMu    sync.Mutex
	modelsCache []qoderapi.Model
	modelsAt    time.Time

	regionMu  sync.Mutex
	regionKey string
	regionVal string
)

// ModelsTTL 模型目录缓存时长。
const ModelsTTL = 10 * time.Minute

// Account 取当前可用的 Qoder 凭证：优先当前登录号，其次号池首个可用号。
func Account() (secret, region string, err error) {
	if snap, e := ideauth.CaptureQoderSnapshot(); e == nil && snap != nil && snap.Auth != nil {
		if s := strings.TrimSpace(snap.Auth.Token); s != "" {
			return s, regionForToken(s), nil
		}
	}
	pool, e := ideauth.LoadQoderPool()
	if e != nil {
		return "", "", e
	}
	for _, a := range pool.Accounts {
		if a != nil && a.Auth != nil {
			if s := strings.TrimSpace(a.Auth.Token); s != "" {
				return s, regionForToken(s), nil
			}
		}
	}
	return "", "", errors.New("Qoder 未登录：请先在面板「IDE 账号池 → Qoder」登录或添加账号")
}

// regionForToken 用 /api/v1/userinfo 探测令牌归属区域（global / cn）。
func regionForToken(token string) string {
	regionMu.Lock()
	defer regionMu.Unlock()
	if regionKey == token && regionVal != "" {
		return regionVal
	}
	hosts := []struct{ region, base string }{
		{"global", "https://openapi.qoder.sh"},
		{"cn", "https://openapi.qoder.com.cn"},
	}
	for _, h := range hosts {
		req, err := http.NewRequest(http.MethodGet, h.base+"/api/v1/userinfo", nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "\"id\"") {
			regionKey, regionVal = token, h.region
			return h.region
		}
	}
	regionKey, regionVal = token, "global"
	return "global"
}

// Bridge 返回缓存的 Bridge；令牌变化时重建并清空模型目录缓存。
func Bridge() (*qoderapi.Bridge, error) {
	secret, region, err := Account()
	if err != nil {
		return nil, err
	}
	key := region + "|" + secret
	bridgeMu.Lock()
	defer bridgeMu.Unlock()
	if bridgeInst != nil && bridgeKey == key {
		return bridgeInst, nil
	}
	log.Printf("qoder: 使用账号令牌 prefix=%s len=%d region=%q", secret[:min(6, len(secret))], len(secret), region)
	// 网关场景不无限等首字：排队时最多重试 3 次、单次最长 45s（默认等 15s），
	// 仍失败则返回明确的 503，避免挂数分钟后收到空 200。
	qoderapi.SetQueuePolicy(false, 3, 45, 15)
	b, err := qoderapi.New(secret, region)
	if err != nil {
		log.Printf("qoder: bridge 初始化失败 region=%q: %v", region, err)
		return nil, err
	}
	bridgeInst, bridgeKey = b, key
	modelsMu.Lock()
	modelsCache, modelsAt = nil, time.Time{}
	modelsMu.Unlock()
	return b, nil
}

// Models 返回上游模型目录（10 分钟缓存）。
func Models() ([]qoderapi.Model, error) {
	modelsMu.Lock()
	if len(modelsCache) > 0 && time.Since(modelsAt) < ModelsTTL {
		ms := modelsCache
		modelsMu.Unlock()
		return ms, nil
	}
	modelsMu.Unlock()

	b, err := Bridge()
	if err != nil {
		return nil, err
	}
	ms, err := qoderapi.Models(b)
	if err != nil {
		log.Printf("qoder: 拉取模型目录失败: %v", err)
		return nil, err
	}
	if len(ms) == 0 {
		return nil, errors.New("qoder: 上游模型目录为空")
	}
	modelsMu.Lock()
	modelsCache, modelsAt = ms, time.Now()
	modelsMu.Unlock()
	return ms, nil
}
