//go:build windows

// appscan_windows.go 客户端安装位置自动发现（Windows）。
//
// 背景：早期只按写死的几个路径探测 IDE 客户端（Qoder / Trae / WorkBuddy），
// 一旦别人把软件装在 D 盘、或装到了 Program Files、或换了个产品名，
// 「切号」就会报「找不到客户端可执行文件」。
//
// 这里做四层递进探测，命中即用：
//  1. 注册表 App Paths（安装器普遍会写，最准）
//  2. 常见安装根目录下一层（LocalAppData\Programs、Program Files 等）
//  3. 卸载表里的 DisplayIcon / InstallLocation（能覆盖自定义安装盘）
//  4. 全盘扫描（仅前面全落空时执行，带时间预算与目录黑名单，结果缓存 10 分钟）
package ideauth

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// procGetDriveType 复用 win.go 里已声明的 kernel32（同一包内不重复声明）。
var procGetDriveType = kernel32.NewProc("GetDriveTypeW")

const driveFixed = 3 // DRIVE_FIXED

// scanCache 缓存扫描结果，避免每次切号都全盘扫一遍。
// 键为镜像名，值为发现路径（空串表示「扫过但没找到」，同样缓存以免反复扫）。
var scanCache sync.Map

// cachedScan 命中缓存则直接返回（含负缓存，10 分钟后过期）。
func cachedScan(key string, ttl time.Duration) (string, bool) {
	if v, ok := scanCache.Load(key); ok {
		e := v.(scanEntry)
		if time.Since(e.at) < ttl {
			return e.path, true
		}
	}
	return "", false
}

type scanEntry struct {
	path string
	at   time.Time
}

// registryAppPaths 从注册表 App Paths 取可执行文件完整路径。
// 同时查 HKCU / HKLM，并覆盖 64 位与 32 位视图（32 位安装器会写进 WOW6432Node）。
func registryAppPaths(imageNames []string) []string {
	out := make([]string, 0, len(imageNames))
	base := `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\`
	for _, img := range imageNames {
		sub := base + img
		for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
			for _, view := range []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY} {
				k, err := registry.OpenKey(root, sub, registry.QUERY_VALUE|view)
				if err != nil {
					continue
				}
				val, _, verr := k.GetStringValue("")
				_ = k.Close()
				if verr != nil {
					continue
				}
				val = strings.Trim(strings.TrimSpace(val), `"`)
				if val != "" {
					out = append(out, val)
				}
			}
		}
	}
	return out
}

// uninstallHints 从卸载表里捞安装目录线索（自定义安装盘的唯一可靠来源）。
func uninstallHints(imageNames []string) []string {
	roots := []struct {
		key  registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
	}
	out := make([]string, 0, 8)
	for _, r := range roots {
		k, err := registry.OpenKey(r.key, r.path, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			continue
		}
		names, err := k.ReadSubKeyNames(-1)
		_ = k.Close()
		if err != nil {
			continue
		}
		for _, n := range names {
			sk, err := registry.OpenKey(r.key, r.path+`\`+n, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			icon, _, _ := sk.GetStringValue("DisplayIcon")
			installLoc, _, _ := sk.GetStringValue("InstallLocation")
			_ = sk.Close()
			for _, img := range imageNames {
				// DisplayIcon 通常就是主程序：C:\X\Y\App.exe,0
				if icon != "" {
					ic := strings.Trim(strings.TrimSpace(strings.Split(icon, ",")[0]), `"`)
					if strings.EqualFold(filepath.Base(ic), img) {
						out = append(out, ic)
					}
				}
				if installLoc != "" {
					p := filepath.Join(strings.Trim(strings.TrimSpace(installLoc), `"`), img)
					if st, serr := os.Stat(p); serr == nil && !st.IsDir() {
						out = append(out, p)
					}
				}
			}
		}
	}
	return out
}

// commonInstallRoots 返回各用户/系统级的常见安装根目录，供「下一层扫描」使用。
func commonInstallRoots() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	add := func(p string) {
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		if seen[strings.ToLower(p)] {
			return
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			seen[strings.ToLower(p)] = true
			out = append(out, p)
		}
	}
	local := os.Getenv("LOCALAPPDATA")
	add(filepath.Join(local, "Programs"))
	add(local)
	add(os.Getenv("ProgramFiles"))
	add(os.Getenv("ProgramFiles(x86)"))
	add(os.Getenv("ProgramW6432"))
	add(os.Getenv("APPDATA"))
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
	}
	return out
}

