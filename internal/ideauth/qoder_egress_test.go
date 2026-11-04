package ideauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// useTempEgressFile 把出口配置指向临时目录，避免污染真实 data/ide_accounts。
func useTempEgressFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "qoder_egress.json")
	t.Setenv("QODER_EGRESS_FILE", p)
	return p
}

// 验收①：无配置时必须与 QoderDeviceInfo() 完全一致（现网行为不变）。
func TestQoderEgressDefaultRealMatchesDeviceInfo(t *testing.T) {
	path := useTempEgressFile(t)

	prof := QoderEgressFor(" Mixed@Case.COM ")
	if prof.DeviceMode != QoderDeviceModeReal {
		t.Fatalf("无配置时 device_mode 必须是 real，实际 %q", prof.DeviceMode)
	}
	if prof.ProxyURL != "" {
		t.Fatalf("无配置时 proxy_url 必须为空，实际 %q", prof.ProxyURL)
	}

	dev, err := QoderDeviceForEmail(" Mixed@Case.COM ")
	if err != nil {
		t.Fatalf("QoderDeviceForEmail: %v", err)
	}
	real := QoderDeviceInfo()
	if real == nil {
		t.Fatal("QoderDeviceInfo() 返回 nil")
	}
	if !reflect.DeepEqual(dev, real) {
		t.Fatalf("无配置时必须与 QoderDeviceInfo 完全一致\n derived/实际: %+v\n 真实设备:     %+v", dev, real)
	}
	if dev.Derived || dev.Synthetic || dev.ProxyURL != "" {
		t.Fatalf("默认设备不应带派生/代理标记: derived=%v synthetic=%v proxy=%q",
			dev.Derived, dev.Synthetic, dev.ProxyURL)
	}
	// 配置文件必须仍然不存在（读默认值不能顺手创建文件）。
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("读取默认配置不应创建文件 %s（err=%v）", path, err)
	}
}

// 验收②：derived 同邮箱稳定、不同邮箱不同，且 Token/Code 必须真实发送。
func TestQoderDeviceForEmailDerivedStable(t *testing.T) {
	useTempEgressFile(t)

	dir := filepath.Join(t.TempDir(), "profile-a")
	prof, err := SetQoderEgress("Alice@Example.com", QoderDeviceModeDerived, "", dir)
	if err != nil {
		t.Fatalf("SetQoderEgress: %v", err)
	}
	if prof.Email != "alice@example.com" {
		t.Fatalf("邮箱键必须小写，实际 %q", prof.Email)
	}

	a1, err := QoderDeviceForEmail("alice@example.com")
	if err != nil {
		t.Fatalf("QoderDeviceForEmail(alice): %v", err)
	}
	a2, err := QoderDeviceForEmail(" ALICE@EXAMPLE.COM ")
	if err != nil {
		t.Fatalf("QoderDeviceForEmail(ALICE): %v", err)
	}
	if !reflect.DeepEqual(a1, a2) {
		t.Fatalf("同一邮箱两次派生必须完全一致\n a1=%+v\n a2=%+v", a1, a2)
	}
	if !a1.Derived {
		t.Error("派生设备必须 Derived=true")
	}
	if a1.Synthetic {
		t.Error("派生设备必须 Synthetic=false（否则不会发送 Token/Code）")
	}
	if a1.MachineID == "" || a1.MachineToken == "" || a1.MachineCode == "" || a1.MachineHostname == "" {
		t.Fatalf("派生字段不能为空: %+v", a1)
	}
	if len(a1.MachineID) != 36 || strings.Count(a1.MachineID, "-") != 4 {
		t.Errorf("MachineID 必须是 UUID 形态，实际 %q", a1.MachineID)
	}
	if len(a1.MachineCode) != 32 {
		t.Errorf("MachineCode 必须是 32 位 hex，实际 %q（%d 位）", a1.MachineCode, len(a1.MachineCode))
	}
	if !strings.HasPrefix(a1.MachineHostname, "DESKTOP-") || len(a1.MachineHostname) != len("DESKTOP-")+7 {
		t.Errorf("MachineHostname 形态错误: %q", a1.MachineHostname)
	}

	// Token/Code 必须真的出现在请求头里（否则上游不下发活动）。
	h := http.Header{}
	a1.Apply(h)
	if h.Get("Cosy-MachineToken") == "" || h.Get("Cosy-MachineCode") == "" {
		t.Error("派生设备必须发送 Cosy-MachineToken / Cosy-MachineCode")
	}

	// 不同邮箱必须派生不同设备。
	if _, err := SetQoderEgress("bob@example.com", QoderDeviceModeDerived, "", ""); err != nil {
		t.Fatalf("SetQoderEgress(bob): %v", err)
	}
	b, err := QoderDeviceForEmail("bob@example.com")
	if err != nil {
		t.Fatalf("QoderDeviceForEmail(bob): %v", err)
	}
	if a1.MachineID == b.MachineID {
		t.Error("不同邮箱的 MachineID 不能相同")
	}
	if a1.MachineToken == b.MachineToken {
		t.Error("不同邮箱的 MachineToken 不能相同")
	}
	if a1.MachineCode == b.MachineCode {
		t.Error("不同邮箱的 MachineCode 不能相同")
	}
	if a1.MachineHostname == b.MachineHostname {
		t.Error("不同邮箱的 MachineHostname 不能相同")
	}

	// 其余字段复制真实设备。
	real := QoderDeviceInfo()
	if real == nil {
		t.Fatal("QoderDeviceInfo() 返回 nil")
	}
	if a1.ClientType != real.ClientType || a1.MachineOS != real.MachineOS ||
		a1.MachineType != real.MachineType || a1.Version != real.Version {
		t.Fatalf("派生设备必须复制真实设备的公共字段\n derived=%+v\n real=%+v", a1, real)
	}
}

