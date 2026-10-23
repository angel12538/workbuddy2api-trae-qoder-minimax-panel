// Package appdir 统一解析「程序自己的目录」。
//
// 为什么需要：早期代码用 os.Getwd() 推导 auths/ 与 data/ide_accounts/，
// 这在「从项目目录运行」时没问题，但分发给别人后用户可能是双击 exe、
// 或用快捷方式启动（工作目录可能是 C:\Windows\System32），
// 于是号池会被写到一个莫名其妙的地方，表现为「账号全丢」。
// 这里统一改为以 exe 所在目录为基准，二进制放哪，数据就跟到哪。
package appdir

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	once sync.Once
	dir  string
)

// Dir 返回程序所在目录（exe 同级）。
// go run / go test 场景下 exe 落在临时构建目录里，此时回落当前工作目录。
func Dir() string {
	once.Do(func() {
		if p, err := os.Executable(); err == nil {
			if real, lerr := filepath.EvalSymlinks(p); lerr == nil {
				p = real
			}
			d := filepath.Dir(p)
			if d != "" && !isTempBuildDir(d) {
				dir = d
				return
			}
		}
		wd, err := os.Getwd()
		if err != nil || wd == "" {
			wd = "."
		}
		dir = wd
	})
	return dir
}

// isTempBuildDir 判断是否 go run / go test 的临时构建目录。
func isTempBuildDir(d string) bool {
	lower := strings.ToLower(filepath.Clean(d))
	if strings.Contains(lower, "go-build") {
		return true
	}
	if t := os.TempDir(); t != "" {
		if strings.HasPrefix(lower, strings.ToLower(filepath.Clean(t))) {
			return true
		}
	}
	return false
}

// Resolve 把相对路径按程序目录解析成绝对路径；绝对路径与空串原样返回。
func Resolve(p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(Dir(), p)
}

// Chdir 把当前工作目录切到程序目录。
//
// 服务内部仍有少量相对路径（config 里的 ./auths、./data/state.json 等），
// 启动时统一对齐到 exe 目录，可以一次性兜住这些历史写法。
func Chdir() error {
	return os.Chdir(Dir())
}
