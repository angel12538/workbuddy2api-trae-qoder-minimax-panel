package ideauth

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 号池多账号签到（契约 C-QD-LEDGER@1 / C-QD-API@1）。
//
// 背景：Qoder 的每日签到（Campaign 领取）只需要两样东西——
//   1) 该账号的 access token（401 时用 refresh_token 续期）；
//   2) 设备指纹 Cosy-*（machineToken/machineCode 等）。
// token 号池里每个账号都存了；设备指纹由 QoderDeviceForEmail 按账号解析
// （默认 = 真实设备，配置了 device_mode=derived / proxy_url 才走独立设备/出口）。
//
// ⚠️ 每日额度是不是每个号一份（2026-11-02 vs 2026-11-04 实测）：
//
//	2026-11-02：服务端回过 SAME_PERSON_ALREADY_CLAIMED，当时判成「按人发，
//	同一人一天只有一份」；
//	2026-11-04：两个号（一个 real、一个 derived 设备）各自拿到**不同的
//	grantId**、各自 claimedAt、额度余额也各自独立（1700 / 900）⇒ 服务端
//	**没有**判这两个号同人，额度是**按账号**各发一份。
//
//	所以「同一人只发一份」只在服务端**明确回** SAME_PERSON_ALREADY_CLAIMED
//	时成立，不能拿它当默认前提。判据见 qoder_person.go 与
//	qoderStrategySnapshot。
//
// T6 起「一键签到」的口径（逐条对应契约 C-QD-API@1）：
//   - **每个号都打一遍**，结果分七相：claimed / already / blocked / pending /
//     failed / login_required / error；七相之和必须等于实际尝试的账号数；
//   - 每个结果都带 strategy / rotation_rank / quota_group / quota_taken_by；
//   - blocked / pending / failed / login_required / error 的 credits 一律为 0
//     （活动面值放 benefit_amount），checked_in_today 一律 false —— 绝不允许把
//     「没签上」显示成「已签到」（旧实现用 Failed==0 && Blocked==0 判聚合，
//     会把 pending 也算成已签）；
//   - 每次尝试都写逐日账本（AppendQoderClaim），用来做按人轮换与审计。
//
// ⚠️ strategy 只能反映**有证据的**结论：拿到本号自己的 grantId → isolated
// （本号独占一份）；服务端明确判同人 / 额度归属另有其人 → shared_person；
// 其余一律 unknown（归属未核实），绝不默认扣上「服务端判定同人」的帽子。

// 七相结果（契约 C-QD-API@1 的 phase 枚举，禁止新增/改名）。
const (
	QoderPhaseClaimed       = "claimed"
	QoderPhaseAlready       = "already"
	QoderPhaseBlocked       = "blocked"
	QoderPhasePending       = "pending"
	QoderPhaseFailed        = "failed"
	QoderPhaseLoginRequired = "login_required"
	QoderPhaseError         = "error"
)

// 策略枚举（契约 C-QD-API@1 的 strategy 枚举，禁止新增/改名）。
const (
	QoderStrategyIsolated     = "isolated"
	QoderStrategySharedPerson = "shared_person"
	QoderStrategyRotated      = "rotated" // 本轮实现不使用（保留值，见契约）
	QoderStrategyUnassigned   = "unassigned"
	QoderStrategyUnknown      = "unknown"
)

// qoderPendingNotAssignedMsg 服务端明确「本号与对方不是同一个人」时 pending 的说明。
//
// 文案必须与 strategy 自洽：unassigned 只表示「本号今天没有被分配到额度」，
// 不能写成与某个号「是同一个人」的肯定句，也不能出现 shared_person 才成立的
// 「额度已被对方领走」。两端口点（POST checkin / GET checkin-status）共用本常量。
//
// ⚠️ 2026-11-09 改写：旧文案只说「服务端没下发 + 已核实未被占用」，用户看到
// 一句「没下发」完全不知道下一步做什么，表现为「两种签到方式都试过还是签不了」。
// 实测（QQ 号 11-08 / 11-09 连续 pending）：该号的活动列表里**根本没有当天的
// 每日 CLAIM_BENEFIT**，只剩一条早先的促销；而它最近一次领到的是 11-07 的
// 特殊活动（act-20261105-316，2000 分）。根因是**该号的客户端登录身份过期**：
// 每日活动只对「在 Qoder 客户端登录过（= 服务端给这个号发过 scope）」的账号
// 下发，纯 API 签不到没下发的活动 —— 必须让用户在客户端里用这个号真登录一次。
// 所以这里要直接把「下一步怎么做」写出来。
const qoderPendingNotAssignedMsg = "服务端今天没有给本号下发每日额度（已核实：这份额度没有被号池里的其它账号领走）" +
	"；多半是这个号的客户端登录身份过期了 —— 请点该号的「重新登录该号」，在 Qoder 客户端里用这个号扫码登录一次，" +
	"服务端会重新给它下发当日活动，之后面板就能自动签到"

// qoderPendingUnknownMsg 证据不足时的 pending 说明（未探测出任何同人结论）。
const qoderPendingUnknownMsg = "未观察到今日签到活动（Qoder 每天 10:00 后下发；后台会自动重试）"

// QoderCheckinItem 单账号签到结果（契约 C-QD-API@1 的 results[] 元素）。
//
// Credits 只表示**本号本次实际入账**的积分；blocked/pending/failed/
// login_required/error 必须为 0，活动面值放 BenefitAmount（可选）。
type QoderCheckinItem struct {
	Email          string  `json:"email"`
	Name           string  `json:"name,omitempty"`
	UID            string  `json:"uid,omitempty"`
	Phase          string  `json:"phase"`
	Msg            string  `json:"msg,omitempty"`
	Credits        float64 `json:"credits"`
	BenefitAmount  float64 `json:"benefit_amount,omitempty"`
	CheckedInToday bool    `json:"checked_in_today"`
	ClaimedAt      string  `json:"claimed_at"`
	DeviceOK       bool    `json:"device_ok"`
	Refreshed      bool    `json:"refreshed,omitempty"`
	Exe            string  `json:"umid_exe,omitempty"`
	Base           string  `json:"base,omitempty"`
	Scope          string  `json:"scope,omitempty"`
	ClaimCID       string  `json:"claim_cid,omitempty"`
	QuotaGroup     int     `json:"quota_group"`
	QuotaTakenBy   string  `json:"quota_taken_by"`
	Strategy       string  `json:"strategy"`
	RotationRank   int     `json:"rotation_rank"`
	LastClaimedAt  string  `json:"last_claimed_at,omitempty"`
}

// QoderQuotaGroupView 同一个人的额度分组（用于解释「为什么这个号没领到」）。
//
// QuotaGroup = 组内账号数（与服务端「一人一天一份」的口径一致）；ClaimedBy =
// 本轮/今日真正拿到那份额度的账号；Credits = 该账号入账的积分。
type QoderQuotaGroupView struct {
	QuotaGroup int      `json:"quota_group"`
	Emails     []string `json:"emails"`
	ClaimedBy  string   `json:"claimed_by,omitempty"`
	Credits    float64  `json:"credits"`
}

// QoderCheckinPoolResult 一次多号签到的汇总（面板直接展示）。
type QoderCheckinPoolResult struct {
	Results []QoderCheckinItem `json:"results"`
	// RotationOrder 本轮**实际尝试顺序**（契约 rotation_order / rotation_rank 1 起）。
	RotationOrder []string `json:"rotation_order"`
	// QuotaGroups 只列出「同一人组」里账号数 > 1 的组（单号组没有分桶意义）。
	QuotaGroups []QoderQuotaGroupView `json:"quota_groups"`

	Claimed int `json:"claimed_accounts"` // 本次新领取的账号数
	Already int `json:"already_accounts"` // 今日已领过的账号数
	// Blocked 今天**没签上**的账号数：服务端判定「同一个人另一账号已领走每日额度」。
	// 这不是失败（重试也没用），也不是"已签到"（本号根本没领到）—— 单列一栏。
	Blocked int `json:"blocked_accounts"`
	// Pending 今天服务端**没有给本号下发**每日额度的账号数（与"被别人领走"不同：
	// 被领走的是同一个人；pending 是这个号本来就不在当天活动名单里）。
	Pending int `json:"pending_accounts"`
	// Failed 真正失败的尝试数（上游请求失败 / 领取被拒，但凭证还在）。
	Failed int `json:"failed_accounts"`
	// LoginRequired 需要重新登录的账号数（凭证缺失/失效且续期失败）。
	LoginRequired int `json:"login_required_accounts"`
	// Error 本地/结构性错误（设备或出口配置非法、号池读取异常等）。
	Error int `json:"error_accounts"`

	// Credits 本次**新增**的积分（只累加 claimed）；AlreadyCredits 是今日已领账号的额度，
	// 单独返回，避免把「今日已领」误报成「本次又赚了」。
	Credits        float64 `json:"credits_total"`
	AlreadyCredits float64 `json:"credits_already"`
	NoDevice       bool    `json:"no_device"` // 缺机器指纹（易被风控）
	// CheckedInToday 顶层聚合：**所有**账号都是 claimed/already 时才为 true。
	// 任何一个 blocked / pending / failed / login_required / error 都必须为 false
	// （旧实现只看 Failed/Blocked，把 pending 也算成已签）。
	CheckedInToday bool `json:"checked_in_today"`
	// LedgerError 账本写入失败的原因（审计辅助数据，不影响本次签到结论）。
	LedgerError string `json:"ledger_error,omitempty"`
}

// qoderPoolItem 号池里待处理的一个账号。
type qoderPoolItem struct {
	snap  *QoderSnapshot
	email string
}

// QoderCheckinPool 按号池逐个账号签到。emails 为空 = 号池全部账号。
//
// 每个账号独立使用自己的 token；401 时各自续期，并把轮换后的 token 写回**号池**
// （绝不写客户端 auth.v1.dat —— 那是「当前登录号」的凭据，串写会直接把别的号顶掉）。
func QoderCheckinPool(emails []string) (*QoderCheckinPoolResult, error) {
	return QoderCheckinPoolOrdered(emails, "")
}

