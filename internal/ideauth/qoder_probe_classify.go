package ideauth

// ---------------------------------------------------------------------------
// C-QD-PROBE@1 只读隔离探测：输出结构、只读探测封装与机械分类
//
// 只读约束（契约 2.2.9，硬约束）：
//   - 只允许 GET /sash/api/v1/me/campaigns
//   - 只允许 GET /sash/api/v1/me/campaigns/{cid}/reward
//   - 本文件与 cmd/qoderprobe 中不得出现任何 claim（POST）请求，也不得触发
//     deviceToken/refresh（那会消耗并轮换 refresh_token —— 探测是只读的，
//     不能把号池里的凭证弄失效）。
//
// 结论分类全部是**纯函数**（不读环境、不读文件、不发网络请求），
// 严格按计划 2.2.9 的机械规则执行，不做任何人工美化：
//   - 两个号在同一次探测里都看到可领的 CLAIM_BENEFIT，且互相用对方 cid 探测
//     明确不是 same_person → isolated_viable；
//   - 所有已执行变体都复现 SAME_PERSON_ALREADY_CLAIMED，且 device/ip/scope/
//     email_domain 四个维度都被排除 → server_person_binding；
//   - 其余（缺代理 / 网络失败 / 活动未下发 / 问不出结论）→ inconclusive，
//     并把具体缺口写进 missing。
// ---------------------------------------------------------------------------

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// QoderProbeVersion 是输出 JSON 的契约版本（2.2.9 固定为 1）。
const QoderProbeVersion = 1

// 只读接口路径。只暴露这两个常量，防止调用方顺手拼出 claim 路径。
const (
	QoderProbePathCampaigns = "/sash/api/v1/me/campaigns"
)

// QoderProbeRewardPath 返回奖励查询路径（只读 GET）。
func QoderProbeRewardPath(campaignID string) string {
	return fmt.Sprintf("/sash/api/v1/me/campaigns/%s/reward", url.PathEscape(strings.TrimSpace(campaignID)))
}

// 结论枚举（契约 2.2.9）。
const (
	QoderProbeConclusionIsolatedViable      = "isolated_viable"
	QoderProbeConclusionServerPersonBinding = "server_person_binding"
	QoderProbeConclusionInconclusive        = "inconclusive"
)

// criterion_inferred 枚举（计划 2.2.9 赋值规则）。
const (
	QoderProbeCriterionPhone               = "phone"
	QoderProbeCriterionRealName            = "real_name"
	QoderProbeCriterionEmailDomain         = "email_domain"
	QoderProbeCriterionDevice              = "device"
	QoderProbeCriterionIP                  = "ip"
	QoderProbeCriterionScope               = "scope"
	QoderProbeCriterionServerPersonBinding = "server_person_binding"
	QoderProbeCriterionUnknown             = "unknown"
)

// 缺口枚举：写进输出 JSON 的 missing，供 T6/T14 决定下一步。
const (
	QoderProbeMissingProxyNotConfigured = "proxy_not_configured"
	QoderProbeMissingCriterionUnknown   = "criterion_unknown"
	QoderProbeMissingNoVariant          = "no_variant_executed"
)

// ErrQoderProbeTokenExpired 表示本地记录显示令牌已过期。
//
// 探测**不**刷新令牌：refresh 会消耗并轮换 refresh_token，而 T5 是只读任务
// （不改号池）。过期就如实记录缺口，由 T6/T14 的写路径去续期。
var ErrQoderProbeTokenExpired = errors.New("token_expired: 只读探测不刷新令牌（会轮换 refresh_token），请先由写路径续期")

// QoderProbeAccount 输出 JSON 的账号条目（字段与契约 2.2.9 完全一致）。
type QoderProbeAccount struct {
	Email        string `json:"email"`
	UserID       string `json:"user_id"`
	Name         string `json:"name"`
	Scope        string `json:"scope"`
	PhonePresent bool   `json:"phone_present"`
	EmailDomain  string `json:"email_domain"`
	MachineID    string `json:"machine_id"`
	DeviceID     string `json:"device_id"`

	// Phone 只用于分类「手机号是否相同」，不落盘：契约字段固定，不能多输出
	// 一个 phone（手机号属于敏感字段）。
	Phone string `json:"-"`
}

