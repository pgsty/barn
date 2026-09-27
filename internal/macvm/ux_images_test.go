package macvm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImageUXManagerPruneWaitReportsOwnerAndCancellationPreservesCache(t *testing.T) {
	s := testStore(t)
	var progress bytes.Buffer
	m := &Manager{Store: s, Progress: &progress}
	held, err := m.acquireState(context.Background(), "mac setup fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	if _, err := s.mkdir("images", "ipsw"); err != nil {
		t.Fatal(err)
	}
	path, _, err := s.installerPaths("26B100")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged installer"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if removed, err := m.PruneImages(ctx, true); !errors.Is(err, context.DeadlineExceeded) || len(removed) != 0 {
		t.Fatalf("prune wait ignored cancellation: %v %v", removed, err)
	}
	if !strings.Contains(progress.String(), "Waiting for mac setup fixture (pid ") || !strings.Contains(progress.String(), "before mac image prune; Ctrl-C cancels this wait") {
		t.Fatalf("missing actionable owner notice: %s", &progress)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cancelled wait removed installer: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if removed, err := m.PruneImages(context.Background(), true); err != nil || len(removed) != 1 || removed[0] != path {
		t.Fatalf("unblocked manager prune failed: %v %v", removed, err)
	}
}

func TestImageUXDamagedInstallerCanBeListedAndPruned(t *testing.T) {
	for _, damage := range []string{"missing-json", "invalid-json", "wrong-build", "wrong-size", "wrong-hash-format", "failed-hash-verification"} {
		t.Run(damage, func(t *testing.T) {
			s := testStore(t)
			held := holdStore(t, s)
			base := testBase(t, s, held, "protected-base")
			cfg, err := NewConfig(DefaultSubnet)
			if err != nil {
				t.Fatal(err)
			}
			cfg.DefaultBaseID = base.ID
			if err := s.SaveConfig(held, cfg); err != nil {
				t.Fatal(err)
			}
			slot := testSlot(t, "mac1", base.ID)
			if err := s.SaveSlot(held, slot); err != nil {
				t.Fatal(err)
			}
			body := "small installer fixture"
			spec := fixtureSpec("", body)
			source := filepath.Join(t.TempDir(), "fixture.ipsw")
			if err := os.WriteFile(source, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			installer, err := s.ImportIPSW(context.Background(), source, spec, DownloadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "missing-json":
				err = os.Remove(installer.Path + ".json")
			case "invalid-json":
				err = os.WriteFile(installer.Path+".json", []byte("{broken"), 0o600)
			case "wrong-build":
				installer.Build = "26B999"
				err = writeJSON(installer.Path+".json", installer)
			case "wrong-size":
				installer.SizeBytes++
				err = writeJSON(installer.Path+".json", installer)
			case "wrong-hash-format":
				installer.SHA256 = "wrong"
				err = writeJSON(installer.Path+".json", installer)
			case "failed-hash-verification":
				err = os.WriteFile(installer.Path, []byte(strings.Repeat("x", len(body))), 0o600)
				if err == nil {
					if _, checkErr := cachedInstaller(context.Background(), installer.Path, spec); checkErr == nil {
						t.Fatal("accepted corrupted installer")
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			entries, err := s.ImageList()
			if err != nil {
				t.Fatalf("damaged installer broke image ls: %v", err)
			}
			found := false
			for _, entry := range entries {
				if entry.Kind == "installer" {
					found = true
					if entry.State != "damaged" || entry.Detail == "" || entry.SizeBytes != int64(len(body)) {
						t.Fatalf("damage not visible: %+v", entry)
					}
				}
			}
			if !found {
				t.Fatal("damaged installer hidden")
			}
			if removed, err := s.PruneImages(held, false); err != nil || len(removed) != 0 {
				t.Fatalf("ordinary prune removed installer or protected base: %v %v", removed, err)
			}
			removed, err := s.PruneImages(held, true)
			if err != nil || len(removed) != 1 || removed[0] != installer.Path {
				t.Fatalf("damaged cache cannot be pruned: %v %v", removed, err)
			}
			if _, err := s.LoadBase(base.ID); err != nil {
				t.Fatalf("prune lost referenced/default base: %v", err)
			}
			if _, err := s.LoadSlot(slot.Name); err != nil {
				t.Fatalf("prune lost slot: %v", err)
			}
			if _, err := os.Stat(installer.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cache remains: %v", err)
			}
			if data, err := os.ReadFile(source); err != nil || string(data) != body {
				t.Fatalf("prune changed external source: %q %v", data, err)
			}
		})
	}
}

func TestImageUXDamagedInstallerStillHonorsDownloadLock(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	download, err := s.lockInstaller(context.Background(), "26B100")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = download.Release() }()
	path, _, err := s.installerPaths("26B100")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("missing metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.PruneImages(held, true); err != nil || len(removed) != 0 {
		t.Fatalf("removed active damaged cache: %v %v", removed, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err := download.Release(); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.PruneImages(held, true); err != nil || len(removed) != 1 {
		t.Fatalf("idle damaged cache not removed: %v %v", removed, err)
	}
}

func TestImageUXInstallerMetadataSymlinkPrunesOnlyLink(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	if _, err := s.mkdir("images", "ipsw"); err != nil {
		t.Fatal(err)
	}
	path, _, err := s.installerPaths("26B100")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("installer"), 0o600); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.json")
	if err := os.WriteFile(external, []byte("private outside data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, path+".json"); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.PruneImages(held, true); err != nil || len(removed) != 1 {
		t.Fatalf("safe unlink failed: %v %v", removed, err)
	}
	if data, err := os.ReadFile(external); err != nil || string(data) != "private outside data" {
		t.Fatalf("prune followed metadata link: %q %v", data, err)
	}
}
