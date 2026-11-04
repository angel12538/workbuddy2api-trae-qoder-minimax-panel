package ideauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Qoder 每账号出口（设备模式 / 代理 / 浏览器 profile）
//
// 配置文件：<PoolDir>/qoder_egress.json（可用 QODER_EGRESS_FILE 覆盖），0600，原子写。
// 键一律使用「小写去空格」的邮箱，避免同一个人在号池里出现两种写法后各配各的。
//
// 默认行为（文件不存在 / 账号无配置）：device_mode=real、proxy_url 为空 —— 也就是
// 现网行为：真实设备指纹 + 进程默认网络。配置只在显式写入后生效，不改变成功路径。
// ---------------------------------------------------------------------------

// 设备模式枚举。契约 C-QD-EGRESS@1 只允许这两个值。
const (
	QoderDeviceModeReal    = "real"
	QoderDeviceModeDerived = "derived"
)

// 出口配置校验失败的原因（供面板/探测按错误码展示，消息里带具体细节）。
var (
	ErrInvalidDeviceMode = errors.New("invalid_device_mode")
	ErrInvalidProxyURL   = errors.New("invalid_proxy_url")
)

// QoderEgressProfile 单个账号的出口配置。
type QoderEgressProfile struct {
	Email      string `json:"email"`
	DeviceMode string `json:"device_mode"` // real|derived
	ProxyURL   string `json:"proxy_url,omitempty"`
	ProfileDir string `json:"profile_dir,omitempty"`
	UpdatedAt  string `json:"updated_at"`
}

// QoderEgressPool 出口配置文件结构（profiles 键为小写邮箱）。
type QoderEgressPool struct {
	Profiles map[string]QoderEgressProfile `json:"profiles"`
}

// qoderEgressMu 串行化出口配置的「读-改-写」，避免两个请求互相覆盖。
var qoderEgressMu sync.Mutex

// qoderEgressPath 返回出口配置文件路径。
func qoderEgressPath() string {
	if v := strings.TrimSpace(os.Getenv("QODER_EGRESS_FILE")); v != "" {
		return v
	}
	return filepath.Join(PoolDir(), "qoder_egress.json")
}

// normalizeQoderEmail 统一邮箱键：小写 + 去首尾空格。
func normalizeQoderEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// LoadQoderEgress 读取出口配置（文件不存在返回空池，不报错）。
func LoadQoderEgress() (*QoderEgressPool, error) {
	qoderEgressMu.Lock()
	defer qoderEgressMu.Unlock()
	return loadQoderEgressLocked()
}

// loadQoderEgressLocked 在已持锁的情况下读取（供 SetQoderEgress 复用）。
func loadQoderEgressLocked() (*QoderEgressPool, error) {
	p := qoderEgressPath()
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &QoderEgressPool{Profiles: map[string]QoderEgressProfile{}}, nil
		}
		return nil, fmt.Errorf("读取出口配置 %s 失败: %w", p, err)
	}
	var pool QoderEgressPool
	if len(strings.TrimSpace(string(raw))) == 0 {
		return &QoderEgressPool{Profiles: map[string]QoderEgressProfile{}}, nil
	}
	if err := json.Unmarshal(raw, &pool); err != nil {
		return nil, fmt.Errorf("解析出口配置 %s 失败: %w", p, err)
	}
	if pool.Profiles == nil {
		pool.Profiles = map[string]QoderEgressProfile{}
	}
	return &pool, nil
}

// SaveQoderEgress 原子写回出口配置。
//
// 键在写盘前统一规范化为小写去空格，并回填 profile.Email，保证读出来的键与
// 记录里的 email 一致（T6 面板会按 profile.Email 展示）。
func SaveQoderEgress(p *QoderEgressPool) error {
	qoderEgressMu.Lock()
	defer qoderEgressMu.Unlock()
	return saveQoderEgressLocked(p)
}

func saveQoderEgressLocked(p *QoderEgressPool) error {
	if p == nil {
		return fmt.Errorf("nil egress pool")
	}
	profiles := make(map[string]QoderEgressProfile, len(p.Profiles))
	for k, v := range p.Profiles {
		em := normalizeQoderEmail(v.Email)
		if em == "" {
			em = normalizeQoderEmail(k)
		}
		if em == "" {
			continue
		}
		v.Email = em
		if v.DeviceMode == "" {
			v.DeviceMode = QoderDeviceModeReal
		}
		profiles[em] = v
	}
	out := QoderEgressPool{Profiles: profiles}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	path := qoderEgressPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// atomicWrite 写临时文件用 0600 权限，rename 后仍是 0600。
	if err := atomicWrite(path, b); err != nil {
		return fmt.Errorf("写入出口配置 %s 失败: %w", path, err)
	}
	return nil
}

// QoderEgressFor 返回账号的出口配置；无配置返回 {DeviceMode:"real"}。
//
// 读文件失败（损坏/无权限）也按「未配置」处理：设备解析是签到前置步骤，
// 不能因为一个坏掉的配置文件把整个签到链路打死；需要严格报错的调用方用
// QoderDeviceForEmail。
func QoderEgressFor(email string) QoderEgressProfile {
	if pool, err := LoadQoderEgress(); err == nil {
		return qoderEgressProfileOf(pool, email)
	}
	return QoderEgressProfile{Email: normalizeQoderEmail(email), DeviceMode: QoderDeviceModeReal}
}