// QoderProbeReward 单个账号对一条活动实例的只读奖励查询结果。
type QoderProbeReward struct {
	Status      string `json:"status"`
	FailureCode string `json:"failure_code"`
	GrantID     string `json:"grant_id"`
	CampaignID  string `json:"campaign_id"`
}

// QoderProbeAccountResult 单变体下单账号的探测结果。
type QoderProbeAccountResult struct {
	Email          string   `json:"email"`
	DeviceMode     string   `json:"device_mode"`
	Proxy          string   `json:"proxy"`
	CampaignsHTTP  int      `json:"campaigns_http"`
	ClaimCampaigns int      `json:"claim_campaigns"`
	CampaignIDs    []string `json:"campaign_ids"`
	CampaignStates []string `json:"campaign_states"`
	// StartAt/EndAt 是本号首个 CLAIM_BENEFIT 活动的窗口（RFC3339，UTC+8）。
	//
	// 团队裁决（@1 向后兼容澄清）新增的可选字段：只有定了活动窗口，才能判断
	// 一个 CLAIMED 活动是不是**今天**下发的 —— 只看状态会把昨天的陈旧活动当成
	// 今天的额度（现网曾经的假签到根因）。空值时 omitempty 不输出，旧消费方无感。
	StartAt string           `json:"start_at,omitempty"`
	EndAt   string           `json:"end_at,omitempty"`
	Reward  QoderProbeReward `json:"reward"`
	Error   string           `json:"error"`

	// 实际使用的设备指纹（不落盘）：分类需要判断「设备是否真的不同」。
	MachineID    string `json:"-"`
	MachineToken string `json:"-"`
}

// QoderProbeVariant 一个探测变体（baseline / derived / derived_proxy）。
type QoderProbeVariant struct {
	Name       string                    `json:"name"`
	PerAccount []QoderProbeAccountResult `json:"per_account"`
}

// QoderProbePairVerdict 一对账号在某个变体下的同人判定。
type QoderProbePairVerdict struct {
	A        string `json:"a"`
	B        string `json:"b"`
	Variant  string `json:"variant"`
	Verdict  string `json:"verdict"` // owner|same_person|other|unknown
	Evidence string `json:"evidence"`
}

// QoderProbeReport 是 C-QD-PROBE@1 的完整输出（字段与 2.2.9 完全一致）。
type QoderProbeReport struct {
	Version           int                     `json:"version"`
	At                string                  `json:"at"`
	Accounts          []QoderProbeAccount     `json:"accounts"`
	Variants          []QoderProbeVariant     `json:"variants"`
	PairVerdicts      []QoderProbePairVerdict `json:"pair_verdicts"`
	CriterionInferred string                  `json:"criterion_inferred"`
	Conclusion        string                  `json:"conclusion"`
	Missing           []string                `json:"missing"`
}

// QoderProbeConclusion 分类结果。
type QoderProbeConclusion struct {
	CriterionInferred string
	Conclusion        string
	Missing           []string
}

// ---------------------------------------------------------------------------
// 只读探测封装
// ---------------------------------------------------------------------------

// QoderProbeReward 只读地问一次「这条活动实例对我意味着什么」。
//
// 与 qoderProbeReward 的唯一区别：本地记录显示令牌已过期时直接返回
// ErrQoderProbeTokenExpired，**不**触发 refresh —— 探测不允许写任何东西，
// 而 refresh 会轮换 refresh_token（丢了这个新 token，号池里的旧 token 就废了）。
func QoderProbeRewardQuery(auth *QoderAuth, dev *QoderDevice, cid, scope string) (QoderRewardProbe, error) {
	if auth == nil {
		return QoderRewardProbe{}, fmt.Errorf("账号登录态为空")
	}
	if strings.TrimSpace(cid) == "" {
		return QoderRewardProbe{}, fmt.Errorf("活动实例 id 为空")
	}
	if qoderAuthExpired(auth) {
		return QoderRewardProbe{}, ErrQoderProbeTokenExpired
	}
	return qoderProbeReward(auth, dev, cid, scope), nil
}

