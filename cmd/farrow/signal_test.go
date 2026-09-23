package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	privatevm "github.com/pgsty/farrow/internal/private"
	"github.com/pgsty/farrow/internal/spec"
	"github.com/pgsty/farrow/internal/state"
)

func TestCancelledContextRendersOneStructuredFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := runContext(ctx, []string{"--json", "version"}, &stdout, &stderr); code != exitCancelled {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 2 || payload["error"] != "cancelled" || payload["message"] != "cancelled" {
		t.Fatalf("payload=%v", payload)
	}
	if got := strings.TrimSpace(stderr.String()); got != "error: cancelled" {
		t.Fatalf("stderr=%q", got)
	}
}

func TestProgressStopsWithParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())
	var stderr bytes.Buffer
	_, _, errOut, err := prepareOutput([]string{"--verbose"}, &bytes.Buffer{}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	item := startProgress(ctx, errOut, "cancellable progress")
	cancel()
	select {
	case <-item.done:
	case <-time.After(time.Second):
		t.Fatal("progress goroutine did not stop after parent cancellation")
	}
	item.Stop(ErrCancelled)
}

func TestFollowLogsReturnsCancelled(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FARROW_HOME", root)
	resolved := spec.Resolved{
		Schema: 1, Name: "private", Image: "u24", Network: "private", SSHUser: "dba",
		Private: &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"},
		Nodes:   []spec.Node{{Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB}},
	}
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := (state.Store{Root: root}).WriteDeployment(state.DeploymentState{
		Schema: state.DeploymentSchema, FarrowVersion: "test", SpecHash: hash,
		Resolved: resolved, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.TODO())
	var stdout, stderr bytes.Buffer
	result := make(chan int, 1)
	go func() {
		result <- runContext(ctx, []string{"logs", "--source", "events", "--follow"}, &stdout, &stderr)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case code := <-result:
		if code != exitCancelled {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow logs did not stop after cancellation")
	}
	if stdout.Len() != 0 || strings.Count(stderr.String(), "error:") != 1 || !strings.Contains(stderr.String(), "cancelled") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestInterruptedLifecycleIsRecordedInTheEventLog(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FARROW_HOME", root)
	resolved := spec.Resolved{
		Schema: 1, Name: "private", Image: "u24", Network: "private", SSHUser: "dba",
		Private: &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"},
		Nodes:   []spec.Node{{Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB}},
	}
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := (state.Store{Root: root}).WriteDeployment(state.DeploymentState{
		Schema: state.DeploymentSchema, FarrowVersion: "test", SpecHash: hash,
		Resolved: resolved, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(context.TODO())
	cancel()
	manager := privatevm.Manager{FarrowVersion: "test", OperationID: "018f4b8e-1234-4abc-9def-0123456789ab"}
	var stderr bytes.Buffer
	recordCancelledLifecycle(parent, manager, "up", privatevm.Status{}, context.Canceled, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("audit append reported a problem: %q", stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil {
		t.Fatalf("events.jsonl=%q err=%v", data, err)
	}
	if event["action"] != "up" || event["level"] != "error" || !strings.HasPrefix(event["message"].(string), "cancelled by signal") {
		t.Fatalf("event=%v", event)
	}
}

func TestSSHChildInterruptHelper(t *testing.T) {
	if os.Getenv("FARROW_TEST_SSH_INTERRUPT_HELPER") != "1" {
		return
	}
	ctx, stop := interruptContext(context.TODO())
	defer stop()
	_, err := executeSSHProcess(
		ctx, "exec", "meta", "dba", "127.0.0.1", 22,
		"/bin/sh", []string{"-c", "trap 'exit 42' INT; sleep 0.2; printf 'ready\\n' >&2; while :; do sleep 1; done"}, []string{"remote"}, io.Discard, os.Stderr,
	)
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 42 {
		t.Fatalf("ssh child error=%v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("remote SIGINT cancelled the Farrow parent: %v", ctx.Err())
	}
	// Once the child is gone, SIGINT must cancel Farrow again: the child
	// window suspends the registration, it does not drop it.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT after the SSH child exited did not cancel the Farrow context")
	}
}

func TestSSHChildSIGINTDoesNotCancelParent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSSHChildInterruptHelper$")
	command.Env = append(os.Environ(), "FARROW_TEST_SSH_INTERRUPT_HELPER=1")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stderr).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("helper readiness=%q err=%v", line, err)
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("helper process: %v", err)
	}
}
