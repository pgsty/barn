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