// QoderProbeVerdict 把两个视角的奖励查询结果交给既有的同人判定函数。
func QoderProbeVerdict(ownerProbe, holderProbe QoderRewardProbe) string {
	return qoderPersonVerdict(ownerProbe, holderProbe)
}

// ParseQoderProbeCampaigns 解析活动列表（兼容 data 包裹与顶层两种形态）。
func ParseQoderProbeCampaigns(raw []byte) []QoderCampaign {
	return qoderCampaignsOf(raw)
}

// DeriveQoderProbeDevice 按契约 2.2.6 派生探测用设备。
//
//   - mode=real：克隆 base（QoderDeviceInfo() 的结果），与现网真实设备一致；
//   - mode=derived：调用 qoderDeriveDevice（与 QoderDeviceForEmail 完全同一条
//     派生规则），保证同一邮箱跨进程稳定；
//   - proxyURL：规范化后写入 dev.ProxyURL；空 = 进程默认出口。socks5 等不支持
//     的 scheme 直接报错（绝不静默直连）。
//
// 不用 QoderDeviceForEmail 是为了让变体（real/derived）由 CLI 显式控制，
// 不依赖也不修改 qoder_egress.json（T5 边界：不改配置文件）。
func DeriveQoderProbeDevice(base *QoderDevice, email, mode, proxyURL string) (*QoderDevice, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		m = QoderDeviceModeReal
	}
	proxy, err := normalizeQoderProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	if base == nil {
		base = &QoderDevice{}
	}
	switch m {
	case QoderDeviceModeReal:
		cp := *base
		cp.ProxyURL = proxy
		cp.Derived = false
		cp.Synthetic = false
		return &cp, nil
	case QoderDeviceModeDerived:
		dev := qoderDeriveDevice(email, base)
		dev.ProxyURL = proxy
		return dev, nil
	default:
		return nil, fmt.Errorf("%w: 只允许 %q 或 %q，收到 %q",
			ErrInvalidDeviceMode, QoderDeviceModeReal, QoderDeviceModeDerived, mode)
	}
}

