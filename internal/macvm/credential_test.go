package macvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type memoryCredentialItem struct{ owner, password string }
type fakeCredentialOwner struct {
	identity, key    string
	items            map[string]memoryCredentialItem
	fail             map[string]error
	after            func(string)
	wrongRead        bool
	allowForeignRead bool
}

func (o *fakeCredentialOwner) get(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := o.fail["get"]; err != nil {
		return "", err
	}
	item, ok := o.items[o.key]
	if !ok {
		return "", &SecretError{Operation: "get", Code: "keychain:-25300"}
	}
	if item.owner != o.identity && !o.allowForeignRead {
		return "", &SecretError{Operation: "get", Code: "keychain:-25244"}
	}
	if o.wrongRead {
		return "incorrect-synthetic-value", nil
	}
	return item.password, nil
}
func (o *fakeCredentialOwner) set(ctx context.Context, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.fail["set"]; err != nil {
		return err
	}
	if item, ok := o.items[o.key]; ok && item.owner != o.identity {
		return &SecretError{Operation: "set", Code: "keychain:-25244"}
	}
	o.items[o.key] = memoryCredentialItem{o.identity, value}
	if o.after != nil {
		o.after("set")
	}
	return o.fail["after-set"]
}
func (o *fakeCredentialOwner) delete(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.fail["delete"]; err != nil {
		return err
	}
	if item, ok := o.items[o.key]; ok && item.owner != o.identity {
		return &SecretError{Operation: "delete", Code: "keychain:-25244"}
	}
	delete(o.items, o.key)
	if o.after != nil {
		o.after("delete")
	}
	return o.fail["after-delete"]
}

func credentialFixture() (*fakeCredentialOwner, *fakeCredentialOwner, *fakeCredentialOwner) {
	items := map[string]memoryCredentialItem{"main": {"old", "synthetic-password-kept-only-in-memory"}}
	return &fakeCredentialOwner{identity: "old", key: "main", items: items, fail: map[string]error{}},
		&fakeCredentialOwner{identity: "new", key: "main", items: items, fail: map[string]error{}},
		&fakeCredentialOwner{identity: "old", key: "backup", items: items, fail: map[string]error{}}
}

func TestCredentialMigrationOwnershipAndRollback(t *testing.T) {
	for _, phase := range []string{"success", "old-read", "backup-write", "backup-verify", "old-delete", "new-write", "new-write-partial", "new-read", "new-verify", "cancel-after-delete", "delete-completed-with-error", "rollback-write", "rollback-delete"} {
		t.Run(phase, func(t *testing.T) {
			old, current, backup := credentialFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failed := errors.New("simulated runner failure")
			switch phase {
			case "old-read":
				old.fail["get"] = failed
			case "backup-write":
				backup.fail["set"] = failed
			case "backup-verify":
				backup.after = func(string) { backup.wrongRead = true }
			case "old-delete":
				old.fail["delete"] = failed
			case "new-write":
				current.fail["set"] = failed
			case "new-write-partial":
				current.fail["after-set"] = failed
			case "new-read":
				current.after = func(string) { current.fail["get"] = failed }
			case "new-verify":
				current.wrongRead = true
			case "cancel-after-delete":
				old.after = func(action string) {
					if action == "delete" {
						cancel()
					}
				}
			case "delete-completed-with-error":
				old.fail["after-delete"] = failed
			case "rollback-write":
				current.fail["set"], old.fail["set"] = failed, failed
			case "rollback-delete":
				current.fail["after-set"], current.fail["delete"] = failed, failed
			}
			err := migrateCredential(ctx, old, current, backup, nil)
			if phase == "success" {
				if err != nil {
					t.Fatal(err)
				}
				if old.items["main"].owner != "new" || old.items["main"].password != "synthetic-password-kept-only-in-memory" {
					t.Fatal("new owner did not preserve password")
				}
				if _, ok := old.items["backup"]; ok {
					t.Fatal("successful migration retained backup")
				}
				return
			}
			if err == nil {
				t.Fatal("injected failure was ignored")
			}
			if strings.Contains(err.Error(), "synthetic-password-kept-only-in-memory") {
				t.Fatal("password leaked through error")
			}
			if phase == "rollback-write" || phase == "rollback-delete" {
				if old.items["backup"].password != "synthetic-password-kept-only-in-memory" || !strings.Contains(err.Error(), "backup was preserved") {
					t.Fatalf("failed rollback lost recoverable backup: %v", err)
				}
			} else if old.items["main"].owner != "old" || old.items["main"].password != "synthetic-password-kept-only-in-memory" {
				t.Fatal("original credential was not restored")
			}
			if phase == "cancel-after-delete" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation cause was lost")
			}
		})
	}
}

