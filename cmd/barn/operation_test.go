package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/diagnostics"
	"github.com/pgsty/barn/internal/spec"
)

func TestSetupFailureIsTraceableWithoutDeployment(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("BARN_HOME", root)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"setup", "-f", filepath.Join(t.TempDir(), "missing-sensitive-input.yml"), "--json"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("code=%d output=%s %s", code, &stdout, &stderr)
	}
	var setup setupResult
	if err := json.Unmarshal(stdout.Bytes(), &setup); err != nil || setup.OperationID == "" {
		t.Fatalf("missing setup operation: %s %v", &stdout, err)
	}
	if _, err := os.Stat(filepath.Join(root, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup failure created deployment: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"logs", "--source", "events", "--json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("logs code=%d: %s %s", code, &stdout, &stderr)
	}
	var logs logResult
	if err := json.Unmarshal(stdout.Bytes(), &logs); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(logs.Content), "\n")
	if len(lines) != 2 || strings.Contains(logs.Content, "sensitive-input") {
		t.Fatalf("unsafe or incomplete trace: %s", logs.Content)
	}
	for index, line := range lines {
		var event diagnostics.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.OperationID != setup.OperationID || event.Action != "setup" {
			t.Fatalf("unrelated operation: %+v", event)
		}
		if index == 1 && event.Level != "error" {
			t.Fatalf("lost failure: %+v", event)
		}
	}
}

func TestNestedSetupAndLifecycleReuseOperationID(t *testing.T) {
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "state"))
	ctx, id, err := operationContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, setupErr := runSetupCommand(ctx, "invalid-profile", setupCLIOptions{Mode: "host"}, outputJSON, false, io.Discard)
	var setupFailure typedCommandError
	if !errors.As(setupErr, &setupFailure) || setupFailure.commandFailure().OperationID != id {
		t.Fatalf("setup lost parent operation: %v", setupErr)
	}
	for range 2 {
		_, retryErr := runPrivateCommand(ctx, "up", spec.Resolved{}, nil, "", "applied deployment state", false, false, false, false, false, io.Discard)
		var failure typedCommandError
		if !errors.As(retryErr, &failure) || failure.commandFailure().OperationID != id {
			t.Fatalf("retry changed parent operation: %v", retryErr)
		}
	}
}

func TestSetupDryRunDoesNotCreateDiagnosticState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("BARN_HOME", root)
	_, _ = runSetupCommand(context.Background(), "invalid-profile", setupCLIOptions{Mode: "host", DryRun: true}, outputJSON, false, io.Discard)
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run wrote diagnostic state: %v", err)
	}
}

func TestEarlyLifecycleFailureIncludesOperationID(t *testing.T) {
	t.Setenv("BARN_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"up", "-f", filepath.Join(t.TempDir(), "missing.yml"), "--json"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("code=%d: %s %s", code, &stdout, &stderr)
	}
	var failure commandFailure
	if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil || failure.OperationID == "" {
		t.Fatalf("early failure lost operation id: %s %v", &stdout, err)
	}
}
