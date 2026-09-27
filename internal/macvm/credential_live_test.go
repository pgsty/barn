package macvm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in native test: a disposable UUID in the login Keychain, never a VM's
// credential. The two apps must have different signing identities.
func TestLiveCredentialOwnerSurvivesAppReplacement(t *testing.T) {
	before, after := os.Getenv("FARROW_MAC_CREDENTIAL_TEST_RUNNER"), os.Getenv("FARROW_MAC_CREDENTIAL_TEST_NEXT_RUNNER")
	if before == "" || after == "" {
		t.Skip("requires two native app builds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	first, second := Runner{Binary: before}, Runner{Binary: after}
	oldHash, err := runnerDigest(before)
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := runnerDigest(after)
	if err != nil {
		t.Fatal(err)
	}
	if oldHash == newHash {
		t.Fatal("test requires different signed binaries")
	}
	store := testStore(t)
	instance, _ := NewInstanceID()
	password, err := NewPassword()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.deletePasswordAtService(context.Background(), secretService(store.Root), instance) }()
	if err := first.SetPassword(ctx, store.Root, instance, password); err != nil {
		t.Fatal(err)
	}
	owner, err := second.credentialRunner(ctx, store.Root, instance)
	if err != nil || owner.Binary == first.Binary || owner.Binary == second.Binary {
		t.Fatalf("owner not retained independently: %v", err)
	}
	actual, err := second.Password(ctx, store.Root, instance)
	if err != nil || actual != password {
		t.Fatalf("password did not survive app replacement: %v", err)
	}
	if err := second.DeletePassword(ctx, store.Root, instance); err != nil {
		t.Fatal(err)
	}
	if _, err := first.passwordAtService(ctx, secretService(store.Root), instance); !secretMissing(err) {
		t.Fatalf("original item was not deleted through retained owner: %v", err)
	}
	path, _ := credentialRecordPath(store.Root, instance)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("deleted item retained its owner reference")
	}
}

