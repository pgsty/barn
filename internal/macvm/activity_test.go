package macvm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pgsty/barn/internal/activity"
)

func TestQueuedProgressDoesNotRecoverMachineState(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	pending := filepath.Join(store.Root, "slots", deletingMachinePrefix+"dev")
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Store: store}
	m.queueNodes([]string{"dev"})
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("rendering progress changed pending recovery state: %v", err)
	}
}

func TestConcurrentMachineProgressKeepsNativeOutputInItsOwnRow(t *testing.T) {
	var mu sync.Mutex
	var events []activity.Event
	m := &Manager{Report: func(event activity.Event) { mu.Lock(); defer mu.Unlock(); events = append(events, event) }}
	m.Runner.Progress = NewProgressWriter(m.Report)
	var wg sync.WaitGroup
	for _, name := range []string{"mac1", "mac2"} {
		child := m.nodeProgress(name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				_, _ = fmt.Fprintf(child.Runner.Progress, "{\"phase\":\"%s\"}\n", name)
			}
			child.Report.Report(activity.Event{Item: "base", State: "ready", Done: true})
		}()
	}
	wg.Wait()
	for _, event := range events {
		if event.Item != "" || event.Done || event.State == "ready" {
			t.Fatalf("nested stage falsely completed the VM: %+v", event)
		}
		if event.Message != "" && event.Message != event.Node {
			t.Fatalf("native output crossed machine rows: %+v", event)
		}
	}
	if len(events) != 62 {
		t.Fatalf("lost native updates: %d", len(events))
	}
}

func TestStopProgressKeepsSuccessfulAndFailedMachines(t *testing.T) {
	m := lifecycleManager(t)
	testMachine(t, m, "dev", "10.10.30.0/24", true)
	var mu sync.Mutex
	events := make(map[string][]activity.Event)
	m.Report = func(event activity.Event) {
		mu.Lock()
		defer mu.Unlock()
		events[event.Node] = append(events[event.Node], event)
	}
	report, err := m.Stop(context.Background(), []string{"dev", "missing"}, true)
	if err == nil || len(report.Machines) != 1 {
		t.Fatalf("partial stop = %+v %v", report, err)
	}
	for name, want := range map[string]string{"dev": "stopped", "missing": "failed"} {
		updates := events[name]
		if len(updates) < 2 || updates[0].State != "queued" {
			t.Fatalf("missing queued row: %+v", updates)
		}
		last := updates[len(updates)-1]
		if last.State != want || !last.Done {
			t.Fatalf("wrong final row: %+v", last)
		}
		if name == "dev" && last.Address == "" {
			t.Fatal("final node lost address")
		}
	}
}

func TestMacCompletionDistinguishesNoWaitAndSSHReady(t *testing.T) {
	var events []activity.Event
	m := &Manager{Report: func(event activity.Event) { events = append(events, event) }}
	m.finishNode("dev", Outcome{Action: "started", State: "running"}, nil)
	m.finishNode("dev", Outcome{Action: "started", State: "running", Ready: true}, nil)
	if events[0].State != "started" || strings.Contains(events[0].Message, "SSH ready") || events[1].Message != "SSH ready: barn mac ssh dev" {
		t.Fatalf("completion = %+v", events)
	}
}
