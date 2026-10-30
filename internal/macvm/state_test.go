package macvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMachineNames(t *testing.T) {
	for _, name := range []string{"mac1", "dev", "build-2", "a", strings.Repeat("a", 32)} {
		if !ValidName(name) {
			t.Errorf("rejected %q", name)
		}
	}
	for _, name := range []string{"", "1mac", "Mac1", "dev-", "-dev", "a_b", "a.b", "../x", strings.Repeat("a", 33)} {
		if ValidName(name) {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestReadEmptyStoreCreatesNothing(t *testing.T) {
	s := testStore(t)
	if _, err := s.LoadConfig(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config: %v", err)
	}
	machines, err := s.ListMachines()
	if err != nil || len(machines) != 0 {
		t.Fatalf("machines %v %v", machines, err)
	}
	if name, err := s.DefaultMachine(); err != nil || name != DefaultMachine {
		t.Fatalf("default %q %v", name, err)
	}
	if _, err := os.Stat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading created %s", s.Root)
	}
}

func TestStoreFollowsUserSelectedDirectoryAliases(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(parent, "physical")
	if err := os.MkdirAll(filepath.Join(physical, "existing"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)
	t.Setenv("HOME", parent)
	for _, test := range []struct{ name, path, target string }{
		{"physical", physical, physical},
		{"final-link", alias, physical},
		{"parent-link", filepath.Join(alias, "existing"), filepath.Join(physical, "existing")},
		{"missing-suffix", filepath.Join(alias, "missing", "barn"), filepath.Join(physical, "missing", "barn")},
		{"relative", "alias/relative", filepath.Join(physical, "relative")},
		{"home-relative", "~/alias/home-relative", filepath.Join(physical, "home-relative")},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, err := NewStore(test.path)
			if err != nil || s.Root != filepath.Join(test.target, "mac") {
				t.Fatalf("store = %#v, %v", s, err)
			}
			if _, err := s.ListMachines(); err != nil {
				t.Fatal(err)
			}
			// The first two spellings intentionally address the same store.
			if test.name != "final-link" {
				if _, err := os.Stat(s.Root); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("read created a store: %v", err)
				}
			}
			if err := s.SavePassword("mac1", "test-password"); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewStore(test.target)
			if err != nil {
				t.Fatal(err)
			}
			if password, err := reopened.Password("mac1"); err != nil || password != "test-password" {
				t.Fatalf("physical store read = %q, %v", password, err)
			}
		})
	}
}

func TestStoreStillRejectsOwnedSymlinksThroughHomeAlias(t *testing.T) {
	for _, component := range []string{"mac", "mac/slots", "mac/config.json"} {
		t.Run(component, func(t *testing.T) {
			physical := t.TempDir()
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(physical, alias); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(physical, component)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), path); err != nil {
				t.Fatal(err)
			}
			s, err := NewStore(alias)
			if err == nil {
				_, err = s.Path(strings.TrimPrefix(component, "mac/"))
			}
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("owned symlink accepted: %v", err)
			}
		})
	}
}

func TestMachineRoundTripAndIdentityIsImmutable(t *testing.T) {
	m, _ := testManager(t)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	loaded, err := m.Store.LoadMachine("dev")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.InstanceID != machine.InstanceID || loaded.Network.Address != "10.10.30.10" || loaded.Network.Gateway != "10.10.30.1" || !loaded.Clipboard {
		t.Fatalf("round trip changed the machine: %+v", loaded)
	}
	held := holdStore(t, m.Store)
	changed := *loaded
	changed.InstanceID, _ = NewInstanceID()
	if err := m.Store.SaveMachine(held, &changed); err == nil {
		t.Fatal("instance identity changed in place")
	}
}

