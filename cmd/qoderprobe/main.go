// Command qoderprobe 是 C-QD-PROBE@1 的只读隔离探测 CLI。
//
// 它把每个 Qoder 账号在若干变体（baseline / derived / derived_proxy）下对上游
// 打一次**只读**探测，判断「服务端是不是把这两个号算成同一个人」，从而回答
// 用户的问题：能不能让每个 Qoder 账号各领一份。
//
// 硬约束（契约 2.2.9）：
//   - 只调用 GET /sash/api/v1/me/campaigns 与
//     GET /sash/api/v1/me/campaigns/{cid}/reward；
//   - 绝不调用 claim（POST），绝不触发 deviceToken/refresh（会轮换
//     refresh_token），绝不写号池、绝不改 qoder_egress.json。
//
// 用法：
//
//	cd D:/WorkBuddy/源码
//	go run ./cmd/qoderprobe \
//	  -emails "2108643821@qq.com,xu20060420fan@gmail.com" \
//	  -variants baseline,derived,derived_proxy \
//	  -out ../data/qoder_identity_probe.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "qoderprobe: %v\n", err)
		os.Exit(1)
	}
}

// probeAccount 是本次探测的账号输入。
type probeAccount struct {
	email string
	snap  *ideauth.QoderSnapshot
	auth  *ideauth.QoderAuth
	acc   ideauth.QoderProbeAccount
	// prepareErr 采集前的确定性错误（账号不在池里 / 无 token / token 过期等）。
	prepareErr string
}

// probeResult 是单账号单变体的采集结果（含后续交判定用的设备信息）。
type probeResult struct {
	res    ideauth.QoderProbeAccountResult
	cid    string // 本号首个 CLAIM_BENEFIT 实例 id（没有为空）
	dev    *ideauth.QoderDevice
	auth   *ideauth.QoderAuth
	scope  string
	errMsg string
}