// QoderCheckinPoolOrdered 同 QoderCheckinPool，但 priority 指定的账号先打。
//
// 「先打先得」是 Qoder 的硬规则：每日 100 Credits 按**人**给，同一个人的多个
// 账号一天只有一份（实测服务端返回 failureCode=SAME_PERSON_ALREADY_CLAIMED）。
// priority 让用户能指定「今天这 100 给哪个号」—— 一键依然是把**所有**号都打一遍，
// 只是顺序可指定，其余号照常尝试并如实回报结果。
func QoderCheckinPoolOrdered(emails []string, priority string) (*QoderCheckinPoolResult, error) {
	pool, err := LoadQoderPool()
	if err != nil {
		return nil, err
	}
	items := qoderPoolItems(pool, emails)
	order := qoderRotationOrder(items, priority)
	items = qoderReorderItems(items, order)

	out := &QoderCheckinPoolResult{
		Results:       make([]QoderCheckinItem, 0, len(items)),
		RotationOrder: order,
		QuotaGroups:   []QoderQuotaGroupView{},
	}
	baseDev := QoderDeviceInfo()
	out.NoDevice = baseDev == nil || baseDev.MachineToken == ""

	// 「同一人」分组：**只认服务端判定过的关系**（见 qoder_person.go）。
	groupSize := QoderSamePersonGroupSizes(pool.Accounts)
	// T5 探测结论只用于 strategy 文案；文件缺失/损坏 = 默认策略（shared_person + 轮换）。
	probeConclusion := qoderProbeConclusion()

	// 上一次真领到的时刻要在写账本之前取好：本次 claimed 写进账本后会变成最近一条，
	// 而结果项里的 last_claimed_at 要展示的是「本次之前」的历史（claimed 号除外）。
	prevClaimed := make(map[string]string, len(items))
	for _, it := range items {
		prevClaimed[normalizeQoderEmail(it.email)] = qoderLedgerLastClaimedRFC3339(it.email)
	}

	// 本次运行里已知的活动实例 id（账号 → cid）：先领到的号会贡献自己的 cid，
	// 后面的号就拿它去**只读**问一次「我是不是同一个人」。
	knownCID := map[string]string{}
	// 每个账号今天那份「是否同一个人」的只读判定结论（QoderVerdict*）。
	// strategy 由 qoderStrategySnapshot 统一推导，必须拿到它才能区分
	// pending+unassigned（明确非同人）与 pending+unknown（证据不足）。
	verdicts := make(map[string]string, len(items))
	// 号池回写补丁（token 轮换 / ClaimCID）。全部通过 UpdateQoderPool 落盘。
	type poolPatch struct {
		access, refresh, claimCID string
	}
	patches := map[string]poolPatch{}

	// clientFresh：本轮用了「客户端当前登录态」的账号 → 客户端那份凭证。
	// 用来把新 token 写回号池（号池快照本来停在旧 token 上）。
	clientFresh := map[string]*QoderAuth{}

	// 需要二次判定的账号（自己的列表里没有今天的活动）。
	type pendingProbe struct {
		item qoderPoolItem
		res  *QoderCheckinResult
		auth *QoderAuth
		dev  *QoderDevice
		rank int
	}
	var probes []pendingProbe

	// 「今天已领（already）」的账号：也要问一次「这份额度是不是**我自己的」。
	//
	// 为什么：上游活动列表对同一人组的所有号都返回**同一条**已领活动，所以
	// 「already」不等于「本号领过」。不问的话：
	//   - POST 侧只能写「归属未核实」，而 GET 扫描侧问得出「是我自己的」，
	//     同一个号两个说法（前端和用户都会懵）；
	//   - 同人组里后打的号会显示 +100，而那 100 其实进了另一个号的账。
	//
	// 探测是**只读**的（GET .../campaigns/{cid}/reward），不碰任何额度。
	type alreadyProbe struct {
		item qoderPoolItem
		res  *QoderCheckinResult
		auth *QoderAuth
		dev  *QoderDevice
		rank int
	}
	var alreadyProbes []alreadyProbe

	rank := 0
	for _, it := range items {
		rank++
		email := it.email
		key := normalizeQoderEmail(email)
		auth := cloneQoderAuth(it.snap.Auth)
		scope := QoderQuotaScopeKey(it.snap)

		// 客户端里这个号可能持有一份**更新的会话**（用户在软件里登录/领过额度，
		// 客户端会自己刷新 token + scope）。号池里的快照还停在旧值时，面板拿着
		// 旧 token 去问上游 —— 上游对认不出来的会话一律回 200 + 空 campaigns，
		// 于是面板显示「今日未下发」，而软件里明明有任务（2026-11-09 用户反馈）。
		// 这里先对齐：客户端登录的就是这个号时，用客户端那份凭证，并把新 token
		// 写回号池（下面的 patches 通道）。
		if ca := QoderClientAuthFor(email); ca != nil &&
			strings.TrimSpace(ca.Token) != "" &&
			strings.TrimSpace(ca.Token) != strings.TrimSpace(auth.Token) {
			auth = cloneQoderAuth(ca)
			it.snap.Auth = cloneQoderAuth(ca)
			clientFresh[key] = ca
		}

		// 按账号解析设备/出口（默认真实设备，行为与现网一致）。
		dev, devErr := qoderDeviceForEmailSafe(email)
		if devErr != nil {
			verdicts[key] = QoderVerdictUnknown
			out.Results = append(out.Results, QoderCheckinItem{
				Email:          email,
				Name:           qoderItemName(it.snap),
				UID:            qoderItemUID(it.snap),
				Phase:          QoderPhaseError,
				Msg:            "该账号设备/出口配置不可用：" + devErr.Error(),
				Credits:        0,
				CheckedInToday: false,
				Scope:          scope,
				QuotaGroup:     groupSize[key],
				Strategy:       qoderStrategySnapshot(email, QoderPhaseError, "", groupSize[key], QoderVerdictUnknown, probeConclusion),
				RotationRank:   rank,
				LastClaimedAt:  prevClaimed[key],
			})
			continue
		}
		// 出口代理配置非法（手写进 json 的 socks5 等）：QoderDo 会在发请求时才报，
		// 那时会被误判成「需要重新登录」。这里提前判掉，给出准确的 error 相位。
		if _, perr := parseQoderProxyURL(dev.ProxyURL); perr != nil {
			verdicts[key] = QoderVerdictUnknown
			out.Results = append(out.Results, QoderCheckinItem{
				Email:          email,
				Name:           qoderItemName(it.snap),
				UID:            qoderItemUID(it.snap),
				Phase:          QoderPhaseError,
				Msg:            "该账号出口代理配置非法：" + perr.Error(),
				CheckedInToday: false,
				DeviceOK:       dev.MachineToken != "" && dev.MachineID != "",
				Scope:          scope,
				QuotaGroup:     groupSize[key],
				Strategy:       qoderStrategySnapshot(email, QoderPhaseError, "", groupSize[key], QoderVerdictUnknown, probeConclusion),
				RotationRank:   rank,
				LastClaimedAt:  prevClaimed[key],
			})
			continue
		}

		res, at, rt := qoderCheckinAuth(auth, dev, scope)
		if res == nil {
			res = &QoderCheckinResult{Phase: QoderPhaseError, Msg: "签到未返回结果"}
		}
		res.Email = firstNonEmpty(email, res.Email)
		res.Scope = scope
		res.QuotaGroup = groupSize[key]

		// 用的是客户端那份会话时，把客户端的 token 一并写回号池（下面统一落盘）。
		if ca := clientFresh[key]; ca != nil {
			if at == "" {
				at = strings.TrimSpace(ca.Token)
			}
			if rt == "" {
				rt = strings.TrimSpace(ca.RefreshToken)
			}
		}

		// token / ClaimCID 变更收集起来，最后一次性走 UpdateQoderPool。
		if at != "" || rt != "" || res.ClaimCID != "" {
			p := patches[key]
			if at != "" {
				p.access = at
			}
			if rt != "" {
				p.refresh = rt
			}
			if res.ClaimCID != "" {
				p.claimCID = res.ClaimCID
			}
			patches[key] = p
		}
		// 内存里的快照也同步：稍后同人判定要用它当 holder 凭证。
		if at != "" {
			it.snap.Auth.Token = at
		}
		if rt != "" {
			it.snap.Auth.RefreshToken = rt
		}
		if res.ClaimCID != "" {
			it.snap.ClaimCID = res.ClaimCID
			knownCID[key] = res.ClaimCID
		}

		// 相位归一：qoderCheckinAuth 的 error 代表**上游请求失败**（链路还在），
		// 归为 failed；本地/结构性错误已在上面单独归为 error。
		if res.Phase == QoderPhaseError {
			res.Phase = QoderPhaseFailed
		}
		// T5 实测：上游没给某号下发活动实例时（claim_campaigns=0），claim 可能直接
		// 返回 404 GRANT_NOT_FOUND —— 这既不是「同人已领」（blocked），也不是失败：
		// 本号今天根本没有活动可领，落 pending（契约明确要求）。
		if res.Phase == QoderPhaseFailed && qoderIsGrantNotFound(res.Msg) {
			res.Phase = QoderPhasePending
			res.Msg = "上游未下发活动实例（claim_campaigns=0，claim 返回 404 GRANT_NOT_FOUND）"
		}

		if res.Phase == QoderPhasePending {
			// 自己的列表里没有今天的活动：交给下面的二次判定（只读地问服务端）。
			probes = append(probes, pendingProbe{item: it, res: res, auth: auth, dev: dev, rank: rank})
			continue
		}
		if res.Phase == QoderPhaseAlready {
			// 「今天已领」不等于「本号领过」：上游对同一人组的所有号返回同一条已领
			// 活动。放到主循环**之后**统一只读探测，那时 knownCID 才是全的
			// （要拿别人实例 id 去找真正的主人）。
			alreadyProbes = append(alreadyProbes, alreadyProbe{item: it, res: res, auth: auth, dev: dev, rank: rank})
			continue
		}

		// 签到侧自己就拿到「同人已领」结论时（claim 返回 BLOCKED），把 verdict 记下来，
		// 供最终 strategy 重算使用（blocked 恒为 shared_person）。
		directVerdict := QoderVerdictUnknown
		switch res.Phase {
		case QoderPhaseBlocked:
			directVerdict = QoderVerdictSamePerson
		case QoderPhaseClaimed:
			// 本次真的领到了：额度就在**本号**账上（自己的 grantId）⇒ 本号独占一份。
			directVerdict = QoderVerdictOwner
		}
		verdicts[key] = directVerdict
		item := qoderItemFromResult(it, res, rank, groupSize[key], probeConclusion, prevClaimed[key], directVerdict)
		qoderApplyCheckinSideEffects(item, res)
		out.Results = append(out.Results, item)
	}

	// 二次判定：自己的列表里没有今天的活动 → **只读**地问服务端「我是不是那个人」。
	//
	// 做法：拿号池里另一个账号今天那条活动实例 id，用**只读**接口
	// GET /sash/api/v1/me/campaigns/{cid}/reward 问两次 —— 一次用本号凭证，
	// 一次用实例主人的凭证，然后比 grantId / failureCode：
	//
	//	grantId 相同                    → 本号就是主人（今天这份在本号账上）
	//	BLOCKED + SAME_PERSON_ALREADY_… → 服务端判定**同一个人**，对方已领走
	//	其它                            → 不是同一个人（本号今天只是没被发额度）
	//
	// ⚠️ 为什么是只读（2026-11-02）：上一版是拿别人的实例 id 去 POST claim，
	// 那是**写操作** —— 万一两个号不是同一个人，就可能把别人的额度领走。
	// 只读判定拿到的结论一样，但绝不碰别人的额度。
	//
	// ⚠️ 为什么候选不再按 scope 过滤：实测两个号的 scope **不同**却共享同一份
	// 额度（scope 不是分组键），按 scope 过滤会把真正的「同一人」排除掉。
	for i := range probes {
		p := &probes[i]
		email := p.item.email
		key := normalizeQoderEmail(email)
		// 候选按邮箱排序，保证每次挑到的是同一个（map 遍历顺序随机）。
		var candidates []string
		for em := range knownCID {
			if strings.EqualFold(em, email) {
				continue
			}
			candidates = append(candidates, em)
		}
		sort.Strings(candidates)
		// 号池快照里可能已存着别的号的实例 id（上次扫描记下的）。
		var snapCands []string
		for _, o := range pool.Accounts {
			if o == nil || o.ClaimCID == "" {
				continue
			}
			if strings.EqualFold(qoderSnapshotEmail(o), email) {
				continue
			}
			snapCands = append(snapCands, strings.TrimSpace(qoderSnapshotEmail(o)))
		}
		sort.Strings(snapCands)

		verdict := QoderVerdictUnknown
		holder := ""
		amount := 0.0
		claimedAt := ""
		for _, em := range candidates {
			cid := knownCID[normalizeQoderEmail(em)]
			holderAuth := qoderPoolAuth(pool.Accounts, em)
			if cid == "" || holderAuth == nil {
				continue
			}
			ownerProbe := qoderProbeReward(p.auth, p.dev, cid, QoderQuotaScopeKey(p.item.snap))
			holderProbe := qoderProbeReward(holderAuth, baseDev, cid, QoderQuotaScopeKey(p.item.snap))
			v := qoderPersonVerdict(ownerProbe, holderProbe)
			if v == QoderVerdictSamePerson || v == QoderVerdictOwner {
				verdict, holder = v, em
				amount = ownerProbe.Credits
				claimedAt = ownerProbe.ClaimedAt
				break
			}
			if v == QoderVerdictOther {
				// 明确判定「不是同一个人」：本号今天没被发额度，与对方无关。
				// 历史同人 link 由此被反证 → 立即撤销（unknown 不清，见函数注释）。
				qoderForgetPersonLinkIfContradicted(email, em, v)
				if verdict == QoderVerdictUnknown {
					verdict = QoderVerdictOther
				}
			}
		}
		if verdict == QoderVerdictUnknown && len(snapCands) > 0 {
			// 本次运行没拿到 cid（别的号今天还没领），退回号池快照里存的实例 id。
			for _, em := range snapCands {
				var cid string
				for _, o := range pool.Accounts {
					if o != nil && strings.EqualFold(qoderSnapshotEmail(o), em) {
						cid = o.ClaimCID
						break
					}
				}
				holderAuth := qoderPoolAuth(pool.Accounts, em)
				if cid == "" || holderAuth == nil {
					continue
				}
				ownerProbe := qoderProbeReward(p.auth, p.dev, cid, QoderQuotaScopeKey(p.item.snap))
				holderProbe := qoderProbeReward(holderAuth, baseDev, cid, QoderQuotaScopeKey(p.item.snap))
				v := qoderPersonVerdict(ownerProbe, holderProbe)
				if v == QoderVerdictSamePerson || v == QoderVerdictOwner {
					verdict, holder = v, em
					amount = ownerProbe.Credits
					claimedAt = ownerProbe.ClaimedAt
					break
				}
				if v == QoderVerdictOther {
					// 快照路径同样可能拿到明确反证：撤销历史 link，保证下次扫描/签到
					// 不再把这两个号算成一个额度组。
					qoderForgetPersonLinkIfContradicted(email, em, v)
					if verdict == QoderVerdictUnknown {
						verdict = QoderVerdictOther
					}
				}
			}
		}

		switch verdict {
		case QoderVerdictSamePerson:
			// 服务端明确答复：**同一个人**今天已经领过了 —— 也就是说**本号今天
			// 并没有签到**，今天的 100 积分也没进它的账。
			//
			// ⚠️ 这里绝不能写 MarkCheckedIn —— 那是"假签到"：本号压根没领到。
			// 也不能算进 Already（那也是"今天签过了"的意思）。单列 blocked，
			// 并把之前可能存在的误记清掉（Unmark）。
			p.res.Phase = QoderPhaseBlocked
			p.res.Msg = "今天没签上：服务端判定本号与 " + holder +
				" 是同一个人（SAME_PERSON_ALREADY_CLAIMED），今天那份每日额度已由对方领走"
			p.res.CheckedInToday = false
			p.res.QuotaTakenBy = holder
			if amount > 0 {
				// 暂存活动面值：构建结果项时转成 benefit_amount，credits 归零。
				p.res.Credits = amount
			}
			if err := saveQoderPersonLink(email, holder); err != nil {
				// 关系表写失败不改变本次签到结论（blocked 依旧如实回报）。
				log.Printf("[qoder-person] 记录同人关系 %s ↔ %s 失败（不影响签到结论）：%v", email, holder, err)
			}
		case QoderVerdictOwner:
			// 本号就是这条实例的主人（列表没显示，但服务端账上有）。
			p.res.Phase = QoderPhaseAlready
			p.res.Msg = "今日已领取（服务端奖励记录）"
			p.res.CheckedInToday = true
			if amount > 0 {
				p.res.Credits = amount
			}
			if claimedAt != "" {
				p.res.ClaimedAt = claimedAt
				p.res.ClaimedToday = true
			}
		case QoderVerdictOther:
			// 服务端明确说：本号与对方**不是同一个人**，今天这份额度不归本号，
			// 也不是被对方抢走 —— 就是「服务端今天没给本号下发」。
			p.res.Phase = QoderPhasePending
			p.res.Msg = qoderPendingNotAssignedMsg
			p.res.CheckedInToday = false
			p.res.QuotaTakenBy = ""
		default:
			// 问不出结论：先按本地可信记录兜底，再按「同人组里今天已有持有者」兜底
			// （与 GET checkin-status 的 QuotaHolderInGroup 完全同一口径），
			// 最后才如实说"没观察到" —— 否则同一账号在两端点会一个是
			// shared_person、一个是 unknown。
			if CheckinRecordTrusted("qoder", email) {
				p.res.Phase = QoderPhaseAlready
				p.res.Msg = "今日已领取（本地记录）"
				p.res.CheckedInToday = true
			} else if holder, ok := QuotaHolderInGroup(email); ok {
				p.res.Phase = QoderPhasePending
				p.res.QuotaTakenBy = holder
				p.res.Msg = "今日每日额度已被同一个人的另一个账号 " + holder +
					" 领取（服务端判定同一人，一天只发一份、先打先得）"
				p.res.CheckedInToday = false
			} else {
				p.res.Phase = QoderPhasePending
				p.res.Msg = qoderPendingUnknownMsg
				p.res.CheckedInToday = false
			}
		}

		verdicts[key] = verdict
		item := qoderItemFromResult(p.item, p.res, p.rank, groupSize[key], probeConclusion, prevClaimed[key], verdict)
		qoderApplyCheckinSideEffects(item, p.res)
		out.Results = append(out.Results, item)
	}

	// 「already」号的归属探测（只读）：问服务端「这条已领活动是不是我自己的」。
	//
	// 判据与 GET 扫描侧 path A 完全一致（同一个 cid、同一套 grantId 比对）：
	//   - 拿到自己的 grantId ⇒ 本号独占一份（verdict=owner）；
	//   - 服务端回 BLOCKED + SAME_PERSON_ALREADY_CLAIMED ⇒ 本号被拒，那份额度
	//     是同人的另一个号领的 ⇒ 点名 + credits 归零（由 qoderFillAlreadyQuotaOwner
	//     完成）+ checked_in_today=false；
	//   - 问不出结论 ⇒ 归属未核实（unknown），**不猜**。
	//
	// ⚠️ 必须在这里做（主循环之后）：找主人要用到别的号的实例 id，knownCID 到
	// 这时才是全的。
	for i := range alreadyProbes {
		ap := &alreadyProbes[i]
		email := ap.item.email
		key := normalizeQoderEmail(email)
		scope := QoderQuotaScopeKey(ap.item.snap)
		verdict := QoderVerdictUnknown
		if ap.res.ClaimCID != "" {
			probe := qoderProbeReward(ap.auth, ap.dev, ap.res.ClaimCID, scope)
			switch {
			case strings.EqualFold(probe.Status, "BLOCKED") &&
				strings.Contains(strings.ToUpper(probe.FailureCode), "SAME_PERSON"):
				verdict = QoderVerdictSamePerson
				// 找出真正领走这份额度的号：用**它自己的凭证**只读问同一个 cid。
				holder := ""
				for em := range knownCID {
					if strings.EqualFold(em, email) {
						continue
					}
					ha := qoderPoolAuth(pool.Accounts, em)
					if ha == nil {
						continue
					}
					hp := qoderProbeReward(ha, baseDev, ap.res.ClaimCID, scope)
					if hp.GrantID != "" {
						holder = em
						break
					}
				}
				ap.res.QuotaTakenBy = holder
				ap.res.CheckedInToday = false
				ap.res.Msg = "今日额度已被同一个人的另一个账号 " + holder +
					" 领走（服务端判定同一人，一天只发一份；本号账上未新增积分）"
				if err := saveQoderPersonLink(email, holder); err != nil {
					log.Printf("[qoder-person] 记录同人关系 %s ↔ %s 失败（不影响签到结论）：%v", email, holder, err)
				}
			case probe.GrantID != "":
				verdict = QoderVerdictOwner
				// 上游给了本号这份额度的**领取时刻**：落进 Claims，账本才知道
				// 「上次领到」是什么时候（否则这个号永远显示没有领到时间）。
				if t, err := time.Parse(time.RFC3339, strings.TrimSpace(probe.ClaimedAt)); err == nil {
					_ = MarkClaimedAt("qoder", email, t)
				}
			}
		}
		verdicts[key] = verdict
		item := qoderItemFromResult(ap.item, ap.res, ap.rank, groupSize[key], probeConclusion, prevClaimed[key], verdict)
		if verdict == QoderVerdictSamePerson {
			// 服务端判本号被拒 ⇒ 本号今天**没签上**（那份额度在同人另一个号账上）。
			// 绝不能显示成「今日已签到」。
			item.CheckedInToday = false
		}
		qoderApplyCheckinSideEffects(item, ap.res)
		out.Results = append(out.Results, item)
	}

	// 判定出「同一人」关系（或被反证撤销）后重算组大小：组是**判定**出来的，
	// 不是按 scope 猜的。strategy 依赖组大小（组内 >1 = 服务端已判同人，绝不可能是
	// isolated），因此在这里统一重算一次 —— 与 GET checkin-status 用同一个函数。
	if sizes := QoderSamePersonGroupSizes(pool.Accounts); len(sizes) > 0 {
		for i := range out.Results {
			r := &out.Results[i]
			key := normalizeQoderEmail(r.Email)
			r.QuotaGroup = sizes[key]
			r.Strategy = qoderStrategySnapshot(r.Email, r.Phase, r.QuotaTakenBy, r.QuotaGroup, verdicts[key], probeConclusion)
		}
	}

	// 同人组内「already」号的额度归属回填（实测 2026-11-04）：
	// 服务端按人发额度，先打的号 claimed（真入账），后打的号回 already，
	// 但拿到的是**同一个 campaign 实例** —— 那 100 积分只进了先打那个号的账。
	//
	// 判据用**活动实例 id**：同一个 cid = 同一份额度 = 同一个人的那一份。
	// 找到组内今天 claimed 且 cid 相同的号 ⇒ already 号不是主人：
	//   - credits 归零（不是它入账的）；
	//   - quota_taken_by 点名是谁领的；
	//   - 文案改成人话。
	// 找不到 ⇒ already 就是本号自己今天早先领的，credits 照实显示。
	//
	// ⚠️ 必须在**汇总之前**做：汇总里的 AlreadyCredits 直接累加 r.Credits，
	// 回填晚了就会把虚记的 100 算进"今日已领额度"。
	qoderFillAlreadyQuotaOwner(out.Results)

	// 结果按本轮顺序（rotation_rank）排：pending / already 的结果是二次判定后
	// 才补进来的，不排序的话表格顺序会和「本轮顺序」对不上。
	sort.SliceStable(out.Results, func(i, j int) bool {
		return out.Results[i].RotationRank < out.Results[j].RotationRank
	})

	// 汇总：七相计数天然满足「之和 == len(results)」，因为每个结果只落一个相位。
	for _, r := range out.Results {
		switch r.Phase {
		case QoderPhaseClaimed:
			out.Claimed++
			out.Credits += r.Credits
		case QoderPhaseAlready:
			out.Already++
			out.AlreadyCredits += r.Credits
		case QoderPhaseBlocked:
			out.Blocked++
		case QoderPhasePending:
			out.Pending++
		case QoderPhaseFailed:
			out.Failed++
		case QoderPhaseLoginRequired:
			out.LoginRequired++
		default:
			out.Error++
		}
	}
	// 顶层 checked_in_today：只有全部都是 claimed/already 时才是 true。
	// ⚠️ 而且 already 还必须是**本号自己**的那份：额度归属同人另一个号的 already
	// 号没签到（服务端把它拒了），不能算进「已签到」。
	out.CheckedInToday = len(out.Results) > 0
	for _, r := range out.Results {
		if r.Phase != QoderPhaseClaimed && r.Phase != QoderPhaseAlready {
			out.CheckedInToday = false
			break
		}
		if r.QuotaTakenBy != "" && !strings.EqualFold(normalizeQoderEmail(r.QuotaTakenBy), normalizeQoderEmail(r.Email)) {
			out.CheckedInToday = false
			break
		}
	}
	out.QuotaGroups = qoderQuotaGroupViews(pool.Accounts, out.Results)

	// 逐日账本：**每次尝试都写**（含 blocked/pending/failed 等没领到的），用于审计
	// 与轮换。账本是辅助数据，写失败不改变本次签到结论，只记在 ledger_error 里。
	for _, r := range out.Results {
		if err := AppendQoderClaim(qoderClaimRecordOf(r)); err != nil && out.LedgerError == "" {
			out.LedgerError = err.Error()
		}
	}

	// token / ClaimCID 写回号池。必须走 UpdateQoderPool（契约 C-QD-POOL@1）：
	// 裸 Load+Save 会与面板/扫描的写者互相覆盖。
	if len(patches) > 0 {
		if err := UpdateQoderPool(func(f *QoderPoolFile) error {
			for _, a := range f.Accounts {
				if a == nil || a.Auth == nil {
					continue
				}
				p, ok := patches[normalizeQoderEmail(qoderSnapshotEmail(a))]
				if !ok {
					continue
				}
				if p.access != "" {
					a.Auth.Token = p.access
				}
				if p.refresh != "" {
					a.Auth.RefreshToken = p.refresh
				}
				if p.claimCID != "" {
					a.ClaimCID = p.claimCID
				}
			}
			return nil
		}); err != nil {
			return out, err
		}
	}

	sort.SliceStable(out.Results, func(i, j int) bool { return out.Results[i].Email < out.Results[j].Email })
	return out, nil
}

