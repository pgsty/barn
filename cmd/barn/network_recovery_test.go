package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/failure"
	netpreflight "github.com/pgsty/barn/internal/network/preflight"
	"github.com/pgsty/barn/internal/platform"
	privatevm "github.com/pgsty/barn/internal/private"
)

func TestUpPreparesHostForRepairableLogDirectory(t *testing.T) {
	report := netpreflight.Report{
		OS: "darwin", CIDR: "10.10.10.0/24",
		Installation: netpreflight.Installation{Status: "protected", CIDR: "10.10.10.0/24", Healthy: true},
		Findings: []netpreflight.Finding{{
			Code: "installation.log_directory", Severity: netpreflight.Error,
			Evidence: "/var/log/barn-vmnet has mode 0744; expected 0755 (owner root:wheel is correct)",
			Fix:      "run barn up to repair it, or run: sudo chmod 0755 /var/log/barn-vmnet",
		}},
	}
	failure := &privatevm.NetworkPreflightError{Report: report}
	if !needsHostSetup("up", failure) || !setupNeedsNetworkInstall(report) || networkRepairNeedsRestart(report) {
		t.Fatal("up/setup did not enter recovery for the owned log directory")
	}
	if message := failure.Error(); !strings.Contains(message, "0744") || !strings.Contains(message, "sudo chmod 0755") {
		t.Fatalf("noninteractive up lost the actionable error: %s", message)
	}
	for _, code := range []string{"installation.log_directory_unsafe", "route.overlap", "vmnet.sharing_probe"} {
		report.Findings[0].Code = code
		if needsHostSetup("up", &privatevm.NetworkPreflightError{Report: report}) {
			t.Fatalf("up tried automatic recovery for %s", code)
		}
	}
}

func TestLifecycleHostRecoveryIsLimitedToRepairableStartupFailures(t *testing.T) {
	repairable := &privatevm.NetworkPreflightError{Report: netpreflight.Report{
		CIDR: "10.10.10.0/24", Installation: netpreflight.Installation{Status: "protected", CIDR: "10.10.10.0/24"},
		Findings: []netpreflight.Finding{{Code: "installation.not_ready", Severity: netpreflight.Error}},
	}}
	conflict := &privatevm.NetworkPreflightError{Report: netpreflight.Report{
		Findings: []netpreflight.Finding{{Code: "route.overlap", Severity: netpreflight.Error}},
	}}
	for _, command := range []string{"up", "start", "restart", "reload"} {
		for _, err := range []error{repairable, &exec.Error{Name: "qemu-img", Err: exec.ErrNotFound}} {
			if !needsHostSetup(command, err) {
				t.Errorf("%s did not prepare its missing host prerequisite: %v", command, err)
			}
		}
		for _, err := range []error{nil, conflict, errors.New("guest readiness failed")} {
			if needsHostSetup(command, err) {
				t.Errorf("%s requested host setup for an unrelated failure: %v", command, err)
			}
		}
	}
	for _, command := range []string{"plan", "status", "stop", "destroy", "purge", "recreate"} {
		if needsHostSetup(command, repairable) {
			t.Errorf("%s must not install dependencies or replay a destructive command", command)
		}
	}
}

func TestLogRepairDoesNotSkipRepairOfMissingRoute(t *testing.T) {
	report := netpreflight.Report{Findings: []netpreflight.Finding{
		{Code: "installation.log_directory", Severity: netpreflight.Error},
		{Code: "installation.route_missing", Severity: netpreflight.Error},
	}}
	if !networkRepairNeedsRestart(report) {
		t.Fatal("permission repair suppressed recovery of the missing network route")
	}
}

func TestMissingQEMURecoveryMatchesHostSetupCapabilities(t *testing.T) {
	for _, test := range []struct {
		name    string
		profile platform.Profile
		setup   bool
	}{
		{"linux-native", platform.Profile{OS: "linux", Arch: "amd64", QEMUBinary: "qemu-system-x86_64"}, true},
		{"linux-foreign", platform.Profile{OS: "linux", Arch: "arm64", QEMUBinary: "qemu-system-aarch64", Emulated: true}, false},
		{"darwin-foreign", platform.Profile{OS: "darwin", Arch: "amd64", QEMUBinary: "qemu-system-x86_64", Emulated: true}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := platform.FindQEMUBinary(test.profile, func(name string) (string, error) {
				return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
			})
			if !errors.Is(err, exec.ErrNotFound) {
				t.Errorf("missing executable cause was lost: %v", err)
			}
			_, reason, next := failure.Classify(err)
			if reason != "qemu_missing" || (next == "barn setup") != test.setup {
				t.Errorf("inaccurate recovery hint: reason=%s next=%s", reason, next)
			}
			for _, command := range []string{"up", "start", "restart", "reload"} {
				if needsHostSetup(command, err) != test.setup {
					t.Errorf("%s setup=%t, want %t: %v", command, needsHostSetup(command, err), test.setup, err)
				}
			}
			if !test.setup && !strings.Contains(next, test.profile.QEMUBinary) {
				t.Errorf("foreign runtime hint omits the required emulator: %s", next)
			}
		})
	}
	if needsHostSetup("up", platform.QEMUMissing("qemu-img", errors.New("permission denied"))) {
		t.Fatal("unusable executable incorrectly triggered package installation")
	}
}
