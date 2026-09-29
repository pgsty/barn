package macvm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyFixture writes the earlier two-slot layout: one shared subnet, IP
// pins, v1 records and no password files.
func legacyFixture(t *testing.T) (*Manager, map[string]string) {
	t.Helper()
	requireMac(t)
	s := testStore(t)
	m := &Manager{Store: s, SSHHome: t.TempDir()}
	m.hostRoutes = func(context.Context) ([]DarwinRoute, error) {
		// The earlier daemon's bridge still holds the shared subnet.
		return ParseDarwinRouteTable("Destination Gateway Flags Netif Expire\n10.10.20/24 link#34 UC bridge101 !\n10.10.10/24 link#19 UC bridge100 !\n")
	}
	held := holdStore(t, s)
	base := testBase(t, s, held, "26A428-legacy")
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	// A random installation never matches a daemon installed on this host.
	installation, _ := NewInstanceID()
	config := map[string]any{"schema_version": 1, "installation_id": installation,
		"network": map[string]any{"subnet": "10.10.20.0/24", "gateway": "10.10.20.1", "network_id": "8eedc0ff-89c1-4719-a7b0-c5d45be34823"},
		"slots": []map[string]any{{"name": "mac1", "ip": "10.10.20.10", "mac": "5e:40:07:08:6c:4e", "cpu": 4, "memory_bytes": DefaultMemoryBytes, "disk_bytes": DefaultDiskBytes},
			{"name": "mac2", "ip": "10.10.20.11", "mac": "b6:b2:45:43:e2:65", "cpu": 4, "memory_bytes": DefaultMemoryBytes, "disk_bytes": DefaultDiskBytes}},
		"default_base_id": base.ID, "created_at": time.Now().UTC()}
	writeFixtureJSON(t, filepath.Join(s.Root, "config.json"), config)
	ids := map[string]string{"mac1": "953f3ce8-110e-4f09-9d17-76a3ea6351e4", "mac2": "b421c297-f4db-419f-84bd-bd5ddfb8efad"}
	for name, id := range ids {
		dir, err := s.mkdir("slots", name)
		if err != nil {
			t.Fatal(err)
		}
		writeFixtureJSON(t, filepath.Join(dir, "state.json"), map[string]any{"schema_version": 1, "name": name, "instance_id": id, "base_id": base.ID,
			"state": "stopped", "user": "vonng", "initialized": true, "cpu": 4, "memory_bytes": DefaultMemoryBytes, "disk_bytes": DefaultDiskBytes,
			"version": "27.0", "build": "26A428", "observed_version": "27.0", "observed_build": "26A428", "password_ref": id, "created_at": time.Now().UTC()})
		ip := map[string]string{"mac1": "10.10.20.10", "mac2": "10.10.20.11"}[name]
		if err := os.WriteFile(filepath.Join(dir, "known_hosts"), []byte(ip+" ecdsa-sha2-nistp256 AAAAE2VjZHNh"+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ensureSSHKey(dir); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"disk.asif", "machine-id.bin", "auxiliary-storage.bin", "runner.lock"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(name+" "+file), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if installed, err := m.legacyHelperInstalled(context.Background()); err != nil || installed {
		t.Fatalf("fixture collides with an installed daemon: %v %v", installed, err)
	}
	return m, ids
}

func writeFixtureJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateConvertsTheEarlierLayoutAndKeepsData(t *testing.T) {
	m, ids := legacyFixture(t)
	if err := m.Store.SavePassword("mac2", "kept-password"); err != nil {
		t.Fatal(err)
	}
	var plan MigrationPlan
	report, err := m.Migrate(context.Background(), MigrateOptions{Confirm: func(p MigrationPlan) error { plan = p; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Machines) != 2 || plan.Machines[0].Address != "10.10.20.10" || plan.Machines[1].Address != "10.10.21.10" || plan.Machines[1].OldAddress != "10.10.20.11" {
		t.Fatalf("plan %+v", plan)
	}
	if plan.Machines[0].Password != "unavailable" || plan.Machines[1].Password != "kept" || plan.RemoveHelper {
		t.Fatalf("plan passwords/helper %+v", plan)
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "mac1") {
		t.Fatalf("warnings %v", report.Warnings)
	}
	config, err := m.Store.LoadConfig()
	if err != nil || !validUUID(config.InstallationID) || config.DefaultBaseID != "26A428-legacy" {
		t.Fatalf("config %+v %v", config, err)
	}
	for name, id := range ids {
		machine, err := m.Store.LoadMachine(name)
		if err != nil {
			t.Fatal(err)
		}
		if machine.InstanceID != id || !machine.Initialized || machine.State != "stopped" || machine.User != "vonng" {
			t.Fatalf("%s %+v", name, machine)
		}
		dir, _ := m.Store.MachinePath(name)
		for _, file := range []string{"disk.asif", "machine-id.bin", "auxiliary-storage.bin"} {
			if data, err := os.ReadFile(filepath.Join(dir, file)); err != nil || string(data) != name+" "+file {
				t.Fatalf("%s %s changed: %q %v", name, file, data, err)
			}
		}
		hosts, _ := os.ReadFile(filepath.Join(dir, "known_hosts"))
		if !strings.HasPrefix(string(hosts), machine.HostKeyAlias()+" ecdsa-sha2-nistp256 AAAAE2VjZHNh"+name) {
			t.Fatalf("%s pin %q", name, hosts)
		}
		for _, backup := range []string{"known_hosts.v1", "state.v1.json"} {
			if _, err := os.Stat(filepath.Join(dir, backup)); err != nil {
				t.Fatalf("%s backup %s: %v", name, backup, err)
			}
		}
	}
	mac1, _ := m.Store.LoadMachine("mac1")
	mac2, _ := m.Store.LoadMachine("mac2")
	if mac1.MAC != "5e:40:07:08:6c:4e" || mac2.MAC != "b6:b2:45:43:e2:65" || mac2.Network.Subnet != "10.10.21.0/24" {
		t.Fatalf("mac1 %+v mac2 %+v", mac1, mac2)
	}
	if password, err := m.Store.Password("mac2"); err != nil || password != "kept-password" {
		t.Fatalf("kept password %q %v", password, err)
	}
	if _, err := os.Stat(filepath.Join(m.Store.Root, "config.v1.json")); err != nil {
		t.Fatal("config backup missing")
	}
	again, err := m.Migrate(context.Background(), MigrateOptions{Confirm: func(MigrationPlan) error { t.Fatal("a finished migration asked again"); return nil }})
	if err != nil || !again.AlreadyCurrent {
		t.Fatalf("repeat %+v %v", again, err)
	}
}

func TestMigrateRefusesARunningEarlierMachine(t *testing.T) {
	m, _ := legacyFixture(t)
	dir, _ := m.Store.MachinePath("mac1")
	held := holdRunnerLockFile(t, filepath.Join(dir, "runner.lock"))
	defer func() { _ = held.Release() }()
	if _, err := m.Migrate(context.Background(), MigrateOptions{}); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("migrated a running machine: %v", err)
	}
	if schema, _ := schemaOf(filepath.Join(m.Store.Root, "config.json")); schema != legacySchemaVersion {
		t.Fatal("a refused migration changed the config")
	}
}

func TestMigrateOnEmptyStoreIsANoOp(t *testing.T) {
	requireMac(t)
	m := &Manager{Store: testStore(t)}
	report, err := m.Migrate(context.Background(), MigrateOptions{})
	if err != nil || !report.AlreadyCurrent {
		t.Fatalf("%+v %v", report, err)
	}
}
