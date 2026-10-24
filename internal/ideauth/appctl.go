package ideauth

import (
	"bytes"
	"os"
	"strings"
	"time"
)

// 客户端进程控制：一键切号时「结束客户端 → 写入目标账号凭据 → 重新启动客户端」。
// 全部为 best-effort：找不到进程/可执行文件时返回描述而非致命错误，让切号本身仍能完成。

// clientApp 描述一个本地客户端进程。
type clientApp struct {
	Key        string
	ImageNames []string // taskkill /IM 用的进程名
	ExeGlobs   []string // 可执行文件候选（支持环境变量展开）
}

var clientApps = map[string]clientApp{
	"qoder": {
		Key:        "qoder",
		ImageNames: []string{"Qoder.exe", "Qoder CN.exe"},
		ExeGlobs: []string{
			`%LOCALAPPDATA%\Programs\Qoder\Qoder.exe`,
			`%LOCALAPPDATA%\Programs\Qoder CN\Qoder CN.exe`,
			`%PROGRAMFILES%\Qoder\Qoder.exe`,
		},
	},
	"trae": {
		Key:        "trae",
		ImageNames: []string{"Trae CN.exe", "Trae.exe", "TRAE SOLO CN.exe"},
		ExeGlobs: []string{
			`%LOCALAPPDATA%\Programs\Trae CN\Trae CN.exe`,
			`%LOCALAPPDATA%\Programs\Trae\Trae.exe`,
			`%LOCALAPPDATA%\Programs\TRAE SOLO CN\TRAE SOLO CN.exe`,
		},
	},
	"workbuddy": {
		Key:        "workbuddy",
		ImageNames: []string{"WorkBuddy.exe"},
		ExeGlobs: []string{
			`%PROGRAMFILES%\WorkBuddy\WorkBuddy.exe`,
			`%LOCALAPPDATA%\Programs\WorkBuddy\WorkBuddy.exe`,
		},
	},
	// 国际版客户端是独立程序（WorkBuddyAI.exe），登录态也写在独立的 auth 会话文件里，
	// 切号时必须单独结束/拉起它，否则 International 账号切了也不生效。
	// 注意：2026-10 版本更新后安装位置由 %USERPROFILE%\WorkBuddyAI 迁到
	// %PROGRAMFILES%\WorkBuddy-AI，两个位置都要探测（老版本仍在用户目录）。
	"workbuddyai": {
		Key:        "workbuddyai",
		ImageNames: []string{"WorkBuddyAI.exe"},
		ExeGlobs: []string{
			`%PROGRAMFILES%\WorkBuddy-AI\WorkBuddyAI.exe`,
			`%USERPROFILE%\WorkBuddyAI\WorkBuddyAI.exe`,
			`%LOCALAPPDATA%\Programs\WorkBuddyAI\WorkBuddyAI.exe`,
		},
	},
	// 豆包桌面客户端（Chromium 壳，Cookie 凭据在 %LOCALAPPDATA%\Doubao）。
	// 提取 Cookie 时库文件被独占锁定，必须先结束客户端。
	"doubao": {
		Key:        "doubao",
		ImageNames: []string{"Doubao.exe"},
		ExeGlobs: []string{
			`%LOCALAPPDATA%\Doubao\Application\Doubao.exe`,
		},
	},
}

// WorkBuddyClientKey 返回某版本 WorkBuddy 对应的客户端进程控制键。
func WorkBuddyClientKey(edition string) string {
	switch strings.TrimSpace(edition) {
	case string(WorkBuddyIntl), string(WorkBuddyIntlBuddy):
		return "workbuddyai"
	default:
		return "workbuddy"
	}
}

func expandEnvPath(p string) string {
	// Windows 的 %VAR% 语法 os.ExpandEnv 不认（它只处理 $VAR / ${VAR}），导致
	// clientApps 里所有 %LOCALAPPDATA%\... 形态的候选路径全部落空 → 掉进
	// discoverClientExe 的全盘兜底扫描（实测在豆包 LaunchClient 上卡死数分钟）。
	// 这里先把 %VAR% 展开成真实路径，再交给 os.ExpandEnv 兼容 $VAR 写法。
	out := percentExpand(p)
	return os.ExpandEnv(out)
}