// scanRootsOneLevel 在根目录「下一层」找同名可执行文件（App\App.exe）。
func scanRootsOneLevel(roots []string, imageNames []string) string {
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			for _, img := range imageNames {
				p := filepath.Join(root, e.Name(), img)
				if st, serr := os.Stat(p); serr == nil && !st.IsDir() {
					return p
				}
			}
		}
	}
	return ""
}

// skipScanDir 全盘扫描的目录黑名单：系统目录、缓存、构建产物不必进。
func skipScanDir(name string) bool {
	switch strings.ToLower(name) {
	case "windows", "$recycle.bin", "system volume information", "recovery",
		"perflogs", "msocache", "node_modules", ".git", "appdata",
		"windows.old", "programdata", "$windows.~ws", "$windows.~bt",
		"temp", "tmp", "cache", "logs", ".cache", "packages":
		return true
	}
	return false
}

// fixedDrives 返回本机固定磁盘根（跳过光驱/可移动盘/网络盘，避免卡顿）。
func fixedDrives() []string {
	var out []string
	for c := 'C'; c <= 'Z'; c++ {
		root := string(c) + ":\\"
		p, err := syscall.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		t, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(p)))
		if uint32(t) == driveFixed {
			out = append(out, root)
		}
	}
	return out
}

// scanDrivesForExe 全盘兜底扫描：限定层级与时间预算，命中即停。
//
// 只在前面所有层级都落空时才调用——大多数机器上根本走不到这里。
func scanDrivesForExe(imageNames []string, maxDepth int, budget time.Duration) string {
	deadline := time.Now().Add(budget)
	var found string

	for _, root := range fixedDrives() {
		if found != "" || time.Now().After(deadline) {
			break
		}
		type item struct {
			dir   string
			depth int
		}
		queue := []item{{dir: root, depth: 0}}
		for len(queue) > 0 {
			if time.Now().After(deadline) || found != "" {
				break
			}
			cur := queue[0]
			queue = queue[1:]
			entries, err := os.ReadDir(cur.dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				p := filepath.Join(cur.dir, e.Name())
				if e.IsDir() {
					if cur.depth+1 > maxDepth || skipScanDir(e.Name()) {
						continue
					}
					queue = append(queue, item{dir: p, depth: cur.depth + 1})
					continue
				}
				for _, img := range imageNames {
					if strings.EqualFold(e.Name(), img) {
						found = p
						break
					}
				}
				if found != "" {
					break
				}
			}
		}
	}
	return found
}

// discoverClientExe 四层递进探测，返回命中的可执行文件路径（找不到返回空串）。
func discoverClientExe(imageNames []string) string {
	if len(imageNames) == 0 {
		return ""
	}
	cacheKey := strings.ToLower(strings.Join(imageNames, "|"))
	if p, ok := cachedScan(cacheKey, 10*time.Minute); ok {
		return p
	}

	// 1) 注册表 App Paths
	for _, p := range registryAppPaths(imageNames) {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			scanCache.Store(cacheKey, scanEntry{path: p, at: time.Now()})
			return p
		}
	}

	roots := commonInstallRoots()

	// 2) 常见安装根目录下一层
	if p := scanRootsOneLevel(roots, imageNames); p != "" {
		scanCache.Store(cacheKey, scanEntry{path: p, at: time.Now()})
		return p
	}

	// 3) 卸载表线索
	for _, p := range uninstallHints(imageNames) {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			scanCache.Store(cacheKey, scanEntry{path: p, at: time.Now()})
			return p
		}
	}

	// 4) 全盘兜底（层级 4、预算 8 秒）
	if p := scanDrivesForExe(imageNames, 4, 8*time.Second); p != "" {
		scanCache.Store(cacheKey, scanEntry{path: p, at: time.Now()})
		return p
	}

	scanCache.Store(cacheKey, scanEntry{path: "", at: time.Now()})
	return ""
}

// ResetClientScanCache 清空发现缓存（面板「重新扫描客户端」用）。
func ResetClientScanCache() { scanCache = sync.Map{} }
