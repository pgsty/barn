package private

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/state"
)

func privateKeyPurgeFixture(t *testing.T) (Manager, Deployment, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	deploymentValue := Deployment{Root: root}
	keysDir := filepath.Join(root, "keys")
	if err := os.Mkdir(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"id_ed25519": 0o600, "id_ed25519.pub": 0o644, "known_hosts": 0o600} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte("owned"), mode); err != nil {
			t.Fatal(err)
		}
	}
	return Manager{}, deploymentValue, keysDir
}

func TestPrivatePurgeKeysPlansAppliesAndIsIdempotent(t *testing.T) {
	manager, deploymentValue, keysDir := privateKeyPurgeFixture(t)
	report, err := manager.PurgeKeys(context.Background(), false)
	if err != nil || report.Apply || len(report.Actions) != 3 {
		t.Fatalf("plan = %#v, %v", report, err)
	}
	for _, action := range report.Actions {
		if action.Applied {
			t.Fatalf("plan action was applied: %#v", action)
		}
		if _, err := os.Lstat(action.Path); err != nil {
			t.Fatalf("plan removed %s: %v", action.Path, err)
		}
	}
	report, err = manager.PurgeKeys(context.Background(), true)
	if err != nil || !report.Apply || len(report.Actions) != 3 {
		t.Fatalf("apply = %#v, %v", report, err)
	}
	for _, action := range report.Actions {
		if !action.Applied {
			t.Fatalf("apply action was not marked applied: %#v", action)
		}
	}
	if _, err := os.Lstat(keysDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("keys directory remains: %v", err)
	}
	if _, err := os.Lstat(deploymentValue.Root); err != nil {
		t.Fatalf("deployment root was removed: %v", err)
	}
	report, err = manager.PurgeKeys(context.Background(), true)
	if err != nil || len(report.Actions) != 0 {
		t.Fatalf("idempotent apply = %#v, %v", report, err)
	}
}

func TestPrivatePurgeKeysBlocksRetainedArtifactsWithoutDeletingKeys(t *testing.T) {
	manager, deploymentValue, keysDir := privateKeyPurgeFixture(t)
	nodeDir, err := deploymentValue.EnsureNodeDir("meta")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "root.qcow2"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = manager.PurgeKeys(context.Background(), true)
	var stateErr *KeyPurgeStateError
	if !errors.As(err, &stateErr) || !strings.Contains(err.Error(), "retained node artifact") {
		t.Fatalf("retained artifact error = %T %v", err, err)
	}
	for _, name := range []string{"id_ed25519", "id_ed25519.pub", "known_hosts"} {
		if _, err := os.Lstat(filepath.Join(keysDir, name)); err != nil {
			t.Fatalf("blocked purge removed %s: %v", name, err)
		}
	}
}

func TestPrivatePurgeKeysReportsLiveProcessAndDataDisksAsState(t *testing.T) {
	for _, test := range []struct {
		name string
		node state.NodeState
		want string
	}{
		{name: "live", node: state.NodeState{Process: state.ProcessIdentity{PID: os.Getpid()}}, want: "live recorded process"},
		{name: "data", node: state.NodeState{DataDisks: []state.DataDisk{{Name: "data1", Path: "/fixture/data.qcow2"}}}, want: "data disk"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			manager, deploymentValue, _ := privateKeyPurgeFixture(t)
			now := time.Now().UTC()
			test.node.Schema = state.NodeSchema
			test.node.BarnVersion = "test"
			test.node.Node = "meta"
			test.node.VMUUID = "018f4b8e-1234-4abc-9def-0123456789ab"
			test.node.Phase = state.Stopped
			test.node.Generation = 1
			test.node.SpecHash = strings.Repeat("a", 64)
			test.node.CreatedAt = now
			test.node.UpdatedAt = now
			if err := (state.Store{Root: deploymentValue.Root}).WriteNode(test.node); err != nil {
				t.Fatal(err)
			}
			_, err := manager.PurgeKeys(context.Background(), true)
			var stateErr *KeyPurgeStateError
			if !errors.As(err, &stateErr) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("state preflight error = %T %v", err, err)
			}
		})
	}
}

func TestPrivatePurgeKeysRejectsUnsafeKeyArtifactsAndPreservesOutside(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, string)
	}{
		{
			name: "unexpected-entry",
			mutate: func(t *testing.T, _ string, keysDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(keysDir, "manual-key"), []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, outsideDir, keysDir string) {
				t.Helper()
				outside := filepath.Join(outsideDir, "outside-symlink")
				if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(keysDir, "known_hosts")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(keysDir, "known_hosts")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			manager, _, keysDir := privateKeyPurgeFixture(t)
			test.mutate(t, t.TempDir(), keysDir)
			_, err := manager.PurgeKeys(context.Background(), true)
			var integrityErr *KeyPurgeIntegrityError
			if !errors.As(err, &integrityErr) {
				t.Fatalf("unsafe key error = %T %v", err, err)
			}
			if _, err := os.Lstat(filepath.Join(keysDir, "id_ed25519.pub")); err != nil {
				t.Fatalf("integrity failure removed a safe peer key: %v", err)
			}
		})
	}
}
