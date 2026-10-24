package panel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// 回归：「退出当前账号」不能把登录态文件写成占位符。
//
// 2026-10-24 实测踩坑：往 auth.v1.dat 写了 3 字节的 "v10"，结果
//   解密 auth.v1.dat 失败: body too short: 0
// 面板读不了、客户端也读不了，用户连着点了 5 次、每次再备份一份垃圾。
//
// 正确做法：**删掉登录态文件**（= 全新安装状态，客户端启动自然回到登录界面），
// 并且报错要说人话（"未登录 / 已损坏，请重新登录"），不能把
// "body too short: 0" 直接甩给用户。
func TestQoderLogoutKeepsAuthFileReadable(t *testing.T) {
	dir := t.TempDir()
	// 造一个**有效**的登录态（走真加密），代表"当前已登录"。
	t.Setenv("QODER_DATA_DIR", dir)
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	// Local State 里是 DPAPI 保护的密钥 —— DPAPI 按用户解密、与路径无关，
	// 所以复制到临时目录照样能解（密钥不出本机，临时目录随测试一起销毁）。
	realLocalState := filepath.Join(os.Getenv("APPDATA"), "com.qoder.app.stable", "Local State")
	if raw, err := os.ReadFile(realLocalState); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "Local State"), raw, 0o600); err != nil {
			t.Fatalf("准备 Local State 失败: %v", err)
		}
	} else {
		t.Skipf("本机没有 Qoder Local State（%v），跳过", err)
	}
	if err := ideauth.WriteQoderAuth(&ideauth.QoderAuth{
		SchemaVersion: 1,
		User:          ideauth.QoderUser{ID: "u1", Email: "a@x.com"},
	}); err != nil {
		t.Fatalf("准备登录态失败: %v", err)
	}
	authPath := filepath.Join(dir, "auth.v1.dat")
	if fi, err := os.Stat(authPath); err != nil || fi.Size() < 32 {
		t.Fatalf("登录态文件没写出来: %v", err)
	}

	// 退出
	if _, err := qoderClearAuthFile(dir); err != nil {
		t.Fatalf("退出当前账号失败: %v", err)
	}

	// 1) 文件必须没了（而不是变成一个解不开的占位符）
	if _, err := os.Stat(authPath); !os.IsNotExist(err) {
		t.Errorf("退出后应删除 auth.v1.dat（留下占位符会导致 body too short）")
	}
	// 2) 读登录态必须给出**人话**错误，而不是 "body too short: 0"
	_, err := ideauth.ReadQoderAuth()
	if err == nil {
		t.Fatal("退出后不应还能读到登录态")
	}
	if strings.Contains(err.Error(), "body too short") {
		t.Errorf("报错不能是原始的解密错误：%v", err)
	}
	if !strings.Contains(err.Error(), "未登录") && !strings.Contains(err.Error(), "损坏") {
		t.Errorf("报错要说人话（未登录 / 已损坏 / 请重新登录），实际：%v", err)
	}
}
