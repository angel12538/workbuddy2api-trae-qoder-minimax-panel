package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

func TestQoderServerSwitchHandler(t *testing.T) {
	p := qoderTestPanel(t)
	t.Setenv("QODER_SERVER_MODE", "1")
	if err := ideauth.ImportQoderDeviceAuth(&ideauth.QoderAuth{Token: "fake-token", Realm: "cn", User: ideauth.QoderUser{ID: "server-user", Name: "Server"}}, "test-machine"); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	p.qoderSwitch(rr, httptest.NewRequest("POST", "/", strings.NewReader(`{"email":"qoder:cn:server-user","restart":true,"restore_machine_id":true}`)))
	var switched map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &switched); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || switched["switch_mode"] != "server" || switched["restarted"] != false {
		t.Fatalf("switch response: %s", rr.Body.String())
	}
	rr = httptest.NewRecorder()
	p.qoderAccounts(rr, httptest.NewRequest("GET", "/", nil))
	var list map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list["current_email"] != "qoder:cn:server-user" || list["switch_mode"] != "server" {
		t.Fatal("current account lost on reload")
	}
	if strings.Contains(rr.Body.String(), "fake-token") {
		t.Fatal("account endpoint exposed token")
	}
}

// Execute the real switch function: desktop keeps the view frozen while the
// server must refresh the selected account immediately.
func TestQoderServerFrontendSwitchBranches(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script, err := filepath.Abs("../../scripts/test_qoder_server.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, script).CombinedOutput(); err != nil {
		t.Fatalf("Qoder switch branches: %v\n%s", err, out)
	}
}