// qoderEgressProfileOf 从配置池里取账号配置（无配置返回默认 real）。
func qoderEgressProfileOf(pool *QoderEgressPool, email string) QoderEgressProfile {
	key := normalizeQoderEmail(email)
	def := QoderEgressProfile{Email: key, DeviceMode: QoderDeviceModeReal}
	if pool == nil {
		return def
	}
	prof, ok := pool.Profiles[key]
	if !ok {
		return def
	}
	if strings.TrimSpace(prof.Email) == "" {
		prof.Email = key
	}
	if strings.TrimSpace(prof.DeviceMode) == "" {
		prof.DeviceMode = QoderDeviceModeReal
	}
	return prof
}

// SetQoderEgress 写入/更新一个账号的出口配置。
//
// 校验失败返回错误且**不落盘**：mode 必须 ∈ {real,derived}；proxy_url 为空或
// http(s)://host:port（含空格、socks5://、无法解析一律拒绝）。
func SetQoderEgress(email, mode, proxyURL, profileDir string) (*QoderEgressProfile, error) {
	key := normalizeQoderEmail(email)
	if key == "" {
		return nil, fmt.Errorf("email 不能为空")
	}
	devMode, err := normalizeQoderDeviceMode(mode)
	if err != nil {
		return nil, err
	}
	proxy, err := normalizeQoderProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}

	qoderEgressMu.Lock()
	defer qoderEgressMu.Unlock()
	pool, err := loadQoderEgressLocked()
	if err != nil {
		return nil, err
	}
	prof := QoderEgressProfile{
		Email:      key,
		DeviceMode: devMode,
		ProxyURL:   proxy,
		ProfileDir: strings.TrimSpace(profileDir),
		UpdatedAt:  time.Now().Format(time.RFC3339),
	}
	pool.Profiles[key] = prof
	if err := saveQoderEgressLocked(pool); err != nil {
		return nil, err
	}
	out := prof
	return &out, nil
}

// normalizeQoderDeviceMode 校验并规范化设备模式。
func normalizeQoderDeviceMode(mode string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case QoderDeviceModeReal, QoderDeviceModeDerived:
		return m, nil
	default:
		return "", fmt.Errorf("%w: 只允许 %q 或 %q，收到 %q",
			ErrInvalidDeviceMode, QoderDeviceModeReal, QoderDeviceModeDerived, mode)
	}
}

// normalizeQoderProxyURL 校验并规范化代理地址。
//
// 只支持 http/https：socks5 需要额外依赖（golang.org/x/net/proxy），本任务明确
// 不引入；显式报错比「静默直连」安全 —— 静默直连会让账号从本机真实出口发请求，
// 用户却以为已经隔离。
func normalizeQoderProxyURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return "", fmt.Errorf("%w: 地址不能包含空白字符: %q", ErrInvalidProxyURL, raw)
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%w: 无法解析 %q: %v", ErrInvalidProxyURL, raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
	case "socks5", "socks5h", "socks4":
		return "", fmt.Errorf("%w: 暂不支持 %s 代理（未引入 socks 依赖），请使用 http(s)://host:port", ErrInvalidProxyURL, scheme)
	case "":
		return "", fmt.Errorf("%w: 缺少协议头，必须是 http(s)://host:port: %q", ErrInvalidProxyURL, raw)
	default:
		return "", fmt.Errorf("%w: 不支持的协议 %q（仅允许 http/https）", ErrInvalidProxyURL, u.Scheme)
	}
	if strings.TrimSpace(u.Hostname()) == "" {
		return "", fmt.Errorf("%w: 缺少主机名: %q", ErrInvalidProxyURL, raw)
	}
	if u.Port() == "" {
		return "", fmt.Errorf("%w: 缺少端口，必须是 http(s)://host:port: %q", ErrInvalidProxyURL, raw)
	}
	return s, nil
}

// parseQoderProxyURL 校验并解析代理地址（空地址返回 nil, nil）。
func parseQoderProxyURL(raw string) (*url.URL, error) {
	s, err := normalizeQoderProxyURL(raw)
	if err != nil {
		return nil, err
	}
	if s == "" {
		return nil, nil
	}
	return url.Parse(s)
}

// QoderWebChromeArgs 构造 Qoder 网页登录用的 Chrome 启动参数。
//
// 纯函数（不读环境变量、不访问文件系统），便于单元测试「配置了代理的账号
// 会追加 --proxy-server，且 profile 目录保持调用方传入的路径」。
func QoderWebChromeArgs(port int, profileDir, proxyURL, startURL string) []string {
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + profileDir,
		"--no-first-run", "--no-default-browser-check",
	}
	if proxy := strings.TrimSpace(proxyURL); proxy != "" {
		args = append(args, "--proxy-server="+proxy)
	}
	return append(args, startURL)
}
