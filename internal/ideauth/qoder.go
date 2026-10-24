package ideauth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var base64Std = base64.StdEncoding

// QoderAuth 对应 auth.v1.dat 解密后的结构。
type QoderAuth struct {
	SchemaVersion          int        `json:"schemaVersion"`
	Token                  string     `json:"token"`
	RefreshToken           string     `json:"refreshToken"`
	ExpiresAt              string     `json:"expiresAt"`
	RefreshTokenExpiresAt  string     `json:"refreshTokenExpiresAt"`
	User                   QoderUser  `json:"user"`
}

// QoderUser 用户信息。
type QoderUser struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Phone     any    `json:"phone"`
	AvatarURL string `json:"avatarUrl"`
}

const (
	qoderAuthFile     = "auth.v1.dat"
	qoderOverlayFile  = "auth-profile-overlays.v1.dat"
	qoderMachineID    = "auth.machine-id"
	qoderMagic        = "v10"
)

// ReadQoderAuth 解密并返回当前 Qoder 登录账号。
func ReadQoderAuth() (*QoderAuth, error) {
	key, err := QoderOSCryptKey()
	if err != nil {
		return nil, err
	}
	p := filepath.Join(QoderDataDir(), qoderAuthFile)
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("Qoder 未登录（%s 不存在）", qoderAuthFile)
		}
		return nil, fmt.Errorf("读取 %s 失败: %w", qoderAuthFile, err)
	}
	if len(raw) < 3 || string(raw[:3]) != qoderMagic {
		// 常见成因：面板的"退出当前账号"写了占位符、或文件被截断。
		// 这里要说人话，别把 "body too short: 0" 直接甩给用户。
		return nil, fmt.Errorf("%s 格式不对（只有 %d 字节）：Qoder 客户端未登录，或登录态已损坏 —— 请在客户端里重新登录一次", qoderAuthFile, len(raw))
	}
	plain, err := GCMDecryptIV12(key, raw[3:])
	if err != nil {
		return nil, fmt.Errorf("解密 %s 失败（%d 字节，可能已损坏）: %w", qoderAuthFile, len(raw), err)
	}
	var a QoderAuth
	if err := json.Unmarshal(plain, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// WriteQoderAuth 用 raw 的 JSON 内容重新加密写回 auth.v1.dat。
// 会先备份原文件（.bak.<ts>）。
func WriteQoderAuth(a *QoderAuth) error {
	key, err := QoderOSCryptKey()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(a)
	if err != nil {
		return err
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return err
	}
	body, err := GCMEncryptIV12(key, plain, iv)
	if err != nil {
		return err
	}
	out := append([]byte(qoderMagic), body...)
	p := filepath.Join(QoderDataDir(), qoderAuthFile)
	if err := backupFile(p); err != nil {
		return err
	}
	return atomicWrite(p, out)
}

// QoderMachineID 读取 machine-id（明文 UUID）。
func QoderMachineID() (string, error) {
	b, err := os.ReadFile(filepath.Join(QoderDataDir(), qoderMachineID))
	if err != nil {
		return "", err
	}
	return string(trimSpace(b)), nil
}

// QoderDeviceID 从 qoder-data.v1.json 读取 deviceId。
func QoderDeviceID() (string, error) {
	b, err := os.ReadFile(filepath.Join(QoderDataDir(), "qoder-data.v1.json"))
	if err != nil {
		return "", err
	}
	return extractJSONString(b, "deviceId"), nil
}

// QoderScope 从 qoder-data.v1.json 读取 agentAccountScope（账号绑定标识）。
func QoderScope() (string, error) {
	b, err := os.ReadFile(filepath.Join(QoderDataDir(), "qoder-data.v1.json"))
	if err != nil {
		return "", err
	}
	return extractJSONString(b, "agentAccountScope"), nil
}

// QoderSnapshot 是切号时保存的账号快照。
type QoderSnapshot struct {
	Email        string     `json:"email"`
	UserID       string     `json:"user_id"`
	Name         string     `json:"name"`
	Auth         *QoderAuth `json:"auth"`
	MachineID    string     `json:"machine_id"`
	DeviceID     string     `json:"device_id"`
	Scope        string     `json:"scope"`
	SavedAt      string     `json:"saved_at"`
	// ClaimCID 该账号今天那条 CLAIM_BENEFIT 活动的实例 id。
	//
	// 服务端的每日额度按**人**给，同一个人的多个账号共用一条活动实例（实测
	// 2026-10-24：另一个账号领过后，本号列表里就看不到这条活动，用该实例 id
	// 去领会得到 status=BLOCKED / failureCode=SAME_PERSON_ALREADY_CLAIMED）。
	// 记下实例 id，号池里其它号就能拿它去探一次，拿到服务端的真实判定。
	ClaimCID string `json:"claim_cid,omitempty"`
}

// CaptureQoderSnapshot 抓取当前登录态，用于保存到号池。
func CaptureQoderSnapshot() (*QoderSnapshot, error) {
	a, err := ReadQoderAuth()
	if err != nil {
		return nil, err
	}
	mid, _ := QoderMachineID()
	did, _ := QoderDeviceID()
	scope, _ := QoderScope()
	return &QoderSnapshot{
		Email:     a.User.Email,
		UserID:    a.User.ID,
		Name:      a.User.Name,
		Auth:      a,
		MachineID: mid,
		DeviceID:  did,
		Scope:     scope,
		SavedAt:   time.Now().Format(time.RFC3339),
	}, nil
}

// RestoreQoderSnapshot 把快照写回 Qoder 数据目录（实现切号）。
// 会覆盖 auth.v1.dat；machine-id / deviceId 可选恢复。
func RestoreQoderSnapshot(s *QoderSnapshot, restoreMachine bool) error {
	if s == nil || s.Auth == nil {
		return fmt.Errorf("快照为空")
	}
	if err := WriteQoderAuth(s.Auth); err != nil {
		return err
	}
	if restoreMachine && s.MachineID != "" {
		p := filepath.Join(QoderDataDir(), qoderMachineID)
		_ = backupFile(p)
		_ = atomicWrite(p, []byte(s.MachineID))
	}
	return nil
}

// ---------- 文件工具 ----------

func backupFile(p string) error {
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	bak := fmt.Sprintf("%s.bak.%d", p, time.Now().Unix())
	return atomicWrite(bak, b)
}

func atomicWrite(p string, data []byte) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
		end--
	}
	return b[start:end]
}
