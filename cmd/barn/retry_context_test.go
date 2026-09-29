package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/hostshare"
	privatevm "github.com/pgsty/barn/internal/private"
)

func TestScopedRetryPreservesInventoryRepositoryAndFlags(t *testing.T) {
	options := lifecycleRetryOptions{Repository: "https://images.example.test/private/channel", NoWait: true, Rollback: true}
	var out bytes.Buffer
	printNodeFailures(&out, "/labs/team's lab.yml", []privatevm.NodeFailure{{Node: "meta", Stage: "prepare", Error: "temporary failure"}}, options)
	for _, want := range []string{"-f " + shellQuote("/labs/team's lab.yml"), "--repo " + options.Repository, "--no-wait", "--rollback", " meta"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("retry lost %q: %s", want, &out)
		}
	}
	options.Action = "start"
	got := lifecycleRetryCommand("applied deployment state", []string{"meta"}, options)
	if got != "barn start --no-wait meta" {
		t.Fatalf("start retry must use applied state without creating nodes: %s", got)
	}
}

func TestUnrecoverableShareCapabilityDoesNotSuggestBlindRetry(t *testing.T) {
	var out bytes.Buffer
	printNodeFailures(&out, "lab.yml", []privatevm.NodeFailure{{Node: "meta", Stage: "start", Error: "host share cannot be safely opened by QEMU on macOS; preserve needed data", Reason: hostshare.ReasonUnsupportedOnDarwin}})
	if strings.Contains(out.String(), "barn up") || strings.Contains(out.String(), "barn recreate") || !strings.Contains(out.String(), "preserve needed data") {
		t.Fatalf("unsafe or ineffective retry: %s", &out)
	}
}
