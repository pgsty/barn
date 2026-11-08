package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/activity"
)

func TestSSHProgressArrivesBeforeProcessExitAndPreservesCapture(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "ssh")
	// The process cannot finish until the test observes its first live line.
	script := "#!/bin/sh\nprintf 'installing packages\\n'\nwhile [ ! -f \"${0}.continue\" ]; do sleep 0.01; done\nprintf 'finished\\n'\nprintf 'diagnostic\\n' >&2\nexit 17\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshRunnerTestTimeout)
	defer cancel()
	events := make(chan activity.Event, 16)
	results := make(chan NodeResult, 1)
	target := fixtureTarget("meta")
	target.Address = "10.10.10.10"
	go func() {
		results <- (SSHRunner{SSHPath: path, Progress: func(event activity.Event) { events <- event }}).Run(ctx, target, fixtureScript(), false)
	}()
	select {
	case event := <-events:
		if event.Message != "installing packages" || event.State != "running" || event.Address != target.Address {
			t.Errorf("live event = %+v", event)
		}
	case result := <-results:
		t.Fatalf("process exited before live output: %+v", result)
	case <-ctx.Done():
		t.Fatal("no live output before deadline")
	}
	if err := os.WriteFile(path+".continue", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-results:
		if result.Success || result.ExitCode != 17 || result.Stdout != "installing packages\nfinished\n" || result.Stderr != "diagnostic\n" {
			t.Fatalf("capture = %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("process did not finish")
	}
}

func TestProgressRetainsNewestLineAfterCaptureLimit(t *testing.T) {
	output := newLimitedBuffer(32)
	var latest string
	live := &progressOutput{output: output, publish: func(line string) { latest = line }}
	if _, err := live.Write([]byte(strings.Repeat("x", 1<<20))); err != nil {
		t.Fatal(err)
	}
	if len(live.pending) > 4096 || len(latest) > 4096 {
		t.Fatal("unbounded live line")
	}
	if _, err := live.Write([]byte("\rnow ready\n")); err != nil {
		t.Fatal(err)
	}
	live.flush()
	if latest != "now ready" || output.String() != strings.Repeat("x", 32) || !output.truncated {
		t.Fatalf("latest=%q capture=%q truncated=%t", latest, output.String(), output.truncated)
	}
}

func TestExecutorProgressCoversFailuresAndCancelledQueue(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if cancelled {
			cancel()
		}
		runner := &recordingRunner{fail: map[string]bool{"node-1": true}}
		events := make(map[string][]activity.Event)
		targets := []Target{fixtureTarget("meta"), fixtureTarget("node-1")}
		targets[0].Address, targets[1].Address = "10.10.10.10", "10.10.10.11"
		report, err := (Executor{Runner: runner, OperationID: "fixture", Parallelism: 2, Progress: func(event activity.Event) { events[event.Node] = append(events[event.Node], event) }}).Execute(ctx, fixtureScript(), targets, false)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		for index, target := range targets {
			updates := events[target.Node]
			if len(updates) < 2 || updates[0].State != "queued" {
				t.Fatalf("missing queued node: %+v", updates)
			}
			last := updates[len(updates)-1]
			want := "success"
			if target.Node == "node-1" {
				want = "failed"
			}
			if cancelled {
				want = "cancelled"
			}
			if last.State != want || !last.Done || last.Address != target.Address || report.Results[index].Address != target.Address {
				t.Fatalf("final progress = %+v report = %+v", last, report)
			}
		}
		if cancelled && runner.maxActive != 0 {
			t.Fatal("cancelled queue still invoked runner")
		}
	}
}