// qoderPoolItems 收集要处理的账号（号池顺序）。
func qoderPoolItems(pool *QoderPoolFile, emails []string) []qoderPoolItem {
	want := make(map[string]bool, len(emails))
	for _, e := range emails {
		if e = normalizeQoderEmail(e); e != "" {
			want[e] = true
		}
	}
	var items []qoderPoolItem
	for _, snap := range pool.Accounts {
		if snap == nil || snap.Auth == nil {
			continue
		}
		email := strings.TrimSpace(snap.Email)
		if email == "" {
			email = snap.Auth.User.Email
		}
		if email == "" {
			continue
		}
		if len(want) > 0 && !want[normalizeQoderEmail(email)] {
			continue
		}
		items = append(items, qoderPoolItem{snap: snap, email: email})
	}
	return items
}

// qoderReorderItems 按计算出的 rotation_order 重排候选项。
func qoderReorderItems(items []qoderPoolItem, order []string) []qoderPoolItem {
	if len(order) == 0 {
		return items
	}
	byEmail := make(map[string]qoderPoolItem, len(items))
	for _, it := range items {
		byEmail[normalizeQoderEmail(it.email)] = it
	}
	out := make([]qoderPoolItem, 0, len(items))
	used := make(map[string]bool, len(items))
	for _, em := range order {
		key := normalizeQoderEmail(em)
		if it, ok := byEmail[key]; ok && !used[key] {
			out = append(out, it)
			used[key] = true
		}
	}
	// 防御：order 与 items 万一不同步，剩下的按原顺序补上（不重复、不丢项）。
	for _, it := range items {
		key := normalizeQoderEmail(it.email)
		if !used[key] {
			out = append(out, it)
			used[key] = true
		}
	}
	return out
}