// 验收③：proxy_url 为空 / http(s) / socks5 三态；校验失败不落盘。
func TestSetQoderEgressValidation(t *testing.T) {
	path := useTempEgressFile(t)

	bad := []struct {
		name     string
		mode     string
		proxyURL string
		want     error
	}{
		{"空 mode", "", "http://127.0.0.1:1080", ErrInvalidDeviceMode},
		{"非法 mode", "synthetic", "", ErrInvalidDeviceMode},
		{"socks5", QoderDeviceModeReal, "socks5://127.0.0.1:1080", ErrInvalidProxyURL},
		{"socks5h", QoderDeviceModeReal, "socks5h://127.0.0.1:1080", ErrInvalidProxyURL},
		{"内部空格", QoderDeviceModeReal, "http://127.0.0.1 :1080", ErrInvalidProxyURL},
		{"缺端口", QoderDeviceModeReal, "http://127.0.0.1", ErrInvalidProxyURL},
		{"缺协议", QoderDeviceModeReal, "127.0.0.1:1080", ErrInvalidProxyURL},
		{"无法解析", QoderDeviceModeReal, "http://%zz", ErrInvalidProxyURL},
	}
	for _, c := range bad {
		_, err := SetQoderEgress("a@example.com", c.mode, c.proxyURL, "")
		if !errors.Is(err, c.want) {
			t.Errorf("%s: 错误应为 %v，实际 %v", c.name, c.want, err)
		}
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("%s: 校验失败却落盘了（stat err=%v）", c.name, statErr)
		}
	}

	// 三态中的「空」与「http/https」必须能落盘并被读回。
	good := []struct {
		proxyURL string
		want     string
	}{
		{"", ""},
		{"http://127.0.0.1:1080", "http://127.0.0.1:1080"},
		{"https://user:p%40ss@127.0.0.1:1080", "https://user:p%40ss@127.0.0.1:1080"},
	}
	for _, c := range good {
		prof, err := SetQoderEgress("ok@example.com", QoderDeviceModeReal, c.proxyURL, "")
		if err != nil {
			t.Fatalf("proxy=%q 应被接受: %v", c.proxyURL, err)
		}
		if prof.ProxyURL != c.want {
			t.Errorf("proxy 落盘值 = %q，期望 %q", prof.ProxyURL, c.want)
		}
		if got := QoderEgressFor("OK@Example.COM"); got.ProxyURL != c.want {
			t.Errorf("读回 proxy = %q，期望 %q", got.ProxyURL, c.want)
		}
	}

	// 已有配置时，一次非法写入不得改动文件内容。
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置文件: %v", err)
	}
	if _, err := SetQoderEgress("ok@example.com", QoderDeviceModeDerived, "socks5://1.2.3.4:1080", ""); err == nil {
		t.Fatal("socks5 应当被拒绝")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置文件: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("校验失败的写入不应改变配置文件")
	}

	// 配置文件权限必须 0600（Windows 不做权限位断言）。
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("配置文件权限 = %o，期望 600", perm)
		}
	}
}

// 验收③：client 三态 —— 空走默认 transport，http(s) 走 ProxyURL，socks5 报错。
func TestQoderDeviceNewHTTPClient(t *testing.T) {
	// 空 ProxyURL：与 http.DefaultTransport 克隆一致（Proxy 函数相同）。
	c, err := (&QoderDevice{}).NewHTTPClient(5 * time.Second)
	if err != nil {
		t.Fatalf("空 ProxyURL 不应报错: %v", err)
	}
	if c.Timeout != 5*time.Second {
		t.Errorf("timeout = %v，期望 5s", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T，期望 *http.Transport", c.Transport)
	}
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport 不是 *http.Transport")
	}
	if reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(def.Proxy).Pointer() {
		t.Error("空 ProxyURL 必须保留默认 transport 的代理函数")
	}

	// http 代理：Proxy 返回配置的地址（不再受环境变量影响）。
	for _, raw := range []string{"http://127.0.0.1:1080", "https://user:pass@127.0.0.1:1080"} {
		dev := &QoderDevice{ProxyURL: raw}
		c2, err := dev.NewHTTPClient(time.Second)
		if err != nil {
			t.Fatalf("proxy=%q 不应报错: %v", raw, err)
		}
		req, _ := http.NewRequest(http.MethodGet, "https://openapi.qoder.sh/sash/api/v1/me/campaigns", nil)
		u, err := c2.Transport.(*http.Transport).Proxy(req)
		if err != nil {
			t.Fatalf("proxy=%q 解析失败: %v", raw, err)
		}
		if u == nil {
			t.Fatalf("proxy=%q 未生效（Proxy 返回 nil）", raw)
		}
		want, _ := parseQoderProxyURL(raw)
		if u.String() != want.String() {
			t.Errorf("代理地址 = %q，期望 %q", u.String(), want.String())
		}
	}

	// socks5：明确报错，不静默直连。
	_, err = (&QoderDevice{ProxyURL: "socks5://127.0.0.1:1080"}).NewHTTPClient(time.Second)
	if !errors.Is(err, ErrInvalidProxyURL) {
		t.Fatalf("socks5 必须返回 ErrInvalidProxyURL，实际 %v", err)
	}
}

