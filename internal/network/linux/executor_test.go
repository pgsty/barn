package linux

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pgsty/barn/internal/execx"
)

type failingRootRunner struct{}

func (failingRootRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	return execx.Result{ExitCode: 1}, errors.New("fixture rmdir failure")
}

type recordingRootRunner struct {
	binary string
	args   []string
}

func (runner *recordingRootRunner) Run(_ context.Context, binary string, args ...string) (execx.Result, error) {
	runner.binary = binary
	runner.args = append([]string(nil), args...)
	return execx.Result{}, nil
}

func TestRootUnlinkOnlyRemovesFilesOwnedByTheUninstallPlan(t *testing.T) {
	runner := &recordingRootRunner{}
	executor := Executor{Root: runner}
	if err := executor.rootUnlinkIfPlanned(context.Background(), UninstallPlan{}, TmpfilesPath); err != nil {
		t.Fatal(err)
	}
	if runner.binary != "" {
		t.Fatalf("unplanned retired file was unlinked with %s %v", runner.binary, runner.args)
	}
	plan := UninstallPlan{RemoveFiles: []string{TmpfilesPath}}
	if err := executor.rootUnlinkIfPlanned(context.Background(), plan, TmpfilesPath); err != nil {
		t.Fatal(err)
	}
	if runner.binary != "/usr/bin/unlink" || len(runner.args) != 1 || runner.args[0] != TmpfilesPath {
		t.Fatalf("planned unlink = %s %v", runner.binary, runner.args)
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
