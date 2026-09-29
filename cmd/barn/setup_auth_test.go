package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/execx"
	darwinnet "github.com/pgsty/barn/internal/network/darwin"
	netpreflight "github.com/pgsty/barn/internal/network/preflight"
	"golang.org/x/term"
)

// Homebrew's normal entry point resets sudo's timestamp, including on failed
// installs. Its fast --prefix path can avoid that reset for an existing keg.
type setupAuthRunner struct {
	brew, keg          string
	installed          bool
	resetOnPrefix      bool
	credential         bool
	installErr         error
	cancelOnInstall    context.CancelFunc
	prefixes, installs int
}

func (runner *setupAuthRunner) Run(ctx context.Context, binary string, args ...string) (execx.Result, error) {
	if binary == runner.brew {
		switch strings.Join(args, " ") {
		case "--prefix socket_vmnet":
			runner.prefixes++
			if runner.resetOnPrefix {
				runner.credential = false
			}
			if !runner.installed {
				return execx.Result{ExitCode: 1}, errors.New("formula is absent")
			}
			return execx.Result{Stdout: []byte(runner.keg + "\n")}, nil
		case "install socket_vmnet":
			runner.installs++
			runner.credential = false
			if runner.cancelOnInstall != nil {
				runner.cancelOnInstall()
				return execx.Result{ExitCode: 1}, ctx.Err()
			}
			if runner.installErr != nil {
				return execx.Result{ExitCode: 1}, runner.installErr
			}
			runner.installed = true
			return execx.Result{}, nil
		}
	}
	if binary == "/usr/bin/sudo" && strings.Join(args, " ") == "-n -- /usr/bin/true" {
		if !runner.credential {
			return execx.Result{ExitCode: 1}, errors.New("sudo: a password is required")
		}
		return execx.Result{}, nil
	}
	return execx.Result{ExitCode: 1}, errors.New("unexpected fixture command")
}

type setupAuthInstaller struct {
	runner *setupAuthRunner
	calls  int
}

func (installer *setupAuthInstaller) InstallFromHomebrew(ctx context.Context, binaries darwinnet.LocalBinaries, interfaceID, arch, mode, cidr string, apply bool) (darwinnet.InstallReport, error) {
	installer.calls++
	if binaries.Socket != filepath.Join(installer.runner.keg, "bin", "socket_vmnet") || interfaceID == "" || arch != runtime.GOARCH || mode != "host" || cidr != "10.10.10.0/24" || !apply {
		return darwinnet.InstallReport{}, errors.New("network install lost the selected source or network")
	}
	if _, err := (sudoRunner{base: installer.runner}).Run(ctx, "/usr/bin/true"); err != nil {
		return darwinnet.InstallReport{}, err
	}
	return darwinnet.InstallReport{Applied: true, Plan: darwinnet.InstallPlan{State: darwinnet.NetworkState{CIDR: cidr}}}, nil
}

func (installer *setupAuthInstaller) InstallModeNetwork(context.Context, string, string, string, string, string, bool) (darwinnet.InstallReport, error) {
	installer.calls++
	return darwinnet.InstallReport{}, errors.New("unverified fixture archive reached privileged install")
}

