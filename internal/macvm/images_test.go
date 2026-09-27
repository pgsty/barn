package macvm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This is a filesystem fixture, not a bootable ASIF image. A large file with
// one written block makes file length, allocation and VM capacity distinct.
func writeSparseDiskFixture(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("allocated fixture block"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

func TestSparseImageAndOverlayUsageAreSeparateFromGuestCapacity(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	c, err := NewConfig(DefaultSubnet)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig(held, c); err != nil {
		t.Fatal(err)
	}
	base := testBase(t, s, held, "sparse-base")
	basePath, err := s.BasePath(base.ID)
	if err != nil {
		t.Fatal(err)
	}
	const baseFileBytes = int64(64 << 20)
	writeSparseDiskFixture(t, filepath.Join(basePath, "disk.asif"), baseFileBytes)
	_, diskAllocated, err := regularFileUsage(filepath.Join(basePath, "disk.asif"))
	if err != nil {
		t.Fatal(err)
	}
	if diskAllocated >= baseFileBytes {
		t.Skip("test filesystem did not preserve a sparse hole")
	}
	slot := testSlot(t, "mac1", base.ID)
	if err := s.SaveSlot(held, slot); err != nil {
		t.Fatal(err)
	}
	slotPath, err := s.SlotPath("mac1")
	if err != nil {
		t.Fatal(err)
	}
	const overlayBytes = int64(32 << 20)
	writeSparseDiskFixture(t, filepath.Join(slotPath, "disk.asif"), overlayBytes)
	images, err := s.ImageList()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].Kind != "base" || images[0].ID != base.ID {
		t.Fatalf("overlay entered image/prune inventory: %+v", images)
	}
	image := images[0]
	if image.SizeBytes <= baseFileBytes || image.AllocatedBytes < diskAllocated || image.AllocatedBytes >= image.SizeBytes || image.VirtualCapacityBytes != DefaultDiskBytes {
		t.Fatalf("file/allocation/capacity conflated: %+v", image)
	}
	status, err := (&Manager{Store: s}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	view := status.Slots[0]
	if view.DiskFileBytes == nil || *view.DiskFileBytes != overlayBytes || view.DiskAllocatedBytes == nil || *view.DiskAllocatedBytes >= overlayBytes || view.DiskBytes != DefaultDiskBytes {
		t.Fatalf("overlay usage conflated with capacity: %+v", view)
	}
	if status.Slots[1].DiskFileBytes != nil || status.Slots[1].DiskAllocatedBytes != nil || status.Slots[1].DiskBytes != 0 {
		t.Fatalf("empty slot reported measured allocation: %+v", status.Slots[1])
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"disk_file_bytes":33554432`) || !strings.Contains(string(encoded), `"disk_allocated_bytes":`) {
		t.Fatalf("missing structured space measurements: %s", encoded)
	}
	if removed, err := s.PruneImages(held, false); err != nil || len(removed) != 0 {
		t.Fatalf("prune removed referenced base or overlay: %v %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(slotPath, "disk.asif")); err != nil {
		t.Fatal(err)
	}
}

func TestSlotUsageDistinguishesAbsentDiskFromMeasuredZero(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	c, _ := NewConfig(DefaultSubnet)
	if err := s.SaveConfig(held, c); err != nil {
		t.Fatal(err)
	}
	base := testBase(t, s, held, "base")
	if err := s.SaveSlot(held, testSlot(t, "mac1", base.ID)); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Store: s}
	missing, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if missing.Slots[0].DiskFileBytes != nil || missing.Slots[0].DiskAllocatedBytes != nil {
		t.Fatal("absent disk reported a measured zero")
	}
	dir, _ := s.SlotPath("mac1")
	if err := os.WriteFile(filepath.Join(dir, "disk.asif"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	measured, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if measured.Slots[0].DiskFileBytes == nil || *measured.Slots[0].DiskFileBytes != 0 || measured.Slots[0].DiskAllocatedBytes == nil || *measured.Slots[0].DiskAllocatedBytes != 0 {
		t.Fatal("existing empty file lost its measured zero")
	}
}

func TestPrunePreservesDefaultReferencesAndActiveImages(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	for _, id := range []string{"default", "in-use", "busy", "obsolete"} {
		testBase(t, s, held, id)
	}
	c, _ := NewConfig(DefaultSubnet)
	c.DefaultBaseID = "default"
	if err := s.SaveConfig(held, c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSlot(held, testSlot(t, "mac2", "in-use")); err != nil {
		t.Fatal(err)
	}
	busy, err := s.LockBase(context.Background(), "busy")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Release() }()
	removed, err := s.PruneImages(held, false)
	if err != nil || len(removed) != 1 || filepath.Base(removed[0]) != "obsolete" {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	for _, id := range []string{"default", "in-use", "busy"} {
		if _, err := s.LoadBase(id); err != nil {
			t.Errorf("lost %s: %v", id, err)
		}
	}
	entries, err := s.ImageList()
	if err != nil || len(entries) != 3 {
		t.Fatalf("list=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.ID == "in-use" && (len(entry.References) != 1 || entry.References[0] != "mac2") {
			t.Fatalf("missing reference: %+v", entry)
		}
	}
}

func TestPruneFailsClosedOnCorruptReferencesAndSymlinks(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	testBase(t, s, held, "keep-on-error")
	dir, err := s.mkdir("slots", "mac1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneImages(held, false); err == nil {
		t.Fatal("pruned with unreadable references")
	}
	if _, err := s.LoadBase("keep-on-error"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSlot(held, "mac1"); err != nil {
		t.Fatal(err)
	}
	baseDir, _ := s.BasePath("keep-on-error")
	if err := os.Symlink(t.TempDir(), filepath.Join(baseDir, "external")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneImages(held, false); err == nil {
		t.Fatal("pruned with base symlink")
	}
}

func TestInstallerPruneHonorsDownloadLock(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	downloadLock, err := s.lockInstaller(context.Background(), "26A428")
	if err != nil {
		t.Fatal(err)
	}
	_, partial, err := s.installerPaths("26A428")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.PruneImages(held, true); err != nil || len(removed) != 0 {
		t.Fatalf("removed active download: %v %v", removed, err)
	}
	if err := downloadLock.Release(); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.PruneImages(held, false); err != nil || len(removed) != 0 {
		t.Fatalf("default prune removed installer: %v %v", removed, err)
	}
	if removed, err := s.PruneImages(held, true); err != nil || len(removed) != 1 {
		t.Fatalf("installer prune: %v %v", removed, err)
	}
}

func TestBaseIdentityCoversHardwareCapacityAndRecipe(t *testing.T) {
	first, err := BaseImageID("26A428", DefaultDiskBytes, strings.Repeat("a", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		build  string
		disk   int64
		hash   string
		recipe int
	}{{"26B1", DefaultDiskBytes, strings.Repeat("a", 64), 1}, {"26A428", 120 << 30, strings.Repeat("a", 64), 1}, {"26A428", DefaultDiskBytes, strings.Repeat("b", 64), 1}, {"26A428", DefaultDiskBytes, strings.Repeat("a", 64), 2}} {
		id, err := BaseImageID(input.build, input.disk, input.hash, input.recipe)
		if err != nil || id == first {
			t.Fatalf("identity alias: %s %v", id, err)
		}
	}
}