func run() error {
	emailsFlag := flag.String("emails", "", "逗号分隔的账号邮箱（默认号池全部账号）")
	variantsFlag := flag.String("variants", "baseline,derived,derived_proxy", "逗号分隔的变体：baseline,derived,derived_proxy")
	outFlag := flag.String("out", "../data/qoder_identity_probe.json", "输出 JSON 路径")
	proxyAFlag := flag.String("proxy-a", "", "derived_proxy 变体第一个账号的 http(s) 代理（http://host:port）")
	proxyBFlag := flag.String("proxy-b", "", "derived_proxy 变体第二个账号的 http(s) 代理（http://host:port）")
	poolDirFlag := flag.String("pool-dir", "../data/ide_accounts", "号池目录（相对路径按当前工作目录解析）")

	flag.Parse()

	// 号池目录：显式传绝对路径给 ideauth，避免 go run（临时 exe 目录）把
	// 号池解析到构建缓存里。
	if dir := strings.TrimSpace(*poolDirFlag); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("解析号池目录失败: %w", err)
		}
		if st, statErr := os.Stat(abs); statErr != nil || !st.IsDir() {
			return fmt.Errorf("号池目录不存在: %s", abs)
		}
		if err := os.Setenv("IDEAUTH_POOL_DIR", abs); err != nil {
			return fmt.Errorf("设置 IDEAUTH_POOL_DIR 失败: %w", err)
		}
	}

	emails, err := parseProbeEmails(*emailsFlag)
	if err != nil {
		return err
	}
	variantNames, err := parseProbeVariants(*variantsFlag)
	if err != nil {
		return err
	}

	pool, err := ideauth.LoadQoderPool() // 只读：LoadQoderPool 不会写盘。
	if err != nil {
		return fmt.Errorf("读取号池失败: %w", err)
	}
	if len(emails) == 0 {
		for _, a := range pool.Accounts {
			if a != nil && strings.TrimSpace(a.Email) != "" {
				emails = append(emails, strings.TrimSpace(a.Email))
			}
		}
	}
	if len(emails) == 0 {
		return fmt.Errorf("号池里没有账号，且未通过 -emails 指定")
	}

	accounts, err := buildProbeAccounts(pool, emails)
	if err != nil {
		return err
	}

	// 账号凭证与采集前错误在开跑前一次算清（不做任何网络请求、不刷新 token）。
	auths := make([]*ideauth.QoderAuth, len(accounts))
	prepErrs := make([]string, len(accounts))
	for i, acc := range accounts {
		auths[i], prepErrs[i] = prepareAccount(acc.Email, pool)
	}

	// 真实设备只取一次：QoderDeviceInfo 可能启动 runtime-info.exe（首次有耗时）。
	baseDev := ideauth.QoderDeviceInfo()
	if baseDev == nil {
		baseDev = &ideauth.QoderDevice{}
	}

	proxyFor := func(idx int) string {
		switch idx {
		case 0:
			return strings.TrimSpace(*proxyAFlag)
		case 1:
			return strings.TrimSpace(*proxyBFlag)
		default:
			return ""
		}
	}

	var (
		variants []ideauth.QoderProbeVariant
		pairs    []ideauth.QoderProbePairVerdict
		missing  []string
	)

	for _, name := range variantNames {
		if name == "derived_proxy" {
			if proxyFor(0) == "" || proxyFor(1) == "" {
				// 没有两个出口就无法验证 IP 维度：跳过该变体，缺口如实记录。
				missing = append(missing, ideauth.QoderProbeMissingProxyNotConfigured)
				fmt.Fprintln(os.Stderr, "[skip] derived_proxy: -proxy-a / -proxy-b 未同时提供，跳过（missing: proxy_not_configured）")
				continue
			}
		}

		v, ps := runVariant(name, accounts, auths, prepErrs, baseDev, proxyFor)
		variants = append(variants, v)
		pairs = append(pairs, ps...)

		printVariantSummary(v, ps)
	}

	conc := ideauth.ClassifyQoderProbe(accounts, variants, pairs, missing)
	report := ideauth.QoderProbeReport{
		Version:           ideauth.QoderProbeVersion,
		At:                time.Now().Format(time.RFC3339),
		Accounts:          accounts,
		Variants:          variants,
		PairVerdicts:      pairs,
		CriterionInferred: conc.CriterionInferred,
		Conclusion:        conc.Conclusion,
		Missing:           conc.Missing,
	}
	if report.Accounts == nil {
		report.Accounts = []ideauth.QoderProbeAccount{}
	}
	if report.Variants == nil {
		report.Variants = []ideauth.QoderProbeVariant{}
	}
	if report.PairVerdicts == nil {
		report.PairVerdicts = []ideauth.QoderProbePairVerdict{}
	}

	outPath := strings.TrimSpace(*outFlag)
	if outPath == "" {
		return fmt.Errorf("-out 不能为空")
	}
	if abs, aerr := filepath.Abs(outPath); aerr == nil {
		outPath = abs
	}
	if err := writeProbeReport(outPath, &report); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\n结论：conclusion=%s criterion_inferred=%s\n", conc.Conclusion, conc.CriterionInferred)
	if len(conc.Missing) > 0 {
		fmt.Fprintf(os.Stderr, "缺口：missing=%s\n", strings.Join(conc.Missing, ","))
	}
	fmt.Fprintf(os.Stderr, "输出：%s\n", outPath)
	return nil
}

// buildProbeAccounts 从号池构造探测账号（只读，不校验、不刷新 token）。
func buildProbeAccounts(pool *ideauth.QoderPoolFile, emails []string) ([]ideauth.QoderProbeAccount, error) {
	byEmail := map[string]*ideauth.QoderSnapshot{}
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		byEmail[strings.ToLower(strings.TrimSpace(a.Email))] = a
	}
	out := make([]ideauth.QoderProbeAccount, 0, len(emails))
	for _, em := range emails {
		key := strings.ToLower(strings.TrimSpace(em))
		acc := ideauth.QoderProbeAccount{
			Email:       em,
			EmailDomain: emailDomain(em),
		}
		if snap := byEmail[key]; snap != nil {
			acc.Email = snap.Email
			acc.UserID = snap.UserID
			acc.Name = snap.Name
			acc.Scope = strings.TrimSpace(snap.Scope)
			acc.MachineID = snap.MachineID
			acc.DeviceID = snap.DeviceID
			if snap.Auth != nil {
				if strings.TrimSpace(acc.UserID) == "" {
					acc.UserID = snap.Auth.User.ID
				}
				if strings.TrimSpace(acc.Name) == "" {
					acc.Name = snap.Auth.User.Name
				}
				acc.Phone = phoneString(snap.Auth.User.Phone)
			}
		}
		acc.PhonePresent = strings.TrimSpace(acc.Phone) != ""
		out = append(out, acc)
	}
	return out, nil
}