// qoderRotationOrder 计算本轮尝试顺序（契约 C-QD-API@1 的 rotation_order）。
//
// 规则：
//  1. priority 指定的账号排第一（用户显式指定今天这份给谁）；
//  2. 其余账号里「上次没领到」的号优先（逐日账本最近一条 phase != claimed），
//     从没尝试过的号同等优先；
//  3. 再按「上次真正领到」的时刻升序（越久没领到越先），最后按邮箱稳定排序。
//
// 为什么必须轮换：服务端每日额度按**人**给、先打先得，不轮换的话号池里排第一的
// 号天天赢，同组的另一个号一年也轮不到一次（Claims/LastClaimedAt 当初就是为
// 轮换存的，但旧实现只按它排，遇到「都没记录」时会退化成文件顺序）。
func qoderRotationOrder(items []qoderPoolItem, priority string) []string {
	latest := qoderLatestClaimRecords()
	pri := normalizeQoderEmail(priority)

	type entry struct {
		email     string
		key       string
		priority  bool
		missed    bool
		lastTry   time.Time
		lastClaim time.Time
	}
	rows := make([]entry, 0, len(items))
	for _, it := range items {
		key := normalizeQoderEmail(it.email)
		rec, has := latest[key]
		e := entry{email: it.email, key: key, priority: pri != "" && key == pri, missed: true}
		if has {
			// 「上次没领到」= 上一次尝试不是 claimed/already。already 同样是本号
			// 今天已经入账（服务端列表里已是 CLAIMED），不能当成"没领到"而让它
			// 优先 —— 否则已经领到的号会被反复排到最前面，轮换失去意义。
			e.missed = rec.Phase != QoderPhaseClaimed && rec.Phase != QoderPhaseAlready
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(rec.At)); err == nil {
				e.lastTry = t
			}
		}
		if t, ok := LastClaimedAt("qoder", it.email); ok {
			e.lastClaim = t
		}
		rows = append(rows, e)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.priority != b.priority {
			return a.priority
		}
		if a.missed != b.missed {
			return a.missed
		}
		if !a.lastTry.Equal(b.lastTry) {
			return a.lastTry.Before(b.lastTry)
		}
		if !a.lastClaim.Equal(b.lastClaim) {
			return a.lastClaim.Before(b.lastClaim)
		}
		return a.key < b.key
	})
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.email
	}
	return out
}

// qoderSamePersonHolderOf 找出同人组里今天真正领到额度的那号。
//
// 场景：扫描时本号被判 claimed（上游把同一条已领活动展示给同组所有号），
// 探测确认「本号不是主人」—— 但名字还不知道，得从同组里找那个真领的。
func qoderSamePersonHolderOf(email string, out []QoderScanAccount) string {
	key := normalizeQoderEmail(email)
	for _, peer := range QoderSamePersonPeers(email) {
		pk := normalizeQoderEmail(peer)
		for i := range out {
			o := &out[i]
			if normalizeQoderEmail(o.Email) != pk {
				continue
			}
			if o.Phase == QoderPhaseClaimed && o.CheckedInToday {
				return o.Email
			}
		}
	}
	// 同人组里没有 claimed 的（上游把已领活动展示给所有人，但没有一个号
	// 是"主人"）—— 那就退回同组里任意一个号，至少把"同一人组"讲清楚。
	for _, peer := range QoderSamePersonPeers(email) {
		if normalizeQoderEmail(peer) != key {
			return peer
		}
	}
	return ""
}

// qoderFillAlreadyQuotaOwner 回填「already」号今天的额度到底是谁领走的。
//
// ⚠️ 重要更正（实测 2026-11-04 11:28，GET .../campaigns/{cid}/reward 原始响应）：
//
//	2108643821@qq.com       grantId=01a1a477-96d3-…  claimedAt=2026-11-04T02:51:03Z
//	xu20060420fan@gmail.com grantId=01a1a463-e178-…  claimedAt=2026-11-04T02:29:31Z
//
// 两个号看到的是**同一条 campaign**（同一个活动"每天领 100 Credits"，服务端
// 对所有人下发同一个活动实例），但**grantId 不同、claimedAt 不同** ——
// **每个账号各自领到了一份额度**，不是共享一份。
//
// 额度侧同样印证：
//
//	qq     附加包 total=1700 used=0     remaining=1700   （17 份 × 100）
//	gmail  附加包 total=900  used=558   remaining=342    （9 份 × 100）
//
// 所以「服务端按人发、同一人一天只有一份」这个前提是**错的**（它把
// "同一条活动" 误当成 "同一份额度"），按它把后打的号的 100 归零是**错杀**。
//
// 真正的归属判据只有一条：服务端明确回
// failureCode=SAME_PERSON_ALREADY_CLAIMED（本号被拒，没拿到额度）。
// 那时才能说「额度是同人的另一个号领的」，credits 归零 + 点名。
//
// 本函数只处理**已经有明确归属结论**（QuotaTakenBy 非空）的 already 号：
// 把它的 credits 归零（不是它入账的）、面值单列、文案点名。
// 没有归属结论的 already = 本号自己今天早先领过 ⇒ credits 照实显示。
func qoderFillAlreadyQuotaOwner(results []QoderCheckinItem) {
	for i := range results {
		r := &results[i]
		if r.Phase != QoderPhaseAlready || r.QuotaTakenBy == "" {
			continue
		}
		owner := r.QuotaTakenBy
		if normalizeQoderEmail(owner) == normalizeQoderEmail(r.Email) {
			continue // 自己领的，就是本号签到
		}
		r.Credits = 0
		if r.BenefitAmount == 0 {
			r.BenefitAmount = 100
		}
		r.Msg = "今日额度已被同一个人的另一个账号 " + owner +
			" 领走（服务端判定同一人，一天只发一份；本号账上未新增积分）"
	}
}

