package ideauth

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 账号**自身**的 agentAccountScope 解析。
//
// 为什么要单独解析（2026-11-02 实测出的一个真 bug）：
//
//	客户端把「主账号」的数据放在 qoder-data.v1.json 里，其余账号的数据放在
//	qoder-data.v1.ext.json 的 accounts 映射里，键就是各账号的 scope。
//	主文件顶层的 agentAccountScope 字段属于**主账号**，不是「当前登录的那个号」。
//	之前号池存 scope 时直接读主文件，于是给 gmail 也存成了主账号（qq）的 scope。
//
//	怎么判定「哪个 scope 是哪个账号的」——用服务端 agent 运行时 id 交叉验证：
//	客户端每个 scope 桶里记着该账号在服务端注册出来的 serverRuntimeId /
//	serverAgentProfileId；服务端 GET /api/v1/agents 能按账号 token 列出**这个号**
//	自己的 runtimeId / agent id。两者求交集，命中的那个 scope 就是这个账号的。
//
//	实测：qq 的 runtimeId 全部落在主文件（scope 7c3816b4…）里，
//	gmail 的 runtimeId 全部落在 ext 的 accounts["qoder:prod:global:9ecb3ca9…"] 里。
//
//	⚠️ 但 scope **不能**用来判断「两个号是不是同一个人」：这两个号 scope 不同，
//	却照样共享同一份每日额度（服务端按人发，见 qoder_person.go）。

// qoderScopeBucket 一个 scope 桶：scope → 该账号在服务端注册出来的 id 集合。
type qoderScopeBucket struct {
	Scope string
	IDs   map[string]bool
}

// qoderCollectIDs 递归收集结构里的 id 类字段。
func qoderCollectIDs(v any, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				switch k {
				case "serverRuntimeId", "serverAgentProfileId", "serverRegistrationRef", "runtimeId", "id":
					if s != "" {
						out[s] = true
					}
				}
			}
			qoderCollectIDs(val, out)
		}
	case []any:
		for _, e := range t {
			qoderCollectIDs(e, out)
		}
	}
}

// qoderClientScopeBuckets 解析客户端数据文件，返回所有 scope 桶。
func qoderClientScopeBuckets() []qoderScopeBucket {
	dir := QoderDataDir()
	var buckets []qoderScopeBucket

	// 主文件：scope = agentAccountScope，桶里的 id 取自主文件整棵树。
	if raw, err := os.ReadFile(filepath.Join(dir, "qoder-data.v1.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			scope, _ := m["agentAccountScope"].(string)
			scope = strings.TrimSpace(scope)
			if scope != "" {
				ids := map[string]bool{}
				qoderCollectIDs(m, ids)
				buckets = append(buckets, qoderScopeBucket{Scope: scope, IDs: ids})
			}
		}
	}

	// 扩展文件：accounts 映射的键就是各账号的 scope。
	if raw, err := os.ReadFile(filepath.Join(dir, "qoder-data.v1.ext.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			if accs, ok := m["accounts"].(map[string]any); ok {
				for scope, val := range accs {
					scope = strings.TrimSpace(scope)
					if scope == "" || scope == "local" {
						continue
					}
					ids := map[string]bool{}
					qoderCollectIDs(val, ids)
					buckets = append(buckets, qoderScopeBucket{Scope: scope, IDs: ids})
				}
			}
		}
	}
	return buckets
}

// qoderAccountScopeByRuntime 用服务端 /api/v1/agents 与客户端 scope 桶求交集，
// 解出**这个账号自己的** scope。解不出来返回 ("", false)。
func qoderAccountScopeByRuntime(auth *QoderAuth, dev *QoderDevice, scopeHint string) (string, bool) {
	if auth == nil || strings.TrimSpace(auth.Token) == "" {
		return "", false
	}
	buckets := qoderClientScopeBuckets()
	if len(buckets) == 0 {
		return "", false
	}
	if dev == nil {
		dev = &QoderDevice{}
	}
	mine := map[string]bool{}
	for _, b := range qoderOpenAPIBaseListForScope(scopeHint) {
		st, raw, err := qoderDo(b, "/api/v1/agents", http.MethodGet, auth.Token, nil, dev)
		if err != nil || st != http.StatusOK {
			continue
		}
		var resp struct {
			Items []struct {
				ID        string `json:"id"`
				RuntimeID string `json:"runtimeId"`
			} `json:"items"`
		}
		if json.Unmarshal(raw, &resp) != nil {
			continue
		}
		for _, it := range resp.Items {
			if it.ID != "" {
				mine[it.ID] = true
			}
			if it.RuntimeID != "" {
				mine[it.RuntimeID] = true
			}
		}
		break
	}
	if len(mine) == 0 {
		return "", false
	}
	// 命中最多的桶胜出（一个账号理论上只属于一个 scope 桶）。
	best := ""
	bestHits := 0
	for _, b := range buckets {
		hits := 0
		for id := range mine {
			if b.IDs[id] {
				hits++
			}
		}
		if hits > bestHits {
			bestHits, best = hits, b.Scope
		}
	}
	if best == "" || bestHits == 0 {
		return "", false
	}
	return best, true
}

// QoderAccountScope 解析指定登录态**自身**的 scope（解不出返回空串）。
func QoderAccountScope(auth *QoderAuth, dev *QoderDevice) string {
	s, _ := qoderAccountScopeByRuntime(auth, dev, currentQoderScope())
	return s
}

// qoderScopeBucketsForTest 供测试用：返回 scope → id 数量。
func qoderScopeBucketsForTest() map[string]int {
	out := map[string]int{}
	for _, b := range qoderClientScopeBuckets() {
		out[b.Scope] = len(b.IDs)
	}
	return out
}

// qoderSortedScopes 返回排序后的 scope 列表（稳定输出，便于测试与调试）。
func qoderSortedScopes(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