func liveCredentialMigrationFixture(t *testing.T) (context.Context, *Manager, *Slot, Runner) {
	t.Helper()
	before, after := os.Getenv("FARROW_MAC_CREDENTIAL_TEST_RUNNER"), os.Getenv("FARROW_MAC_CREDENTIAL_TEST_NEXT_RUNNER")
	if before == "" || after == "" {
		t.Skip("requires two native app builds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	m, held, slot, _ := lifecycleSlot(t)
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	m.Runner = Runner{Binary: after}
	first := Runner{Binary: before}
	service := secretService(m.Store.Root)
	t.Cleanup(func() {
		for _, runner := range []Runner{first, {Binary: after}, m.Runner} {
			_ = runner.deletePasswordAtService(context.Background(), service, slot.InstanceID)
			_ = runner.deletePasswordAtService(context.Background(), service+".migration", slot.InstanceID)
		}
	})
	password, err := NewPassword()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetPassword(ctx, m.Store.Root, slot.InstanceID, password); err != nil {
		t.Fatal(err)
	}
	return ctx, m, slot, first
}

func TestLiveCredentialMigrationResumesPublication(t *testing.T) {
	for _, phase := range []string{"owner-write-failed", "after-publication", "after-backup-removal", "delete-after-publication"} {
		t.Run(phase, func(t *testing.T) {
			ctx, m, slot, first := liveCredentialMigrationFixture(t)
			oldRunner, oldHash, err := first.retainCredentialRunner(ctx, m.Store.Root)
			if err != nil {
				t.Fatal(err)
			}
			newRunner, newHash, err := m.Runner.retainCredentialRunner(ctx, m.Store.Root)
			if err != nil {
				t.Fatal(err)
			}
			if oldHash == newHash {
				t.Fatal("test requires different signed binaries")
			}
			service := secretService(m.Store.Root)
			old := runnerCredentialOwner{oldRunner, service, slot.InstanceID}
			current := runnerCredentialOwner{newRunner, service, slot.InstanceID}
			backup := runnerCredentialOwner{oldRunner, service + ".migration", slot.InstanceID}
			password, err := old.get(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = migrateCredential(ctx, old, current, backup, func() error {
				if phase == "owner-write-failed" {
					return errors.New("simulated owner record write failure")
				}
				if err := saveCredentialRunnerRecord(m.Store.Root, slot.InstanceID, credentialRunnerRecord{Hash: newHash, MigrationFrom: oldHash}); err != nil {
					return err
				}
				if phase == "after-publication" || phase == "delete-after-publication" {
					return errors.New("simulated interruption after owner publication")
				}
				return nil
			})
			if (err == nil) != (phase == "after-backup-removal") {
				t.Fatalf("unexpected interruption result: %v", err)
			}
			if phase == "delete-after-publication" {
				if err := m.Runner.DeletePassword(ctx, m.Store.Root, slot.InstanceID); err != nil {
					t.Fatalf("could not delete a partially published credential: %v", err)
				}
				if _, err := current.get(ctx); !secretMissing(err) {
					t.Fatalf("deletion left the main credential: %v", err)
				}
				if _, err := backup.get(ctx); !secretMissing(err) {
					t.Fatalf("deletion left the encrypted recovery credential: %v", err)
				}
				if record, err := readCredentialRunnerRecord(m.Store.Root, slot.InstanceID); err != nil || record != nil {
					t.Fatalf("deletion left an owner reference: %v", err)
				}
				return
			}
			if phase != "after-backup-removal" {
				if value, err := backup.get(ctx); err != nil || value != password {
					t.Fatalf("publication failure lost the encrypted backup: %v", err)
				}
				original, err := m.previousCredentialRunner(ctx, slot.InstanceID)
				if err != nil {
					t.Fatal(err)
				}
				if hash, err := runnerDigest(original); err != nil || hash != oldHash {
					t.Fatalf("automatic retry selected the wrong backup owner: %v", err)
				}
			}
			if _, err := m.MigrateCredentials(ctx, slot.Name, ""); err != nil {
				t.Fatalf("automatic publication recovery failed: %v", err)
			}
			if value, err := m.Runner.Password(ctx, m.Store.Root, slot.InstanceID); err != nil || value != password {
				t.Fatalf("publication recovery changed password: %v", err)
			}
			record, err := readCredentialRunnerRecord(m.Store.Root, slot.InstanceID)
			if err != nil || record == nil || record.Hash != newHash || record.MigrationFrom != "" {
				t.Fatalf("publication recovery left stale metadata: %+v %v", record, err)
			}
			if _, err := backup.get(ctx); !secretMissing(err) {
				t.Fatalf("publication recovery left a backup item: %v", err)
			}
			if err := m.Runner.DeletePassword(ctx, m.Store.Root, slot.InstanceID); err != nil {
				t.Fatalf("new owner cannot delete the migrated item: %v", err)
			}
		})
	}
}

func TestLiveCredentialMigrationAppAndStandalone(t *testing.T) {
	ctx, m, slot, first := liveCredentialMigrationFixture(t)
	password, err := first.Password(ctx, m.Store.Root, slot.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.Runner.Binary)
	if err != nil {
		t.Fatal(err)
	}
	bare := Runner{Binary: filepath.Join(t.TempDir(), "standalone-runner")}
	if err := os.WriteFile(bare.Binary, data, 0700); err != nil {
		t.Fatal(err)
	}
	// Match packaging/build-mac.sh: a bundle-signed executable must be
	// re-signed for standalone use after leaving its app resources.
	if err := exec.CommandContext(ctx, "/usr/bin/codesign", "--force", "--sign", "-", "--preserve-metadata=entitlements", bare.Binary).Run(); err != nil {
		t.Fatal(err)
	}
	service := secretService(m.Store.Root)
	t.Cleanup(func() {
		_ = bare.deletePasswordAtService(context.Background(), service, slot.InstanceID)
		_ = bare.deletePasswordAtService(context.Background(), service+".migration", slot.InstanceID)
	})
	m.Runner = bare
	if _, err := m.MigrateCredentials(ctx, slot.Name, ""); err != nil {
		t.Fatalf("app to standalone migration failed: %v", err)
	}
	if owner, err := bare.credentialRunner(ctx, m.Store.Root, slot.InstanceID); err != nil || owner.Binary != bare.Binary {
		t.Fatalf("standalone migration retained the obsolete app reference: %v", err)
	}
	// Reconstruct a process exit after removing the encrypted backup but
	// before removing the final standalone recovery reference.
	oldHash, err := runnerDigest(first.Binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveCredentialRunnerRecord(m.Store.Root, slot.InstanceID, credentialRunnerRecord{MigrationFrom: oldHash}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MigrateCredentials(ctx, slot.Name, ""); err != nil {
		t.Fatalf("standalone publication cleanup could not resume: %v", err)
	}
	if record, err := readCredentialRunnerRecord(m.Store.Root, slot.InstanceID); err != nil || record != nil {
		t.Fatalf("standalone publication cleanup left an owner reference: %v", err)
	}
	if value, err := bare.Password(ctx, m.Store.Root, slot.InstanceID); err != nil || value != password {
		t.Fatalf("standalone password is not usable: %v", err)
	}
	if err := bare.DeletePassword(ctx, m.Store.Root, slot.InstanceID); err != nil {
		t.Fatalf("standalone cannot delete its migrated password: %v", err)
	}
	if err := bare.SetPassword(ctx, m.Store.Root, slot.InstanceID, password); err != nil {
		t.Fatal(err)
	}
	m.Runner = first
	if _, err := m.MigrateCredentials(ctx, slot.Name, bare.Binary); err != nil {
		t.Fatalf("standalone to app migration failed: %v", err)
	}
	if value, err := first.Password(ctx, m.Store.Root, slot.InstanceID); err != nil || value != password {
		t.Fatalf("standalone to app migration changed password: %v", err)
	}
	if err := first.DeletePassword(ctx, m.Store.Root, slot.InstanceID); err != nil {
		t.Fatalf("app cannot delete its migrated password: %v", err)
	}
}

func TestLiveCredentialMigrationPreflightsRetainedOwner(t *testing.T) {
	before, after := os.Getenv("FARROW_MAC_CREDENTIAL_TEST_RUNNER"), os.Getenv("FARROW_MAC_CREDENTIAL_TEST_NEXT_RUNNER")
	if before == "" || after == "" {
		t.Skip("requires two native app builds")
	}
	oldHash, err := runnerDigest(before)
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := runnerDigest(after)
	if err != nil {
		t.Fatal(err)
	}
	if oldHash == newHash {
		t.Fatal("test requires different signed binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m, held, slot, _ := lifecycleSlot(t)
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	m.Runner = Runner{Binary: after}
	first := Runner{Binary: before}
	password, err := NewPassword()
	if err != nil {
		t.Fatal(err)
	}
	service := secretService(m.Store.Root)
	defer func() {
		_ = first.deletePasswordAtService(context.Background(), service, slot.InstanceID)
		_ = m.Runner.deletePasswordAtService(context.Background(), service, slot.InstanceID)
		_ = first.deletePasswordAtService(context.Background(), service+".migration", slot.InstanceID)
	}()
	if err := first.SetPassword(ctx, m.Store.Root, slot.InstanceID, password); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(m.Store.Root, "credentials", "owners", newHash)
	if err := os.WriteFile(blocker, []byte("preserve this unexpected file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MigrateCredentials(ctx, slot.Name, before); err == nil {
		t.Fatal("unsafe retained-owner destination accepted")
	}
	value, err := first.passwordAtService(ctx, service, slot.InstanceID)
	if err != nil || value != password {
		t.Fatalf("failed owner preflight changed Keychain ownership: %v", err)
	}
	// Keychain can allow another signature to read and update while refusing
	// deletion. Recreate this disposable item through its original owner.
	if err := first.deletePasswordAtService(ctx, service, slot.InstanceID); err != nil {
		t.Fatalf("failed owner preflight transferred Keychain ownership: %v", err)
	}
	if err := first.setPasswordAtService(ctx, service, slot.InstanceID, password); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MigrateCredentials(ctx, slot.Name, ""); err != nil {
		t.Fatalf("automatic retry failed: %v", err)
	}
	value, err = m.Runner.Password(ctx, m.Store.Root, slot.InstanceID)
	if err != nil || value != password {
		t.Fatalf("migration retry changed the password: %v", err)
	}
}
