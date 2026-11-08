package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/activity"
	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/pgsty/barn/internal/state"
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
	if child.parent != parent || !strings.Contains(got, "boot 1/2 nodes:") || !strings.Contains(got, "waiting for SSH") || !strings.Contains(got, "\x1b[1A") {
		t.Fatalf("missing shared live region: %q", got)
	}
	if strings.Contains(got, "\x1b[32m") || strings.Contains(got, "\x1b[0m") || !strings.Contains(got, "Password:\n") || !strings.Contains(got, "barn ssh meta") {
		t.Fatalf("color or redraw interfered with prompt: %q", got)
	}
}

func TestProgressRendersEveryNodeWithAddressAndActivity(t *testing.T) {
	now := time.Now()
	item := &progress{
		context: &outputContext{columns: 100},
		started: now.Add(-time.Second),
		event:   activity.Event{Phase: "guest-ready", Message: "Starting nodes"},
		nodes:   make(map[string]activity.Event),
	}
	for index, name := range []string{"d12", "d13", "el10", "el9", "node-1", "node-2", "node-3"} {
		item.nodes[name] = activity.Event{
			Phase: "guest-ready", Node: name, Address: fmt.Sprintf("10.10.10.%d", index+10),
			State: "waiting for guest setup", Message: "waiting for guest setup",
		}
	}
	item.stopped = true
	lines := item.render(now)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "more waiting") || !strings.Contains(joined, "boot 0/7 nodes: 1s [DONE]") || len(lines) != 1+1+len(item.nodes) {
		t.Fatalf("node live region is incomplete: %d lines\n%s", len(lines), joined)
	}
	for index, name := range []string{"d12", "d13", "el10", "el9", "node-1", "node-2", "node-3"} {
		for _, want := range []string{name, fmt.Sprintf("10.10.10.%d", index+10), "waiting", "waiting for guest setup"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("node live region missing %q:\n%s", want, joined)
			}
		}
	}
	for _, line := range lines {
		if displayWidth(line) >= 100 {
			t.Fatalf("line wraps at terminal edge: %q", line)
		}
	}

	item.nodes["d12"] = activity.Event{Phase: "guest-ready", Node: "d12", Address: "10.10.10.10", State: "ready", Message: "SSH ready"}
	item.event.Phase = "event-log"
	if final := strings.Join(item.render(now), "\n"); !strings.Contains(final, "boot 1/7 nodes: 1s [DONE]") || !strings.Contains(final, "barn ssh d12") {
		t.Fatalf("final node activity lost connect command:\n%s", final)
	}
}