// qoderItemFromResult 把一次签到结果归一化为契约结果项。
//
// verdict 是本次只读探测对「本号与该活动实例主人是否同一个人」的结论
// （QoderVerdict* 之一；没探测过传空/unknown）。strategy 由
// qoderStrategySnapshot 统一推导，与 GET checkin-status 用的是同一个函数。
func qoderItemFromResult(it qoderPoolItem, res *QoderCheckinResult, rank, group int, probeConclusion string, prevClaimed string, verdict string) QoderCheckinItem {
	phase := res.Phase
	if !qoderClaimPhases[phase] {
		phase = QoderPhaseError
	}
	item := QoderCheckinItem{
		Email:         it.email,
		Name:          firstNonEmpty(res.Name, qoderItemName(it.snap)),
		UID:           firstNonEmpty(res.UID, qoderItemUID(it.snap)),
		Phase:         phase,
		Msg:           res.Msg,
		DeviceOK:      res.DeviceOK,
		Refreshed:     res.Refreshed,
		Exe:           res.Exe,
		Base:          res.Base,
		Scope:         firstNonEmpty(res.Scope, QoderQuotaScopeKey(it.snap)),
		ClaimCID:      res.ClaimCID,
		QuotaGroup:    group,
		QuotaTakenBy:  res.QuotaTakenBy,
		RotationRank:  rank,
		LastClaimedAt: prevClaimed,
	}
	switch phase {
	case QoderPhaseClaimed:
		// 只有 claimed 才是**本号这次真的入账**的积分。
		item.Credits = res.Credits
		item.CheckedInToday = true
		item.ClaimedAt = res.ClaimedAt
	case QoderPhaseAlready:
		// ⚠️ already 的积分**不一定**是本号入账：上游对同一人组的所有号返回
		// **同一条**已领活动，后打的号拿到的是同一个 campaign 实例 ——
		// 那 100 积分可能进了**另一个号**的账。
		//
		// 归属由只读探测（GET .../campaigns/{cid}/reward，比 grantId）确认：
		// 拿到自己的 grantId ⇒ 本号自己领的，credits 照实显示；
		// 服务端回 BLOCKED+SAME_PERSON ⇒ 本号没领到，credits 归零 + 点名归属
		// （见 qoderFillAlreadyQuotaOwner）。
		item.Credits = res.Credits
		item.CheckedInToday = true
		item.ClaimedAt = res.ClaimedAt
	default:
		// 契约：非 claimed/already 的 credits 必须为 0，活动面值另放 benefit_amount。
		item.Credits = 0
		item.BenefitAmount = res.Credits
		item.CheckedInToday = false
	}
	// T5 口径：上游没有下发活动实例（claim_campaigns=0）的号落 pending，文案必须
	// 让用户一眼看出「不是我签失败了，是服务端今天没给这个号发活动」。
	if phase == QoderPhasePending && !strings.Contains(item.Msg, "claim_campaigns") {
		item.Msg = "上游未下发活动实例（claim_campaigns=0）：" + strings.TrimSpace(item.Msg)
	}
	item.Strategy = qoderStrategySnapshot(item.Email, phase, item.QuotaTakenBy, group, verdict, probeConclusion)
	// 本轮若真领到，last_claimed_at 就用本次的上游时刻（它就是最近一次）。
	if item.ClaimedAt != "" && (phase == QoderPhaseClaimed || phase == QoderPhaseAlready) {
		item.LastClaimedAt = item.ClaimedAt
	}
	return item
}

// qoderStrategySnapshot 推导 strategy 枚举（契约 C-QD-API@1）——**唯一来源**。
//
// `POST ide/qoder/checkin`（本文件）与 `GET ide/qoder/checkin-status`
// （QoderPoolScan）必须调用同一个函数，否则同一时刻同一状态会出现两套结论
// （D-Q1：签到侧 unassigned、状态侧 shared_person）。
//
// 判定规则（2026-11-04 按实测修订）：
//
//	blocked                                     → shared_person（服务端明确判定同人）
//	pending + 同组已有持有者（quotaTakenBy≠空）   → shared_person
//	pending + 明确非同人（verdict=other）         → unassigned（今天没给本号下发）
//	pending + 证据不足（unknown/未探测）           → unknown
//	claimed/already/claimable：
//	    quotaTakenBy≠空（额度归属另有其人）          → shared_person
//	    组内 >1（已被服务端判为同人）               → shared_person
//	    verdict=owner（本号自己的 grantId）         → isolated（本号独占一份）
//	    独立设备/出口 且 probe=isolated_viable      → isolated
//	    其余（证据不足）                            → unknown（归属未核实）
//	login_required/error                        → unknown
//
// ⚠️ 2026-11-04 实测更正：旧实现对 claimed/already/claimable **一律**回
// shared_person，面板上于是每个号都挂着「策略：服务端判定同人」。但服务端
// 从没判过这两个号同人（person.json 为空、也没回过 SAME_PERSON_ALREADY_CLAIMED），
// 两号各自拿到**不同的 grantId**、各自 +100 —— 拿没有证据的结论当事实说，
// 会把用户引到完全错误的方向上。现在只在真有证据时才说「服务端判定同人」。
//
// 独立设备/出口由邮箱查 qoder_egress.json 得出（不依赖调用方手里的 dev 对象），
// 保证两个端点在相同配置下得到相同答案。
func qoderStrategySnapshot(email, phase, quotaTakenBy string, groupSize int, verdict, probeConclusion string) string {
	switch phase {
	case QoderPhaseBlocked:
		return QoderStrategySharedPerson
	case QoderPhasePending:
		if strings.TrimSpace(quotaTakenBy) != "" {
			return QoderStrategySharedPerson
		}
		if strings.EqualFold(strings.TrimSpace(verdict), QoderVerdictOther) {
			return QoderStrategyUnassigned
		}
		return QoderStrategyUnknown
	case QoderPhaseClaimed, QoderPhaseAlready, "claimable":
		if strings.TrimSpace(quotaTakenBy) != "" {
			// 额度明确归属同组的另一个号 → 服务端视角下是同一个人。
			return QoderStrategySharedPerson
		}
		if groupSize > 1 {
			// 服务端已把本号判进某个同人组 → 绝不是独立额度。
			return QoderStrategySharedPerson
		}
		if strings.EqualFold(strings.TrimSpace(verdict), QoderVerdictOwner) {
			// 本号自己的 grantId ⇒ 这份额度确实在**本号**账上（实测 2026-11-04：
			// 两个号看到同一条 campaign，但 grantId/claimedAt 各不相同，各领各的）。
			return QoderStrategyIsolated
		}
		if qoderEgressIndependentForEmail(email) &&
			strings.EqualFold(strings.TrimSpace(probeConclusion), "isolated_viable") {
			return QoderStrategyIsolated
		}
		// 证据不足：如实说「归属未核实」，不许默认扣上「服务端判定同人」的帽子。
		return QoderStrategyUnknown
	default:
		return QoderStrategyUnknown
	}
}

// QoderStrategyFor 单号（号池为空）路径的 strategy 推导入口。
//
// 内部调用与批量签到/扫描完全相同的 qoderStrategySnapshot，并自行读取 T5 探测
// 结论文件 —— 面板不需要也不应该自己判断 strategy。
func QoderStrategyFor(email, phase, quotaTakenBy string, groupSize int, verdict string) string {
	return qoderStrategySnapshot(email, phase, quotaTakenBy, groupSize, verdict, qoderProbeConclusion())
}

// qoderEgressIndependentForEmail 报告该账号是否使用独立设备/出口。
//
// 「独立」的口径来自 qoder_egress.json：device_mode=derived 或配置了 proxy_url。
// 读取失败按「未配置」处理（QoderEgressFor 的既有语义），与签到侧设备解析一致。
func qoderEgressIndependentForEmail(email string) bool {
	prof := QoderEgressFor(email)
	return strings.EqualFold(strings.TrimSpace(prof.DeviceMode), QoderDeviceModeDerived) ||
		strings.TrimSpace(prof.ProxyURL) != ""
}

// qoderIsGrantNotFound 判断上游错误是否是「活动实例不存在」。
//
// 2026-11-03 实测（T5）：上游没给某号下发活动实例时，claim 会回
// 404 GRANT_NOT_FOUND。这种号今天就是没活动可领，按 pending 处理
// （不是 blocked、也不是 failed）。
func qoderIsGrantNotFound(msg string) bool {
	u := strings.ToUpper(msg)
	if strings.Contains(u, "GRANT_NOT_FOUND") {
		return true
	}
	return strings.Contains(u, "HTTP404") || strings.Contains(u, "HTTP 404")
}

// qoderDeviceForEmailSafe 按账号解析设备；配置损坏时回落真实设备（不阻断签到）。
func qoderDeviceForEmailSafe(email string) (*QoderDevice, error) {
	dev, err := QoderDeviceForEmail(email)
	if err == nil && dev != nil {
		return dev, nil
	}
	fallback := QoderDeviceInfo()
	if fallback == nil {
		fallback = &QoderDevice{}
	}
	fallback.Synthetic = false
	// 配置读不出来时返回 fallback + 原始错误：调用方据此把该号标成 error，
	// 而不是拿一个没有任何出口信息的设备偷偷去打上游。
	return fallback, err
}

// qoderItemName / qoderItemUID 从号池快照取展示字段。
func qoderItemName(snap *QoderSnapshot) string {
	if snap == nil {
		return ""
	}
	if strings.TrimSpace(snap.Name) != "" {
		return snap.Name
	}
	if snap.Auth != nil {
		return snap.Auth.User.Name
	}
	return ""
}

func qoderItemUID(snap *QoderSnapshot) string {
	if snap == nil {
		return ""
	}
	if strings.TrimSpace(snap.UserID) != "" {
		return snap.UserID
	}
	if snap.Auth != nil {
		return snap.Auth.User.ID
	}
	return ""
}