// QoderDo 的代理配置错误必须立即返回（不进入 1.5s 的重试等待）。
func TestQoderDoInvalidProxyFailsFast(t *testing.T) {
	start := time.Now()
	_, _, err := QoderDo("https://openapi.qoder.sh", "/sash/api/v1/me/campaigns",
		http.MethodGet, "token", nil, &QoderDevice{ProxyURL: "socks5://127.0.0.1:1080"})
	if !errors.Is(err, ErrInvalidProxyURL) {
		t.Fatalf("期望 ErrInvalidProxyURL，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1200*time.Millisecond {
		t.Errorf("代理配置错误不应重试（耗时 %v）", elapsed)
	}
}

// QoderDoAny（契约 @1 澄清新增）：按候选 base 尝试，返回命中 base，并透传设备头与代理。
func TestQoderDoAnyReturnsHitBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cosy-MachineId"); got == "" {
			t.Errorf("请求必须带设备头 Cosy-MachineId")
		}
		if got := r.Header.Get("Cosy-MachineToken"); got == "" {
			t.Errorf("请求必须带设备头 Cosy-MachineToken")
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"token invalid"}`))
	}))
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	dev := &QoderDevice{MachineID: "mid-1", MachineToken: "tok-1", MachineCode: "code-1"}
	status, raw, base, err := QoderDoAny("qoder:prod:global:x", "/sash/api/v1/me/campaigns",
		http.MethodGet, "token", nil, dev)
	if err != nil {
		t.Fatalf("QoderDoAny: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d，期望 401（HTTP 响应即命中，不能被当成网络失败）", status)
	}
	if base != srv.URL {
		t.Errorf("base = %q，期望 %q", base, srv.URL)
	}
	if len(raw) == 0 {
		t.Error("raw body 不能为空")
	}
}

// QoderDoAny 遇到代理配置错误必须立即失败（不遍历 base、不重试）。
func TestQoderDoAnyInvalidProxyFailsFast(t *testing.T) {
	start := time.Now()
	_, _, _, err := QoderDoAny("", "/sash/api/v1/me/campaigns", http.MethodGet, "token", nil,
		&QoderDevice{ProxyURL: "socks5://127.0.0.1:1080"})
	if !errors.Is(err, ErrInvalidProxyURL) {
		t.Fatalf("期望 ErrInvalidProxyURL，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1200*time.Millisecond {
		t.Errorf("代理配置错误不应重试（耗时 %v）", elapsed)
	}
}

// 验收⑤（纯函数部分）：Chrome 启动参数对配置了代理的账号追加 --proxy-server，
// 且 profile 目录保持调用方传入的路径（与 qoderWebProfileDir 一致）。
func TestQoderWebChromeArgsProxy(t *testing.T) {
	dir := filepath.Join(string(filepath.Separator), "tmp", "qoder-web", "ab12cd34ef56")
	startURL := "https://qoder.com/account/usage"

	noProxy := QoderWebChromeArgs(9222, dir, "", startURL)
	for _, a := range noProxy {
		if strings.HasPrefix(a, "--proxy-server") {
			t.Fatalf("未配置代理时不应出现 --proxy-server，实际参数 %v", noProxy)
		}
	}
	if len(noProxy) < 2 || noProxy[1] != "--user-data-dir="+dir {
		t.Fatalf("--user-data-dir 必须等于传入的 profile 目录: %v", noProxy)
	}
	if noProxy[len(noProxy)-1] != startURL {
		t.Fatalf("最后一个参数必须是起始 URL: %v", noProxy)
	}

	withProxy := QoderWebChromeArgs(9222, dir, "http://127.0.0.1:1080", startURL)
	found := false
	proxyIdx := -1
	for i, a := range withProxy {
		if a == "--proxy-server=http://127.0.0.1:1080" {
			found = true
			proxyIdx = i
		}
	}
	if !found {
		t.Fatalf("配置代理后必须追加 --proxy-server: %v", withProxy)
	}
	if withProxy[len(withProxy)-1] != startURL || proxyIdx >= len(withProxy)-1 {
		t.Fatalf("--proxy-server 必须在起始 URL 之前: %v", withProxy)
	}
	if withProxy[1] != "--user-data-dir="+dir {
		t.Fatalf("配置代理不得改变 profile 目录: %v", withProxy)
	}
}
