package execx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDisplayKeepsArgumentsSeparate(t *testing.T) {
	t.Parallel()
	got := Display("qemu-img", "create", "a b", "$(touch nope)")
	if !strings.Contains(got, `"a b"`) || !strings.Contains(got, `"$(touch nope)"`) {
		t.Fatalf("display command did not quote arguments: %s", got)
	}
}

func TestOSRunnerBoundsOutput(t *testing.T) {
	printf, err := exec.LookPath("printf")
	if err != nil {
		t.Skip("printf executable unavailable")
	}
	runner := OSRunner{Timeout: 5 * time.Second, OutputLimit: 32}
	result, err := runner.Run(context.Background(), printf, "%s", strings.Repeat("x", 4096))
	if err != nil {
		t.Fatalf("run printf: %v", err)
	}
	if len(result.Stdout) != 32 {
		t.Fatalf("stdout length = %d, want 32", len(result.Stdout))
	}
}

func TestOSRunnerInheritsProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9443")
	printenv, err := exec.LookPath("printenv")
	if err != nil {
		t.Skip("printenv executable unavailable")
	}
	result, err := (OSRunner{Timeout: 5 * time.Second}).Run(context.Background(), printenv, "HTTPS_PROXY")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(result.Stdout)); got != "http://127.0.0.1:9443" {
		t.Fatalf("inherited HTTPS_PROXY = %q", got)
	}
}

func TestOSRunnerPassesExtraFileAsFD3(t *testing.T) {
	t.Parallel()
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("cat executable unavailable")
	}
	file, err := os.CreateTemp(t.TempDir(), "fd-input-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close extra-file fixture: %v", err)
		}
	})
	if _, err := file.WriteString("inherited-fd-3"); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	result, err := (OSRunner{Timeout: 5 * time.Second}).RunWithExtraFiles(context.Background(), catPath, []*os.File{file}, "/dev/fd/3")
	if err != nil || string(result.Stdout) != "inherited-fd-3" {
		t.Fatalf("extra file result=%q err=%v", result.Stdout, err)
	}
}

func TestCommandErrorNamesProgramAndLastStderrLine(t *testing.T) {
	t.Parallel()
	_, err := (OSRunner{}).Run(context.Background(), "/bin/sh", "-c", "printf 'first\\n\\033[31msecond\\tline\\033[0m\\n' >&2; exit 100")
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("error = %v", err)
	}
	if got, want := err.Error(), "sh exited with status 100: second line"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if tail := commandErr.StderrTail(8); len(tail) != 2 || tail[0] != "first" {
		t.Fatalf("tail = %q", tail)
	}
}

func TestCommandErrorReportsTimeoutAndSignal(t *testing.T) {
	t.Parallel()
	_, err := (OSRunner{Timeout: 50 * time.Millisecond}).Run(context.Background(), "/bin/sleep", "5")
	if err == nil || err.Error() != "sleep timed out after 50ms" {
		t.Fatalf("timeout error = %v", err)
	}
	_, err = (OSRunner{}).Run(context.Background(), "/bin/sh", "-c", "kill -KILL $$")
	if err == nil || err.Error() != "sh was killed by SIGKILL" {
		t.Fatalf("signal error = %v", err)
	}
}

func TestCommandErrorNamesProgramRunThroughSudo(t *testing.T) {
	t.Parallel()
	err := &CommandError{Binary: "/usr/bin/sudo", Args: []string{"-n", "--", "/opt/farrow/libexec/farrow-hosts-helper", "--target", "/etc/hosts"}, ExitCode: 1, Stderr: "bad target"}
	if got, want := err.Error(), "farrow-hosts-helper (via sudo) exited with status 1: bad target"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}
