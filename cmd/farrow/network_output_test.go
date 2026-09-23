package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/pgsty/farrow/internal/hostconfig"
	darwinnet "github.com/pgsty/farrow/internal/network/darwin"
)

func TestFreshMacNetworkInstallPointsAtSetup(t *testing.T) {
	boundary := newCommandError(exitRuntime, errNetworkNotInstalled)
	if boundary.code != exitCapability || boundary.failure.Next != "farrow setup" || boundary.failure.Reason != "network_absent" {
		t.Fatalf("failure = %+v code=%d", boundary.failure, boundary.code)
	}
	busy := newCommandError(exitRuntime, darwinnet.ErrVMNetSharingBusy)
	if busy.code != exitResource || !strings.Contains(busy.failure.Next, "farrow setup -c") {
		t.Fatalf("vmnet busy = %+v code=%d", busy.failure, busy.code)
	}
}

func TestHostPlanTextShowsChangeWithoutDigests(t *testing.T) {
	var out bytes.Buffer
	report := hostconfig.Report{Plan: hostconfig.Plan{Action: "install", Target: "/etc/hosts", Changed: true, BeforeSHA256: strings.Repeat("a", 64), AfterSHA256: strings.Repeat("b", 64), Lines: []string{"+ 10.10.10.10 meta"}}}
	if err := hostsOutcome(report, "farrow hosts install --yes").text(&out, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "aaaa") || strings.Contains(text, "sha256") || !strings.Contains(text, "planned") || !strings.Contains(text, "farrow hosts install --yes") {
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
