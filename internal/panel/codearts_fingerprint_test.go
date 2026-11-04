package panel

// codearts_fingerprint_test.go 身份槽最小指纹/出口的单测。
//
// 全部是纯函数断言，不起浏览器、不写真实 data/：槽根与号池文件都被指到
// t.TempDir()，代理相关的环境变量在用例内显式设置/清空，避免被开发机上的
// 真实 CODEARTS_PROXY 干扰。

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// codeartsFingerprintTestIsolate 给用例一套完全独立的槽根/号池路径 + 干净代理环境。
//
// 即使被测函数目前不读盘，也统一隔离：以后有人给指纹加「读槽 meta 里的历史值」
// 时，测试不会意外摸到真实 data/。
func codeartsFingerprintTestIsolate(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEARTS_SLOT_ROOT", filepath.Join(dir, "codearts-slots"))
	t.Setenv("CODEARTS_POOL_FILE", filepath.Join(dir, "codearts-pool.json"))
	t.Setenv(codeartsFingerprintProxyEnvGlobal, "")
}

// TestCodeartsFingerprintStablePerSlot 同一槽多次派生必须完全一致。
//
// 这是本功能的核心不变量：如果同槽每次签到窗口尺寸/时区都在变，「同一台设备」
// 的叙事就断了，比不做指纹更可疑。
func TestCodeartsFingerprintStablePerSlot(t *testing.T) {
	codeartsFingerprintTestIsolate(t)

	account := &codearts.Account{ID: "acc-1", SlotID: "slot-a"}
	first := codeartsFingerprintFor(account)
	second := codeartsFingerprintFor(account)
	if first != second {
		t.Fatalf("同一槽两次派生不一致：%+v vs %+v", first, second)
	}

	// 换一个对象实例（模拟进程重启后重新读号池）也必须一致。
	reloaded := &codearts.Account{ID: "acc-1", SlotID: "slot-a"}
	if again := codeartsFingerprintFor(reloaded); again != first {
		t.Fatalf("同 SlotID 的新对象派生不一致：%+v vs %+v", again, first)
	}

	if !strings.HasPrefix(first.UserAgent, "Mozilla/5.0 (Windows NT 10.0; Win64; x64)") {
		t.Fatalf("UA 必须是真实的 Windows Chrome UA，实际 %q", first.UserAgent)
	}
	if !strings.Contains(first.UserAgent, "Chrome/"+codeartsFingerprintChromeMajor+".") {
		t.Fatalf("UA 主版本必须是项目固定的 %s，实际 %q",
			codeartsFingerprintChromeMajor, first.UserAgent)
	}
	if first.AcceptLanguage != "zh-CN,zh;q=0.9" {
		t.Fatalf("AcceptLanguage 不符：%q", first.AcceptLanguage)
	}
	if first.Timezone != "Asia/Shanghai" {
		t.Fatalf("Timezone 不符：%q", first.Timezone)
	}
	if first.Width <= 0 || first.Height <= 0 {
		t.Fatalf("窗口尺寸必须为正：%dx%d", first.Width, first.Height)
	}
}