// qoderApplyCheckinSideEffects 把签到结果落到本地签到记录（checkin_log.json）。
//
// 只有真领到（claimed）与今日已领（already）才写「已签到」；blocked 还要清掉
// 可能存在的误记 —— 旧实现把被服务端拒绝的号也写进记录，面板于是整天显示
// 「已签到」，而它根本没领到。
func qoderApplyCheckinSideEffects(item QoderCheckinItem, res *QoderCheckinResult) {
	switch item.Phase {
	case QoderPhaseClaimed:
		if res != nil && res.ClaimedToday {
			_ = MarkClaimedAt("qoder", item.Email, claimStamp(res))
		}
		_ = MarkCheckedIn("qoder", item.Email)
	case QoderPhaseAlready:
		// already 是「今日已领」（服务端活动列表已是 CLAIMED / 奖励记录在本号账上）。
		// 有上游给的 claimedAt 时同样落 Claims：轮换排序要的是「上次真正领到」，
		// already 的号今天已经入账，不能因为少了这条记录被当成"从没领到"而反复优先。
		if res != nil && strings.TrimSpace(res.ClaimedAt) != "" {
			_ = MarkClaimedAt("qoder", item.Email, claimStamp(res))
		}
		_ = MarkCheckedIn("qoder", item.Email)
	case QoderPhaseBlocked:
		_ = UnmarkCheckedIn("qoder", item.Email)
	}
}

// qoderClaimRecordOf 把结果项转成一条逐日账本记录（契约 C-QD-LEDGER@1）。
func qoderClaimRecordOf(item QoderCheckinItem) QoderClaimRecord {
	at := strings.TrimSpace(item.ClaimedAt)
	if at == "" {
		at = time.Now().In(qoderCST).Format(time.RFC3339)
	} else if t, err := time.Parse(time.RFC3339, at); err == nil {
		at = t.In(qoderCST).Format(time.RFC3339)
	}
	return QoderClaimRecord{
		Email:        item.Email,
		Date:         qoderClaimDayOf(at),
		Phase:        item.Phase,
		Credits:      item.Credits,
		CampaignID:   item.ClaimCID,
		QuotaTakenBy: item.QuotaTakenBy,
		Strategy:     item.Strategy,
		At:           at,
	}
}

// qoderClaimDayOf 解析 RFC3339 并取 UTC+8 日期；解析失败回落今天。
func qoderClaimDayOf(at string) string {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(at)); err == nil {
		return qoderClaimDay(t)
	}
	return time.Now().In(qoderCST).Format("2006-01-02")
}

// qoderQuotaGroupViews 组装 quota_groups（只保留同人组里账号数 > 1 的组）。
func qoderQuotaGroupViews(accounts []*QoderSnapshot, results []QoderCheckinItem) []QoderQuotaGroupView {
	groups := QoderQuotaGroups(accounts)
	byEmail := make(map[string]QoderCheckinItem, len(results))
	for _, r := range results {
		byEmail[normalizeQoderEmail(r.Email)] = r
	}
	out := make([]QoderQuotaGroupView, 0, len(groups))
	for _, emails := range groups {
		if len(emails) <= 1 {
			continue
		}
		sort.Strings(emails)
		v := QoderQuotaGroupView{QuotaGroup: len(emails), Emails: emails}
		for _, em := range emails {
			r, ok := byEmail[normalizeQoderEmail(em)]
			if !ok {
				continue
			}
			if r.Phase == QoderPhaseClaimed || r.Phase == QoderPhaseAlready {
				v.ClaimedBy = r.Email
				v.Credits = r.Credits
				break
			}
			if v.ClaimedBy == "" && strings.TrimSpace(r.QuotaTakenBy) != "" {
				// 组里没人 claimed 但服务端已判定额度被对方领走：如实点名。
				v.ClaimedBy = r.QuotaTakenBy
			}
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].Emails) == 0 || len(out[j].Emails) == 0 {
			return len(out[i].Emails) < len(out[j].Emails)
		}
		return out[i].Emails[0] < out[j].Emails[0]
	})
	return out
}

// ---------------------------------------------------------------------------
// 只读探测结论（契约 C-QD-PROBE@1，仅消费 conclusion 字段）
// ---------------------------------------------------------------------------

// qoderProbeConclusion 读取 T5 只读探测的结论，仅用于选择 strategy 文案。
//
// 文件缺失 / 损坏 / 未知值 → 返回 ""（调用方按默认 shared_person + 轮换处理）。
// 路径候选：QODER_PROBE_FILE、<PoolDir 的上一级>/qoder_identity_probe.json
// （CLI 的 -out ../data/qoder_identity_probe.json）、<PoolDir>/qoder_identity_probe.json。
func qoderProbeConclusion() string {
	for _, p := range qoderProbePathCandidates() {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var f struct {
			Conclusion string `json:"conclusion"`
		}
		if json.Unmarshal(raw, &f) != nil {
			continue
		}
		switch c := strings.ToLower(strings.TrimSpace(f.Conclusion)); c {
		case "isolated_viable", "server_person_binding", "inconclusive":
			return c
		}
	}
	return ""
}

// qoderProbePathCandidates 探测结论文件的候选路径。
//
// 路径候选：QODER_PROBE_FILE、<PoolDir 的上一级>/qoder_identity_probe.json
// （CLI 的 -out ../data/qoder_identity_probe.json）、<PoolDir>/qoder_identity_probe.json。
func qoderProbePathCandidates() []string {
	var candidates []string
	if v := strings.TrimSpace(os.Getenv("QODER_PROBE_FILE")); v != "" {
		candidates = append(candidates, v)
	}
	return append(candidates,
		filepath.Join(filepath.Dir(PoolDir()), "qoder_identity_probe.json"),
		filepath.Join(PoolDir(), "qoder_identity_probe.json"),
	)
}

// QoderProbeSummary 读取 T5 只读探测的结论摘要（契约 C-QD-PROBE@1 的
// criterion_inferred / conclusion / at / missing），供面板 egress 端点展示。
//
// 只读、只取这几个固定字段（不返回 token/账号明细）；文件不存在返回 nil，
// 面板如实显示「尚无探测结论」，不编造。
func QoderProbeSummary() map[string]any {
	for _, p := range qoderProbePathCandidates() {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var f struct {
			At                string   `json:"at"`
			CriterionInferred string   `json:"criterion_inferred"`
			Conclusion        string   `json:"conclusion"`
			Missing           []string `json:"missing"`
		}
		if json.Unmarshal(raw, &f) != nil {
			continue
		}
		conclusion := strings.ToLower(strings.TrimSpace(f.Conclusion))
		criterion := strings.ToLower(strings.TrimSpace(f.CriterionInferred))
		if conclusion == "" && criterion == "" {
			continue
		}
		out := map[string]any{"conclusion": conclusion, "criterion_inferred": criterion}
		if strings.TrimSpace(f.At) != "" {
			out["at"] = f.At
		}
		if len(f.Missing) > 0 {
			out["missing"] = f.Missing
		}
		return out
	}
	return nil
}

// updateQoderAccount 在号池锁内按邮箱改动单个账号（契约 C-QD-POOL@1）。
//
// fn 内**禁止**再调用 LoadQoderPool/SaveQoderPool/QoderScopePeers —— 互斥锁
// 不可重入，会死锁。
func updateQoderAccount(email string, fn func(*QoderSnapshot)) error {
	ex := normalizeQoderEmail(email)
	if ex == "" || fn == nil {
		return nil
	}
	return UpdateQoderPool(func(f *QoderPoolFile) error {
		for _, a := range f.Accounts {
			if a == nil {
				continue
			}
			if normalizeQoderEmail(qoderSnapshotEmail(a)) != ex {
				continue
			}
			fn(a)
			break
		}
		return nil
	})
}

