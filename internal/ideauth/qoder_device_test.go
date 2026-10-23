package ideauth

import (
	"net/http"
	"testing"
)

// 回归：Qoder 活动按设备判定，同一设备只能签一个号。
// 因此每个账号必须拿到**独立且稳定**的 Cosy-MachineId，且不能带物理设备的 MachineToken。
func TestQoderDevicePerAccount(t *testing.T) {
	a := &QoderAuth{User: QoderUser{ID: "uid-a", Email: "a@test.com"}}
	b := &QoderAuth{User: QoderUser{ID: "uid-b", Email: "b@test.com"}}

	da := qoderDeviceForAccount(a)
	db := qoderDeviceForAccount(b)
	if da == nil || db == nil {
		t.Fatal("设备对象为空")
	}
	if da.MachineID == "" || len(da.MachineID) != 36 {
		t.Fatalf("machineId 应是不带横杠的 UUID（36 位），得到 %q", da.MachineID)
	}
	if da.MachineID == db.MachineID {
		t.Fatalf("两个账号不能共用同一个 machineId（否则第二个号会被判为已签）")
	}
	if !da.Synthetic {
		t.Error("派生设备必须标记 Synthetic（决定是否发送物理设备令牌）")
	}
	// 关键：绝不能把物理设备的 MachineToken/MachineCode 发出去，否则又串回设备维度。
	h := http.Header{}
	da.Apply(h)
	if h.Get("Cosy-MachineToken") != "" || h.Get("Cosy-MachineCode") != "" {
		t.Errorf("请求头不应包含物理设备令牌: token=%q code=%q",
			h.Get("Cosy-MachineToken"), h.Get("Cosy-MachineCode"))
	}
	if h.Get("Cosy-MachineId") != da.MachineID {
		t.Errorf("Cosy-MachineId 未按账号派生写入: %q", h.Get("Cosy-MachineId"))
	}
	// 同一账号多次派生必须一致（否则每次签到都是"新设备"，风控上也不合理）
	if again := qoderDeviceForAccount(a); again.MachineID != da.MachineID {
		t.Errorf("同一账号的设备标识必须稳定: %s != %s", again.MachineID, da.MachineID)
	}
}
