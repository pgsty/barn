package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/activity"
	privatevm "github.com/pgsty/farrow/internal/private"
	"github.com/pgsty/farrow/internal/state"
)

func TestNoColorProgressStillRedrawsAndSharesOneRegion(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	var stderr bytes.Buffer
	output := &outputContext{format: outputText, stderrFile: true, stderrTTY: true, columns: 80}
	writer := &outputWriter{Writer: &stderr, context: output, stderr: true}
	parent := startProgress(context.Background(), writer, "Starting")
	child := startProgress(context.Background(), writer, "Network")
	child.Report(activity.Event{Phase: "guest-ready", Node: "meta", State: "ready"})
	child.Report(activity.Event{Phase: "guest-ready", Node: "node-1", State: "waiting for SSH"})
	parent.mu.Lock()
	parent.repaint(parent.started.Add(time.Second))
	parent.mu.Unlock()
	resume := suspendProgress(writer)
	bestEffortln(writer, "Password:")
	parent.mu.Lock()
	parent.repaint(parent.started.Add(2 * time.Second))
	parent.mu.Unlock()
	resume()
	child.Stop(nil)
	parent.Stop(nil)
	got := stderr.String()
	if child.parent != parent || !strings.Contains(got, "1/2 nodes ready") || !strings.Contains(got, "waiting for SSH") || !strings.Contains(got, "\x1b[1A") {
		t.Fatalf("missing shared live region: %q", got)
	}
	if strings.Contains(got, "\x1b[32m") || strings.Contains(got, "\x1b[0m") || !strings.HasSuffix(got, "Password:\n") {
		t.Fatalf("color or redraw interfered with prompt: %q", got)
	}
}

func TestProgressPlainLogsAreBounded(t *testing.T) {
	var stderr bytes.Buffer
	output := &outputContext{format: outputText, stderrFile: true}
	item := startProgress(context.Background(), &outputWriter{Writer: &stderr, context: output, stderr: true}, "Installing dependencies")
	item.mu.Lock()
	for second := range 31 {
		item.repaint(item.started.Add(time.Duration(second) * time.Second))
	}
	item.mu.Unlock()
	item.Stop(nil)
	if strings.Count(stderr.String(), "\n") != 2 || strings.Contains(stderr.String(), "\x1b") {
		t.Fatalf("plain log heartbeat flooded or used terminal controls: %q", stderr.String())
	}
}

func TestProgressFitsNarrowTerminalAndResumeRate(t *testing.T) {
	now := time.Now()
	event := activity.Event{Phase: "image-download", Message: "下载很长的虚拟机镜像名称", CurrentBytes: 300 << 20, StartBytes: 200 << 20, TotalBytes: 400 << 20, StartedAt: now.Add(-10 * time.Second)}
	for _, columns := range []int{40, 80, 120} {
		item := &progress{started: now.Add(-10 * time.Second), context: &outputContext{columns: columns}, event: event}
		for _, line := range item.render(now) {
			if displayWidth(line) >= columns {
				t.Fatalf("%d-column terminal wraps %q", columns, line)
			}
		}
	}
	if got := formatActivity(event, now); !strings.Contains(got, "10.0 MiB/s") || !strings.Contains(got, "ETA 10s") {
		t.Fatalf("resumed speed includes old bytes: %s", got)
	}
}

func TestTablesUseDisplayColumnsAndNarrowCards(t *testing.T) {
	if got := displayWidth("\x1b[32m中文e\u0301\x1b[0m"); got != 5 {
		t.Fatalf("display width = %d", got)
	}
	var out bytes.Buffer
	printTable(&out, []string{"NAME", "STATE"}, [][]string{{"中文", "ready"}, {"abc", "ready"}}, 1)
	lines := strings.Split(out.String(), "\n")
	for _, line := range lines[1:3] {
		prefix := strings.Split(line, "ready")[0]
		if displayWidth(prefix) != 6 {
			t.Fatalf("misaligned table: %q", out.String())
		}
	}
	out.Reset()
	writer := &outputWriter{Writer: &out, context: &outputContext{format: outputText, stdoutTTY: true, columns: 40}}
	path := "/long/path/that/must/not/be/truncated/for/recovery"
	printTable(writer, []string{"NAME", "PATH"}, [][]string{{"meta", path}}, -1)
	if !strings.Contains(out.String(), "path:") || !strings.Contains(out.String(), path) {
		t.Fatalf("narrow card lost recovery path: %q", out.String())
	}
}

