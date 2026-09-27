package macvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/lock"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "farrow"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func holdStore(t *testing.T, s *Store) *lock.File {
	t.Helper()
	held, err := (&Manager{Store: s}).acquireState(context.Background(), "test state")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := held.Release(); err != nil {
			t.Error(err)
		}
	})
	return held
}

func testBase(t *testing.T, s *Store, held *lock.File, id string) *BaseImage {
	t.Helper()
	b := &BaseImage{SchemaVersion: SchemaVersion, ID: id, Version: "27.0", Build: "26A428", HardwareModelHash: strings.Repeat("a", 64), InstallerSHA256: strings.Repeat("b", 64), State: "ready", DiskBytes: DefaultDiskBytes, RecipeVersion: 1, CreatedAt: time.Now().UTC()}
	if err := s.SaveBase(held, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func testSlot(t *testing.T, name, baseID string) *Slot {
	t.Helper()
	id, err := NewInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	return &Slot{SchemaVersion: SchemaVersion, Name: name, InstanceID: id, BaseID: baseID, State: "created", User: "farrow", CPU: DefaultCPU, MemoryBytes: DefaultMemoryBytes, DiskBytes: DefaultDiskBytes, CreatedAt: time.Now().UTC()}
}

func TestReadEmptyStoreDoesNotCreateState(t *testing.T) {
	s := testStore(t)
	if _, err := s.LoadConfig(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadConfig: %v", err)
	}
	slots, err := s.ListSlots()
	if err != nil || len(slots) != 2 || slots[0].Name != "mac1" || slots[1].Name != "mac2" || slots[0].State != "empty" || slots[1].State != "empty" {
		t.Fatalf("slots=%+v err=%v", slots, err)
	}
	if _, err := os.Stat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created state directory: %v", err)
	}
}

func TestConfigIdentityAndReservedAddressesSurviveInstances(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	c, err := NewConfig(DefaultSubnet)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig(held, c); err != nil {
		t.Fatal(err)
	}
	base := testBase(t, s, held, "26A428-one")
	c.DefaultBaseID = base.ID
	if err := s.SaveConfig(held, c); err != nil {
		t.Fatal(err)
	}
	slot := testSlot(t, "mac1", base.ID)
	if err := s.SaveSlot(held, slot); err != nil {
		t.Fatal(err)
	}
	oldID := slot.InstanceID
	slot.InstanceID, _ = NewInstanceID()
	if err := s.SaveSlot(held, slot); err == nil {
		t.Fatal("replaced instance without destroy")
	}
	if err := s.DeleteSlot(held, "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSlot(held, slot); err != nil {
		t.Fatal(err)
	}
	if slot.InstanceID == oldID {
		t.Fatal("replacement identity did not change")
	}
	got, err := s.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != c.Network || got.Slots[0] != c.Slots[0] || got.Slots[1] != c.Slots[1] {
		t.Fatalf("reset changed reservations: %+v", got)
	}
	got.Network.Subnet = "10.10.21.0/24"
	if err := s.SaveConfig(held, got); err == nil {
		t.Fatal("changed saved network identity")
	}
	stored, err := s.LoadConfig()
	if err != nil || stored.Network != c.Network {
		t.Fatalf("invalid save damaged config: %v", err)
	}
}

func TestStateRequiresCorrectLiveLock(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	c, _ := NewConfig(DefaultSubnet)
	if err := s.SaveConfig(nil, c); err == nil {
		t.Fatal("nil token accepted")
	}
	other := testStore(t)
	wrong := holdStore(t, other)
	if err := s.SaveConfig(wrong, c); err == nil {
		t.Fatal("wrong store token accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	if next, err := (&Manager{Store: s}).acquireState(ctx, "test state"); !errors.Is(err, context.DeadlineExceeded) {
		if next != nil {
			_ = next.Release()
		}
		t.Fatalf("parallel lock did not wait: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig(held, c); err == nil {
		t.Fatal("released token accepted")
	}
}

func TestStoreRejectsSymlinksTraversalAndMalformedState(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	for _, part := range []string{"../linux", "/tmp/escape", "slots/../mac2", "slots//mac1"} {
		if _, err := s.Path(part); err == nil {
			t.Errorf("accepted path %q", part)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.Root, "slots")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSlot(held, "mac1"); err == nil {
		t.Fatal("followed slots symlink")
	}
	if _, err := s.Path("slots", "mac1", "state.json"); err == nil {
		t.Fatal("read through slots symlink")
	}
	if err := os.WriteFile(filepath.Join(s.Root, "config.json"), []byte(`{"schema_version":1} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadConfig(); err == nil {
		t.Fatal("accepted malformed config")
	}
	if _, err := NewStore(filepath.Join(s.Root, "slots")); err == nil {
		t.Fatal("store allowed symlinked home")
	}
}

func TestDeleteSlotCannotTouchLinuxOrOtherSlot(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	base := testBase(t, s, held, "base-a")
	for _, name := range []string{"mac1", "mac2"} {
		if err := s.SaveSlot(held, testSlot(t, name, base.ID)); err != nil {
			t.Fatal(err)
		}
	}
	linux := filepath.Join(filepath.Dir(s.Root), "linux-sentinel")
	if err := os.WriteFile(linux, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSlot(held, "../../"); err == nil {
		t.Fatal("delete accepted traversal")
	}
	if err := s.DeleteSlot(held, "mac1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSlot("mac2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadBase(base.ID); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(linux); err != nil || string(data) != "keep" {
		t.Fatalf("linux data changed: %q %v", data, err)
	}
}

func TestReadyStateRequiresInitializationAndReadyBase(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	slot := testSlot(t, "mac1", "missing")
	if err := s.SaveSlot(held, slot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing base: %v", err)
	}
	base := testBase(t, s, held, "base-one")
	slot.BaseID = base.ID
	slot.State = "ready"
	if err := s.SaveSlot(held, slot); err == nil {
		t.Fatal("uninitialized ready state accepted")
	}
	slot.Initialized = true
	if err := s.SaveSlot(held, slot); err != nil {
		t.Fatal(err)
	}
	base.State = "failed"
	if err := s.SaveBase(held, base); err == nil {
		t.Fatal("ready base was mutable")
	}
}

func TestMetadataRejectsSymlinksAndOversizedDocuments(t *testing.T) {
	dir := t.TempDir()
	actual := filepath.Join(dir, "actual.json")
	link := filepath.Join(dir, "link.json")
	if err := os.WriteFile(actual, []byte(`{"key":"value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	var value map[string]string
	if err := readJSON(link, &value); err == nil {
		t.Fatal("metadata symlink was followed")
	}
	if err := os.WriteFile(actual, []byte(`{"key":"value"}`+strings.Repeat(" ", 1<<20)+`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readJSON(actual, &value); err == nil {
		t.Fatal("oversized metadata hid trailing data beyond decoder limit")
	}
}

func TestOrphanedSlotIsNotEmptyOrReplaceable(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	base := testBase(t, s, held, "preserved-base")
	dir, err := s.mkdir("slots", "mac1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.asif"), []byte("orphan disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSlot("mac1"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan treated as empty: %v", err)
	}
	if err := s.SaveSlot(held, testSlot(t, "mac1", base.ID)); err == nil {
		t.Fatal("overwrote orphan identity")
	}
	if _, err := s.PruneImages(held, false); err == nil {
		t.Fatal("pruned base with unresolved orphan references")
	}
	if _, err := s.LoadBase(base.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSlotRejectsRootAndForeignCredentialReference(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	base := testBase(t, s, held, "base")
	slot := testSlot(t, "mac1", base.ID)
	slot.User = "root"
	if err := s.SaveSlot(held, slot); err == nil {
		t.Fatal("allowed root as daily user")
	}
	slot.User = "farrow"
	slot.PasswordRef, _ = NewInstanceID()
	if err := s.SaveSlot(held, slot); err == nil {
		t.Fatal("allowed foreign credential reference")
	}
	slot.PasswordRef = slot.InstanceID
	if err := s.SaveSlot(held, slot); err != nil {
		t.Fatal(err)
	}
}

func TestMissingConfigDoesNotRegenerateNetworkForExistingSlot(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	base := testBase(t, s, held, "base")
	if err := s.SaveSlot(held, testSlot(t, "mac1", base.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadConfig(); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing config treated as fresh setup: %v", err)
	}
	replacement, _ := NewConfig(DefaultSubnet)
	if err := s.SaveConfig(held, replacement); err == nil {
		t.Fatal("regenerated network identity with an existing instance")
	}
}
