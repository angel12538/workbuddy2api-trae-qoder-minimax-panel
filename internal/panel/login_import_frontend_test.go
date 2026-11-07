package panel

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestQoderDoubaoImportFrontend(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping login import frontend flow test")
	}
	script, err := filepath.Abs("../../scripts/test_login_import.cjs")
	if err != nil {
		t.Fatal(err)
	}
	panel, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, script, panel).CombinedOutput(); err != nil {
		t.Fatalf("login import frontend flow failed: %v\n%s", err, out)
	}
}
