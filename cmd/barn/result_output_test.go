package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/activity"
	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/pgsty/barn/internal/provision"
	"github.com/pgsty/barn/internal/spec"
)

func TestItemProgressSharesLayoutAndKeepsTransferMetrics(t *testing.T) {
	now := time.Now()
	item := &progress{enabled: true, tty: true, operation: "image pull", started: now.Add(-10 * time.Second), context: &outputContext{columns: 100, color: true, stderrTTY: true}}
	item.Report(activity.Event{Item: "u24", State: "checking"})
	item.Report(activity.Event{Phase: "image-download", Message: "downloading image", CurrentBytes: 100 << 20, TotalBytes: 200 << 20, StartedAt: now.Add(-10 * time.Second)})
	got := strings.Join(item.render(now), "\n")
	for _, want := range []string{"IMAGE", "MESSAGE", "downloading", "MiB/s", "ETA"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	item.Report(activity.Event{Item: "u24", State: "ready", Message: "verified cache", Done: true})
	item.stopped = true
	got = strings.Join(item.render(now), "\n")
	if strings.Contains(got, "SSH ready") || !strings.Contains(got, "verified cache") || !strings.Contains(got, ansiGreen+"ready") || !strings.Contains(got, "[DONE]") {
		t.Fatalf("invalid item completion: %s", got)
	}
	item.context.columns = 25
	got = strings.Join(item.render(now), "\n")
	if strings.Contains(got, "MESSAGE") || strings.Contains(got, "verified cache") {
		t.Fatalf("narrow item table: %s", got)
	}
}

func TestTallLiveTableFallsBackButFreezesEveryResult(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	var output bytes.Buffer
	state := &outputContext{format: outputText, stdoutTTY: true, stderrTTY: true, stderrFile: true, columns: 100, rows: 6}
	writer := &outputWriter{Writer: &output, context: state, stderr: true}
	item := startOperationProgress(context.Background(), writer, "checking host", "doctor")
	for i := 0; i < 12; i++ {
		item.Report(activity.Event{Item: fmt.Sprintf("check-%02d", i), State: "ok", Message: "passed", Done: true})
	}
	item.mu.Lock()
	lines := item.renderLive(time.Now())
	item.repaint(item.started.Add(time.Second))
	item.mu.Unlock()
	if len(lines) >= state.rows {
		t.Fatalf("live table exceeds viewport: %v", lines)
	}
	item.Stop(errors.New("last check failed"))
	got := output.String()
	if !strings.Contains(got, "[FAILED]") || strings.Contains(got, "[DONE]") || !lifecycleProgressFrozen(writer) {
		t.Fatalf("incorrect final result: %q", got)
	}
	for i := 0; i < 12; i++ {
		if !strings.Contains(got, fmt.Sprintf("check-%02d", i)) {
			t.Fatalf("missing check %d: %s", i, got)
		}
	}
	// The next operation owns a fresh result and cannot suppress its output
	// because a preceding operation froze a table.
	next := startOperationProgress(context.Background(), writer, "checking", "next")
	if lifecycleProgressFrozen(writer) {
		t.Error("previous frozen result leaked into next operation")
	}
	next.Stop(nil)
}

func TestStaticResultAdaptsWithoutMutatingRowsOrRedirectedDetails(t *testing.T) {
	rows := [][]string{{"meta", "10.10.10.10", "ready", "SSH ready: barn ssh meta"}}
	var output bytes.Buffer
	state := &outputContext{format: outputText, stdoutTTY: true, columns: 35, color: true}
	writer := &outputWriter{Writer: &output, context: state}
	printTable(writer, []string{"NAME", "ADDRESS", "STATE", "MESSAGE"}, rows, 2)
	if strings.Contains(output.String(), "MESSAGE") || !strings.Contains(output.String(), ansiGreen+"ready") {
		t.Fatalf("narrow colored table: %q", &output)
	}
	output.Reset()
	state.stdoutTTY = false
	printTable(writer, []string{"NAME", "ADDRESS", "STATE", "MESSAGE"}, rows, 2)
	if !strings.Contains(output.String(), rows[0][3]) || strings.Contains(output.String(), "\x1b") {
		t.Fatalf("redirected result lost text or gained controls: %q", &output)
	}
}

func TestPlanTableDistinguishesIntentAndMissingNodes(t *testing.T) {
	var out bytes.Buffer
	plan := privatevm.LifecyclePlan{Action: "blocked-removal", Nodes: []string{"meta", "peer"}, Resize: []string{"peer"}, Missing: []string{"old"}}
	resolved := spec.Resolved{Nodes: []spec.Node{{Name: "meta", Address: "10.10.10.10"}, {Name: "peer", Address: "10.10.10.11"}}}
	printPlanTable(&out, plan, resolved)
	for _, want := range []string{"NAME", "ADDRESS", "STATE", "unchanged", "planned", "would resize", "blocked", "explicit destroy required"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "resized") || strings.Contains(out.String(), "removed") {
		t.Fatalf("plan claimed applied changes: %s", &out)
	}
}

func TestPlanSummaryUsesResultStates(t *testing.T) {
	for _, test := range []struct {
		name, action, blocked, state, message string
	}{
		{name: "unchanged", action: "none", state: "unchanged", message: "no changes: 2 node(s) (meta, peer) match the applied inventory"},
		{name: "planned", action: "converge", state: "planned", message: "converge changes are ready to apply"},
		{name: "blocked", action: "blocked-removal", blocked: "remove old explicitly", state: "blocked", message: "blocked: remove old explicitly"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, message := planSummary(privatevm.LifecyclePlan{Action: test.action, Blocked: test.blocked, Nodes: []string{"meta", "peer"}})
			if state != test.state || message != test.message {
				t.Fatalf("summary = %q, %q; want %q, %q", state, message, test.state, test.message)
			}
		})
	}
}

func TestProvisionSummaryColorsCompletionMarker(t *testing.T) {
	state := &outputContext{format: outputText, color: true, stdoutTTY: true, columns: 100}
	var output bytes.Buffer
	writer := &outputWriter{Writer: &output, context: state}
	printProvisionReport(writer, writer, provision.Report{
		Results:    []provision.NodeResult{{Node: "meta", Address: "10.10.10.10", Success: true, ExitCode: 0, DurationMS: 10}},
		Successful: 1,
		DurationMS: 10,
	})
	if !strings.Contains(output.String(), ansiGreen+"[DONE]") {
		t.Fatalf("provision output = %q", output.String())
	}
}

func TestMacLiveResultUsesMacCommandAndCorrectOperationCount(t *testing.T) {
	item := &progress{operation: "mac start", stopped: true, started: time.Now(), context: &outputContext{columns: 100}, nodes: map[string]activity.Event{
		"dev": {Node: "dev", State: "ready", Message: "SSH ready: barn mac ssh dev"},
	}}
	got := strings.Join(item.render(time.Now()), "\n")
	if !strings.Contains(got, "mac boot 1/1 nodes") || !strings.Contains(got, "SSH ready: barn mac ssh dev") || strings.Contains(got, "SSH ready: barn ssh") {
		t.Fatalf("Mac result = %s", got)
	}
	item.operation = "mac destroy"
	item.nodes["dev"] = activity.Event{Node: "dev", State: "stopped"}
	if _, done, _ := item.nodeProgress(); done != 0 {
		t.Fatal("intermediate stop completed destroy")
	}
	item.nodes["dev"] = activity.Event{Node: "dev", State: "removed"}
	if _, done, _ := item.nodeProgress(); done != 1 {
		t.Fatal("removal did not complete destroy")
	}
}
