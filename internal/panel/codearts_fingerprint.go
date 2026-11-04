package panel

// codearts_fingerprint.go CodeArts 身份槽的「最小浏览器指纹 + 可选独立出口」。
//
// 定位：槽（SlotID/SlotDir/DeviceGUID/SlotProfileDir，见 internal/codearts）解决的是
// 「profile 容器 + 设备 ID 不共用」；本文件补上另一半——同一个进程同时驱动多个槽时，
// 服务端从 HTTP 头 / JS 环境里还能看到「所有账号的 UA、语言、时区、窗口尺寸一模一样」。
// 这里给每个槽派生一组**稳定且互不相同**的最小差异值。
//
// 明确的边界（后续接手的人请勿随意扩大）：
//  1. 只做「最小差异」：不做 WebGL / Canvas / WebGL vendor / hardwareConcurrency /
//     字体列表 / 音频指纹 等深层伪造。这些值的真实来源是本机 GPU 与系统字体，
//     硬编码假值会与 navigator、性能指标、实际渲染结果自相矛盾（等于把可疑信号
//     做得更明显），而且 Chromium 一升级就会失真。要做深指纹，正确做法是每槽
//     独立 profile（已由 SlotDir/SlotProfileDir 提供），而不是在 JS 层造假。
//  2. 所有值必须是**真实存在**的：UA 主版本跟随项目当前固定的 Chrome 版本
//     （internal/codearts/api.go 的 apiUA），绝不编造一个不存在的大版本号。
//  3. 出口代理是**可选**的：环境变量 CODEARTS_PROXY_<SlotID> 优先、CODEARTS_PROXY
//     兜底；只接受 http(s)://host:port。非法值一律当没配置——一个写错的环境变量
//     不应该把浏览器的网络打挂。
//  4. 本文件全部是纯函数（除读取环境变量外无副作用），便于单测直接断言。
//
// 与 codearts_slot.go 的 codeartsChromeArgs 的关系：那个函数负责
// 「--remote-debugging-port / --user-data-dir / headless / startURL」这些**与账号无关**
// 的骨架参数；本文件的 codeartsChromeArgsForAccount 在其结果上叠加槽指纹。
//
// ⚠️ 命名说明：本文件**不能**把叠加函数命名为 codeartsChromeArgs——该名字已被
// codearts_slot.go 的骨架参数函数占用（Go 不支持重载），且那个文件不在本任务
// 可改范围内。故叠加函数叫 codeartsChromeArgsForAccount，实际叠加逻辑在
// codeartsFingerprintArgs 里。

