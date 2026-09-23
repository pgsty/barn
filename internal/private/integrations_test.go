package private

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/lock"
)

func TestPrivateSSHConfigInstallContainsEveryNodeAndAddress(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	t.Setenv("FARROW_HOME", startConfig.Deployment.Root)
	keysDir := filepath.Join(startConfig.Deployment.Root, "keys")
	if err := os.Mkdir(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"id_ed25519": 0o600, "known_hosts": 0o600} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte("fixture"), mode); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	manager := Manager{FarrowVersion: "test"}
	standalone, err := manager.SSHConfig(context.Background())
	if err != nil || !strings.Contains(standalone, "Host meta 10.10.10.10 admin.example") {
		t.Fatalf("standalone SSH config=%q err=%v", standalone, err)
	}
	if !strings.Contains(standalone, `IdentityFile "`) || !strings.Contains(standalone, `UserKnownHostsFile "`) {
		t.Fatalf("standalone SSH paths are not quoted: %q", standalone)
	}
	selectedManager := manager
	selectedManager.Nodes = []string{"node-1"}
	selected, err := selectedManager.SSHConfig(context.Background())
	if err != nil || !strings.Contains(selected, "Host node-1 10.10.10.11") || strings.Contains(selected, "Host meta ") {
		t.Fatalf("selected SSH config=%q err=%v", selected, err)
	}
	result, err := manager.InstallSSHConfig(context.Background(), "lab", home)
	if err != nil || !result.Changed {
		t.Fatalf("install result=%#v err=%v", result, err)
	}
	fragment, err := os.ReadFile(result.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Host lab-meta meta 10.10.10.10 admin.example", "Host lab-node-1 node-1 10.10.10.11", "Port 2222", "Port 2223"} {
		if !strings.Contains(string(fragment), expected) {
			t.Fatalf("fragment lacks %q:\n%s", expected, fragment)
		}
	}
	second, err := manager.InstallSSHConfig(context.Background(), "lab", home)
	if err != nil || second.Changed {
		t.Fatalf("idempotent install result=%#v err=%v", second, err)
	}
	removed, err := manager.RemoveSSHConfig("lab", home)
	if err != nil || !removed.Changed {
		t.Fatalf("remove result=%#v err=%v", removed, err)
	}
}

func TestPrivateSSHConfigSkipsDesiredNodesWithoutCommittedState(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	t.Setenv("FARROW_HOME", startConfig.Deployment.Root)
	if err := os.Remove(filepath.Join(startConfig.Deployment.Root, "nodes", "node-1", "state.json")); err != nil {
		t.Fatal(err)
	}
	keysDir := filepath.Join(startConfig.Deployment.Root, "keys")
	if err := os.Mkdir(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"id_ed25519", "known_hosts"} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := (Manager{FarrowVersion: "test"}).InstallSSHConfig(context.Background(), "farrow", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := os.ReadFile(result.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fragment), "farrow-meta meta") || strings.Contains(string(fragment), "node-1") {
		t.Fatalf("partial deployment SSH fragment:\n%s", fragment)
	}
	if _, err := (Manager{FarrowVersion: "test", Nodes: []string{"node-1"}}).InstallSSHConfig(context.Background(), "farrow", t.TempDir()); err == nil || !strings.Contains(err.Error(), "has not been created") {
		t.Fatalf("explicit missing-node SSH config error = %v", err)
	}
}

func TestPrivateHostEntriesIncludeDeclaredAliases(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	t.Setenv("FARROW_HOME", startConfig.Deployment.Root)
	manager := Manager{FarrowVersion: "test"}
	entries, err := manager.HostEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("host entries = %#v", entries)
	}
	if entries[0].Address != "10.10.10.10" || strings.Join(entries[0].Names, ",") != "meta,admin.example" || entries[1].Address != "10.10.10.11" || strings.Join(entries[1].Names, ",") != "node-1" {
		t.Fatalf("host entries = %#v", entries)
	}
}

func TestPrivateSSHConfigRejectsSymlinkedKeysDirectory(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	t.Setenv("FARROW_HOME", startConfig.Deployment.Root)
	outside := t.TempDir()
	for name := range map[string]struct{}{"id_ed25519": {}, "known_hosts": {}} {
		if err := os.WriteFile(filepath.Join(outside, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(startConfig.Deployment.Root, "keys")); err != nil {
		t.Fatal(err)
	}
	manager := Manager{FarrowVersion: "test"}
	if _, err := manager.InstallSSHConfig(context.Background(), "lab", t.TempDir()); err == nil {
		t.Fatal("symlinked deployment keys directory was accepted")
	}
}

func TestConnectionsLockedRequiresAndReusesExclusiveDeploymentLock(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	t.Setenv("FARROW_HOME", startConfig.Deployment.Root)
	keysDir := filepath.Join(startConfig.Deployment.Root, "keys")
	if err := os.Mkdir(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"id_ed25519", "known_hosts"} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := Manager{FarrowVersion: "test"}
	if _, err := manager.ConnectionsLocked(context.Background(), startConfig.Deployment, nil); err == nil || !strings.Contains(err.Error(), "exclusive deployment lock") {
		t.Fatalf("missing token error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(startConfig.Deployment.Root, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	deploymentLock, err := lock.Acquire(context.Background(), deploymentLockPath(startConfig.Deployment.Root), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := deploymentLock.Release(); err != nil {
			t.Errorf("release integration test lock: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = manager.ConnectionsLocked(ctx, startConfig.Deployment, deploymentLock)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("locked snapshot error = %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("locked helper attempted to reacquire its own deployment lock: %v", ctx.Err())
	}
}
