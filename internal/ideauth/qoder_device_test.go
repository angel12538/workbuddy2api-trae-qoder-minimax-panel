package ideauth

import (
	"net/http"
	"strings"
	"testing"
)

// 回归（2026-10-24 实测修正）：Qoder 的每日签到活动**只对真实设备指纹下发**。
// 旧策略（每账号派生合成 MachineId、不发送 MachineToken）导致活动列表里永远
// 没有签到活动 —— 表现为"扫描不到签到活动"。正确策略：全部账号使用本机真实
// runtime-info 指纹（machineToken/machineCode/machineId 三件套）；每设备每日
// 限领一个账号是服务端硬规则，由扫描结果如实说明。
func TestQoderDeviceRealFingerprint(t *testing.T) {
	a := &QoderAuth{User: QoderUser{ID: "uid-a", Email: "a@test.com"}}
	b := &QoderAuth{User: QoderUser{ID: "uid-b", Email: "b@test.com"}}

	da := qoderDeviceForAccount(a)
	db := qoderDeviceForAccount(b)
	if da == nil || db == nil {
		t.Fatal("设备对象为空")
	}
	if da.Synthetic {
		t.Error("设备必须按真实指纹发送（Synthetic=false）")
	}
	// 请求头必须带物理设备令牌三件套（活动下发的必要条件）。
	h := http.Header{}
	da.Apply(h)
	if h.Get("Cosy-MachineToken") == "" || h.Get("Cosy-MachineCode") == "" {
		t.Errorf("请求头必须包含物理设备令牌: token=%q code=%q",
			h.Get("Cosy-MachineToken"), h.Get("Cosy-MachineCode"))
	}
	if h.Get("Cosy-MachineId") == "" {
		t.Error("Cosy-MachineId 不能为空")
	}
	// 真实设备对所有账号一致（每设备每日限领一次由服务端判定）。
	if da.MachineID != db.MachineID {
		t.Errorf("所有账号应使用同一真实设备指纹: %s != %s", da.MachineID, db.MachineID)
	}
	if !strings.HasPrefix(da.MachineID, "") && len(da.MachineID) == 0 {
		t.Error("machineId 为空")
	}
}