func TestProgressHidesMessageWhenTerminalIsNarrowAndCapsNodeColumn(t *testing.T) {
	longName := strings.Repeat("node", 20)
	item := &progress{
		context: &outputContext{columns: 60},
		started: time.Now().Add(-time.Second),
		event:   activity.Event{Phase: "guest-ready"},
		nodes: map[string]activity.Event{
			longName: {Phase: "guest-ready", Node: longName, Address: "10.10.10.10", State: "waiting for guest setup", Message: "waiting for guest setup"},
			"d12":    {Phase: "guest-ready", Node: "d12", Address: "10.10.10.12", State: "ready", Message: "SSH ready"},
		},
	}
	lines := item.render(time.Now())
	if strings.Contains(lines[0], "MESSAGE") || strings.Contains(strings.Join(lines, "\n"), "waiting for guest setup") {
		t.Fatalf("narrow table still rendered message column:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(lines[0], "NAME") || strings.Contains(strings.Join(lines, "\n"), longName) {
		t.Fatalf("node table did not use top aligned capped column:\n%s", strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		if displayWidth(line) >= 60 {
			t.Fatalf("narrow table wraps at terminal edge: %q", line)
		}
	}

	item.context = &outputContext{columns: 160}
	wideLines := item.render(time.Now())
	for _, line := range wideLines {
		addressIndex := strings.Index(line, "10.10.10.10")
		if addressIndex < 0 {
			continue
		}
		if displayWidth(strings.TrimRight(line[:addressIndex], " ")) > progressNodeMaxWidth {
			t.Fatalf("node column exceeded %d columns: %q", progressNodeMaxWidth, line)
		}
		return
	}
	t.Fatalf("wide table lost the long node address:\n%s", strings.Join(wideLines, "\n"))
}

func TestProgressUsesCompactStateColumnAndTerminalColors(t *testing.T) {
	item := &progress{
		context: &outputContext{columns: 100, color: true, stderrTTY: true},
		tty:     true,
		nodes: map[string]activity.Event{
			"d12": {Node: "d12", Address: "10.10.10.12", State: "ready"},
			"d13": {Node: "d13", Address: "10.10.10.13", State: "waiting for guest setup"},
		},
	}
	lines := item.render(time.Now())
	header := lines[0]
	if strings.Index(header, "STATE")-strings.Index(header, "ADDRESS") != len("10.10.10.12")+2 {
		t.Fatalf("state column has unexpected spacing: %q", header)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, ansiGreen+"ready") || !strings.Contains(joined, ansiCyan+"waiting") {
		t.Fatalf("state column is missing terminal colors: %q", joined)
	}
}

func TestStatusTableUsesNameAddressStateOrder(t *testing.T) {
	var out bytes.Buffer
	printStatusTable(&out, privatevm.Status{Nodes: []privatevm.NodeStatus{{
		Name: "d12", Address: "10.10.10.12", State: state.Running,
	}}}, false)
	first := strings.Split(out.String(), "\n")[0]
	nameIndex := strings.Index(first, "NAME")
	addressIndex := strings.Index(first, "ADDRESS")
	stateIndex := strings.Index(first, "STATE")
	if nameIndex < 0 || addressIndex <= nameIndex || stateIndex <= addressIndex {
		t.Fatalf("status table columns are not aligned with progress: %q", first)
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
	for _, want := range []string{"1/3 nodes ready", "barn ssh meta", "barn up -f '/tmp/my lab.yml' node-1 node-2", "SSH aliases unavailable"} {
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

func TestLifecycleResultListsEveryReadyNodeConnection(t *testing.T) {
	result := lifecycleResult{
		Status: privatevm.Status{Nodes: []privatevm.NodeStatus{
			{Name: "d12", Address: "10.10.10.12", State: state.Running, Runtime: "running", Ready: true},
			{Name: "d13", Address: "10.10.10.13", State: state.Running, Runtime: "running", Ready: true},
		}},
	}
	result.Message = "created and started 2 node(s)"
	var out bytes.Buffer
	printLifecycleResult(&out, "up", result, false)
	for _, name := range []string{"d12", "d13"} {
		if !strings.Contains(out.String(), "barn ssh "+name) {
			t.Fatalf("missing connection hint for %s: %s", name, out.String())
		}
	}
	if strings.Count(out.String(), "connect:") != 2 {
		t.Fatalf("connection hints were collapsed: %s", out.String())
	}
}

func TestFrozenLifecycleProgressDoesNotRepeatCompletionSummary(t *testing.T) {
	var out bytes.Buffer
	contextState := &outputContext{format: outputText, stdoutTTY: true, finalProgress: true}
	writer := &outputWriter{Writer: &out, context: contextState}
	result := lifecycleResult{Status: privatevm.Status{Nodes: []privatevm.NodeStatus{
		{Name: "d12", State: state.Running, Runtime: "running", Ready: true},
	}}}
	result.Message = "created and started 1 node(s)"
	printLifecycleResult(writer, "up", result, false)
	if out.Len() != 0 {
		t.Fatalf("frozen live result was repeated below the table: %s", out.String())
	}
}

func TestFrozenCleanupRetainsRecoveryAndNotes(t *testing.T) {
	var out bytes.Buffer
	writer := &outputWriter{Writer: &out, context: &outputContext{format: outputText, stdoutTTY: true, finalProgress: true}}
	result := lifecycleResult{
		Status:   privatevm.Status{Note: "update the inventory control role"},
		Failures: []privatevm.NodeFailure{{Node: "node-1", Stage: "stop", Error: "shutdown timed out"}},
	}
	printLifecycleResult(writer, "destroy", result, false)
	for _, want := range []string{"shutdown timed out", "barn stop node-1", "update the inventory control role"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("cleanup lost recovery context %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "barn up") || strings.Contains(out.String(), "NAME") {
		t.Fatalf("failed cleanup suggested starting a VM or repeated the table: %s", &out)
	}
}

func TestResizeRestartFailureSuggestsStart(t *testing.T) {
	result := lifecycleResult{
		Source:       "/tmp/lab.yml",
		retryOptions: lifecycleRetryOptions{Action: "resize"},
		Failures:     []privatevm.NodeFailure{{Node: "meta", Stage: "start", Error: "resized node meta but could not restart it"}},
	}
	var out bytes.Buffer
	printLifecycleResult(&out, "resize", result, false)
	if !strings.Contains(out.String(), "barn start meta") || strings.Contains(out.String(), "barn resize meta") {
		t.Fatalf("resize restart recovery hint = %s", out.String())
	}
}

func TestBareCommandPrintsFullHelp(t *testing.T) {
	t.Setenv("BARN_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != exitOK {
		t.Fatalf("bare barn must succeed: %d", code)
	}
	for _, want := range []string{"Usage:", "Setup:", "Lifecycle:", "Guest Access:", "barn up", "Use \"barn [command] --help\""} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("bare barn help missing %q:\n%s", want, out.String())
		}
	}
	var help bytes.Buffer
	if code := run([]string{"--help"}, &help, io.Discard); code != exitOK || help.String() != out.String() {
		t.Errorf("bare barn differs from barn --help (code %d)", code)
	}
	if errOut.Len() != 0 {
		t.Fatalf("bare barn help wrote stderr: %s", errOut.String())
	}
}

// Structured output keeps the documented actions[] document: one JSON value on
// stdout, never help text.
func TestBareCommandStructuredOutputKeepsActions(t *testing.T) {
	t.Setenv("BARN_HOME", t.TempDir())
	for _, format := range []string{"--json", "--yaml"} {
		var out, errOut bytes.Buffer
		if code := run([]string{format}, &out, &errOut); code != exitOK || errOut.Len() != 0 {
			t.Fatalf("%s bare barn code=%d stderr=%s", format, code, errOut.String())
		}
		if strings.Contains(out.String(), "Usage:") || !strings.Contains(out.String(), "barn up") {
			t.Fatalf("%s bare barn = %s", format, out.String())
		}
		if format == "--json" {
			var document struct {
				Actions []welcomeAction `json:"actions"`
			}
			if err := json.Unmarshal(out.Bytes(), &document); err != nil || len(document.Actions) == 0 {
				t.Fatalf("bare barn JSON = %v %s", err, out.String())
			}
		}
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
	for _, want := range []string{"setup failed", "barn ssh meta", "barn logs meta", "barn recreate -f '/tmp/my lab.yml' meta", "replaces root and non-persistent disks", "barn up -f '/tmp/my lab.yml' node-1"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "barn up -f '/tmp/my lab.yml' meta") || strings.Contains(out.String(), "--force") {
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
