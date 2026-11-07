// Package qoderapi 把 qoder2api 的 Bridge（Qoder 协议客户端 + OpenAI 兼容转换）
// 以少量稳定 API 暴露给主网关，避免主程序直接依赖其 internal 包。
package qoderapi

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"qoder2api/account"
	"qoder2api/internal/bridge"
	"qoder2api/internal/cosy"
	"qoder2api/logger"
)

// 子模块自身的日志只保留错误级：避免把上游 50KB 模型清单原文打进主网关日志。
func init() { logger.SetLevel("error") }

// basepromptJSON Qoder 会话模板（与 qoder2api 自带的一致）。
//
//go:embed baseprompt.json
var basepromptJSON []byte

// Bridge 是 qoder2api 的桥接实例（单账号模式）。
type Bridge = bridge.Bridge

// Model 是上游模型目录条目。
type Model = bridge.QoderModel

// New 用 Qoder OAuth 凭证（dt- 开头的 access token）构造单账号 Bridge。
// region: "cn" / "global"（空或未知按 global）。
func New(secret, region string) (*Bridge, error) {
	// baseprompt.json 是模板：{UUID1..5} 与 {TIME1} 必须先替换成真实值才是合法 JSON
	//（qoder2api 的 service.go 里同样处理）。
	raw := string(basepromptJSON)
	for _, k := range []string{"{UUID1}", "{UUID2}", "{UUID3}", "{UUID4}", "{UUID5}"} {
		raw = strings.ReplaceAll(raw, k, cosy.NewUUID())
	}
	raw = strings.ReplaceAll(raw, "{TIME1}", fmt.Sprintf("%d", cosy.UnixMs()))
	var tmpl map[string]any
	if err := json.Unmarshal([]byte(raw), &tmpl); err != nil {
		return nil, err
	}
	return bridge.NewBridgeWithFP(secret, account.NormalizeRegion(region), tmpl, nil)
}

// SetQueuePolicy 应用排队重试策略（网关侧不无限等待首字，避免请求悬挂）。
func SetQueuePolicy(firstToken bool, maxRetries, waitCapSec, waitDefaultSec int) {
	bridge.ApplyQueueSettings(maxRetries, waitCapSec, waitDefaultSec, firstToken)
}

// Models 拉取上游模型目录。
func Models(b *Bridge) ([]Model, error) {
	if b == nil {
		return nil, bridge.ErrNoAccount
	}
	return b.ListAvailableModels()
}