import (
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

const (
	// codeartsFingerprintChromeMajor 当前固定的 Chrome 主版本。
	//
	// 与 internal/codearts/api.go 的 apiUA 保持同一主版本（那边是纯 HTTP 接口的
	// UA，这边是浏览器里的 UA）。两边不一致时，同一个账号在「HTTP 签到」与
	// 「浏览器签到」两条路径上就是两台设备，反而制造矛盾信号。
	// TestCodeartsFingerprintUAKeepsProjectChromeMajor 会交叉校验，改一处忘改另一处会红。
	codeartsFingerprintChromeMajor = "156"

	// codeartsFingerprintAcceptLanguage 华为云控制台默认的中文 Accept-Language。
	codeartsFingerprintAcceptLanguage = "zh-CN,zh;q=0.9"

	// codeartsFingerprintLang 启动参数 --lang 的值（只保留主语言）。
	codeartsFingerprintLang = "zh-CN"

	// codeartsFingerprintTimezone 控制台用户所在的时区（不按 IP 猜，固定东八区）。
	codeartsFingerprintTimezone = "Asia/Shanghai"

	// codeartsFingerprintPlatform navigator.platform 的覆盖值：与 UA 里的
	// "Windows NT 10.0; Win64; x64" 对应，必须一起改。
	codeartsFingerprintPlatform = "Win32"

	// codeartsFingerprintProxyEnvPrefix 每槽专属出口的环境变量前缀。
	codeartsFingerprintProxyEnvPrefix = "CODEARTS_PROXY_"

	// codeartsFingerprintProxyEnvGlobal 全局兜底出口。
	codeartsFingerprintProxyEnvGlobal = "CODEARTS_PROXY"
)

// codeartsFingerprintUserAgent 浏览器 UA。
//
// 不用 per-slot 随机版本：浏览器真实版本只有本机 Chrome 一个，编造别的大版本
// 会与 TLS/JA3、Client Hints、实际渲染能力对不上。差异由窗口尺寸承担。
var codeartsFingerprintUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + codeartsFingerprintChromeMajor + ".0.0.0 Safari/537.36"

// codeartsFingerprintViewports 窗口尺寸候选。
//
// 都取自常见 Windows 笔记本/外接屏的可用视口，差值不大（避免出现 4K 这种
// 与「同一台机器」明显矛盾的组合）。只在这里扩，不要在调用点写魔法数字。
var codeartsFingerprintViewports = [][2]int{
	{1600, 1000}, // 1920x1200 / 2560x1600 上的常用窗口
	{1536, 864},  // 1920x1080 缩放 125% 后的可用视口
	{1920, 1080}, // 1080p 全屏
}

// codeartsFingerprint 一个槽的浏览器最小指纹。
//
// 零值表示「没有可用的槽身份」：调用方据此**不加任何参数**，确保普通登录/签到
// 路径与引入指纹前完全一致。
type codeartsFingerprint struct {
	UserAgent      string
	AcceptLanguage string
	Timezone       string
	Width          int
	Height         int
	// ProxyURL 归一化后的 http(s)://host:port；空 = 不走独立出口。
	ProxyURL string
}

// empty 是否是一个「不做指纹」的零值。
func (fp codeartsFingerprint) empty() bool {
	return fp.UserAgent == "" && fp.AcceptLanguage == "" &&
		fp.Timezone == "" && fp.Width <= 0 && fp.Height <= 0 && fp.ProxyURL == ""
}

// codeartsFingerprintFor 按槽的稳定身份派生最小指纹。
//
// 稳定性：只依赖 SlotID（SlotID 空时依次退回 ID / DeviceGUID），同一槽跨进程、
// 跨重启、跨多次签到都得到同一组值——这一点很关键，如果每次签到窗口尺寸都变，
// 「同一台设备」的叙事就断了，比不做指纹更糟。
//
// 无槽（a 为空，或 SlotID/ID/DeviceGUID 全空）→ 返回零值，调用方不做任何改动。
func codeartsFingerprintFor(a *codearts.Account) codeartsFingerprint {
	slotID := codeartsSlotIdentityOf(a)
	if slotID == "" {
		return codeartsFingerprint{}
	}
	width, height := codeartsFingerprintViewportOf(slotID)
	return codeartsFingerprint{
		UserAgent:      codeartsFingerprintUserAgent,
		AcceptLanguage: codeartsFingerprintAcceptLanguage,
		Timezone:       codeartsFingerprintTimezone,
		Width:          width,
		Height:         height,
		ProxyURL:       codeartsProxyURLForSlot(slotID),
	}
}

// codeartsSlotIdentityOf 用于派生指纹的稳定槽身份。
//
// 派生顺序与 codearts.EnsureSlot 的 SlotID 补全规则一致（SlotID 空 → ID），
// 这样「还没跑过 EnsureSlot 的旧号池条目」与「补全后的同一条目」得到同一指纹。
func codeartsSlotIdentityOf(a *codearts.Account) string {
	if a == nil {
		return ""
	}
	for _, candidate := range []string{a.SlotID, a.ID, a.DeviceGUID} {
		if v := strings.TrimSpace(candidate); v != "" {
			return v
		}
	}
	return ""
}

// codeartsFingerprintViewportOf 按槽身份哈希稳定选一个窗口尺寸。
//
// 为什么是哈希而不是随机/自增：随机会让同槽每次不同（见 codeartsFingerprintFor
// 的稳定性说明）；自增依赖遍历顺序，号池一排序就全变。
func codeartsFingerprintViewportOf(slotID string) (width, height int) {
	if len(codeartsFingerprintViewports) == 0 {
		return 0, 0
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(slotID))
	idx := int(hasher.Sum32() % uint32(len(codeartsFingerprintViewports)))
	viewport := codeartsFingerprintViewports[idx]
	return viewport[0], viewport[1]
}

// codeartsProxyURLForSlot 该槽的独立出口（可能为空）。
//
// 优先级：CODEARTS_PROXY_<SlotID> → CODEARTS_PROXY_<SlotID 规范化> → CODEARTS_PROXY。
//
// 专属值写错时**继续往下看兜底**，而不是直接判定「这个号不走代理」：环境变量写错
// 属于配置笔误，不应该静默把某个号暴露到默认出口。两者都非法/缺失才留空。
func codeartsProxyURLForSlot(slotID string) string {
	for _, key := range codeartsProxyEnvKeysOf(slotID) {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		if normalized := codeartsNormalizeProxyURL(raw); normalized != "" {
			return normalized
		}
	}
	return ""
}

// codeartsProxyEnvKeysOf 依次要查的环境变量名。
//
// 环境变量名在 Windows 上虽然允许 "-" 等字符，但很多工具（docker-compose/.env/
// systemd）不接受，所以额外提供一份「大写 + 非字母数字替换成下划线」的写法，
// 两种写法都能命中。
func codeartsProxyEnvKeysOf(slotID string) []string {
	id := strings.TrimSpace(slotID)
	if id == "" {
		return []string{codeartsFingerprintProxyEnvGlobal}
	}
	keys := []string{codeartsFingerprintProxyEnvPrefix + id}
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToUpper(r)
		}
		return '_'
	}, id)
	if normalized != id {
		keys = append(keys, codeartsFingerprintProxyEnvPrefix+normalized)
	}
	keys = append(keys, codeartsFingerprintProxyEnvGlobal)
	return keys
}