// prepareAccount 返回账号的认证信息与采集前错误（不做任何网络请求）。
func prepareAccount(email string, pool *ideauth.QoderPoolFile) (*ideauth.QoderAuth, string) {
	var snap *ideauth.QoderSnapshot
	for _, a := range pool.Accounts {
		if a != nil && strings.EqualFold(strings.TrimSpace(a.Email), strings.TrimSpace(email)) {
			snap = a
			break
		}
	}
	if snap == nil {
		return nil, "account_not_in_pool"
	}
	if snap.Auth == nil || (strings.TrimSpace(snap.Auth.Token) == "" && strings.TrimSpace(snap.Auth.RefreshToken) == "") {
		return nil, "no_credentials"
	}
	if strings.TrimSpace(snap.Auth.Token) == "" {
		return nil, "no_access_token: 只有 refresh_token，只读探测不刷新"
	}
	if localTokenExpired(snap.Auth.ExpiresAt) {
		// 只读探测不刷新：refresh 会轮换 refresh_token，不写回号池会让它失效。
		return nil, "token_expired: 只读探测不刷新令牌（会轮换 refresh_token），请先由写路径续期"
	}
	return snap.Auth, ""
}

// localTokenExpired 判断本地记录的过期时间（与 qoderAuthExpired 同样的 2 分钟余量）。
func localTokenExpired(expiresAt string) bool {
	s := strings.TrimSpace(expiresAt)
	if s == "" {
		return false // 没有过期时间：交给上游 401 说话，不主动判死
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return false
	}
	return time.Now().Add(2 * time.Minute).After(t)
}

