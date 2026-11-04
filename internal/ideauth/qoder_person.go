package ideauth

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Qoder 每日额度的「同一人」判定。
//
// 2026-11-02 实测（全部真打上游，不是推测）：
//
//  1. 每日 100 Credits 的活动实例是**按人**共享的：qq 号与 gmail 号拿到的是同一条
//     campaignId（01a199dc-ac26-7663-b572-5e467adb6832），qq 先领之后，
//     gmail 用同一个实例 id 查询奖励会得到
//     {"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED"}，
//     而且服务端给 gmail **单独建了一条 grant**（grantId 与 qq 的不同）——
//     说明「同一人已领」是服务端对**人**的判定，而不是对账号、也不是对设备。
//  2. 两个号的 **agentAccountScope 其实不同**
//     （qq=7c3816b4…、gmail=9ecb3ca9…，用客户端数据里的 serverRuntimeId 与
//     服务端 /api/v1/agents 交叉验证得到），可它们照样共享同一份每日额度。
//     ⇒ scope **不能**用来判断「是不是同一个人」，之前拿 scope 分组是错的。
//  3. 设备维度同样无关：两个号的 machine_id / device_id 都不同。
//
// 所以面板要判「这个号今天为什么没签上」，只能问服务端本人 —— 而**读**接口
// GET /sash/api/v1/me/campaigns/{cid}/reward 就能问：
//
//	本人查自己领过的实例 → {"status":"CLAIMED","grantId":"01a19a17-2836…"}
//	同一人查该实例       → {"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED",
//	                        "grantId":"01a19a17-2c30…"}
//
// 这个探测是**只读**的：不写任何状态、不领任何东西，纯粹拿服务端的判定说话。
// （早期实现是拿别的号的实例 id 去 POST claim，虽然能问出结论，但那是写操作，
//  万一两个号不是同一人，就可能动到别人的额度 —— 已改成只读。）

// qoderPathReward 奖励查询（只读）。
const qoderPathReward = "/sash/api/v1/me/campaigns/%s/reward"

// 同一人判定的结论。
const (
	// QoderVerdictOwner 本号就是这条实例的主人（今天这份已在本号账上）。
	QoderVerdictOwner = "owner"
	// QoderVerdictSamePerson 服务端判定：本号与该实例的主人是**同一个人**，
	// 今天那份每日额度已经被对方领走，本号今天没有自己的一份。
	QoderVerdictSamePerson = "same_person"
	// QoderVerdictOther 与该实例的主人**不是同一个人**（本号今天属于「没发额度」，
	// 而不是「被别人抢走」）。
	QoderVerdictOther = "other"
	// QoderVerdictUnknown 上游没给出可判定的答复。
	QoderVerdictUnknown = "unknown"
)

// QoderRewardProbe 一次奖励查询的原始结果。
type QoderRewardProbe struct {
	HTTPStatus  int
	Status      string // CLAIMED / BLOCKED / ""
	GrantID     string
	CampaignID  string
	CampaignKey string
	FailureCode string
	Credits     float64
	ClaimedAt   string
}

