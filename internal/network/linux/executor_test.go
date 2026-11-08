package linux

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/execx"
)

type failingRootRunner struct{}

func (failingRootRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	return execx.Result{ExitCode: 1}, errors.New("fixture rmdir failure")
}

func TestInstallProgressMarksPlanFailureBeforeMutation(t *testing.T) {
	var events []activity.Event
	executor := Executor{Progress: func(event activity.Event) { events = append(events, event) }}
	report, err := executor.InstallConfig(context.Background(), Config{}, true)
	if err == nil || report.Applied || len(events) != 2 {
		t.Fatalf("report=%+v err=%v events=%+v", report, err, events)
	}
	last := events[len(events)-1]
	if last.Item != "plan" || last.State != "failed" || !last.Done || last.Message != err.Error() {
		t.Fatalf("lost plan failure: %+v", last)
	}
}

func TestRootRmdirTreatsAnAlreadyAbsentOwnedDirectoryAsSuccess(t *testing.T) {
	executor := Executor{Root: failingRootRunner{}}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := executor.rootRmdir(context.Background(), missing); err != nil {
		t.Fatalf("absent directory cleanup = %v", err)
	}
	existing := t.TempDir()
	if err := executor.rootRmdir(context.Background(), existing); err == nil {
		t.Fatal("real rmdir failure for an existing directory was hidden")
	}
}