// codeartsNormalizeProxyURL 校验并归一化代理地址；非法返回 ""。
//
// 只接受 http://host:port 与 https://host:port（Chromium --proxy-server 支持这两
// 种 scheme）。socks5 需要 --proxy-server="socks5://..." 且行为与 http 代理在
// Cookie/证书链路上不同，未在本项目验证过，故明确拒绝——宁可不用代理，也不要用
// 一个没验证过的出口把会话弄坏。
//
// 没有端口一律拒绝：Chromium 对 http://host 会回落到 80，看似能用，但实际是把
// 「代理没起来」伪装成「网络抽风」，排查成本极高。
func codeartsNormalizeProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil {
		return ""
	}
	switch scheme := strings.ToLower(parsed.Scheme); scheme {
	case "http", "https":
	default:
		return ""
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return ""
	}
	portText := parsed.Port()
	if portText == "" {
		return ""
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return ""
	}
	// 只保留 scheme://[userinfo@]host:port：路径/查询/片段对代理没有意义，
	// 出现它们基本是抄错了别的 URL。
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	if parsed.User != nil {
		return scheme + "://" + parsed.User.String() + "@" + parsed.Host
	}
	return scheme + "://" + parsed.Host
}

// codeartsRedactProxy 日志用的脱敏代理地址（userinfo 里的账号密码不能进日志）。
func codeartsRedactProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil {
		return "***"
	}
	if parsed.User != nil {
		parsed.User = url.User("***")
	}
	return parsed.String()
}

