package ideauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorkBuddy 客户端的「真实登录会话文件」。
//
// 客户端（CodeBuddy/WorkBuddy 桌面版）把当前登录态写在：
//
//	%LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\<id>.info
//
// 其中 <id> 由产品决定：国内版 workbuddy-desktop，国际版 workbuddy-desktop-ai。
// 文件是明文 JSON（{account, accounts, auth, allAccounts}），其中 token 字段用
// keyblob 的 envelope 加密，但 envelope 里的 keyId 对所有账号相同（同一把 keyblob
// 主密钥），因此**整份文件可直接跨账号备份与恢复** —— 这正是「真实切号」的依据
// （此前的实现只写了 account-snapshot.json 标记，daemon 并不据此换会话）。

// workBuddyAuthDir 返回 WorkBuddy 登录会话文件所在目录。
func workBuddyAuthDir() string {
	la := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if la == "" {
		home, _ := os.UserHomeDir()
		la = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(la, "CodeBuddyExtension", "Data", "Public", "auth")
}

// workBuddyAuthFileUID 读取一个 .info 会话文件里的 account.uid。
func workBuddyAuthFileUID(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var v struct {
		Account struct {
			UID string `json:"uid"`
		} `json:"account"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return strings.TrimSpace(v.Account.UID)
}

// workBuddyIsPrimaryAuthFile 判断是否为主会话文件（如 workbuddy-desktop.info），
// 而非 <id>.<时间戳>.<pid>.<uuid>.info 形式的历史备份。
func workBuddyIsPrimaryAuthFile(name string) bool {
	return strings.HasSuffix(name, ".info") && strings.Count(name, ".") == 1
}

// WorkBuddyAuthFileForUID 在 auth 目录里找 account.uid == uid 的会话文件路径。
// 优先返回主会话文件，其次才是带时间戳的备份文件。
func WorkBuddyAuthFileForUID(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return ""
	}
	entries, err := os.ReadDir(workBuddyAuthDir())
	if err != nil {
		return ""
	}
	fallback := ""
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".info") || strings.Contains(name, ".observation.") {
			continue
		}
		p := filepath.Join(workBuddyAuthDir(), name)
		if workBuddyAuthFileUID(p) != uid {
			continue
		}
		if workBuddyIsPrimaryAuthFile(name) {
			return p
		}
		if fallback == "" {
			fallback = p
		}
	}
	return fallback
}

// ListWorkBuddyAuthSessions 返回 auth 目录里的 (uid, path) 列表。
func ListWorkBuddyAuthSessions() map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(workBuddyAuthDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".info") || strings.Contains(name, ".observation.") {
			continue
		}
		p := filepath.Join(workBuddyAuthDir(), name)
		if uid := workBuddyAuthFileUID(p); uid != "" {
			out[uid] = p
		}
	}
	return out
}

// captureWorkBuddyAuthFile 把 uid 对应的会话文件原文填入快照，
// 始终以当前主会话文件为准（token 会随客户端刷新而更新）。返回是否发生变化。
func captureWorkBuddyAuthFile(snap *WorkBuddySnapshot) bool {
	if snap == nil || snap.Account == nil {
		return false
	}
	p := WorkBuddyAuthFileForUID(snap.Account.UID)
	if p == "" {
		return false // 该账号当前未登录：保留已有原文
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	if snap.AuthFilePath == p && snap.AuthFileRaw == string(raw) {
		return false
	}
	snap.AuthFilePath = p
	snap.AuthFileRaw = string(raw)
	return true
}

// RestoreWorkBuddyAuthFile 把快照里的会话文件原文写回客户端读取的位置（真实切号）。
// 返回写入的路径。
func RestoreWorkBuddyAuthFile(snap *WorkBuddySnapshot) (string, error) {
	if snap == nil || snap.AuthFileRaw == "" {
		return "", fmt.Errorf("该账号尚未在本机客户端登录过，未记录登录会话，无法切号。请先在对应版本的 WorkBuddy 客户端登录一次该账号，面板会自动记录，之后即可一键切号")
	}
	p := snap.AuthFilePath
	if p == "" {
		if snap.Account == nil {
			return "", fmt.Errorf("快照缺少账号信息")
		}
		// 回退：用当前目录里同名文件，或按 uid 反查。
		p = WorkBuddyAuthFileForUID(snap.Account.UID)
	}
	if p == "" {
		// 目标文件从未存在过：按保存时文件名重建到 auth 目录。
		p = filepath.Join(workBuddyAuthDir(), "workbuddy-desktop.info")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err == nil {
		_ = backupFile(p)
	}
	if err := atomicWrite(p, []byte(snap.AuthFileRaw)); err != nil {
		return "", err
	}
	// 校验写回后的 uid 是否为目标账号。
	if got := workBuddyAuthFileUID(p); got != "" && snap.Account != nil && snap.Account.UID != "" && got != snap.Account.UID {
		return p, fmt.Errorf("会话文件写回校验失败：期望 %s，实际 %s", snap.Account.UID, got)
	}
	return p, nil
}
