package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/failure"
	privatevm "github.com/pgsty/barn/internal/private"
)

func TestScopedRetryPreservesInventoryRepositoryAndFlags(t *testing.T) {
	options := lifecycleRetryOptions{Repository: "https://images.example.test/private/channel", NoWait: true, Rollback: true, Image: "el9:stable", Arch: "amd64"}
	var out bytes.Buffer
	printNodeFailures(&out, "/labs/team's lab.yml", []privatevm.NodeFailure{{Node: "meta", Stage: "prepare", Error: "temporary failure"}}, options)
	for _, want := range []string{"-f " + shellQuote("/labs/team's lab.yml"), "--repo " + options.Repository, "--no-wait", "--rollback", "--image el9:stable", "--arch amd64", " meta"} {
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

func TestOverrideDriftRecoveryKeepsTheRequestedImageAndArchitecture(t *testing.T) {
	options := lifecycleRetryOptions{Image: "u24", Arch: "amd64", Repository: "https://images.example.test", NoWait: true, Rollback: true}
	err := lifecycleOverrideRecovery(privatevm.ErrRecreateRequired, "/labs/my lab.yml", []string{"meta"}, options)
	class, reason, next := failure.Classify(err)
	if !errors.Is(err, privatevm.ErrRecreateRequired) || class != failure.Conflict || reason != "recreate_required" {
		t.Fatalf("lost drift classification: %v, %s, %s", err, class, reason)
	}
	for _, command := range strings.Split(next, ", then ") {
		for _, want := range []string{"-f '/labs/my lab.yml'", "--image u24", "--arch amd64", "--repo https://images.example.test", " meta"} {
			if !strings.Contains(command, want) {
				t.Fatalf("drift recovery lost %q: %s", want, command)
			}
		}
		if strings.Contains(command, "--rollback") {
			t.Fatalf("recovery passed an unsupported flag: %s", command)
		}
	}
	if !strings.HasPrefix(next, "barn plan ") || !strings.Contains(next, ", then barn recreate ") {
		t.Fatalf("unexpected recovery: %s", next)
	}
}

func TestOverrideResizeRecoveryKeepsDesiredStateWithoutUnsupportedFlags(t *testing.T) {
	options := lifecycleRetryOptions{Image: "u24", Arch: "arm64", Repository: "https://images.example.test", NoWait: true, Rollback: true}
	err := lifecycleOverrideRecovery(privatevm.ErrResizeRequired, "applied deployment state", []string{"node1"}, options)
	_, _, next := failure.Classify(err)
	if next != "barn resize --image u24 --arch arm64 node1" {
		t.Fatalf("resize recovery lost effective desired state: %s", next)
	}
	root := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{})
	resize, _, findErr := root.Find([]string{"resize"})
	if findErr != nil {
		t.Fatal(findErr)
	}
	if err := resize.ParseFlags(strings.Fields(next)[2:]); err != nil {
		t.Fatalf("suggested resize command is not accepted: %v", err)
	}
}

func TestPartialArchitectureRecoveryDoesNotRepeatTheBlockedRecreation(t *testing.T) {
	options := lifecycleRetryOptions{Arch: "amd64", Image: "u24"}
	err := lifecycleOverrideRecovery(privatevm.ErrWholeDeploymentRequired, "applied deployment state", []string{"meta"}, options)
	_, reason, next := failure.Classify(err)
	if !errors.Is(err, privatevm.ErrRecreateRequired) || reason != "recreate_required" {
		t.Fatalf("selection error lost recreation classification: %v", err)
	}
	if !strings.Contains(next, "select all existing nodes") || !strings.Contains(next, "barn plan --image u24 --arch amd64") || strings.Contains(next, "barn recreate") {
		t.Fatalf("recovery repeated a blocked or wider destructive command: %s", next)
	}
}