// runVariant 执行一个变体：每账号 campaigns → 本号 reward → 跨账号双向 reward。
func runVariant(name string, accounts []ideauth.QoderProbeAccount, auths []*ideauth.QoderAuth, prepErrs []string, baseDev *ideauth.QoderDevice, proxyFor func(int) string) (ideauth.QoderProbeVariant, []ideauth.QoderProbePairVerdict) {
	mode := ideauth.QoderDeviceModeReal
	if name == "derived" || name == "derived_proxy" {
		mode = ideauth.QoderDeviceModeDerived
	}

	results := make([]probeResult, 0, len(accounts))
	for i, acc := range accounts {
		pr := probeResult{auth: nil, scope: acc.Scope}
		proxy := ""
		if name == "derived_proxy" {
			proxy = proxyFor(i)
		}
		dev, derr := ideauth.DeriveQoderProbeDevice(baseDev, acc.Email, mode, proxy)
		if derr != nil {
			pr.res = ideauth.QoderProbeAccountResult{
				Email:      acc.Email,
				DeviceMode: mode,
				Proxy:      ideauth.RedactProxyURL(proxy),
				Error:      ideauth.SanitizeQoderProbeText(derr.Error()),
			}
			results = append(results, pr)
			continue
		}
		pr.dev = dev

		auth, prepErr := (*ideauth.QoderAuth)(nil), ""
		if i < len(auths) {
			auth = auths[i]
		}
		if i < len(prepErrs) {
			prepErr = prepErrs[i]
		}
		if prepErr != "" {
			pr.errMsg = prepErr
			pr.res = ideauth.QoderProbeAccountResult{
				Email:        acc.Email,
				DeviceMode:   mode,
				Proxy:        ideauth.RedactProxyURL(dev.ProxyURL),
				Error:        prepErr,
				MachineID:    dev.MachineID,
				MachineToken: dev.MachineToken,
			}
			results = append(results, pr)
			continue
		}
		pr.auth = auth

		res := ideauth.QoderProbeAccountResult{
			Email:          acc.Email,
			DeviceMode:     mode,
			Proxy:          ideauth.RedactProxyURL(dev.ProxyURL),
			CampaignIDs:    []string{},
			CampaignStates: []string{},
			MachineID:      dev.MachineID,
			MachineToken:   dev.MachineToken,
		}

		// 1) 只读：GET campaigns。
		st, raw, base, cerr := ideauth.QoderDoAny(acc.Scope, ideauth.QoderProbePathCampaigns, http.MethodGet, auth.Token, nil, dev)
		res.CampaignsHTTP = st
		if cerr != nil {
			res.Error = ideauth.SanitizeQoderProbeText(cerr.Error())
			pr.res = res
			results = append(results, pr)
			_ = base
			continue
		}
		if st != http.StatusOK {
			res.Error = ideauth.SanitizeQoderProbeText(fmt.Sprintf("campaigns HTTP %d: %s", st, truncateBody(raw)))
			pr.res = res
			results = append(results, pr)
			continue
		}

		campaigns := ideauth.ParseQoderProbeCampaigns(raw)
		var claimIDs []string
		var firstStart, firstEnd int64
		for _, c := range campaigns {
			if c.ActionType != "CLAIM_BENEFIT" {
				continue
			}
			id := strings.TrimSpace(c.CampaignID)
			if id == "" {
				continue
			}
			if len(claimIDs) == 0 {
				firstStart, firstEnd = c.StartAt, c.EndAt
			}
			res.CampaignIDs = append(res.CampaignIDs, id)
			res.CampaignStates = append(res.CampaignStates, strings.TrimSpace(c.ClaimStatus))
			claimIDs = append(claimIDs, id)
			// 活动窗口必须看：判定「这条 CLAIMED 是不是今天的」只能靠 startAt，
			// 只看状态会把昨天的陈旧活动误当成今天的额度（现网曾经的假签到根因）。
			fmt.Fprintf(os.Stderr,
				"    activity cid=%s name=%q status=%s startAt=%s endAt=%s today=%v amount=%.0f\n",
				id, c.Name, c.ClaimStatus, unixCST(c.StartAt), unixCST(c.EndAt),
				unixCSTIsToday(c.StartAt), c.Benefit.Amount)
		}
		res.StartAt = unixCST(firstStart)
		res.EndAt = unixCST(firstEnd)
		res.ClaimCampaigns = len(claimIDs)
		if len(claimIDs) > 0 {
			pr.cid = claimIDs[0]
			// 2) 只读：本号对自己首个 CLAIM_BENEFIT 实例查 reward。
			probe, perr := ideauth.QoderProbeRewardQuery(auth, dev, pr.cid, acc.Scope)
			if perr != nil {
				res.Error = ideauth.SanitizeQoderProbeText(perr.Error())
			} else {
				res.Reward = ideauth.QoderProbeReward{
					Status:      probe.Status,
					FailureCode: probe.FailureCode,
					GrantID:     probe.GrantID,
					CampaignID:  probe.CampaignID,
				}
				if res.Reward.CampaignID == "" {
					res.Reward.CampaignID = pr.cid
				}
			}
		}
		pr.res = res
		results = append(results, pr)
	}

	// 3) 跨账号双向只读探测：a 用 b 的实例查，b 用 a 的实例查。
	var ps []ideauth.QoderProbePairVerdict
	for i := range results {
		for j := range results {
			if i == j {
				continue
			}
			a, b := &results[i], &results[j]
			if a.auth == nil || b.auth == nil || a.dev == nil || b.dev == nil {
				continue
			}
			if strings.TrimSpace(b.cid) == "" {
				ps = append(ps, ideauth.QoderProbePairVerdict{
					A: a.res.Email, B: b.res.Email, Variant: name,
					Verdict:  ideauth.QoderVerdictUnknown,
					Evidence: "对方没有 CLAIM_BENEFIT 活动实例（活动未下发或已被领完即删），无法探测",
				})
				continue
			}
			ownerProbe, operr := ideauth.QoderProbeRewardQuery(a.auth, a.dev, b.cid, a.scope)
			if operr != nil {
				ps = append(ps, ideauth.QoderProbePairVerdict{
					A: a.res.Email, B: b.res.Email, Variant: name,
					Verdict:  ideauth.QoderVerdictUnknown,
					Evidence: ideauth.SanitizeQoderProbeText(operr.Error()),
				})
				continue
			}
			holderProbe, herr := ideauth.QoderProbeRewardQuery(b.auth, b.dev, b.cid, b.scope)
			if herr != nil {
				ps = append(ps, ideauth.QoderProbePairVerdict{
					A: a.res.Email, B: b.res.Email, Variant: name,
					Verdict:  ideauth.QoderVerdictUnknown,
					Evidence: ideauth.SanitizeQoderProbeText(herr.Error()),
				})
				continue
			}
			v := ideauth.QoderProbeVerdict(ownerProbe, holderProbe)
			ps = append(ps, ideauth.QoderProbePairVerdict{
				A:       a.res.Email,
				B:       b.res.Email,
				Variant: name,
				Verdict: v,
				Evidence: fmt.Sprintf(
					"%s 查 %s 的实例 %s → http=%d status=%s failure=%s grant=%s；%s 查同一实例 → http=%d status=%s grant=%s",
					a.res.Email, b.res.Email, b.cid,
					ownerProbe.HTTPStatus, ownerProbe.Status, ownerProbe.FailureCode, ownerProbe.GrantID,
					b.res.Email, holderProbe.HTTPStatus, holderProbe.Status, holderProbe.GrantID,
				),
			})
		}
	}

	v := ideauth.QoderProbeVariant{Name: name, PerAccount: make([]ideauth.QoderProbeAccountResult, 0, len(results))}
	for _, r := range results {
		v.PerAccount = append(v.PerAccount, r.res)
	}
	return v, ps
}