var (
	qoderProbeProxyCredRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s]+@`)
	qoderProbeTokenRe     = regexp.MustCompile(`\b(dt|drt)-[A-Za-z0-9_\-]{4,}`)
)

// RedactProxyURL 脱敏代理地址里的凭证（保留 scheme/host:port，便于识别出口）。
func RedactProxyURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return "***"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}

// SanitizeQoderProbeText 对错误原文做脱敏与截断（不把凭证/token 写进 JSON 与日志）。
func SanitizeQoderProbeText(s string) string {
	s = qoderProbeProxyCredRe.ReplaceAllString(s, "$1***@")
	s = qoderProbeTokenRe.ReplaceAllString(s, "$1-***")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// 机械分类（纯函数）
// ---------------------------------------------------------------------------

// ClassifyQoderProbe 按契约 2.2.9 的机械规则给出 criterion_inferred /
// conclusion / missing。
//
// extraMissing 由 CLI 注入无法从探测结果反推的缺口（例如用户显式选择了不带
// 代理的变体组合）；函数内部还会从结果里补充可反推的缺口。
func ClassifyQoderProbe(accounts []QoderProbeAccount, variants []QoderProbeVariant, pairs []QoderProbePairVerdict, extraMissing []string) QoderProbeConclusion {
	out := QoderProbeConclusion{
		CriterionInferred: QoderProbeCriterionUnknown,
		Conclusion:        QoderProbeConclusionInconclusive,
		Missing:           []string{},
	}
	missing := map[string]bool{}
	addMissing := func(s string) {
		if strings.TrimSpace(s) != "" {
			missing[s] = true
		}
	}
	for _, m := range extraMissing {
		addMissing(m)
	}

	// 1. 已执行变体 + 可反推的缺口。
	var executed []QoderProbeVariant
	for _, v := range variants {
		if len(v.PerAccount) == 0 {
			continue
		}
		executed = append(executed, v)
		for _, pa := range v.PerAccount {
			em := normalizeQoderEmail(pa.Email)
			low := strings.ToLower(pa.Error)
			switch {
			case pa.CampaignsHTTP == 0 && pa.Error != "":
				addMissing("request_failed:" + em)
				if strings.Contains(low, "token_expired") {
					addMissing("token_expired:" + em)
				}
			case pa.CampaignsHTTP != 0 && pa.CampaignsHTTP != 200:
				addMissing(fmt.Sprintf("campaigns_http_%d:%s", pa.CampaignsHTTP, em))
			case pa.CampaignsHTTP == 200 && pa.ClaimCampaigns == 0 && pa.Error == "":
				addMissing("no_claim_benefit:" + em)
			}
		}
	}
	if len(executed) == 0 {
		addMissing(QoderProbeMissingNoVariant)
		out.Missing = sortedQoderProbeKeys(missing)
		return out
	}

	// 2. 是否配置过任何出口代理（没有代理 → IP 维度无法被验证）。
	proxySeen := false
	for _, v := range executed {
		for _, pa := range v.PerAccount {
			if strings.TrimSpace(pa.Proxy) != "" {
				proxySeen = true
			}
		}
	}
	if !proxySeen {
		addMissing(QoderProbeMissingProxyNotConfigured)
	}

	// 3. 复现判定：哪些变体拿到了服务端的 SAME_PERSON_ALREADY_CLAIMED。
	reproduced := map[string]bool{}
	for _, pv := range pairs {
		if pv.Verdict == QoderVerdictSamePerson {
			reproduced[pv.Variant] = true
		}
	}
	allReproduced := len(executed) > 0
	for _, v := range executed {
		if !reproduced[v.Name] {
			allReproduced = false
			break
		}
	}

	// 4. 规则 1：isolated_viable。
	//    任一变体里每个账号都看到可领的 CLAIM_BENEFIT，且互相用对方 cid 探测
	//    **明确**不是 same_person（owner/other 才算明确；unknown 什么也没证明）。
	if len(accounts) >= 2 && qoderProbeIsolated(accounts, executed, pairs) {
		out.Conclusion = QoderProbeConclusionIsolatedViable
		out.CriterionInferred = qoderProbeCriterionForReproduced(accounts, executed, pairs)
		if out.CriterionInferred == "" {
			out.CriterionInferred = QoderProbeCriterionUnknown
		}
		out.Missing = sortedQoderProbeKeys(missing)
		return out
	}

	// 5. 规则 2/3：排除各维度后决定 server_person_binding 还是 inconclusive。
	ex := qoderProbeExclusions(accounts, executed, pairs)
	criterion := qoderProbeCriterionForReproduced(accounts, executed, pairs)

	if allReproduced && ex.device && ex.ip && ex.scope && ex.domain {
		out.Conclusion = QoderProbeConclusionServerPersonBinding
		out.CriterionInferred = QoderProbeCriterionServerPersonBinding
		out.Missing = sortedQoderProbeKeys(missing)
		return out
	}

	out.Conclusion = QoderProbeConclusionInconclusive
	out.CriterionInferred = criterion
	if out.CriterionInferred == "" {
		out.CriterionInferred = QoderProbeCriterionUnknown
	}
	if len(missing) == 0 {
		addMissing(QoderProbeMissingCriterionUnknown)
	}
	out.Missing = sortedQoderProbeKeys(missing)
	return out
}

// qoderProbeIsolated 实现规则 1 的机械判定。
func qoderProbeIsolated(accounts []QoderProbeAccount, variants []QoderProbeVariant, pairs []QoderProbePairVerdict) bool {
	if len(accounts) < 2 {
		return false
	}
	for _, v := range variants {
		if len(v.PerAccount) < 2 {
			continue
		}
		allClaimable := true
		for _, pa := range v.PerAccount {
			if !qoderProbeAccountClaimable(pa) {
				allClaimable = false
				break
			}
		}
		if !allClaimable {
			continue
		}
		ok := true
		for _, a := range v.PerAccount {
			for _, b := range v.PerAccount {
				if strings.EqualFold(a.Email, b.Email) {
					continue
				}
				pv, found := qoderProbeFindPair(pairs, v.Name, a.Email, b.Email)
				if !found {
					ok = false
					continue
				}
				if pv.Verdict != QoderVerdictOther && pv.Verdict != QoderVerdictOwner {
					ok = false
				}
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// qoderProbeAccountClaimable 该账号在该变体里是否看到可领的 CLAIM_BENEFIT。
func qoderProbeAccountClaimable(pa QoderProbeAccountResult) bool {
	if pa.CampaignsHTTP != 200 || pa.ClaimCampaigns <= 0 {
		return false
	}
	for _, s := range pa.CampaignStates {
		if strings.EqualFold(strings.TrimSpace(s), "CLAIMABLE") {
			return true
		}
	}
	return false
}

// qoderProbeExclusions 判断 device/ip/scope/email_domain 四个维度是否被排除。
//
// 语义（严格）：对**每一对**被判定同人的账号，必须存在某个**同样复现同人**的
// 变体让该维度确实不同，才算排除该维度。只在某个变体里不同、但那个变体没有
// 复现，说明不了任何事。
type qoderProbeExclusionSet struct {
	device bool
	ip     bool
	scope  bool
	domain bool
	// ipVerified 是否拿到过任何出口代理证据（无代理 → IP 维度只能算未知）。
	ipVerified bool
}

func qoderProbeExclusions(accounts []QoderProbeAccount, variants []QoderProbeVariant, pairs []QoderProbePairVerdict) qoderProbeExclusionSet {
	ex := qoderProbeExclusionSet{}

	type pairKey struct{ a, b string }
	repPairs := map[pairKey]bool{}
	for _, pv := range pairs {
		if pv.Verdict != QoderVerdictSamePerson {
			continue
		}
		a, b := normalizeQoderEmail(pv.A), normalizeQoderEmail(pv.B)
		if a == "" || b == "" || a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		repPairs[pairKey{a, b}] = true
	}
	if len(repPairs) == 0 {
		return ex
	}

	// 每个维度必须对**每一对**同人账号都有「确实不同」的复现证据，才算排除。
	allDevice, allIP, allScope, allDomain := true, true, true, true
	for key := range repPairs {
		deviceProof, ipProof, scopeProof, domainProof := false, false, false, false
		for _, pv := range pairs {
			if pv.Verdict != QoderVerdictSamePerson {
				continue
			}
			a, b := normalizeQoderEmail(pv.A), normalizeQoderEmail(pv.B)
			if a > b {
				a, b = b, a
			}
			if a != key.a || b != key.b {
				continue
			}
			ra, okA := qoderProbeFindResult(variants, pv.Variant, a)
			rb, okB := qoderProbeFindResult(variants, pv.Variant, b)
			if okA && okB {
				da, db := qoderProbeDeviceIdent(ra), qoderProbeDeviceIdent(rb)
				if da != "" && db != "" && da != db {
					deviceProof = true
				}
				if strings.TrimSpace(ra.Proxy) != "" && strings.TrimSpace(rb.Proxy) != "" {
					ex.ipVerified = true
					if ra.Proxy != rb.Proxy {
						ipProof = true
					}
				}
			}
			aa, okAA := qoderProbeFindAccount(accounts, a)
			ab, okAB := qoderProbeFindAccount(accounts, b)
			if okAA && okAB {
				if strings.TrimSpace(aa.Scope) != "" && strings.TrimSpace(ab.Scope) != "" && aa.Scope != ab.Scope {
					scopeProof = true
				}
				if strings.TrimSpace(aa.EmailDomain) != "" && strings.TrimSpace(ab.EmailDomain) != "" && aa.EmailDomain != ab.EmailDomain {
					domainProof = true
				}
			}
		}
		allDevice = allDevice && deviceProof
		allIP = allIP && ipProof
		allScope = allScope && scopeProof
		allDomain = allDomain && domainProof
	}
	ex.device, ex.ip, ex.scope, ex.domain = allDevice, allIP, allScope, allDomain
	return ex
}

// qoderProbeCriterionForReproduced 按 2.2.9 的赋值顺序推断 criterion_inferred。
//
// 顺序（严格）：phone → real_name → email_domain（其他维度全不同）→
// device/ip/scope（未排除的维度）→ server_person_binding → unknown。
func qoderProbeCriterionForReproduced(accounts []QoderProbeAccount, variants []QoderProbeVariant, pairs []QoderProbePairVerdict) string {
	var repPairs []QoderProbePairVerdict
	for _, pv := range pairs {
		if pv.Verdict == QoderVerdictSamePerson {
			repPairs = append(repPairs, pv)
		}
	}
	if len(repPairs) == 0 {
		return QoderProbeCriterionUnknown
	}

	phoneSame := false
	nameSame := false
	for _, pv := range repPairs {
		a, okA := qoderProbeFindAccount(accounts, pv.A)
		b, okB := qoderProbeFindAccount(accounts, pv.B)
		if !okA || !okB {
			continue
		}
		if a.PhonePresent && b.PhonePresent &&
			strings.TrimSpace(a.Phone) != "" && strings.TrimSpace(a.Phone) == strings.TrimSpace(b.Phone) {
			phoneSame = true
		}
		// 姓名相同必须是「两号都有姓名且相同」；空姓名不算相同。
		if n := strings.TrimSpace(a.Name); n != "" && n == strings.TrimSpace(b.Name) {
			nameSame = true
		}
	}
	if phoneSame {
		return QoderProbeCriterionPhone
	}
	if nameSame {
		return QoderProbeCriterionRealName
	}

	ex := qoderProbeExclusions(accounts, variants, pairs)
	if ex.device && ex.ip && ex.scope && qoderProbeDomainSame(accounts, repPairs) {
		// 设备/IP/scope 全不同、只有邮箱域名相同 → 域名是唯一无法排除的维度。
		return QoderProbeCriterionEmailDomain
	}
	if !ex.device {
		return QoderProbeCriterionDevice
	}
	if !ex.ipVerified {
		// 没有代理证据：IP 维度既没被排除也没被证实，不能编一个结论。
		return QoderProbeCriterionUnknown
	}
	if !ex.ip {
		return QoderProbeCriterionIP
	}
	if !ex.scope {
		return QoderProbeCriterionScope
	}
	if qoderProbeDomainSame(accounts, repPairs) {
		return QoderProbeCriterionEmailDomain
	}
	if ex.domain {
		return QoderProbeCriterionServerPersonBinding
	}
	return QoderProbeCriterionUnknown
}

// qoderProbeDomainSame 是否存在被判定同人的账号对，两号邮箱域名非空且相同。
func qoderProbeDomainSame(accounts []QoderProbeAccount, repPairs []QoderProbePairVerdict) bool {
	for _, pv := range repPairs {
		a, okA := qoderProbeFindAccount(accounts, pv.A)
		b, okB := qoderProbeFindAccount(accounts, pv.B)
		if !okA || !okB {
			continue
		}
		da, db := strings.ToLower(strings.TrimSpace(a.EmailDomain)), strings.ToLower(strings.TrimSpace(b.EmailDomain))
		if da != "" && da == db {
			return true
		}
	}
	return false
}

// qoderProbeDeviceIdent 取设备的比较标识（优先 MachineID，其次 Token+Hostname）。
func qoderProbeDeviceIdent(r QoderProbeAccountResult) string {
	if id := strings.TrimSpace(r.MachineID); id != "" {
		return "id:" + id
	}
	if tok := strings.TrimSpace(r.MachineToken); tok != "" {
		return "tok:" + tok
	}
	return ""
}

func qoderProbeFindResult(variants []QoderProbeVariant, variant, email string) (QoderProbeAccountResult, bool) {
	for _, v := range variants {
		if v.Name != variant {
			continue
		}
		for _, pa := range v.PerAccount {
			if strings.EqualFold(pa.Email, email) {
				return pa, true
			}
		}
	}
	return QoderProbeAccountResult{}, false
}

func qoderProbeFindAccount(accounts []QoderProbeAccount, email string) (QoderProbeAccount, bool) {
	for _, a := range accounts {
		if strings.EqualFold(a.Email, email) {
			return a, true
		}
	}
	return QoderProbeAccount{}, false
}

func qoderProbeFindPair(pairs []QoderProbePairVerdict, variant, a, b string) (QoderProbePairVerdict, bool) {
	for _, pv := range pairs {
		if pv.Variant != variant {
			continue
		}
		if strings.EqualFold(pv.A, a) && strings.EqualFold(pv.B, b) {
			return pv, true
		}
	}
	return QoderProbePairVerdict{}, false
}

func sortedQoderProbeKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
