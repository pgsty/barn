package vm

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/execx"
)

type refusedSSHRunner struct {
	calls  int
	stderr string
}

func (r *refusedSSHRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	r.calls++
	return execx.Result{Stderr: []byte(r.stderr)}, &execx.CommandError{ExitCode: 255, Stderr: r.stderr}
}

func TestPermanentSSHFailuresDoNotSpendReadinessDeadline(t *testing.T) {
	for _, message := range []string{"Host key verification failed.", "Permission denied (publickey).", "Bad configuration option: unknown"} {
		runner := &refusedSSHRunner{stderr: message}
		lifecycle := Lifecycle{Runner: runner, HostKeyAlias: HostKeyAlias("fixture-instance")}
		_, err := lifecycle.WaitReady(context.Background(), "ssh", "/key", "/known", 2222, ReadyMarker{}, time.Minute)
		if err == nil || runner.calls != 1 || !strings.Contains(err.Error(), message) {
			t.Fatalf("calls=%d err=%v", runner.calls, err)
		}
	}
}

func TestReadinessTimeoutPreservesSSHReasonAndParentCancellation(t *testing.T) {
	runner := &refusedSSHRunner{stderr: "Connection refused"}
	lifecycle := Lifecycle{Runner: runner, HostKeyAlias: HostKeyAlias("fixture-instance")}
	_, err := lifecycle.WaitReady(context.Background(), "ssh", "/key", "/known", 2222, ReadyMarker{}, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "within 20ms: Connection refused") {
		t.Fatalf("timeout lost cause: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lifecycle.WaitReady(ctx, "ssh", "/key", "/known", 2222, ReadyMarker{}, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestSSHTrustFollowsInstanceAcrossPortReuse(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH unavailable")
	}
	first := HostKeyAlias("19c45ee4-59b2-49bd-9b6f-ae5529a4f921")
	second := HostKeyAlias("090206e2-c83b-4f99-a85d-99fb0a12d072")
	if first == second || first == "" {
		t.Fatal("different instances share trust")
	}
	for _, port := range []uint16{2222, 2233} {
		args := SSHArgsForInstance("dba", "/key", "/known", first, port)
		output, err := exec.Command(ssh, append([]string{"-G"}, args...)...).CombinedOutput()
		if err != nil || !strings.Contains(string(output), "hostkeyalias "+first) || !strings.Contains(string(output), "stricthostkeychecking accept-new") {
			t.Fatalf("OpenSSH trust config: %v\n%s", err, output)
		}
	}
}
