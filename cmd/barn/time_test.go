package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/linuxvm"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/timesync"
	"github.com/pgsty/barn/internal/version"
)

func TestTimeNoDeploymentAndInvalidTimeout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("BARN_HOME", root)
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"time", "status"}, exitConflict},
		{[]string{"time", "sync", "--timeout", "0s"}, exitUsage},
		{[]string{"time", "sync", "--timeout", "11m"}, exitUsage},
		{[]string{"time", "auto", "enable", "meta"}, exitUsage},
		{[]string{"--json", "time", "sync", "--automatic"}, exitOK},
	} {
		var out, stderr bytes.Buffer
		if code := run(test.args, &out, &stderr); code != test.code {
			t.Fatalf("%v: code %d stdout=%s stderr=%s", test.args, code, &out, &stderr)
		}
		if test.code == exitOK {
			var result timeRun
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Message != "no deployment" || stderr.Len() != 0 {
				t.Fatalf("automatic no-op: stdout=%s stderr=%s err=%v", &out, &stderr, err)
			}
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("time command created an empty deployment: %v", err)
	}
}

func TestTimePreservesStructuredErrorContractAndQuietStderr(t *testing.T) {
	emptyRoot := filepath.Join(t.TempDir(), "absent")
	t.Setenv("BARN_HOME", emptyRoot)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--json", "time", "status"}, &stdout, &stderr); code != exitConflict {
		t.Fatalf("no deployment code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	if stderr.String() != "error: no deployment state found\nnext: barn up\n" {
		t.Fatalf("structured time status stderr = %q", stderr.String())
	}
	var got commandFailure
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || got.Error != "conflict" || got.Reason != "no_deployment" || got.Next != "barn up" {
		t.Fatalf("no deployment JSON = %+v err=%v raw=%s", got, err, &stdout)
	}

	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	resolved := quickResolved(false, false)
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := (state.Store{Root: root}).WriteDeployment(state.DeploymentState{
		Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash,
		Resolved: resolved, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		args   []string
		reason string
	}{
		{[]string{"--json", "time", "status", "missing"}, "unknown_node"},
		{[]string{"--json", "time", "sync", "meta", "meta"}, ""},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(test.args, &stdout, &stderr); code != exitUsage || strings.Contains(stderr.String(), "Checking guest clocks") {
			t.Fatalf("%v: code=%d stdout=%s stderr=%q", test.args, code, &stdout, stderr.String())
		}
		got = commandFailure{}
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || got.Error != "usage" || got.Reason != test.reason {
			t.Fatalf("%v: failure=%+v err=%v raw=%s", test.args, got, err, &stdout)
		}
	}

	// Successful structured call leaves stderr completely empty.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--json", "time", "status"}, &stdout, &stderr); code != exitOK || stderr.Len() != 0 {
		t.Fatalf("structured success: code=%d stdout=%s stderr=%q", code, &stdout, stderr.String())
	}
}

func TestRenderTimeFormatsTableWithNTPSourceAndPurgeCleansTimeFiles(t *testing.T) {
	synced := true
	runValue := timeRun{
		TimeReport: linuxvm.TimeReport{
			Source: "host",
			Nodes: []linuxvm.TimeNode{
				{
					Name: "meta",
					Result: timesync.Result{
						Action: "adjusted",
						Before: &timesync.Measurement{OffsetMS: -12000.5, UncertaintyMS: 0.12},
						After:  &timesync.Measurement{OffsetMS: 0.03, UncertaintyMS: 0.09},
						NTP:    timesync.NTP{Synchronized: &synced, Service: "chrony", Source: "10.10.10.1"},
					},
				},
				{Name: "node-1", Result: timesync.Result{Action: "skipped-stopped"}},
			},
		},
		At: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}
	var out bytes.Buffer
	if err := renderTime(&out, runValue, false); err != nil {
		t.Fatal(err)
	}
	rendered := out.String()
	for _, want := range []string{"NAME", "STATE", "OFFSET", "NTP", "MESSAGE", "adjusted", "synced (chrony, 10.10.10.1)", "skipped-stopped"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered time output missing %q:\n%s", want, rendered)
		}
	}

	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARN_HOME", root)
	for _, name := range []string{"time-sync.json", "time-last.json"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := (linuxvm.Manager{BarnVersion: version.Version}).PurgeDeployment(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"time-sync.json", "time-last.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("purgeDeployment left %s behind: %v", name, err)
		}
	}
}
