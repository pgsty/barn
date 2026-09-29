package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	netpreflight "github.com/pgsty/barn/internal/network/preflight"
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
