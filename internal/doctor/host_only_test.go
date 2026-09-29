package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHostSetupChecksDoNotInspectUnrelatedDeploymentState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte("broken state"), 0600); err != nil {
		t.Fatal(err)
	}
	// Missing executables short-circuit expensive host probes without consulting
	// the real network; dependency failures still remain visible to setup.
	probe := Probe{HostOnly: true, LookPath: func(name string) (string, error) { return "", &exec.Error{Name: name, Err: exec.ErrNotFound} }}
	report := probe.Run(context.Background())
	if !report.HasErrors() {
		t.Fatal("host dependency failures were hidden")
	}
	for _, check := range report.Checks {
		if check.Name == "deployment" || check.Name == "data-root" || check.Class == ClassNetwork {
			t.Fatalf("unrelated check blocked host setup: %+v", check)
		}
	}
	probe.HostOnly = false
	// The full doctor still diagnoses the invalid state.
	for _, check := range probe.deploymentChecks() {
		if check.Name == "deployment" && check.Status == Error {
			return
		}
	}
	t.Fatal("full doctor lost deployment diagnostics")
}