func newSetupAuthRunner(t *testing.T) *setupAuthRunner {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(directory, "cache"))
	t.Setenv("BARN_VMNET_ARCHIVE", "")
	t.Setenv("BARN_REPO", "")
	bin := filepath.Join(directory, "bin")
	keg := filepath.Join(directory, "Cellar", "socket_vmnet", darwinnet.ReleaseVersion)
	for _, path := range []string{bin, filepath.Join(keg, "bin")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	brew := filepath.Join(bin, "brew")
	for _, path := range []string{brew, filepath.Join(keg, "bin", "socket_vmnet"), filepath.Join(keg, "bin", "socket_vmnet_client")} {
		if err := os.WriteFile(path, []byte("fixture; never executed\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return &setupAuthRunner{brew: brew, keg: keg}
}

func TestSetupDarwinAuthenticatesAfterHomebrew(t *testing.T) {
	for _, test := range []struct {
		name          string
		installed     bool
		resetOnPrefix bool
		wantInstalls  int
	}{
		{name: "new formula clears credential", wantInstalls: 1},
		{name: "existing formula slow prefix clears credential", installed: true, resetOnPrefix: true},
		{name: "existing formula fast prefix", installed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := newSetupAuthRunner(t)
			runner.installed, runner.resetOnPrefix = test.installed, test.resetOnPrefix
			installer := &setupAuthInstaller{runner: runner}
			authentications := 0
			authorize := func() error {
				authentications++
				runner.credential = true
				return nil
			}
			step, uncertain, err := installSetupDarwinNetwork(context.Background(), "host", "", "10.10.10.0/24", runner, authorize, io.Discard, installer)
			if err != nil || uncertain || step.Status != "installed" || !step.Changed || step.Detail != "10.10.10.0/24" {
				t.Fatalf("network install = %#v, uncertain=%t, err=%v", step, uncertain, err)
			}
			if authentications != 1 || installer.calls != 1 || runner.installs != test.wantInstalls {
				t.Fatalf("auth=%d privileged installs=%d brew installs=%d", authentications, installer.calls, runner.installs)
			}
		})
	}
}

func TestSetupDarwinSourceFailureDoesNotAuthenticate(t *testing.T) {
	for _, source := range []string{"explicit archive", "failed brew repository fallback", "cancelled brew fallback"} {
		t.Run(source, func(t *testing.T) {
			runner := newSetupAuthRunner(t)
			installer := &setupAuthInstaller{runner: runner}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			repo := ""
			wantError := ""
			switch source {
			case "explicit archive":
				archive := filepath.Join(t.TempDir(), "corrupt.tar.gz")
				if err := os.WriteFile(archive, []byte("corrupt archive"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("BARN_VMNET_ARCHIVE", archive)
				wantError = "BARN_VMNET_ARCHIVE"
			case "failed brew repository fallback":
				runner.installErr = errors.New("formula download failed")
				repo = t.TempDir()
				release, err := darwinnet.PinnedRelease(runtime.GOARCH)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(repo, "socket_vmnet"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, "socket_vmnet", release.ArchiveName), []byte("corrupt archive"), 0o600); err != nil {
					t.Fatal(err)
				}
				wantError = "repository copy"
			case "cancelled brew fallback":
				runner.cancelOnInstall = cancel
			}
			authentications := 0
			_, uncertain, err := installSetupDarwinNetwork(ctx, "host", repo, "10.10.10.0/24", runner, func() error { authentications++; return nil }, io.Discard, installer)
			if err == nil || uncertain || authentications != 0 || installer.calls != 0 {
				t.Fatalf("source failure: auth=%d installs=%d uncertain=%t err=%v", authentications, installer.calls, uncertain, err)
			}
			if wantError != "" && !strings.Contains(err.Error(), wantError) {
				t.Fatalf("source failure lost its cause: %v", err)
			}
			if source == "cancelled brew fallback" && !errors.Is(err, context.Canceled) {
				t.Fatalf("source failure lost cancellation: %v", err)
			}
			if source == "explicit archive" && runner.prefixes != 0 {
				t.Fatal("explicit archive unexpectedly consulted Homebrew")
			}
			if source != "explicit archive" && runner.installs != 1 {
				t.Fatalf("brew install attempts=%d, want one before fallback", runner.installs)
			}
		})
	}
}

func TestSetupDarwinAuthorizationFailureDoesNotInstall(t *testing.T) {
	runner := newSetupAuthRunner(t)
	runner.installed = true
	installer := &setupAuthInstaller{runner: runner}
	authErr := errors.New("authentication cancelled")
	_, uncertain, err := installSetupDarwinNetwork(context.Background(), "host", "", "10.10.10.0/24", runner, func() error { return authErr }, io.Discard, installer)
	if !errors.Is(err, authErr) || uncertain || installer.calls != 0 || runner.prefixes != 1 {
		t.Fatalf("authorization failure: prefixes=%d installs=%d uncertain=%t err=%v", runner.prefixes, installer.calls, uncertain, err)
	}
}

func TestSetupNetworkRepairStillAuthenticatesWithoutSourcePreparation(t *testing.T) {
	if os.Geteuid() == 0 || term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("requires a non-root, non-interactive sudo session")
	}
	runner := newSetupAuthRunner(t)
	session := &sudoSession{base: runner, stderr: io.Discard}
	defer session.close()
	report := netpreflight.Report{
		CIDR:         "10.10.10.0/24",
		Installation: netpreflight.Installation{Status: "exact", CIDR: "10.10.10.0/24"},
		Findings:     []netpreflight.Finding{{Severity: netpreflight.Error, Code: "installation.not_ready"}},
	}
	_, uncertain, err := applySetupNetwork(context.Background(), "host", "", report, runner, session, io.Discard)
	if err == nil || uncertain || runner.prefixes != 0 || runner.installs != 0 {
		t.Fatalf("repair attempted source preparation: prefixes=%d installs=%d uncertain=%t err=%v", runner.prefixes, runner.installs, uncertain, err)
	}
	report.Findings = nil
	step, uncertain, err := applySetupNetwork(context.Background(), "host", "", report, runner, session, io.Discard)
	if err != nil || uncertain || step.Status != "ready" {
		t.Fatalf("healthy network unnecessarily requested authentication: %#v uncertain=%t err=%v", step, uncertain, err)
	}
}