// percentExpand 展开 Windows 风格的 %VAR% 环境变量引用（不认识的 % 段原样保留）。
func percentExpand(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '%')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		name := s[i+1 : i+1+j]
		b.WriteString(s[:i])
		if v, ok := os.LookupEnv(name); ok && name != "" {
			b.WriteString(v)
		} else {
			b.WriteString("%" + name + "%") // 未定义变量原样保留（与 cmd 语义一致）
		}
		s = s[i+1+j+1:]
	}
}

// findClientExe 在候选路径里找第一个存在的可执行文件。
//
// 查找顺序：写死的候选 → 自动发现（注册表 App Paths / 常见安装根目录 /
// 卸载表线索 / 全盘兜底扫描，见 appscan_windows.go）。
// 自动发现这一步是「换台电脑也能找到客户端」的关键：
// 别人可能把 IDE 装在 D 盘或用自定义目录，只靠候选路径必然落空。
func findClientExe(app clientApp) string {
	for _, g := range app.ExeGlobs {
		p := expandEnvPath(g)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return discoverClientExe(app.ImageNames)
}

// runningClientExe 用 PowerShell 取正在运行的客户端可执行文件路径（取不到返回空串）。
func runningClientExe(app clientApp) string {
	names := make([]string, 0, len(app.ImageNames))
	for _, n := range app.ImageNames {
		names = append(names, "'"+strings.TrimSuffix(n, ".exe")+"'")
	}
	script := "$p = Get-Process -Name " + strings.Join(names, ",") +
		" -ErrorAction SilentlyContinue | Where-Object { $_.Path } | Select-Object -First 1 -ExpandProperty Path; if ($p) { Write-Output $p }"
	out, err := runHidden("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ClientExePath 返回客户端可执行文件路径（优先正在运行的进程，其次安装目录探测）。
func ClientExePath(key string) string {
	app, ok := clientApps[key]
	if !ok {
		return ""
	}
	if p := runningClientExe(app); p != "" {
		return p
	}
	return findClientExe(app)
}

// clientRunning 判断客户端是否有同名进程在运行（tasklist 精确匹配镜像名）。
func clientRunning(app clientApp) bool {
	for _, n := range app.ImageNames {
		out, err := runHidden("tasklist", "/FI", "IMAGENAME eq "+n, "/NH")
		if err != nil {
			continue
		}
		if bytes.Contains(bytes.ToLower(out), bytes.ToLower([]byte(n))) {
			return true
		}
	}
	return false
}

// ClientRunning 报告客户端是否有进程在运行（豆包提取 Cookie 前后判断用）。
func ClientRunning(key string) bool {
	app, ok := clientApps[key]
	if !ok {
		return false
	}
	return clientRunning(app)
}

// KillClient 结束客户端进程（best-effort）。返回给用户看的描述。
//
// 关键点：必须等到进程「真正消失」才返回，否则客户端退出时会把内存里的旧凭据
// 再刷回磁盘，把我们刚写入的新登录态覆盖掉 —— 这正是之前「客户端重启了但没换号」的原因。
func KillClient(key string) string {
	app, ok := clientApps[key]
	if !ok {
		return "未知客户端"
	}
	if !clientRunning(app) {
		return "未检测到正在运行的进程"
	}
	// /T 连同子进程(渲染/工具进程)一起结束。
	for _, n := range app.ImageNames {
		_, _ = runHidden("taskkill", "/IM", n, "/T", "/F")
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !clientRunning(app) {
			time.Sleep(1200 * time.Millisecond) // 再等一拍，确保文件句柄/落盘彻底结束
			return "已结束客户端进程（已确认全部退出）"
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "已发送结束信号，但进程仍未退出，请手动关闭客户端后再切号"
}

// LaunchClient 重新启动客户端（best-effort）。
func LaunchClient(key string) string {
	exe := ClientExePath(key)
	if exe == "" {
		return "未找到客户端可执行文件，请手动启动"
	}
	// cmd /c start 以脱离本进程的方式拉起 GUI；失败也不阻塞。
	if _, err := runHidden("cmd", "/c", "start", "", exe); err != nil {
		return "启动失败：" + err.Error() + "（可手动启动 " + exe + "）"
	}
	return "已重新启动客户端"
}