func TestSaveMachineRequiresTheRightLock(t *testing.T) {
	m, _ := testManager(t)
	machine := testMachine(t, m, "mac1", "10.10.20.0/24", false)
	other, err := m.acquireOperation(context.Background(), "mac2", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Release() }()
	if err := m.Store.SaveMachine(other, machine); err == nil {
		t.Fatal("another machine's operation lock updated mac1")
	}
	own, err := m.acquireOperation(context.Background(), "mac1", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = own.Release() }()
	machine.CPU = 6
	if err := m.Store.SaveMachine(own, machine); err != nil {
		t.Fatal(err)
	}
	fresh := *machine
	fresh.Name = "mac3"
	if err := m.Store.SaveMachine(own, &fresh); err == nil {
		t.Fatal("an operation lock created a new machine")
	}
}

func TestDefaultMachineRules(t *testing.T) {
	m, _ := testManager(t)
	testMachine(t, m, "dev", "10.10.30.0/24", false)
	if name, err := m.Store.DefaultMachine(); err != nil || name != "dev" {
		t.Fatalf("single machine: %q %v", name, err)
	}
	testMachine(t, m, "build", "10.10.31.0/24", false)
	if _, err := m.Store.DefaultMachine(); err == nil || !strings.Contains(err.Error(), "build, dev") {
		t.Fatalf("ambiguous default: %v", err)
	}
	testMachine(t, m, "mac1", "10.10.20.0/24", false)
	if name, err := m.Store.DefaultMachine(); err != nil || name != "mac1" {
		t.Fatalf("mac1 default: %q %v", name, err)
	}
	machines, _ := m.Store.ListMachines()
	if len(machines) != 3 || machines[0].Name != "build" || machines[2].Name != "mac1" {
		t.Fatalf("not sorted: %v", machines)
	}
}

func TestUnsupportedSchemaIsRefused(t *testing.T) {
	s := testStore(t)
	if _, err := s.mkdir("slots", "mac1"); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(s.Root, "config.json")
	if err := os.WriteFile(config, []byte(`{"schema_version":1,"installation_id":"b7b59d76-5828-4627-9972-afcd17ce6c81"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "slots", "mac1", "state.json"), []byte(`{"schema_version":1,"name":"mac1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{func() error { _, err := s.LoadConfig(); return err }(), func() error { _, err := s.LoadMachine("mac1"); return err }(), func() error { _, err := s.ListMachines(); return err }()} {
		if err == nil || !strings.Contains(err.Error(), "unsupported mac metadata schema 1") {
			t.Fatalf("unsupported schema was not refused: %v", err)
		}
	}
}

func TestOrphanedMachineDirectoryIsPreserved(t *testing.T) {
	s := testStore(t)
	if _, err := s.mkdir("slots", "dev"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "slots", "dev", "disk.asif"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadMachine("dev"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a directory with files but no state looked absent: %v", err)
	}
}

func TestStoreRejectsSymlinksAndTraversal(t *testing.T) {
	s := testStore(t)
	if _, err := s.Path("..", "x"); err == nil {
		t.Fatal("traversal accepted")
	}
	if _, err := s.mkdir("slots"); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(s.Root, "slots", "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MachinePath("evil"); err == nil {
		t.Fatal("symlinked machine directory accepted")
	}
	if _, err := s.MachinePath("Evil"); err == nil {
		t.Fatal("invalid name accepted")
	}
}

func TestPasswordFileIsPrivate(t *testing.T) {
	s := testStore(t)
	if err := s.SavePassword("mac1", "secret\nline"); err == nil {
		t.Fatal("multi-line password accepted")
	}
	if err := s.SavePassword("mac1", "Fr9!secret"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Password("mac1"); err != nil || got != "Fr9!secret" {
		t.Fatalf("password %q %v", got, err)
	}
	path, _ := s.passwordPath("mac1")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Password("mac1"); err == nil {
		t.Fatal("readable password file accepted")
	}
	if _, err := s.Password("mac2"); err == nil || !strings.Contains(err.Error(), "no saved GUI password") {
		t.Fatalf("missing password: %v", err)
	}
	password, err := NewPassword()
	if err != nil || len(password) < 30 || strings.ContainsAny(password, "\n\r\x00") {
		t.Fatalf("generated password %q %v", password, err)
	}
}
