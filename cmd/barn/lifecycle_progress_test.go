package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func TestLifecycleProgressCountsOnlyFinalStates(t *testing.T) {
	for _, test := range []struct {
		command, state, want string
	}{
		{"stop", "stopped", "stop 1/2"},
		{"destroy", "stopped", "destroy 0/2"},
		{"destroy", "removed", "destroy 1/2"},
		{"purge", "removed", "purge 0/2"},
		{"purge", "purged", "purge 1/2"},
		{"restart", "stopped", "restart 0/2"},
		{"restart", "ready", "restart 1/2"},
		{"provision", "running", "provision 0/2"},
		{"provision", "success", "provision 1/2"},
		{"provision", "failed", "provision 0/2"},
		{"resize", "ready", "resize 0/2"},
		{"resize", "resized", "resize 1/2"},
		{"resize", "unchanged", "resize 1/2"},
	} {
		t.Run(test.command+"/"+test.state, func(t *testing.T) {
			var output bytes.Buffer
			now := time.Now()
			item := &progress{
				stderr: &output, operation: test.command, context: &outputContext{columns: 40}, started: now.Add(-time.Second),
				event: activity.Event{Phase: "event-log"},
				nodes: map[string]activity.Event{
					"d12": {Node: "d12", Address: "10.10.10.12", State: test.state},
					"d13": {Node: "d13", Address: "10.10.10.13", State: "queued"},
				},
			}
			lines := item.render(now)
			if len(lines) != 4 || !strings.HasPrefix(lines[0], "NAME") || !strings.HasPrefix(lines[len(lines)-1], test.want) || strings.Contains(lines[0], "MESSAGE") {
				t.Fatalf("narrow progress lost command, node count or table: %q", lines)
			}
			item.event = item.nodes["d12"]
			item.logEvent(now)
			if strings.Contains(output.String(), "\x1b[") || !strings.Contains(output.String(), strings.Split(test.want, " ")[1]+" nodes") {
				t.Fatalf("plain log lost completion count or gained terminal controls: %q", &output)
			}
		})
	}
}

func TestLifecycleProgressStopsWithFailureOrCancellation(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	for _, test := range []struct {
		err    error
		marker string
	}{
		{errors.New("cleanup failed"), "[FAILED]"},
		{context.Canceled, "[CANCELLED]"},
	} {
		var output bytes.Buffer
		contextState := &outputContext{format: outputText, stderrFile: true, stderrTTY: true, columns: 100, color: true}
		writer := &outputWriter{Writer: &output, context: contextState, stderr: true}
		parent := startLifecycleProgress(context.Background(), writer, "purge")
		child := startLifecycleProgress(context.Background(), writer, "destroy")
		child.Report(activity.Event{Phase: "destroy", Node: "meta", Address: "10.10.10.10", State: "removed"})
		child.Stop(nil)
		parent.Stop(test.err)
		if got := output.String(); !strings.Contains(got, "purge 0/1") || !strings.Contains(got, test.marker) || strings.Contains(got, "[DONE]") {
			t.Fatalf("nested cleanup lost root operation or reported false success: %q", got)
		}
		style := ansiRed
		if errors.Is(test.err, context.Canceled) {
			style = ansiYellow
		}
		if !strings.Contains(output.String(), style+test.marker+ansiReset) {
			t.Fatalf("completion marker lost terminal color: %q", &output)
		}
	}
}

func TestDestroyAndPurgeFreezeTheirActualResult(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	for _, mode := range []string{"destroy", "purge", "failed-purge"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("BARN_HOME", root)
			t.Setenv("HOME", t.TempDir())
			resolved := quickResolved(false, false)
			peer := resolved.Nodes[0]
			peer.Name, peer.Address, peer.Control = "node-1", "10.10.10.11", false
			resolved.Nodes = append(resolved.Nodes, peer)
			hash, err := spec.Hash(resolved)
			if err != nil {
				t.Fatal(err)
			}
			if err := (state.Store{Root: root}).WriteDeployment(state.DeploymentState{
				Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash, Resolved: resolved, UpdatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
			// The fixture has desired nodes but no VMs. An unrecognized key
			// file lets node removal succeed while the final purge fails.
			if err := os.Mkdir(filepath.Join(root, "keys"), 0o700); err != nil {
				t.Fatal(err)
			}
			if mode == "failed-purge" {
				if err := os.WriteFile(filepath.Join(root, "keys", "unexpected-file"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			output := &outputContext{format: outputText, stdoutTTY: true, stderrTTY: true, stderrFile: true, columns: 100, color: true}
			out := &outputWriter{Writer: &stdout, context: output}
			errOut := &outputWriter{Writer: &stderr, context: output, stderr: true}
			var outcome commandOutcome
			if mode == "destroy" {
				outcome, err = runLifecycleCommand(context.Background(), mode, lifecycleOptions{Force: true}, nil, errOut)
			} else {
				outcome, err = runPurgeCommand(context.Background(), false, nil, errOut)
			}
			if mode == "failed-purge" {
				if err == nil || !strings.Contains(stderr.String(), "[FAILED]") || strings.Contains(stderr.String(), "[DONE]") || strings.Contains(stderr.String(), ansiGreen+"purged") {
					t.Fatalf("failed purge reported success: %v\n%s", err, &stderr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := outcome.text(out, errOut); err != nil {
				t.Fatal(err)
			}
			finalState := "removed"
			if mode == "purge" {
				finalState = "purged"
			}
			for _, want := range []string{mode + " 2/2 nodes:", "[DONE]", "NAME", "ADDRESS", "STATE", "10.10.10.10", "10.10.10.11", ansiGreen + finalState} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("missing %q in final progress:\n%s", want, &stderr)
				}
			}
			if stdout.Len() != 0 || strings.Contains(stderr.String(), "SSH ready") {
				t.Fatalf("cleanup repeated the table or printed an SSH connection: stdout=%q stderr=%q", &stdout, &stderr)
			}
		})
	}
}