// qoderProbeReward 只读地问一次「这条活动实例对我意味着什么」。
func qoderProbeReward(auth *QoderAuth, dev *QoderDevice, cid, scope string) QoderRewardProbe {
	if auth == nil || strings.TrimSpace(cid) == "" {
		return QoderRewardProbe{}
	}
	if dev == nil {
		dev = &QoderDevice{}
	}
	token := auth.Token
	if qoderAuthExpired(auth) {
		if nt, _, ok := qoderRefreshToken(auth, qoderOpenAPIBaseListForScope(scope)[0], dev); ok {
			token = nt
		}
	}
	for _, b := range qoderOpenAPIBaseListForScope(scope) {
		st, raw, err := qoderDo(b, fmt.Sprintf(qoderPathReward, cid), http.MethodGet, token, nil, dev)
		if err != nil {
			continue
		}
		p := QoderRewardProbe{HTTPStatus: st}
		var r struct {
			Status      string `json:"status"`
			GrantID     string `json:"grantId"`
			CampaignID  string `json:"campaignId"`
			CampaignKey string `json:"campaignKey"`
			FailureCode string `json:"failureCode"`
			ErrorCode   string `json:"errorCode"`
			ClaimedAt   string `json:"claimedAt"`
			Benefit     struct {
				Amount float64 `json:"amount"`
			} `json:"benefit"`
			Data *struct {
				Status      string `json:"status"`
				GrantID     string `json:"grantId"`
				CampaignID  string `json:"campaignId"`
				CampaignKey string `json:"campaignKey"`
				FailureCode string `json:"failureCode"`
				ErrorCode   string `json:"errorCode"`
				ClaimedAt   string `json:"claimedAt"`
				Benefit     struct {
					Amount float64 `json:"amount"`
				} `json:"benefit"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		if r.Data != nil {
			r.Status = r.Data.Status
			r.GrantID = r.Data.GrantID
			r.CampaignID = r.Data.CampaignID
			r.CampaignKey = r.Data.CampaignKey
			r.FailureCode = r.Data.FailureCode
			r.ErrorCode = r.Data.ErrorCode
			r.ClaimedAt = r.Data.ClaimedAt
			r.Benefit = r.Data.Benefit
		}
		p.Status = r.Status
		p.GrantID = r.GrantID
		p.CampaignID = r.CampaignID
		p.CampaignKey = r.CampaignKey
		p.FailureCode = firstNonEmpty(r.FailureCode, r.ErrorCode)
		p.ClaimedAt = r.ClaimedAt
		p.Credits = r.Benefit.Amount
		return p
	}
	return QoderRewardProbe{}
}

// qoderPersonVerdict 拿两个视角的奖励查询结果，给出「是不是同一个人」的结论。
//
// ownerProbe = 本号查这条实例；holderProbe = 实例主人查这条实例。
//
//	grantId 相同            → 本号就是主人（今天这份在本号账上）
//	BLOCKED + SAME_PERSON…  → 服务端判定同一人（对方已领走，本号今天没有份）
//	其它                    → 不是同一个人（本号今天只是没被发额度）
func qoderPersonVerdict(ownerProbe, holderProbe QoderRewardProbe) string {
	if ownerProbe.GrantID != "" && ownerProbe.GrantID == holderProbe.GrantID {
		return QoderVerdictOwner
	}
	if strings.EqualFold(ownerProbe.Status, "BLOCKED") &&
		strings.Contains(strings.ToUpper(ownerProbe.FailureCode), "SAME_PERSON") {
		return QoderVerdictSamePerson
	}
	if strings.EqualFold(ownerProbe.Status, "CLAIMED") || strings.EqualFold(ownerProbe.Status, "SUCCESS") {
		return QoderVerdictOwner
	}
	if ownerProbe.HTTPStatus == http.StatusNotFound {
		return QoderVerdictOther
	}
	return QoderVerdictUnknown
}

// ---------------------------------------------------------------------------
// 同一人关系（持久化）
// ---------------------------------------------------------------------------

// qoderPersonLinks 文件：记录「这两个号被服务端判定为同一个人」。
//
// 为什么要存：判定的代价是打上游（每个号一次只读请求）。面板列表页要显示
// 「这个号与谁共享每日额度」，不该每次刷页面都去问一遍上游；而判定结论在同一天
// 内不会变（当天只发一份额度），所以记下来即可。
//
// 格式：{"links":{"a@x.com":"b@x.com"}}（双向存一份即可，读时按无向图处理）。
//
// ⚠️ 写入的结论**可以被反证撤销**：同人判定不是永恒的（账号可能更换手机号/身份，
// 或最初那次判定本身是误判）。当只读探测明确返回 QoderVerdictOther 时，必须
// 用 QoderForgetPersonLink 删掉这对历史关系，否则「同人组」会永久保留，
// 面板继续把两个号算成一组、继续声称「额度被对方领走」——而签到侧已经实测
// 不是同一个人（D-Q1）。unknown 表示证据不足，**不得**据此清除任何关系。
var qoderPersonLinksMu sync.Mutex

type qoderPersonLinksFile struct {
	Links map[string]string `json:"links"`
}

func qoderPersonLinksPath() string {
	return strings.TrimRight(qoderPoolPath(), "/") + ".person.json"
}

// normalizeQoderPersonLinks 归一化关系表：键值小写去空格，丢弃空值与自环。
func normalizeQoderPersonLinks(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.ToLower(strings.TrimSpace(v))
		if k == "" || v == "" || k == v {
			continue
		}
		out[k] = v
	}
	return out
}

// loadQoderPersonLinksLocked 读 person.json（调用方须持 qoderPersonLinksMu）。
func loadQoderPersonLinksLocked() map[string]string {
	raw, err := readFileMaybe(qoderPersonLinksPath())
	if err != nil || raw == "" {
		return map[string]string{}
	}
	var f qoderPersonLinksFile
	if json.Unmarshal([]byte(raw), &f) != nil || f.Links == nil {
		return map[string]string{}
	}
	return normalizeQoderPersonLinks(f.Links)
}

// writeQoderPersonLinksLocked 原子写 person.json（0600，临时文件 + rename）。
//
// 复用号池的 atomicWrite：写入与清除关系都必须原子，否则并发签到/扫描读到的
// 可能是写了一半的关系表。
func writeQoderPersonLinksLocked(links map[string]string) error {
	buf, err := json.MarshalIndent(qoderPersonLinksFile{Links: links}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(qoderPersonLinksPath(), buf)
}

func loadQoderPersonLinks() map[string]string {
	qoderPersonLinksMu.Lock()
	defer qoderPersonLinksMu.Unlock()
	return loadQoderPersonLinksLocked()
}

// saveQoderPersonLink 记下「a 与 b 是同一个人」（双向）。
func saveQoderPersonLink(a, b string) error {
	a = normalizeQoderEmail(a)
	b = normalizeQoderEmail(b)
	if a == "" || b == "" || a == b {
		return nil
	}
	qoderPersonLinksMu.Lock()
	defer qoderPersonLinksMu.Unlock()
	links := loadQoderPersonLinksLocked()
	if links[a] == b && links[b] == a {
		return nil
	}
	links[a] = b
	links[b] = a
	return writeQoderPersonLinksLocked(links)
}

// QoderForgetPersonLink 撤销「a 与 b 是同一个人」这条历史判定（原子写盘）。
//
// 只在**有反证**时调用（见 qoderForgetPersonLinkIfContradicted）：
// 服务端明确判定两者不是同一个人，历史 link 就是过期/错误的结论。
// 没有该关系时直接返回 nil，不写盘、不记日志。
func QoderForgetPersonLink(a, b string) error {
	a = normalizeQoderEmail(a)
	b = normalizeQoderEmail(b)
	if a == "" || b == "" || a == b {
		return nil
	}
	qoderPersonLinksMu.Lock()
	defer qoderPersonLinksMu.Unlock()
	links := loadQoderPersonLinksLocked()
	changed := false
	if links[a] == b {
		delete(links, a)
		changed = true
	}
	if links[b] == a {
		delete(links, b)
		changed = true
	}
	if !changed {
		return nil
	}
	if err := writeQoderPersonLinksLocked(links); err != nil {
		return fmt.Errorf("撤销同人关系 %s ↔ %s 失败: %w", a, b, err)
	}
	// key 按字典序规范化：同一对账号无论调用方传 (a,b) 还是 (b,a)，
	// 每个自然日都只输出一条日志。
	pairKey := a + "|" + b
	if b < a {
		pairKey = b + "|" + a
	}
	qoderNotePersonOnce("forget:"+pairKey,
		"[qoder-person] 探测明确判定不是同一个人，已撤销历史同人关系：%s ↔ %s", a, b)
	return nil
}

// qoderForgetPersonLinkIfContradicted 仅在「探测明确判定不是同一个人」时撤销历史
// 同人关系，返回是否真的清掉了一条关系。
//
// 规则（与 QoderCheckinPoolOrdered / QoderPoolScan 共用，保证两端点口径一致）：
//   - verdict == other → 有反证，清除这对 link；
//   - verdict == same_person / owner → 结论一致或相反方向（对方是主人），保留；
//   - verdict == unknown / 空 → 证据不足，**不清除**（避免把一次网络失败当成反证）。
//
// 清除失败只记日志，不阻断签到/扫描（关系表是解释性数据，不是签到前置条件）。
func qoderForgetPersonLinkIfContradicted(email, other, verdict string) bool {
	if !strings.EqualFold(strings.TrimSpace(verdict), QoderVerdictOther) {
		return false
	}
	a := normalizeQoderEmail(email)
	b := normalizeQoderEmail(other)
	if a == "" || b == "" || a == b {
		return false
	}
	// 先确认这对关系确实存在，避免对不存在的 pair 走一次加锁 + 写盘。
	if links := loadQoderPersonLinks(); links[a] != b {
		return false
	}
	if err := QoderForgetPersonLink(a, b); err != nil {
		log.Printf("[qoder-person] 撤销同人关系失败（不影响签到结论）：%v", err)
		return false
	}
	return true
}

// qoderPersonLogMu/qoderPersonLogDay/qoderPersonLogSet 实现「同一 key 每个自然日
// 最多一条日志」：关系表的清除属于低频事件，但一次批量扫描会对同一 pair 反复
// 判定，必须去重，否则日志会被刷屏。
var (
	qoderPersonLogMu  sync.Mutex
	qoderPersonLogDay string
	qoderPersonLogSet = map[string]bool{}
)

// qoderNotePersonOnce 同一 key 在 UTC+8 自然日内最多输出一条日志。
func qoderNotePersonOnce(key, format string, args ...any) {
	day := time.Now().In(qoderCST).Format("2006-01-02")
	qoderPersonLogMu.Lock()
	if qoderPersonLogDay != day {
		qoderPersonLogDay = day
		qoderPersonLogSet = map[string]bool{}
	}
	if qoderPersonLogSet[key] {
		qoderPersonLogMu.Unlock()
		return
	}
	qoderPersonLogSet[key] = true
	qoderPersonLogMu.Unlock()
	log.Printf(format, args...)
}

// QoderSamePersonPeers 返回号池里与指定账号**被服务端判定为同一人**的其它账号。
//
// 这是「是否共享每日额度」的唯一可信依据（实测依据见本文件顶部）。
// 没有判定记录时返回空 —— 调用方要按「未知」处理，**不能**当成「独立」。
func QoderSamePersonPeers(email string) []string {
	ex := strings.ToLower(strings.TrimSpace(email))
	if ex == "" {
		return nil
	}
	links := loadQoderPersonLinks()
	// 关系是链式的（A~B、B~C ⇒ A、B、C 同一人），用并查集展开。
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if p, ok := parent[x]; ok && p != x {
			r := find(p)
			parent[x] = r
			return r
		}
		if _, ok := parent[x]; !ok {
			parent[x] = x
		}
		return parent[x]
	}
	for k, v := range links {
		parent[find(k)] = find(v)
	}
	root := find(ex)
	var out []string
	for k := range parent {
		if k == ex {
			continue
		}
		if find(k) == root {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// QoderSamePersonGroupSizes 返回每个账号所在「同一人组」的大小（含自己）。
// 没有判定记录的账号算独立一组（大小 1）。
func QoderSamePersonGroupSizes(accounts []*QoderSnapshot) map[string]int {
	links := loadQoderPersonLinks()
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if p, ok := parent[x]; ok && p != x {
			r := find(p)
			parent[x] = r
			return r
		}
		if _, ok := parent[x]; !ok {
			parent[x] = x
		}
		return parent[x]
	}
	for k, v := range links {
		parent[find(k)] = find(v)
	}
	emails := make([]string, 0, len(accounts))
	for _, a := range accounts {
		em := strings.ToLower(strings.TrimSpace(qoderSnapshotEmail(a)))
		if em == "" {
			continue
		}
		emails = append(emails, em)
		find(em)
	}
	count := map[string]int{}
	for _, em := range emails {
		count[find(em)]++
	}
	out := map[string]int{}
	for _, em := range emails {
		out[em] = count[find(em)]
	}
	return out
}

// readFileMaybe 读小文件（关系表读失败按空表处理，调用方不关心具体错误）。
func readFileMaybe(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