func TestPartialLifecycleCountsMissingStateAndRetainsConnect(t *testing.T) {
	result := lifecycleResult{Source: "/tmp/my lab.yml", Status: privatevm.Status{Nodes: []privatevm.NodeStatus{{Name: "meta", Ready: true, State: state.Running, Runtime: "running", Address: "10.10.10.10"}}}, Failures: []privatevm.NodeFailure{{Node: "node-1", Error: "disk full"}, {Node: "node-2", Error: "disk full"}}, Warnings: []lifecycleWarning{{Code: "ssh_config", Message: "SSH aliases unavailable", Detail: "fixture full diagnostics"}}}
	var out bytes.Buffer
	printLifecycleResult(&out, "up", result, false)
	for _, want := range []string{"1/3 nodes ready", "farrow ssh meta", "farrow up -f '/tmp/my lab.yml' node-1 node-2", "SSH aliases unavailable"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	if strings.Count(out.String(), "disk full") != 1 || strings.Contains(out.String(), "fixture full diagnostics") {
		t.Fatalf("unfolded details: %s", out.String())
	}
	data, err := json.Marshal(result)
	if err != nil || !strings.Contains(string(data), "fixture full diagnostics") {
		t.Fatalf("JSON lost diagnostics: %s, %v", data, err)
	}
}

func TestBareCommandKeepsTheNextStepsCompact(t *testing.T) {
	t.Setenv("FARROW_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != exitUsage {
		t.Fatalf("missing-command exit contract changed: %d", code)
	}
	if !strings.Contains(out.String(), "farrow up") || !strings.Contains(out.String(), "farrow --help") || strings.Count(out.String(), "\n") > 8 || errOut.Len() != 0 {
		t.Fatalf("noisy onboarding: %s %s", out.String(), errOut.String())
	}
}

func TestProgressTimingsAccumulateRepeatedPhasesOnce(t *testing.T) {
	start := time.Now()
	item := &progress{timings: make(map[string]time.Duration)}
	item.recordPhase("preflight", start)
	item.recordPhase("preflight", start.Add(time.Second)) // another node update
	item.recordPhase("guest-ready", start.Add(2*time.Second))
	item.recordPhase("preflight", start.Add(5*time.Second)) // setup retry
	item.recordPhase("guest-ready", start.Add(6*time.Second))
	item.recordPhase("", start.Add(8*time.Second))
	if item.timings["preflight"] != 3*time.Second || item.timings["guest-ready"] != 5*time.Second || strings.Join(item.phaseOrder, ",") != "preflight,guest-ready" {
		t.Fatalf("repeated events double-counted phase time: %v %v", item.timings, item.phaseOrder)
	}
}

func TestVerboseNestedProgressPrintsOneTimingSummaryOnStderr(t *testing.T) {
	var stderr bytes.Buffer
	output := &outputContext{format: outputJSON, verbose: true}
	writer := &outputWriter{Writer: &stderr, context: output, stderr: true}
	parent := startProgress(context.Background(), writer, "Starting")
	child := startProgress(context.Background(), writer, "Preparing")
	child.Report(activity.Event{Phase: "preflight", Message: "Checking the host"})
	child.Report(activity.Event{Phase: "guest-ready", Node: "meta", State: "ready"})
	child.Stop(nil)
	parent.Stop(nil)
	parent.Stop(nil)
	if strings.Count(stderr.String(), "timing preflight") != 1 || strings.Count(stderr.String(), "timing guest-ready") != 1 || strings.Count(stderr.String(), "Starting finished") != 1 || strings.Contains(stderr.String(), "Preparing finished") {
		t.Fatalf("nested operation lost or duplicated timings: %s", stderr.String())
	}
}

func TestBootstrapFailureOffersInspectionInsteadOfEndlessRetry(t *testing.T) {
	result := lifecycleResult{Source: "/tmp/my lab.yml", Status: privatevm.Status{Nodes: []privatevm.NodeStatus{
		{Name: "meta", State: state.Running, Error: "guest bootstrap failed during data-disks"},
		{Name: "node-1", State: state.Running, Error: "guest did not become ready"},
	}}, Failures: []privatevm.NodeFailure{
		{Node: "meta", Stage: "bootstrap", Error: "guest bootstrap failed during data-disks"},
		{Node: "node-1", Stage: "readiness", Error: "guest did not become ready"},
	}}
	var out bytes.Buffer
	printLifecycleResult(&out, "up", result, false)
	for _, want := range []string{"setup failed", "farrow ssh meta", "farrow logs meta", "farrow recreate -f '/tmp/my lab.yml' meta", "replaces root and non-persistent disks", "farrow up -f '/tmp/my lab.yml' node-1"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "farrow up -f '/tmp/my lab.yml' meta") || strings.Contains(out.String(), "--force") {
		t.Fatalf("unsafe or ineffective recovery: %s", out.String())
	}
}

func TestSubsecondOperationsDoNotPrintTransientProgress(t *testing.T) {
	var out bytes.Buffer
	item := &progress{stderr: &out, started: time.Now(), event: activity.Event{Message: "Checking ready guest"}}
	item.repaint(item.started.Add(900 * time.Millisecond))
	if out.Len() != 0 {
		t.Fatalf("subsecond progress noise: %s", out.String())
	}
	item.repaint(item.started.Add(time.Second))
	if out.Len() == 0 {
		t.Fatal("long operations have no progress")
	}
}
