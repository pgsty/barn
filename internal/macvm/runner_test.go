package macvm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/failure"
)

func fakeRunnerScript(t *testing.T, output string) Runner {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat <<'EOF'\n"+output+"\nEOF\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return Runner{Binary: path}
}

func TestRunnerProtocolMismatchIsACapabilityError(t *testing.T) {
	err := fakeRunnerScript(t, `{"ok":true,"protocol_version":1,"version":"0.9.0"}`).CheckProtocol(context.Background())
	class, reason, _ := failure.Classify(err)
	if class != failure.Capability || reason != "mac_runner_protocol" || !strings.Contains(err.Error(), "0.9.0") {
		t.Fatalf("err=%v class=%s reason=%s", err, class, reason)
	}
	if err := fakeRunnerScript(t, `{"ok":true,"protocol_version":2}`).CheckProtocol(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchFailureSurfacesTheVMLimit(t *testing.T) {
	log := filepath.Join(t.TempDir(), "runner.log")
	content := `{"phase":"starting"}
{"ok":false,"error":{"code":"virtual_machine_limit","message":"macOS allows two macOS virtual machines at a time"}}
`
	if err := os.WriteFile(log, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	err := launchFailure(log, nil)
	if class, reason, _ := failure.Classify(err); class != failure.Resource || reason != "mac_vm_limit" {
		t.Fatalf("err=%v", err)
	}
	if err := os.WriteFile(log, []byte(`{"ok":false,"error":{"code":"vmnet_network","message":"Cannot create"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := launchFailure(log, nil); err == nil || !strings.Contains(err.Error(), "vmnet_network") {
		t.Fatalf("err=%v", err)
	}
}

func TestRPCRejectsAnotherInstance(t *testing.T) {
	requireMac(t)
	m, _ := testManager(t)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	startFakeRuntime(t, m, machine)
	socket, _ := m.socket("dev", false)
	if _, err := m.Runner.RPC(context.Background(), socket, "00000000-0000-0000-0000-000000000000", "status", false); err == nil {
		t.Fatal("accepted another instance's reply")
	}
	status, err := m.Runner.RPC(context.Background(), socket, machine.InstanceID, "status", false)
	if err != nil || status.State != "running" {
		t.Fatalf("status %+v %v", status, err)
	}
}