// TestCodeartsFingerprintDifferentSlotViewportDiffers 不同槽要能从尺寸上区分开。
//
// 候选只有 3 种，必然存在哈希碰撞，所以断言的是「整体上至少出现 2 种尺寸，
// 且每种都在白名单内」而不是「任意两槽必不同」（后者不成立）。
func TestCodeartsFingerprintDifferentSlotViewportDiffers(t *testing.T) {
	codeartsFingerprintTestIsolate(t)

	allowed := make(map[[2]int]bool, len(codeartsFingerprintViewports))
	for _, viewport := range codeartsFingerprintViewports {
		allowed[viewport] = true
	}

	seen := map[[2]int]bool{}
	derived := make([]codeartsFingerprint, 0, 40)
	for i := 0; i < 40; i++ {
		fp := codeartsFingerprintFor(&codearts.Account{SlotID: "slot-" + strconv.Itoa(i)})
		size := [2]int{fp.Width, fp.Height}
		if !allowed[size] {
			t.Fatalf("槽 slot-%d 的尺寸 %v 不在白名单内", i, size)
		}
		seen[size] = true
		derived = append(derived, fp)
	}
	if len(seen) < 2 {
		t.Fatalf("40 个不同槽只得到 %d 种尺寸，指纹没有区分度：%v", len(seen), seen)
	}

	// 显式找出一个与基准槽尺寸不同的槽，证明「不同 SlotID → 尺寸不同」确实会发生。
	base := derived[0]
	found := false
	for _, fp := range derived[1:] {
		if fp.Width != base.Width || fp.Height != base.Height {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("所有槽尺寸都等于基准 %dx%d，导出逻辑可能有 bug", base.Width, base.Height)
	}
}

// TestCodeartsFingerprintInvalidProxyIgnored 非法出口必须被忽略且不 panic。
func TestCodeartsFingerprintInvalidProxyIgnored(t *testing.T) {
	codeartsFingerprintTestIsolate(t)

	invalid := []string{
		"",
		"   ",
		"socks5://127.0.0.1:1080", // 未验证过的 scheme，明确拒绝
		"socks5h://127.0.0.1:1080",
		"http://127.0.0.1",        // 缺端口
		"http://:8080",            // 缺主机
		"not a url",               // 没有 scheme
		"://bad",                  // 解析失败
		"ftp://127.0.0.1:21",      // 非 http(s)
		"http://127.0.0.1:0",      // 端口越界
		"http://127.0.0.1:70000",  // 端口越界
		"http://127.0.0.1:8080/x", // 带路径（抄错了别的 URL）
		"http://127.0.0.1:8080?token=1",
	}
	for _, raw := range invalid {
		if got := codeartsNormalizeProxyURL(raw); got != "" {
			t.Errorf("非法代理 %q 应被忽略，实际归一化为 %q", raw, got)
		}
	}

	// 真正从环境变量读到的路径也不能 panic，且最终必须留空。
	for _, raw := range invalid {
		t.Setenv(codeartsFingerprintProxyEnvPrefix+"slot-a", raw)
		fp := codeartsFingerprintFor(&codearts.Account{SlotID: "slot-a"})
		if fp.ProxyURL != "" {
			t.Fatalf("非法代理 %q 应从 env 被忽略，实际 ProxyURL=%q", raw, fp.ProxyURL)
		}
	}
}

// TestCodeartsFingerprintProxySlotEnvWins 每槽专属变量优先于全局兜底。
func TestCodeartsFingerprintProxySlotEnvWins(t *testing.T) {
	codeartsFingerprintTestIsolate(t)
	t.Setenv(codeartsFingerprintProxyEnvGlobal, "http://127.0.0.1:7891")
	t.Setenv(codeartsFingerprintProxyEnvPrefix+"slot-a", "http://127.0.0.1:7890")

	if got := codeartsFingerprintFor(&codearts.Account{SlotID: "slot-a"}).ProxyURL; got != "http://127.0.0.1:7890" {
		t.Fatalf("slot-a 应使用专属出口 7890，实际 %q", got)
	}
	if got := codeartsFingerprintFor(&codearts.Account{SlotID: "slot-b"}).ProxyURL; got != "http://127.0.0.1:7891" {
		t.Fatalf("slot-b 没有专属变量，应回退全局出口 7891，实际 %q", got)
	}
}

// TestCodeartsFingerprintProxyGlobalFallback 没有专属变量时用 CODEARTS_PROXY。
func TestCodeartsFingerprintProxyGlobalFallback(t *testing.T) {
	codeartsFingerprintTestIsolate(t)
	t.Setenv(codeartsFingerprintProxyEnvGlobal, "https://proxy.example.com:8443")

	fp := codeartsFingerprintFor(&codearts.Account{SlotID: "slot-c"})
	if fp.ProxyURL != "https://proxy.example.com:8443" {
		t.Fatalf("应回退全局出口，实际 %q", fp.ProxyURL)
	}
}

// TestCodeartsFingerprintProxyInvalidSlotFallsBackToGlobal 专属值写错不能静默变成「不走代理」。
func TestCodeartsFingerprintProxyInvalidSlotFallsBackToGlobal(t *testing.T) {
	codeartsFingerprintTestIsolate(t)
	t.Setenv(codeartsFingerprintProxyEnvPrefix+"slot-d", "socks5://127.0.0.1:1080")
	t.Setenv(codeartsFingerprintProxyEnvGlobal, "http://127.0.0.1:7891")

	fp := codeartsFingerprintFor(&codearts.Account{SlotID: "slot-d"})
	if fp.ProxyURL != "http://127.0.0.1:7891" {
		t.Fatalf("专属值非法时应继续走兜底，实际 %q", fp.ProxyURL)
	}
}

// TestCodeartsFingerprintProxyEnvNormalizedKey 含 "-" 的 SlotID 也支持大写下划线写法。
func TestCodeartsFingerprintProxyEnvNormalizedKey(t *testing.T) {
	codeartsFingerprintTestIsolate(t)
	t.Setenv("CODEARTS_PROXY_CA_TEST_9", "http://127.0.0.1:7892")

	fp := codeartsFingerprintFor(&codearts.Account{SlotID: "ca-test-9"})
	if fp.ProxyURL != "http://127.0.0.1:7892" {
		t.Fatalf("规范化 env 键未命中，实际 %q", fp.ProxyURL)
	}
}

// TestCodeartsFingerprintArgsProxyOnlyWhenConfigured 命令行只在有代理时出现 --proxy-server。
func TestCodeartsFingerprintArgsProxyOnlyWhenConfigured(t *testing.T) {
	codeartsFingerprintTestIsolate(t)

	base := []string{"--remote-debugging-port=9333", "--user-data-dir=/tmp/slot-a"}
	fp := codeartsFingerprint{
		UserAgent:      codeartsFingerprintUserAgent,
		AcceptLanguage: codeartsFingerprintAcceptLanguage,
		Timezone:       codeartsFingerprintTimezone,
		Width:          1600,
		Height:         1000,
	}

	withoutProxy := codeartsFingerprintArgs(base, fp)
	if len(base) != 2 || base[1] != "--user-data-dir=/tmp/slot-a" {
		t.Fatalf("codeartsFingerprintArgs 不应修改入参 base：%v", base)
	}
	if slices.ContainsFunc(withoutProxy, func(arg string) bool {
		return strings.HasPrefix(arg, "--proxy-server")
	}) {
		t.Fatalf("没有代理配置时不应出现 --proxy-server：%v", withoutProxy)
	}
	for _, want := range []string{
		"--window-size=1600,1000",
		"--lang=zh-CN",
		"--accept-lang=zh-CN,zh;q=0.9",
	} {
		if !slices.Contains(withoutProxy, want) {
			t.Fatalf("缺少启动参数 %q：%v", want, withoutProxy)
		}
	}
	// UA 必须走 CDP 覆盖，不能塞进命令行。
	if slices.ContainsFunc(withoutProxy, func(arg string) bool {
		return strings.Contains(arg, "Mozilla/5.0")
	}) {
		t.Fatalf("UA 不应出现在启动参数里：%v", withoutProxy)
	}

	fp.ProxyURL = "http://127.0.0.1:7890"
	withProxy := codeartsFingerprintArgs(base, fp)
	proxyArgs := 0
	for _, arg := range withProxy {
		if strings.HasPrefix(arg, "--proxy-server=") {
			proxyArgs++
			if arg != "--proxy-server=http://127.0.0.1:7890" {
				t.Fatalf("--proxy-server 值不符：%q", arg)
			}
		}
	}
	if proxyArgs != 1 {
		t.Fatalf("有代理时应恰好出现一次 --proxy-server，实际 %d 次：%v", proxyArgs, withProxy)
	}
}

// TestCodeartsFingerprintArgsReplacesExistingWindowSize headless 模板里的旧尺寸要被替换，不能叠加。
func TestCodeartsFingerprintArgsReplacesExistingWindowSize(t *testing.T) {
	codeartsFingerprintTestIsolate(t)

	base := []string{"--headless=new", "--disable-gpu", "--window-size=1600,1000"}
	fp := codeartsFingerprint{
		AcceptLanguage: codeartsFingerprintAcceptLanguage,
		Width:          1920,
		Height:         1080,
	}
	args := codeartsFingerprintArgs(base, fp)

	sizes := make([]string, 0, 1)
	for _, arg := range args {
		if strings.HasPrefix(arg, "--window-size=") {
			sizes = append(sizes, arg)
		}
	}
	if len(sizes) != 1 || sizes[0] != "--window-size=1920,1080" {
		t.Fatalf("窗口尺寸必须唯一且被替换为槽尺寸，实际 %v（全部参数 %v）", sizes, args)
	}
	if len(base) != 3 {
		t.Fatalf("入参 base 被修改：%v", base)
	}
}

// TestCodeartsChromeArgsForAccountKeepsBaseWithoutSlot 无账号/无槽身份时参数与现在逐字节一致。
func TestCodeartsChromeArgsForAccountKeepsBaseWithoutSlot(t *testing.T) {
	codeartsFingerprintTestIsolate(t)

	base := codeartsChromeArgs(9333, filepath.Join(t.TempDir(), "profile"), "about:blank", true)
	for _, account := range []*codearts.Account{nil, {}, {ID: ""}} {
		got := codeartsChromeArgsForAccount(base, account)
		if !slices.Equal(got, base) {
			t.Fatalf("无槽身份时启动参数必须保持原样：%v vs base %v", got, base)
		}
	}

	// 空账号派生出来的指纹必须是零值（调用方据此完全跳过指纹）。
	if fp := codeartsFingerprintFor(nil); !fp.empty() {
		t.Fatalf("nil 账号应得到零值指纹，实际 %+v", fp)
	}
	if fp := codeartsFingerprintFor(&codearts.Account{}); !fp.empty() {
		t.Fatalf("无槽字段的账号应得到零值指纹，实际 %+v", fp)
	}
}

// TestCodeartsFingerprintUAKeepsProjectChromeMajor UA 主版本不得与 HTTP 路径的 apiUA 漂移。
//
// 同一个账号在「纯 HTTP 签到」与「浏览器签到」两条路径上必须是同一台设备；
// 两处写死不同大版本会直接自相矛盾。这里直接读源码交叉校验，改一处忘改另一处会红。
func TestCodeartsFingerprintUAKeepsProjectChromeMajor(t *testing.T) {
	codeartsFingerprintTestIsolate(t)

	raw, err := os.ReadFile(filepath.Join("..", "codearts", "api.go"))
	if err != nil {
		t.Skipf("无法读取 internal/codearts/api.go，跳过主版本交叉校验: %v", err)
	}
	match := regexp.MustCompile(`Chrome/(\d+)\.`).FindSubmatch(raw)
	if match == nil {
		t.Fatalf("internal/codearts/api.go 里没找到 Chrome/<major>. 形式的 UA")
	}
	projectMajor := string(match[1])
	if projectMajor != codeartsFingerprintChromeMajor {
		t.Fatalf("指纹主版本 %s 与 codearts/api.go 的 %s 不一致（同一账号会被当成两台设备）",
			codeartsFingerprintChromeMajor, projectMajor)
	}
	if !strings.Contains(codeartsFingerprintUserAgent, "Chrome/"+projectMajor+".") {
		t.Fatalf("指纹 UA 未使用项目主版本 %s：%q", projectMajor, codeartsFingerprintUserAgent)
	}
}

// TestCodeartsApplyFingerprintEmptyIsNoop 零值指纹是 while-nothing 操作，不因缺少连接而报错。
func TestCodeartsApplyFingerprintEmptyIsNoop(t *testing.T) {
	codeartsFingerprintTestIsolate(t)

	if err := codeartsApplyFingerprint(nil, codeartsFingerprint{}); err != nil {
		t.Fatalf("零值指纹应直接返回 nil，实际 %v", err)
	}
	// 有指纹但没有连接时必须报错（调用方只会记日志），不能静默成功让上层以为已生效。
	if err := codeartsApplyFingerprint(nil, codeartsFingerprint{UserAgent: "UA"}); err == nil {
		t.Fatalf("非零指纹 + nil 连接应返回错误")
	}
}

// TestCodeartsRedactProxyHidesCredentials 日志里不能出现代理账号密码。
func TestCodeartsRedactProxyHidesCredentials(t *testing.T) {
	codeartsFingerprintTestIsolate(t)

	got := codeartsRedactProxy("http://alice:s3cret@127.0.0.1:7890")
	if strings.Contains(got, "s3cret") || strings.Contains(got, "alice") {
		t.Fatalf("脱敏后仍含账号密码：%q", got)
	}
	if !strings.Contains(got, "127.0.0.1:7890") {
		t.Fatalf("脱敏后应保留主机端口：%q", got)
	}
	if got := codeartsRedactProxy(""); got != "" {
		t.Fatalf("空代理应返回空串，实际 %q", got)
	}
}