func printVariantSummary(v ideauth.QoderProbeVariant, ps []ideauth.QoderProbePairVerdict) {
	fmt.Fprintf(os.Stderr, "\n[%s]\n", v.Name)
	for _, pa := range v.PerAccount {
		fmt.Fprintf(os.Stderr,
			"  %s mode=%s proxy=%q http=%d claim_campaigns=%d ids=%v states=%v reward=%+v err=%q\n",
			pa.Email, pa.DeviceMode, pa.Proxy, pa.CampaignsHTTP, pa.ClaimCampaigns,
			pa.CampaignIDs, pa.CampaignStates, pa.Reward, pa.Error)
	}
	for _, pv := range ps {
		fmt.Fprintf(os.Stderr, "  pair %s→%s verdict=%s evidence=%s\n", pv.A, pv.B, pv.Verdict, pv.Evidence)
	}
}

// writeProbeReport 原子写输出 JSON（0600）。
func writeProbeReport(path string, rep *ideauth.QoderProbeReport) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化探测结果失败: %w", err)
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("写入探测结果失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("落盘探测结果失败: %w", err)
	}
	return nil
}

func parseProbeEmails(raw string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		em := strings.TrimSpace(part)
		if em == "" {
			continue
		}
		if !strings.Contains(em, "@") {
			return nil, fmt.Errorf("-emails 含非法邮箱: %q", em)
		}
		key := strings.ToLower(em)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, em)
	}
	return out, nil
}

func parseProbeVariants(raw string) ([]string, error) {
	allowed := map[string]bool{"baseline": true, "derived": true, "derived_proxy": true}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		if !allowed[name] {
			return nil, fmt.Errorf("-variants 含未知变体 %q（只允许 baseline,derived,derived_proxy）", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-variants 不能为空")
	}
	return out, nil
}

func emailDomain(email string) string {
	em := strings.ToLower(strings.TrimSpace(email))
	if i := strings.LastIndex(em, "@"); i >= 0 && i+1 < len(em) {
		return em[i+1:]
	}
	return ""
}

// phoneString 把 auth.User.Phone（any）安全地转成字符串（没有手机号返回空）。
func phoneString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case float64:
		return fmt.Sprintf("%.0f", t)
	case json.Number:
		return strings.TrimSpace(t.String())
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

// unixCST 把秒级时间戳格式化为 UTC+8 时间（0 表示上游没给）。
func unixCST(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).In(time.FixedZone("UTC+8", 8*3600)).Format(time.RFC3339)
}

// qoderProbeCST Qoder 活动体系所在时区（每日 10:00 UTC+8 刷新）。
var qoderProbeCST = time.FixedZone("UTC+8", 8*3600)

// unixCSTIsToday 判断秒级时间戳是否落在 UTC+8 的今天（用于区分今天的活动与
// 昨天遗留的陈旧活动）。上游没给 startAt 时返回 false —— 无法证明是今天的。
func unixCSTIsToday(sec int64) bool {
	if sec <= 0 {
		return false
	}
	now := time.Now().In(qoderProbeCST)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, qoderProbeCST)
	return !time.Unix(sec, 0).In(qoderProbeCST).Before(start)
}

func truncateBody(raw []byte) string {
	s := string(raw)
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return ideauth.SanitizeQoderProbeText(s)
}