// QoderPoolStatuses 号池各账号「今日是否已签到」。
// 只读本地签到日志（MarkCheckedIn 写的），不打上游——签到池进页面时不该发 N 个请求。
func QoderPoolStatuses() []map[string]any {
	pool, err := LoadQoderPool()
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(pool.Accounts))
	for _, snap := range pool.Accounts {
		if snap == nil {
			continue
		}
		email := strings.TrimSpace(snap.Email)
		if email == "" && snap.Auth != nil {
			email = snap.Auth.User.Email
		}
		if email == "" {
			continue
		}
		row := map[string]any{
			"email":            email,
			"name":             snap.Name,
			"user_id":          snap.UserID,
			"checked_in_today": CheckedInToday("qoder", email),
		}
		if t, ok := CheckedInAt("qoder", email); ok {
			row["checked_in_at"] = t.Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
}

// qoderPoolAuth 按邮箱取号池里某个账号的登录态（找不到返回 nil）。
func qoderPoolAuth(accounts []*QoderSnapshot, email string) *QoderAuth {
	ex := normalizeQoderEmail(email)
	if ex == "" {
		return nil
	}
	for _, a := range accounts {
		if a == nil || a.Auth == nil {
			continue
		}
		if normalizeQoderEmail(qoderSnapshotEmail(a)) == ex {
			return a.Auth
		}
	}
	return nil
}

// QuotaHolderInGroup 返回**与指定账号同一个人**的账号里，今天已领到每日额度的那一个。
//
// 2026-11-02 修正：每日额度按**人**发（实测 failureCode=SAME_PERSON_ALREADY_CLAIMED），
// 不是按设备、也不是按账号、也不是按 agentAccountScope —— 两个号 scope 不同却共享
// 同一份额度。所以这里只认**服务端判定过**的「同一人」关系（见 qoder_person.go），
// 没有判定记录时返回 false（不知道就说不知道，不能默认"被占用了"）。
//
// 同组多号时按邮箱排序取第一个，保证结果稳定。
func QuotaHolderInGroup(excludeEmail string) (string, bool) {
	pool, err := LoadQoderPool()
	if err != nil || pool == nil {
		return "", false
	}
	ex := normalizeQoderEmail(excludeEmail)
	if ex == "" {
		return "", false
	}
	peers := QoderSamePersonPeers(ex)
	if len(peers) == 0 {
		return "", false
	}
	var holders []string
	for _, em := range peers {
		if CheckedInToday("qoder", em) && CheckinRecordTrusted("qoder", em) {
			holders = append(holders, em)
		}
	}
	if len(holders) == 0 {
		return "", false
	}
	sort.Strings(holders)
	return holders[0], true
}

// UsedDeviceQuotaToday 报告**同一个人的另一个账号**今天是否已领走每日额度
// （用于向看不到今日活动的账号解释原因）。
//
// 名字保留是为了不破坏既有调用点，语义已从「同设备」改成「同一人」。
func UsedDeviceQuotaToday(excludeEmail string) bool {
	_, ok := QuotaHolderInGroup(excludeEmail)
	return ok
}

// QoderScanAccount 号池单账号的上游核实结果。
type QoderScanAccount struct {
	Email          string `json:"email"`
	Name           string `json:"name,omitempty"`
	UID            string `json:"user_id,omitempty"`
	Phase          string `json:"phase"` // claimed / claimable / pending / login_required / error
	Msg            string `json:"msg,omitempty"`
	CheckedInLocal bool   `json:"checked_in_local"`
	CheckedInAt    string `json:"checked_in_at,omitempty"`
	Refreshed      bool   `json:"refreshed,omitempty"`
	// CheckedInToday **该账号今天是否真的签到了** —— 以**上游核实结果**为准
	// （phase=claimed/already），不采信本地记录。
	//
	// 为什么必须与 CheckedInLocal 分开：本地日志里可能残留旧版写的「纯日期」记录
	// （无时刻信息），按自然日解析会落在当日 00:00，于是"今天签过"为真 —— 而
	// 上游活动列表里根本没有今天的 CLAIM_BENEFIT。把它当"已签到"展示，就是
	// 面板凭空说假话。2026-10-24 实测踩坑：浏览器加的号（gmail）挂着这种旧记录，
	// 面板显示"今日已签到"，而真正领到 100 Credits 的另一个号反而显示"可签到"。
	CheckedInToday bool `json:"checked_in_today"`
	// RecordTrusted 本地记录是否可信（今日 10:05 之后写入的完整时间戳）。
	RecordTrusted bool `json:"record_trusted,omitempty"`
	// Source 判定来源：upstream = 上游活动列表确认；local = 上游看不到今日活动
	// （领完即删），但本地有可信记录。
	Source string `json:"source,omitempty"`
	// DeviceQuotaUsed 本账号看不到今日活动，是因为**同一个人的另一个账号**已领走了
	// 当天唯一一份每日额度（服务端按人发，先打先得；判定码 SAME_PERSON_ALREADY_CLAIMED）。
	DeviceQuotaUsed bool `json:"same_person_taken,omitempty"`
	// Scope 本账号的 agentAccountScope（agent 租户位标识；⚠️ 不是额度分组键）。
	Scope string `json:"scope,omitempty"`
	// QuotaGroup 本账号所在**同一人组**里的账号数（含自己）。>1 = 与别的号同一人。
	QuotaGroup int `json:"quota_group,omitempty"`
	// QuotaTakenBy 同一人组里今天领到每日额度的账号（空 = 还没人领）。
	QuotaTakenBy string `json:"quota_taken_by"`
	// ClaimCID 今天这条 CLAIM_BENEFIT 活动的实例 id（号池里别的号靠它去只读地问
	// 服务端「我是不是同一个人」）。
	ClaimCID string `json:"claim_cid,omitempty"`
	// Strategy 该号当前状态对应的策略（契约 C-QD-API@1 的 strategy 枚举）。
	Strategy string `json:"strategy,omitempty"`
	// LastClaimedAt 最近一次**真正领到**的时刻（RFC3339，空 = 无记录）。
	LastClaimedAt string `json:"last_claimed_at,omitempty"`
	// ClaimHistoryDays 账本里 phase=claimed 的去重日期数（历史已领天数）。
	ClaimHistoryDays int `json:"claim_history_days"`
}

// QoderPoolScan 逐账号向**上游**核实签到状态（并发 2，只查不领）。
//
// 为什么必须有这一步：此前「扫描」只读本地记录，用户永远无法确认面板说的「已签」
// 是不是真的（实测 09:00 的假已签一直挂到中午才被发现）。现在扫描直接问上游：
// claimable = 可领、claimed = 今天真领过、pending = 活动未下发（含只有昨天陈旧
// 活动的情形）。上游不可达时回落本地记录并在 msg 里说明。
func QoderPoolScan(emails []string) []QoderScanAccount {
	pool, err := LoadQoderPool()
	if err != nil {
		return nil
	}
	// 历史账本补写（D-Q2）：号池里可能存在「今天确实领到了（Claims 有今天的
	// 时刻）但账本当天没有 claimed 记录」的账号 —— 那次领取发生在账本文件建立
	// 之前，或由旧版进程完成。不补写会让 claim_history_days 一直低估。
	// 只在「当天缺失」时写一次；失败只记日志，不阻断扫描。
	for _, snap := range pool.Accounts {
		if snap == nil {
			continue
		}
		email := strings.TrimSpace(snap.Email)
		if email == "" && snap.Auth != nil {
			email = strings.TrimSpace(snap.Auth.User.Email)
		}
		if email == "" {
			continue
		}
		if wrote, err := QoderBackfillClaimHistoryForToday(email); err != nil {
			log.Printf("[qoder-ledger] 补写 %s 今日 claimed 记录失败（不阻断扫描）：%v", email, err)
		} else if wrote {
			log.Printf("[qoder-ledger] 已按 Claims 补写 %s 今日 claimed 记录（历史账本缺失）", email)
		}
	}
	want := make(map[string]bool, len(emails))
	for _, e := range emails {
		if e = normalizeQoderEmail(e); e != "" {
			want[e] = true
		}
	}
	type job struct {
		snap *QoderSnapshot
	}
	var jobs []job
	for _, snap := range pool.Accounts {
		if snap == nil || snap.Auth == nil {
			continue
		}
		email := strings.TrimSpace(snap.Email)
		if email == "" {
			email = snap.Auth.User.Email
		}
		if len(want) > 0 && !want[normalizeQoderEmail(email)] {
			continue
		}
		jobs = append(jobs, job{snap: snap})
	}
	out := make([]QoderScanAccount, len(jobs))
	// 同一人组信息：服务端判定过「同一人」的账号共享同一份每日额度，扫描时要按组解释。
	groupSize := QoderSamePersonGroupSizes(pool.Accounts)
	probeConclusion := qoderProbeConclusion()
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			snap := j.snap
			email := strings.TrimSpace(snap.Email)
			if email == "" {
				email = snap.Auth.User.Email
			}
			key := normalizeQoderEmail(email)
			row := QoderScanAccount{
				Email:            email,
				Name:             snap.Name,
				UID:              snap.UserID,
				CheckedInLocal:   CheckedInToday("qoder", email),
				Scope:            QoderQuotaScopeKey(snap),
				QuotaGroup:       groupSize[key],
				LastClaimedAt:    qoderLedgerLastClaimedRFC3339(email),
				ClaimHistoryDays: len(QoderClaimedDates(email, 0)),
			}
			if t, ok := CheckedInAt("qoder", email); ok {
				row.CheckedInAt = t.Format(time.RFC3339)
			}
			auth := cloneQoderAuth(snap.Auth)
			dev, devErr := qoderDeviceForEmailSafe(email)
			if devErr != nil {
				row.Phase = QoderPhaseError
				row.Msg = "该账号设备/出口配置不可用：" + devErr.Error()

				row.CheckedInToday = false
				out[i] = row
				return
			}
			oc := qoderScanAuth(auth, dev, QoderQuotaScopeKey(snap))
			res := oc.result
			// 续期出的新 token 写回号池（绝不写客户端登录态）。
			// 必须走 UpdateQoderPool：裸 Load+Save 会覆盖并发写者的改动（契约 C-QD-POOL@1）。
			if oc.newAccess != "" || oc.newRefresh != "" {
				if oc.newAccess != "" {
					snap.Auth.Token = oc.newAccess
				}
				if oc.newRefresh != "" {
					snap.Auth.RefreshToken = oc.newRefresh
				}
				_ = updateQoderAccount(email, func(a *QoderSnapshot) {
					if a.Auth == nil {
						a.Auth = &QoderAuth{}
					}
					if oc.newAccess != "" {
						a.Auth.Token = oc.newAccess
					}
					if oc.newRefresh != "" {
						a.Auth.RefreshToken = oc.newRefresh
					}
				})
				row.Refreshed = true
			}
			row.Phase = res.Phase
			row.Msg = res.Msg
			row.ClaimCID = res.ClaimCID
			// 记下今天这条活动实例 id：号池里其它号（看不到这条活动的）要靠它去
			// 向服务端要一个真实判定。
			if res.ClaimCID != "" && snap.ClaimCID != res.ClaimCID {
				snap.ClaimCID = res.ClaimCID
				_ = updateQoderAccount(email, func(a *QoderSnapshot) { a.ClaimCID = res.ClaimCID })
			}
			// 本地记录的可信判定：10:05 前写入的今日记录是旧版误报（把昨天的陈旧
			// 已领活动当成了今天的），一律忽略并明确提示。
			trusted := CheckinRecordTrusted("qoder", email)
			row.RecordTrusted = trusted
			if row.CheckedInLocal && !trusted {
				row.CheckedInLocal = false
				row.Msg = strings.TrimSpace(row.Msg + "；本地存在 10:05 前的可疑签到记录（旧版误报），已忽略，以上游为准")
			}
			// 「今天到底签没签」的真值判定：**以上游活动列表为准**。
			//
			//   claimed   → 上游今天的 CLAIM_BENEFIT 已是 CLAIMED → 真签到
			//   claimable → 今天还没领（可领）
			//   pending   → 上游看不到今天的活动（设备名额被占 / 未下发 / 领完即删）
			//               此时只有「本地可信记录」才能判已签，否则一律未签
			//   其它      → 上游不可达，本地可信记录兜底
			//
			// 关键：绝不能让「本地记录说签过」盖过「上游说没签」。旧版正是这么反的 ——
			// 浏览器加的号挂着一条旧格式（纯日期）记录，面板显示"今日已签到"，而真正
			// 领到 100 Credits 的另一个号反而显示"可签到"，两个号整个反了。
			switch res.Phase {
			case QoderPhaseClaimed:
				row.CheckedInToday = true
				row.Source = "upstream"
			case "claimable":
				row.CheckedInToday = false
			case QoderPhasePending:
				if trusted {
					row.CheckedInToday = true
					row.Source = "local"
					row.Msg = "今日已领取（本地记录 " + row.CheckedInAt + "；上游活动列表未见今日活动）"
				} else {
					row.CheckedInToday = false
				}
			default: // login_required / error
				row.CheckedInToday = false
				if trusted {
					row.Phase = QoderPhasePending
					row.CheckedInToday = true
					row.Source = "local"
					row.Msg = "上游核实失败（" + res.Msg + "）；本地记录今日已签 " + row.CheckedInAt
				}
			}
			// 每日额度按**人**给，不按设备、也不按账号（实测 2026-11-02：服务端回
			// failureCode=SAME_PERSON_ALREADY_CLAIMED，且两个号的 agentAccountScope
			// 其实不同）。所以只能问服务端本人，不能拿 scope 猜。
			// 已知「同一人」关系里今天已有其它账号领走额度时，如实说明并点名是谁领的 ——
			// 这与笼统的"活动未下发"是两回事，用户需要分得清。
			if res.Phase == QoderPhasePending && !trusted {
				if holder, ok := QuotaHolderInGroup(email); ok {
					row.DeviceQuotaUsed = true
					row.QuotaTakenBy = holder
					row.Msg = "今日每日额度已被同一个人的另一个账号 " + holder +
						" 领取（服务端判定同一人，一天只发一份、先打先得）"
				}
			}
			// 注意：这里**不**计算 strategy —— 统一放到函数末尾重算，保证
			// 「同人归属」把 quota_taken_by 补齐之后、以及探测撤销 link 之后，
			// strategy 仍然基于最终状态。与 POST checkin 共用同一个函数。
			out[i] = row
		}(i, j)
	}
	wg.Wait()

	// 还不知道「是不是同一人」的号：拿号池里另一个号今天那条活动实例 id，
	// **只读**地问一次服务端（GET .../campaigns/{cid}/reward），拿真实判定。
	// 问出 SAME_PERSON_ALREADY_CLAIMED 就记下「这两个号是同一个人」，
	// 以后面板直接能说清「为什么这个号签不上」。
	cidOf := make(map[string]string, len(out))
	for _, r := range out {
		if r.ClaimCID != "" {
			cidOf[normalizeQoderEmail(r.Email)] = r.ClaimCID
		}
	}

	// 探测得到的「是否同一个人」结论（邮箱 → QoderVerdict*）。strategy 必须基于
	// 最终结论推导，所以先收集、最后统一计算。
	verdictByEmail := make(map[string]string, len(out))
	for i := range out {
		r := &out[i]
		// ⚠️ **claimed 也要探测**（2026-11-04 实测踩坑）：
		// 上游活动列表对**同一人组的所有号**都返回今天这条已领活动（服务端按人发，
		// 同一活动实例对同组账号都可见）。所以扫描看到"今天的活动已 CLAIMED"
		// 就判 claimed 是不够的 —— 后打的号也会被判成"真领到"。
		//
		// 实测现场：qq 与 gmail 看到**同一个** campaign 实例
		// 01a199dd-c0ea-721e-8751-ea0379af700e（qq 领的），但扫描把两个号
		// 都报成 claimed → 面板两个号都显示"+100"，凭空多出 100。
		//
		// 判据：只读地问一次 GET .../campaigns/{cid}/reward，比对 grantId ——
		// grantId 相同 = 同一个人的同一份额度；不同 = 本号不是主人。
		//
		// 只有 pending 且无本地可信记录时才需要"问是不是同一人"；
		// claimed 的问题是"是不是**我**领的"，同样要问。
		if r.Phase != QoderPhasePending && r.Phase != QoderPhaseClaimed {
			continue
		}
		if r.Phase == QoderPhasePending && r.RecordTrusted {
			continue
		}
		// claimed 号必须自己有 cid 才能问。
		if r.Phase == QoderPhaseClaimed && r.ClaimCID == "" {
			continue
		}
		em := normalizeQoderEmail(r.Email)
		myAuth := qoderPoolAuth(pool.Accounts, em)
		if myAuth == nil {
			continue
		}
		// 路径 A（claimed）：用**本号自己的** cid 问服务端"这条实例对我意味着什么"。
		//   拿到 status=CLAIMED + 自己的 grantId ⇒ 本号真领到了（实测 2026-11-04：
		//   两个号看到同一条 campaign，但 grantId 与 claimedAt 各不相同，
		//   **各领各的一份**，不是共享一份）。
		//   只有服务端明确回 BLOCKED/SAME_PERSON_ALREADY_CLAIMED（本号被拒），
		//   才是「额度是同人的另一个号领的」，这时才降级。
		if r.Phase == QoderPhaseClaimed {
			probe := qoderProbeReward(myAuth, QoderDeviceInfo(), r.ClaimCID, r.Scope)
			switch {
			case strings.EqualFold(probe.Status, "BLOCKED") &&
				strings.Contains(strings.ToUpper(probe.FailureCode), "SAME_PERSON"):
				// 服务端明确：本号与主人是**同一个人**，额度已被对方领走。
				r.Phase = QoderPhaseAlready
				r.CheckedInToday = false
				r.DeviceQuotaUsed = true
				holder := r.QuotaTakenBy
				if holder == "" {
					holder = qoderSamePersonHolderOf(r.Email, out)
				}
				r.QuotaTakenBy = holder
				r.Msg = "今日额度已被同一个人的另一个账号 " + holder +
					" 领走（服务端判定同一人，一天只发一份；本号账上未新增积分）"
				verdictByEmail[em] = QoderVerdictSamePerson
			case probe.GrantID != "":
				// 拿到自己的 grantId ⇒ 本号就是主人，真领到了。
				verdictByEmail[em] = QoderVerdictOwner
				// 上游给了本号这份额度的领取时刻：落进 Claims（账本要知道
				// 「上次领到」是什么时候，否则这个号永远显示没有领到时间）。
				if t, err := time.Parse(time.RFC3339, strings.TrimSpace(probe.ClaimedAt)); err == nil {
					_ = MarkClaimedAt("qoder", em, t)
				}
			}
			continue
		}
		// 路径 B（pending）：本号看不到今天的活动 ⇒ 问"我是不是同一个人"。
		var peers []string
		for other, cid := range cidOf {
			if other == em || cid == "" {
				continue
			}
			peers = append(peers, other)
		}
		sort.Strings(peers)
		for _, other := range peers {
			cid := cidOf[other]
			ownerAuth := qoderPoolAuth(pool.Accounts, other)
			if ownerAuth == nil {
				continue
			}
			ownerProbe := qoderProbeReward(myAuth, QoderDeviceInfo(), cid, r.Scope)
			holderProbe := qoderProbeReward(ownerAuth, QoderDeviceInfo(), cid, r.Scope)
			switch qoderPersonVerdict(ownerProbe, holderProbe) {
			case QoderVerdictSamePerson:
				r.DeviceQuotaUsed = true
				r.QuotaTakenBy = other
				r.Msg = "今日每日额度已被同一个人的另一个账号 " + other +
					" 领取（服务端判定同一人，一天只发一份、先打先得）"
				verdictByEmail[em] = QoderVerdictSamePerson
				if err := saveQoderPersonLink(em, other); err != nil {
					log.Printf("[qoder-person] 记录同人关系 %s ↔ %s 失败（不影响扫描结论）：%v", em, other, err)
				}
			case QoderVerdictOther:
				// 明确反证：历史同人 link 必须撤销，否则面板会永远把这俩算成一组
				// （D-Q1）。同时清掉 job 阶段基于旧 link 得出的「额度被同人占用」。
				qoderForgetPersonLinkIfContradicted(em, other, QoderVerdictOther)
				r.DeviceQuotaUsed = false
				r.QuotaTakenBy = ""
				r.Msg = qoderPendingNotAssignedMsg
				verdictByEmail[em] = QoderVerdictOther
			}
			break
		}
	}

	// 同一人归属：把「今天谁领走了那份额度」补到同一人组的其它号上，
	// 面板就能说清「这个号为什么没签上」。
	for i := range out {
		r := &out[i]
		if r.QuotaTakenBy != "" {
			continue
		}
		for _, peer := range QoderSamePersonPeers(r.Email) {
			for _, o := range out {
				if strings.EqualFold(o.Email, peer) && o.Phase == QoderPhaseClaimed && o.CheckedInToday {
					r.QuotaTakenBy = peer
					break
				}
			}
			if r.QuotaTakenBy != "" {
				break
			}
		}
	}
	// ⚠️ already + 额度归属是**同人的另一个号** ⇒ 本号今天**没有签到**、
	// 账上也没新增积分（服务端按人发，一天一份）。
	//
	// 2026-11-04 实测现场：qq 号 claimed（真入账 +100），gmail 号回 already
	// 但拿到的是**同一个 campaign 实例** —— 上游说"今日已领取"，可那 100 进了
	// qq 的账。旧实现两号都显示"✓ 今日已签到 / +100 Credits"，凭空多出 100。
	//
	// 这里把 already 号改成：不是本号签到 + 如实说明归属。
	for i := range out {
		r := &out[i]
		if r.Phase != QoderPhaseAlready || r.QuotaTakenBy == "" {
			continue
		}
		if strings.EqualFold(r.QuotaTakenBy, r.Email) {
			continue // 自己领的，就是本号签到
		}
		r.DeviceQuotaUsed = true
		r.CheckedInToday = false
		r.Msg = "今日额度已由同一个人的另一个账号 " + r.QuotaTakenBy +
			" 领取（服务端判定同一人，一天只发一份；本号账上未新增积分）"
	}
	// 判定出「同一人」关系（或被反证撤销）后重算组大小：组是**判定**出来的，
	// 不是按 scope 猜的，所以必须等判定完成再统计。
	//
	// strategy 也在这里统一重算（全函数唯一出口）：它依赖最终的 quota_taken_by /
	// 组大小 / 探测结论，任何提前计算都会在 link 撤销后留下过期结论（D-Q1）。
	sizes := QoderSamePersonGroupSizes(pool.Accounts)
	for i := range out {
		r := &out[i]
		key := normalizeQoderEmail(r.Email)
		r.QuotaGroup = sizes[key]
		if r.QuotaGroup <= 0 {
			// 兜底：至少算本号自己一组（组大小 0 会让前端少了分桶信息）。
			r.QuotaGroup = 1
		}
		r.Strategy = qoderStrategySnapshot(r.Email, r.Phase, r.QuotaTakenBy, r.QuotaGroup, verdictByEmail[key], probeConclusion)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// QoderUsageFor 查询指定账号的额度（email 为空 = 当前登录账号）。
// 号池里的账号直接用自己保存的 token 查，不需要切号。
func QoderUsageFor(email string) *QoderUsageResult {
	email = strings.TrimSpace(email)
	if email == "" {
		return QoderUsage()
	}
	pool, err := LoadQoderPool()
	if err != nil {
		return &QoderUsageResult{Phase: "error", Email: email, Msg: "读取号池失败: " + err.Error()}
	}
	for _, snap := range pool.Accounts {
		if snap == nil || snap.Auth == nil {
			continue
		}
		if strings.EqualFold(snap.Email, email) || strings.EqualFold(snap.Auth.User.Email, email) {
			auth := cloneQoderAuth(snap.Auth)
			return qoderUsageAuth(auth, qoderDeviceForAccount(auth), QoderQuotaScopeKey(snap))
		}
	}
	return &QoderUsageResult{Phase: "error", Email: email, Msg: "号池里没有该账号"}
}

// cloneQoderAuth 复制一份登录态，避免改动号池对象上的共享指针。
func cloneQoderAuth(a *QoderAuth) *QoderAuth {
	if a == nil {
		return nil
	}
	c := *a
	return &c
}

// claimStamp 本次领取的真实时刻：优先用上游返回的 claimedAt，拿不到才回落本机时钟。
//
// 为什么在意这个：签到记录的可信判定要比较"这条记录晚于今天活动下发"，两边都用
// 本机时钟时，机器钟一慢（实测慢 8 小时）真领到的记录就被判成旧版误报，面板显示
// "没签上"、重试循环整天空跑。上游给的时刻不受本机时钟影响。
func claimStamp(res *QoderCheckinResult) time.Time {
	if res != nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(res.ClaimedAt)); err == nil {
			return t
		}
	}
	return time.Now()
}