func TestCredentialMigrationResumesAfterProcessExit(t *testing.T) {
	for _, phase := range []string{"before-delete", "after-delete", "after-write", "after-write-with-shared-read", "cleanup-failed"} {
		t.Run(phase, func(t *testing.T) {
			old, current, backup := credentialFixture()
			backup.items["backup"] = old.items["main"]
			switch phase {
			case "after-delete":
				delete(old.items, "main")
			case "after-write", "after-write-with-shared-read", "cleanup-failed":
				old.items["main"] = memoryCredentialItem{"new", backup.items["backup"].password}
			}
			if phase == "after-write-with-shared-read" {
				old.allowForeignRead = true
			}
			if phase == "cleanup-failed" {
				backup.fail["delete"] = errors.New("simulated backup cleanup failure")
				if err := migrateCredential(context.Background(), old, current, backup, nil); err == nil || !strings.Contains(err.Error(), "password was verified") {
					t.Fatalf("cleanup failed without clear committed state: %v", err)
				}
				delete(backup.fail, "delete")
			}
			if err := migrateCredential(context.Background(), old, current, backup, nil); err != nil {
				t.Fatal(err)
			}
			if old.items["main"].owner != "new" || old.items["main"].password != "synthetic-password-kept-only-in-memory" {
				t.Fatal("resumed migration changed password")
			}
			if _, ok := old.items["backup"]; ok {
				t.Fatal("resumed migration did not remove backup")
			}
		})
	}
}

func TestCredentialMigrationRejectsConflictingBackup(t *testing.T) {
	old, current, backup := credentialFixture()
	backup.items["backup"] = memoryCredentialItem{"old", "different-synthetic-password"}
	if err := migrateCredential(context.Background(), old, current, backup, nil); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("accepted conflicting backup: %v", err)
	}
	if old.items["main"].owner != "old" || backup.items["backup"].password != "different-synthetic-password" {
		t.Fatal("conflict changed credentials")
	}
}

func TestCredentialMigrationPublishesOwnerBeforeRemovingBackup(t *testing.T) {
	for _, phase := range []string{"publish-failed", "publish-completed-with-error", "cancel-after-publish", "backup-cleanup-failed"} {
		t.Run(phase, func(t *testing.T) {
			old, current, backup := credentialFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failed := errors.New("simulated metadata failure")
			published := false
			publish := func() error {
				if old.items["main"].owner != "new" || old.items["backup"].owner != "old" {
					t.Fatal("publication was not protected by a verified item and backup")
				}
				if phase == "publish-failed" {
					return failed
				}
				published = true
				switch phase {
				case "publish-completed-with-error":
					return failed
				case "cancel-after-publish":
					cancel()
				case "backup-cleanup-failed":
					backup.fail["delete"] = failed
				}
				return nil
			}
			if err := migrateCredential(ctx, old, current, backup, publish); err == nil {
				t.Fatal("publication or cleanup failure was ignored")
			} else if strings.Contains(err.Error(), old.items["main"].password) {
				t.Fatal("password leaked through publication error")
			}
			if old.items["backup"].password != "synthetic-password-kept-only-in-memory" {
				t.Fatal("uncompleted publication lost its encrypted recovery backup")
			}
			if published != (phase != "publish-failed") {
				t.Fatal("unexpected publication state")
			}
			delete(backup.fail, "delete")
			if err := migrateCredential(context.Background(), old, current, backup, func() error { published = true; return nil }); err != nil {
				t.Fatal(err)
			}
			if !published || old.items["main"].owner != "new" || old.items["main"].password != "synthetic-password-kept-only-in-memory" {
				t.Fatal("retry did not preserve the new owner and original password")
			}
			if _, exists := old.items["backup"]; exists {
				t.Fatal("completed retry left its backup behind")
			}
		})
	}
}

func TestCredentialErrorSlotContextPreservesRollbackAndCause(t *testing.T) {
	original := errors.Join(context.Canceled, &SecretError{Operation: "set", Code: "keychain:-25244"})
	err := CredentialErrorForSlot("mac2", original)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "migrate mac2") || !strings.Contains(err.Error(), "--from") {
		t.Fatalf("lost recovery context: %v", err)
	}
	for _, code := range []string{"secret\nsynthetic-password", "keychain:synthetic-password", "keychain:-9999999999999999999999999999"} {
		if err := secretCommandError([]string{"secret", "get"}, code); strings.Contains(err.Error(), "synthetic-password") || strings.Contains(err.Error(), "999999") {
			t.Fatal("untrusted code echoed as public diagnostic")
		}
	}
}

func TestCredentialMigrationRejectsRunningSlotBeforeReadingSecrets(t *testing.T) {
	if HostSupported() != nil || os.Geteuid() == 0 {
		t.Skip("manager requires ordinary macOS user")
	}
	for _, mode := range []string{"runner-lock", "rpc-running", "auto-running"} {
		t.Run(mode, func(t *testing.T) {
			m, held, slot, dir := lifecycleSlot(t)
			if err := held.Release(); err != nil {
				t.Fatal(err)
			}
			var capture string
			m.Runner, capture = lifecycleRunner(t, "running")
			original := filepath.Join(t.TempDir(), "original-runner")
			if err := os.WriteFile(original, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if mode == "auto-running" {
				original = ""
			}
			if mode == "runner-lock" {
				holdRunnerLock(t, dir)
			}
			if _, err := m.MigrateCredentials(context.Background(), slot.Name, original); err == nil || !strings.Contains(err.Error(), "farrow mac stop mac1") {
				t.Fatalf("migration accepted active guest: %v", err)
			}
			if mode == "runner-lock" {
				if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("busy migration invoked a runner")
				}
			} else {
				data, err := os.ReadFile(capture)
				if err != nil || strings.Contains(string(data), "secret") {
					t.Fatalf("active migration invoked secret command: %s %v", data, err)
				}
			}
		})
	}
}
