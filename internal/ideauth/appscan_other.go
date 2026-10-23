//go:build !windows

// appscan_other.go 非 Windows 平台的空实现（本项目主要面向 Windows，
// 保留这个文件是为了让 go vet / 跨平台编译不报缺失符号）。
package ideauth

import "time"

func discoverClientExe(imageNames []string) string { return "" }

func scanDrivesForExe(imageNames []string, maxDepth int, budget time.Duration) string { return "" }

// ResetClientScanCache 清空发现缓存（非 Windows 无缓存，留空实现）。
func ResetClientScanCache() {}
