package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/hostconfig"
	darwinnet "github.com/pgsty/barn/internal/network/darwin"
)

func TestFreshMacNetworkInstallPointsAtSetup(t *testing.T) {
	boundary := newCommandError(exitRuntime, errNetworkNotInstalled)
	if boundary.code != exitCapability || boundary.failure.Next != "barn setup" || boundary.failure.Reason != "network_absent" {
		t.Fatalf("failure = %+v code=%d", boundary.failure, boundary.code)
	}
	busy := newCommandError(exitRuntime, darwinnet.ErrVMNetSharingBusy)
	if busy.code != exitResource || !strings.Contains(busy.failure.Next, "barn setup -c") {
		t.Fatalf("vmnet busy = %+v code=%d", busy.failure, busy.code)
	}
}

func TestNetworkPlanAndApplyKeepDistinctTerminalResults(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	for _, apply := range []bool{false, true} {
		var output bytes.Buffer
		state := &outputContext{format: outputText, stdoutTTY: true, stderrTTY: true, stderrFile: true, columns: 100, rows: 30}
		writer := &outputWriter{Writer: &output, context: state, stderr: true}
		outcome, pending, err := networkStep(context.Background(), context.Background(), writer, "uninstall", apply, func(reporter activity.Reporter) (commandOutcome, bool, error) {
			if !apply && reporter != nil {
				t.Fatal("plan must not freeze an applied progress table")
			}
			steps := activity.Steps{Reporter: reporter, Phase: "network"}
			steps.Start("service", "stopping", "stopping service")
			steps.Done("stopped", "service stopped")
			steps.Start("files", "removing", "removing files")
			steps.Done("removed", "owned files removed")
			return networkRemovalOutcome(nil, []string{"/fixture/network.json"}, nil, apply, "barn network uninstall --yes"), !apply, nil
		})
		if err != nil || pending == apply {
			t.Fatalf("apply=%t pending=%t err=%v", apply, pending, err)
		}
		if err := outcome.text(writer, writer); err != nil {
			t.Fatal(err)
		}
		got := output.String()
		if strings.Count(got, "ITEM") != 1 || strings.Contains(got, "[DONE]") != apply || lifecycleProgressFrozen(writer) != apply {
			t.Fatalf("apply=%t output=%q", apply, got)
		}
		if !apply && (!strings.Contains(got, "planned") || !strings.Contains(got, "/fixture/network.json") || !strings.Contains(got, "barn network uninstall --yes")) {
			t.Fatalf("plan lost reviewed paths or next command: %q", got)
		}
	}
}

func TestFailedNetworkStepDoesNotClaimCompletion(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	var output bytes.Buffer
	state := &outputContext{format: outputText, stdoutTTY: true, stderrTTY: true, stderrFile: true, columns: 100, rows: 30}
	writer := &outputWriter{Writer: &output, context: state, stderr: true}
	failure := errors.New("fixture bridge attach failure; rollback completed")
	_, _, err := networkStep(context.Background(), context.Background(), writer, "install", true, func(reporter activity.Reporter) (commandOutcome, bool, error) {
		steps := activity.Steps{Reporter: reporter, Phase: "network"}
		steps.Start("helper", "checking", "testing non-root attachment")
		steps.Fail(failure)
		return commandOutcome{}, false, failure
	})
	if !errors.Is(err, failure) || !strings.Contains(output.String(), "[FAILED]") || strings.Contains(output.String(), "[DONE]") || !strings.Contains(output.String(), "rollback completed") {
		t.Fatalf("failed result: %q %v", &output, err)
	}
}

func TestHostPlanTextShowsChangeWithoutDigests(t *testing.T) {
	var out bytes.Buffer
	report := hostconfig.Report{Plan: hostconfig.Plan{Action: "install", Target: "/etc/hosts", Changed: true, BeforeSHA256: strings.Repeat("a", 64), AfterSHA256: strings.Repeat("b", 64), Lines: []string{"+ 10.10.10.10 meta"}}}
	if err := hostsOutcome(report, "barn hosts install --yes").text(&out, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "aaaa") || strings.Contains(text, "sha256") || !strings.Contains(text, "planned") || !strings.Contains(text, "barn hosts install --yes") {
		t.Fatalf("hosts plan text:\n%s", text)
	}
}

func TestConfirmPlanDefaults(t *testing.T) {
	if err := confirmPlan("? ", true, strings.NewReader("\n"), &bytes.Buffer{}); err != nil {
		t.Fatalf("[Y/n] Enter = %v", err)
	}
	if err := confirmPlan("? ", false, strings.NewReader("\n"), &bytes.Buffer{}); !errors.Is(err, ErrCancelled) || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("[y/N] Enter = %v", err)
	}
}
