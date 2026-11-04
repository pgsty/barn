package image

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"

	"github.com/pgsty/barn/internal/failure"
)

func writeDefaultManifestState(t *testing.T, manager ManifestManager, value ManifestState) {
	t.Helper()
	if err := manager.validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(ManifestRegistry{Schema: ManifestRegistrySchema, Repositories: map[string]ManifestState{"default": value}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.statePath(), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// `barn image reset` by an earlier binary records that binary's embedded
// revision while keeping a higher accepted high-water mark. After an upgrade
// the active selection still means "this binary's catalog": every catalog
// command must keep working, and the high-water mark must survive.
func TestEmbeddedSelectionFromEarlierBinaryAdoptsCurrentBaseline(t *testing.T) {
	for _, highest := range []uint64{EmbeddedManifestVersion, EmbeddedManifestVersion + 5} {
		manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data"), Keys: testManifestRoots(t)}
		writeDefaultManifestState(t, manager, ManifestState{
			Schema: ManifestStateSchema, HighestVersion: highest, Active: "embedded", ActiveVersion: EmbeddedManifestVersion - 1,
			ActiveDigest: strings.Repeat("0", 64), Source: "embedded", AcceptedAt: time.Now().UTC(),
		})
		catalog, current, err := manager.Current()
		if err != nil {
			t.Fatalf("highest=%d: current after upgrade: %v", highest, err)
		}
		if catalog.Version != EmbeddedManifestVersion || current.Active != "embedded" || current.ActiveVersion != EmbeddedManifestVersion || current.HighestVersion != highest {
			t.Fatalf("highest=%d: current = %d %#v", highest, catalog.Version, current)
		}
		path := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion+6, privateKey(t, developmentPrivateRoot1), nil)
		if synced, err := manager.Sync(context.Background(), path, false); err != nil || synced.ActiveVersion != EmbeddedManifestVersion+6 {
			t.Fatalf("highest=%d: update after upgrade = %#v %v", highest, synced, err)
		}
	}
	// The preserved high-water mark still refuses an older signed catalog.
	manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data"), Keys: testManifestRoots(t)}
	writeDefaultManifestState(t, manager, ManifestState{
		Schema: ManifestStateSchema, HighestVersion: EmbeddedManifestVersion + 5, Active: "embedded", ActiveVersion: EmbeddedManifestVersion - 1,
		ActiveDigest: strings.Repeat("0", 64), Source: "embedded", AcceptedAt: time.Now().UTC(),
	})
	path := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion+1, privateKey(t, developmentPrivateRoot1), nil)
	if _, err := manager.Sync(context.Background(), path, false); err == nil || !strings.Contains(err.Error(), "high-water mark") {
		t.Fatalf("older catalog accepted after baseline adoption: %v", err)
	}
}

// The same revision with different bytes is still equivocation.
func TestEmbeddedSelectionWithSameRevisionDifferentDigestIsIntegrity(t *testing.T) {
	manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data"), Keys: testManifestRoots(t)}
	writeDefaultManifestState(t, manager, ManifestState{
		Schema: ManifestStateSchema, HighestVersion: EmbeddedManifestVersion, Active: "embedded", ActiveVersion: EmbeddedManifestVersion,
		ActiveDigest: strings.Repeat("0", 64), Source: "embedded", AcceptedAt: time.Now().UTC(),
	})
	_, _, err := manager.Current()
	if class, reason, _ := failure.Classify(err); class != failure.Integrity || reason != "catalog_equivocation" {
		t.Fatalf("same-revision embedded digest mismatch = %v (%s/%s)", err, class, reason)
	}
}

// Every catalog signature failure is an integrity failure, so the command exits 7.
func TestCatalogSignatureFailuresAreIntegrity(t *testing.T) {
	key := privateKey(t, developmentPrivateRoot1)
	tampered := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion+1, key, nil)
	data, err := os.ReadFile(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tampered, append(data, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	garbage := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion+1, key, nil)
	if err := os.WriteFile(garbage+".minisig", []byte("untrusted comment: x\nAAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unknown := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion+1, key, nil)
	_, other, err := minisign.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	unknownData, err := os.ReadFile(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown+".minisig", minisign.Sign(other, unknownData), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion+1, key, nil)
	if err := os.Remove(missing + ".minisig"); err != nil {
		t.Fatal(err)
	}
	empty := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion+1, key, nil)
	if err := os.WriteFile(empty+".minisig", nil, 0600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"tampered": tampered, "garbage": garbage, "unknown key": unknown, "missing": missing, "empty": empty} {
		manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data"), Keys: testManifestRoots(t)}
		_, err := manager.Sync(context.Background(), path, false)
		if class, reason, _ := failure.Classify(err); class != failure.Integrity || reason != "catalog_signature" {
			t.Errorf("%s: sync = %v (%s/%s)", name, err, class, reason)
		}
		var classified *failure.Error
		if !errors.As(err, &classified) {
			t.Errorf("%s: error is not classified: %T", name, err)
		}
	}
}

func TestOfficialMirrorsAdoptNewEmbeddedBaseline(t *testing.T) {
	for _, repository := range []string{"", "https://repo.pigsty.io/barn", "https://repo.pigsty.cc/barn", "https://custom.example/barn"} {
		t.Run(repository, func(t *testing.T) {
			manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data"), Repository: repository, Keys: testManifestRoots(t)}
			path := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion-1, privateKey(t, developmentPrivateRoot1), nil)
			// Simulate a catalog accepted by an older binary (rather than an
			// intentional downgrade by this binary, whose high-water mark is newer).
			current, err := manager.Sync(context.Background(), path, true)
			if err != nil {
				t.Fatal(err)
			}
			current.HighestVersion = current.ActiveVersion
			if err := manager.writeState(current); err != nil {
				t.Fatal(err)
			}
			catalog, active, err := manager.Current()
			if err != nil {
				t.Fatal(err)
			}
			if repository == "https://custom.example/barn" {
				if catalog.Version != EmbeddedManifestVersion-1 {
					t.Fatal("custom repository inherited official version namespace")
				}
			} else {
				if catalog.Version != EmbeddedManifestVersion || active.Active != "embedded" {
					t.Fatalf("official repository did not advance: %+v", active)
				}
				if _, err := manager.Sync(context.Background(), path, false); err == nil {
					t.Fatal("old official catalog bypassed new baseline high-water mark")
				}
			}
		})
	}
}

func TestIdenticalCatalogMayBeResigned(t *testing.T) {
	for _, corruptCache := range []bool{false, true} {
		manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data"), Keys: testManifestRoots(t)}
		key := privateKey(t, developmentPrivateRoot1)
		path := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion+1, key, nil)
		current, err := manager.Sync(context.Background(), path, false)
		if err != nil {
			t.Fatal(err)
		}
		if corruptCache {
			cached := versionSignaturePath(filepath.Join(manager.versions(), current.Active), current.KeyID)
			if err := os.WriteFile(cached, []byte("broken"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		signature := minisign.SignWithComments(key, data, "timestamp:1787486500", "resigned identical catalog")
		if err := os.WriteFile(path+".minisig", signature, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Sync(context.Background(), path, false); err != nil {
			t.Fatalf("resigned catalog refused: %v", err)
		}
		_, active, err := manager.Current()
		if err != nil || active.ActiveDigest != current.ActiveDigest {
			t.Fatalf("resigned catalog not usable: %+v %v", active, err)
		}
	}
}

// Exit 7 means a verification mismatch. A cached signature that is gone breaks
// the active state pointer (integrity); one the host cannot read is a host
// error and keeps the runtime classification.
func TestCachedSignatureReadFailuresAreClassifiedByCause(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode 0000 file")
	}
	manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data"), Keys: testManifestRoots(t)}
	path := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion+1, privateKey(t, developmentPrivateRoot1), nil)
	synced, err := manager.Sync(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	cached := versionSignaturePath(filepath.Join(manager.versions(), synced.Active), synced.KeyID)
	if err := os.Chmod(cached, 0); err != nil {
		t.Fatal(err)
	}
	_, _, err = manager.Current()
	if err == nil {
		t.Fatal("unreadable cached signature was accepted")
	}
	if class, _, _ := failure.Classify(err); class == failure.Integrity {
		t.Fatalf("unreadable cached signature classified as integrity: %v", err)
	}
	if err := os.Chmod(cached, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cached); err != nil {
		t.Fatal(err)
	}
	_, _, err = manager.Current()
	if class, reason, _ := failure.Classify(err); class != failure.Integrity || reason != "catalog_state" {
		t.Fatalf("missing cached signature = %v (%s/%s)", err, class, reason)
	}
}
