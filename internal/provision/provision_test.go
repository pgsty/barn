package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/vm"
)

const sshRunnerTestTimeout = 20 * time.Second

func TestLoadScriptSnapshotAndSafety(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "setup.sh")
	if err := os.WriteFile(path, []byte("#!/bin/bash\nprintf 'ok\\n'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := LoadScript(path)
	if err != nil {
		t.Fatal(err)
	}
	if script.Name != "setup.sh" || script.Size != int64(len(script.Content)) || len(script.SHA256) != 64 {
		t.Fatalf("script = %#v", script)
	}
	if err := os.WriteFile(path, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script.Content), "printf") {
		t.Fatal("loaded script was not an immutable snapshot")
	}

	symlink := filepath.Join(root, "link.sh")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadScript(symlink); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink error = %v", err)
	}
	empty := filepath.Join(root, "empty.sh")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadScript(empty); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty error = %v", err)
	}
	nul := filepath.Join(root, "nul.sh")
	if err := os.WriteFile(nul, []byte{'x', 0, 'y'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadScript(nul); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("NUL error = %v", err)
	}
}

type recordingRunner struct {
	mu        sync.Mutex
	active    int
	maxActive int
	fail      map[string]bool
}

func (r *recordingRunner) Run(_ context.Context, target Target, _ Script, _ bool) NodeResult {
	r.mu.Lock()
	r.active++
	if r.active > r.maxActive {
		r.maxActive = r.active
	}
	r.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	if r.fail[target.Node] {
		return NodeResult{Node: target.Node, ExitCode: 17, Error: "fixture failure"}
	}
	return NodeResult{Node: target.Node, Success: true, ExitCode: 0, Stdout: target.Node + "\n"}
}

func fixtureTarget(name string) Target {
	return Target{Node: name, User: "dba", Port: 2200, PrivateKey: "/tmp/key", KnownHosts: "/tmp/known_hosts", HostKeyAlias: vm.HostKeyAlias(name)}
}

func fixtureScript() Script {
	content := []byte("echo ok\n")
	digest := sha256.Sum256(content)
	return Script{Name: "setup.sh", Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), Content: content}
}

func TestExecutorBoundedParallelAndDeterministic(t *testing.T) {
	runner := &recordingRunner{fail: map[string]bool{"node-2": true}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	script := fixtureScript()
	targets := []Target{fixtureTarget("meta"), fixtureTarget("node-1"), fixtureTarget("node-2"), fixtureTarget("node-3")}
	report, err := (Executor{Runner: runner, Parallelism: 2, OperationID: "operation-fixture"}).Execute(ctx, script, targets, true)
	if err != nil {
		t.Fatal(err)
	}
	if runner.maxActive != 2 || report.Successful != 3 || report.Failed != 1 || len(report.Results) != 4 {
		t.Fatalf("max=%d report=%#v", runner.maxActive, report)
	}
	for index, target := range targets {
		if report.Results[index].Node != target.Node {
			t.Fatalf("result order changed: %#v", report.Results)
		}
	}
}

func TestExecutorRequiresDeadlineAndSafeTargets(t *testing.T) {
	script := fixtureScript()
	executor := Executor{Runner: &recordingRunner{}, Parallelism: 1, OperationID: "operation-fixture"}
	if _, err := executor.Execute(context.Background(), script, []Target{fixtureTarget("meta")}, false); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("deadline error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	target := fixtureTarget("meta")
	target.PrivateKey = "relative"
	if _, err := executor.Execute(ctx, script, []Target{target}, false); err == nil || !strings.Contains(err.Error(), "non-absolute") {
		t.Fatalf("target error = %v", err)
	}
}

func TestExecutorRejectsInconsistentScriptSnapshotBeforeRunner(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Script)
	}{
		{name: "size", mutate: func(script *Script) { script.Size-- }},
		{name: "digest", mutate: func(script *Script) { script.SHA256 = strings.Repeat("a", 64) }},
		{name: "uppercase digest", mutate: func(script *Script) { script.SHA256 = strings.ToUpper(script.SHA256) }},
		{name: "content", mutate: func(script *Script) { script.Content = append(script.Content, '#') }},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := fixtureScript()
			test.mutate(&script)
			runner := &recordingRunner{}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := (Executor{Runner: runner, Parallelism: 1, OperationID: "operation-fixture"}).Execute(ctx, script, []Target{fixtureTarget("meta")}, false); err == nil || !strings.Contains(err.Error(), "snapshot is invalid") {
				t.Fatalf("snapshot error = %v", err)
			}
			if runner.maxActive != 0 {
				t.Fatalf("invalid snapshot reached runner: max active = %d", runner.maxActive)
			}
		})
	}
}

func TestSSHRemoteCommandIsFixed(t *testing.T) {
	target := fixtureTarget("meta")
	plain := vmCommandForTest(target, false)
	root := vmCommandForTest(target, true)
	if plain != "'/bin/bash' '-se'" {
		t.Fatalf("plain remote command = %q", plain)
	}
	if root != "'sudo' '-n' '--' '/bin/bash' '-se'" {
		t.Fatalf("sudo remote command = %q", root)
	}
}

func TestSSHRunnerStreamsSnapshotAndCapturesExit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fakeSSH := filepath.Join(root, "ssh")
	if err := os.WriteFile(fakeSSH, []byte("#!/bin/sh\nprintf 'remote=%s\\n' \"$*\"\nprintf 'stdin='\ncat\nprintf 'fixture stderr\\n' >&2\nexit 17\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshRunnerTestTimeout)
	defer cancel()
	script := fixtureScript()
	target := fixtureTarget("meta")
	target.HostKeyAlias = vm.HostKeyAlias("provisioned-instance")
	result := (SSHRunner{SSHPath: fakeSSH}).Run(ctx, target, script, true)
	if result.Success || result.ExitCode != 17 || result.DurationMS < 0 {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(result.Stdout, "'sudo' '-n' '--' '/bin/bash' '-se'") || !strings.Contains(result.Stdout, "stdin=echo ok") || !strings.Contains(result.Stdout, "HostKeyAlias=\""+target.HostKeyAlias+"\"") {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	if result.Stderr != "fixture stderr\n" || !strings.Contains(result.Error, "exit status 17") {
		t.Fatalf("stderr=%q error=%q", result.Stderr, result.Error)
	}
}

func TestSSHRunnerBoundsCapturedOutput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fakeSSH := filepath.Join(root, "ssh")
	if err := os.WriteFile(fakeSSH, []byte("#!/bin/sh\nprintf '123456789'\nprintf 'abcdefghi' >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshRunnerTestTimeout)
	defer cancel()
	script := fixtureScript()
	result := (SSHRunner{SSHPath: fakeSSH, OutputLimit: 4}).Run(ctx, fixtureTarget("meta"), script, false)
	if !result.Success || result.Stdout != "1234" || result.Stderr != "abcd" || !result.StdoutTruncated || !result.StderrTruncated {
		t.Fatalf("bounded result = %#v", result)
	}
}

func vmCommandForTest(target Target, sudo bool) string {
	remote := []string{"/bin/bash", "-se"}
	if sudo {
		remote = []string{"sudo", "-n", "--", "/bin/bash", "-se"}
	}
	args := vm.SSHArgsForInstance(target.User, target.PrivateKey, target.KnownHosts, target.HostKeyAlias, target.Port, remote...)
	return args[len(args)-1]
}
