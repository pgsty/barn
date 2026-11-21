package private

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/timesync"
)

func TestTimeSkipsStoppedAndRejectsUnknownOrDuplicateNodes(t *testing.T) {
	config, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", config.Deployment.Root)
	manager := Manager{LookPath: func(string) (string, error) { return "ssh", nil }}
	report, err := manager.Time(context.Background(), timesync.Options{Sync: true})
	if err != nil || len(report.Nodes) != 2 {
		t.Fatalf("stopped: %+v %v", report, err)
	}
	for _, node := range report.Nodes {
		if node.Action != "skipped-stopped" {
			t.Fatalf("started a stopped node: %+v", node)
		}
	}
	manager.Nodes = []string{"missing"}
	if _, err := manager.Time(context.Background(), timesync.Options{}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("unknown selector accepted: %v", err)
	}
	manager.Nodes = []string{"meta", "meta"}
	_, err = manager.Time(context.Background(), timesync.Options{})
	if class, _, _ := failure.Classify(err); class != failure.Usage {
		t.Fatalf("duplicate selector class = %q, err = %v", class, err)
	}
}

func TestTimeSettlesExitedRunningProcessToSkippedStopped(t *testing.T) {
	config, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", config.Deployment.Root)
	store := state.Store{Root: config.Deployment.Root}
	meta, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	meta.Phase = state.Running
	meta.Process = state.ProcessIdentity{PID: exited.Process.Pid, Executable: "/nonexistent/qemu-system-barn-test", Started: "before-reboot", ArgvHash: "hash"}
	if err := store.WriteNode(meta); err != nil {
		t.Fatal(err)
	}
	manager := Manager{
		LookPath: func(string) (string, error) { return "ssh", nil },
	}
	report, err := manager.Time(context.Background(), timesync.Options{Sync: true, Automatic: true, SkipBusy: true})
	if err != nil || len(report.Nodes) != 2 {
		t.Fatalf("exited VM caused scheduled check failure: %+v %v", report, err)
	}
	for _, node := range report.Nodes {
		if node.Action != "skipped-stopped" {
			t.Fatalf("expected skipped-stopped after settling exited VM: %+v", node)
		}
	}
	settled, err := store.ReadNode("meta")
	if err != nil || settled.Phase != state.Stopped {
		t.Fatalf("exited VM state was not converged to stopped: %+v %v", settled, err)
	}
}

func TestAutomaticTimeDoesNotQueueBehindLifecycleOrCreateDeployment(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	t.Setenv("BARN_HOME", root)
	manager := Manager{}
	report, err := manager.Time(context.Background(), timesync.Options{Sync: true, Automatic: true, SkipBusy: true})
	if err != nil || report.Message != "no deployment" {
		t.Fatalf("empty: %+v %v", report, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("created empty deployment: %v", err)
	}
	config, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", config.Deployment.Root)
	held, err := AcquireLock(context.Background(), config.Deployment, false)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			if err := held.Release(); err != nil {
				t.Error(err)
			}
		})
	}
	defer release()
	report, err = manager.Time(context.Background(), timesync.Options{Sync: true, Automatic: true, SkipBusy: true})
	if err != nil || !strings.HasPrefix(report.Message, "skipped:") {
		t.Fatalf("busy: %+v %v", report, err)
	}

	// Foreground lifecycle SyncGuestTime waits for a brief lock holder instead of
	// spuriously reporting skipped.
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(50 * time.Millisecond)
		release()
	}()
	manager.LookPath = func(string) (string, error) { return "ssh", nil }
	if err := manager.SyncGuestTime(context.Background(), []string{"meta"}); err != nil {
		t.Fatalf("foreground SyncGuestTime did not wait for lock release: %v", err)
	}
	<-released
}

func TestPruneClockRecordsRemovesSelectedAndWholeDeploymentHistory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "time-sync.json")
	if err := os.WriteFile(path, []byte("{\n  \"meta\": {\"uuid\": \"u1\", \"adjusted_at\": \"2026-11-20T00:00:00Z\"},\n  \"node-1\": {\"uuid\": \"u2\", \"adjusted_at\": \"2026-11-20T00:00:00Z\"}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pruneClockRecords(root, []string{"node-1"}, false); err != nil {
		t.Fatal(err)
	}
	records, err := readClockRecords(root)
	if err != nil || len(records) != 1 || records["meta"].UUID != "u1" {
		t.Fatalf("partial prune = %+v %v", records, err)
	}
	if err := pruneClockRecords(root, []string{"meta"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("whole-deployment prune left time-sync.json: %v", err)
	}
}