// codeartsFingerprintArgs 把指纹叠加到浏览器启动参数上。
//
// 追加内容：
//   - --window-size=W,H
//   - --lang=zh-CN
//   - --accept-lang=zh-CN,zh;q=0.9
//   - ProxyURL 非空时 --proxy-server=<url>
//
// **UA 不进命令行**：Chromium 没有 --user-agent 之外还带 accept-language 的稳定
// 组合，而且命令行 UA 对页面里 navigator.userAgentData 无效；统一由 CDP 的
// Emulation.setUserAgentOverride 覆盖（见 codeartsApplyFingerprint），保证刷新后
// 仍是同一份。
//
// 同名参数去重：headless 启动模板里已经有 --window-size=1600,1000，如果直接
// append 会出现两个 --window-size，Chrome 取哪个没有稳定保证；这里替换掉旧值，
// 保证「槽尺寸」真正生效。
//
// base 不会被修改（返回新切片），调用方可以安全复用模板。
func codeartsFingerprintArgs(base []string, fp codeartsFingerprint) []string {
	out := make([]string, 0, len(base)+4)
	out = append(out, base...)
	if fp.empty() {
		return out
	}
	if fp.Width > 0 && fp.Height > 0 {
		out = codeartsUpsertArg(out, "--window-size",
			strconv.Itoa(fp.Width)+","+strconv.Itoa(fp.Height))
	}
	if lang := strings.TrimSpace(fp.AcceptLanguage); lang != "" {
		out = codeartsUpsertArg(out, "--accept-lang", lang)
		// --lang 只要主语言，不接受 "zh-CN,zh;q=0.9" 这种带 q 值的写法。
		out = codeartsUpsertArg(out, "--lang", codeartsFingerprintLang)
	}
	if proxy := strings.TrimSpace(fp.ProxyURL); proxy != "" {
		out = codeartsUpsertArg(out, "--proxy-server", proxy)
	}
	return out
}

// codeartsUpsertArg 删除参数列表里已有的 --key=... 再追加新值，保证参数唯一。
func codeartsUpsertArg(args []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(args)+1)
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			continue
		}
		out = append(out, arg)
	}
	return append(out, key+"="+value)
}

// codeartsChromeArgsForAccount 骨架启动参数 + 该账号槽指纹。
//
// 接入方式（login 窗口 / headless 签到统一走这里）：
//
//	args := codeartsChromeArgsForAccount(
//	    codeartsChromeArgs(port, profileDir, startURL, headless), acc)
//
// 无账号或无槽身份时原样返回骨架参数（codeartsChromeArgsForAccount(nil) 同理），
// 保证普通登录/签到的启动参数与引入指纹前逐字节一致。
func codeartsChromeArgsForAccount(base []string, a *codearts.Account) []string {
	return codeartsFingerprintArgs(base, codeartsFingerprintFor(a))
}

// codeartsApplyFingerprint 在已建立的页面级 CDP 连接上应用指纹。
//
// 调用时机：写完 Cookie（Storage.setCookies）之后、Page.navigate 之前——必须在
// 导航前设置，否则首个请求已经带着真实 UA 发出去，再改就只对后续 XHR 生效。
//
// 失败返回 error，但**调用方只记日志、不阻断**：指纹是「减小差异」，不是登录的
// 前提条件；某些 Chromium/Edge 版本对 Emulation.setTimezoneOverride 支持不完整，
// 因为它失败就放弃整轮签到是本末倒置。
func codeartsApplyFingerprint(ws *wsConn, fp codeartsFingerprint) error {
	// 零值 = 没有槽身份 = 不做任何事。先判它，调用方即使拿不到页面连接
	// （老路径只传 port）也不会因为「没指纹」而报错。
	if fp.empty() {
		return nil
	}
	if ws == nil {
		return fmt.Errorf("CDP 连接为空，无法应用指纹")
	}
	if userAgent := strings.TrimSpace(fp.UserAgent); userAgent != "" {
		params := map[string]any{
			"userAgent": userAgent,
			// platform 与 UA 必须同源（这里是 Windows），否则 navigator.platform
			// 与 UA 字符串打架，比不覆盖更可疑。
			"platform": codeartsFingerprintPlatform,
		}
		if accept := strings.TrimSpace(fp.AcceptLanguage); accept != "" {
			params["acceptLanguage"] = accept
		}
		if _, err := doubaoCDPCall(ws, 71, "Emulation.setUserAgentOverride", params, 10*time.Second); err != nil {
			return fmt.Errorf("设置 UA/语言失败: %w", err)
		}
	}
	if timezone := strings.TrimSpace(fp.Timezone); timezone != "" {
		params := map[string]any{"timezoneId": timezone}
		if _, err := doubaoCDPCall(ws, 72, "Emulation.setTimezoneOverride", params, 10*time.Second); err != nil {
			return fmt.Errorf("设置时区失败: %w", err)
		}
	}
	return nil
}
