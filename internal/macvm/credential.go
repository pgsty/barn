package macvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/farrow/internal/lock"
)

// SecretError deliberately contains no native message, stderr or password.
// Slot can be filled by callers to make the recovery command directly useful.
type SecretError struct {
	Operation string
	Code      string
	Slot      string
}

func (e *SecretError) Error() string {
	detail := "Keychain operation failed"
	switch e.Code {
	case "keychain:-25244":
		detail = "the saved password belongs to a different runner signing identity"
	case "keychain:-25293", "keychain:-25308", "keychain:-128":
		detail = "Keychain access was denied or requires an unlocked login Keychain and user approval"
	case "keychain:-25300":
		detail = "the saved GUI password is missing from the login Keychain"
	case "keychain:-25291":
		detail = "the login Keychain is unavailable"
	case "invalid_response":
		detail = "the runner returned an invalid secret response"
	}
	message := fmt.Sprintf("mac password %s: %s (%s)", e.Operation, detail, e.Code)
	if e.Code == "keychain:-25244" || e.Code == "keychain:-25293" || e.Code == "keychain:-25308" {
		name := e.Slot
		if name == "" {
			name = "<slot>"
		}
		message += fmt.Sprintf("; if this began after replacing the app or runner, stop the VM and run 'farrow mac credentials migrate %s'; if no retained owner is found, add --from /absolute/path/to/original/Farrow\\ Mac.app", name)
	}
	return message
}

func secretCommandError(args []string, code string) error {
	operation := "operation"
	if len(args) > 1 && (args[1] == "get" || args[1] == "set" || args[1] == "delete") {
		operation = args[1]
	}
	if strings.HasPrefix(code, "keychain:") {
		if _, err := strconv.ParseInt(strings.TrimPrefix(code, "keychain:"), 10, 32); err != nil {
			code = "secret_failed"
		}
	} else if code != "invalid_response" {
		code = "secret_failed"
	}
	return &SecretError{Operation: operation, Code: code}
}

// CredentialErrorForSlot adds only public slot context to a safe secret error.
func CredentialErrorForSlot(name string, err error) error {
	var secret *SecretError
	if errors.As(err, &secret) {
		copy := *secret
		copy.Slot = name
		return credentialSlotError{err: err, original: secret.Error(), replacement: copy.Error()}
	}
	return err
}

type credentialSlotError struct {
	err                   error
	original, replacement string
}

func (e credentialSlotError) Error() string {
	return strings.ReplaceAll(e.err.Error(), e.original, e.replacement)
}
func (e credentialSlotError) Unwrap() error { return e.err }

type CredentialMigration struct {
	Slot  string `json:"slot"`
	State string `json:"state"`
}

// MigrateCredentials transfers an existing Keychain item through the binary
// that owns it. It does not change Keychain ACLs, VM state, or guest passwords.
func (m *Manager) MigrateCredentials(ctx context.Context, name, oldRunner string) (result *CredentialMigration, retErr error) {
	name, err := NormalizeSlot(name)
	if err != nil {
		return nil, err
	}
	if err := m.requireRunner(); err != nil {
		return nil, err
	}
	operation, err := m.acquireOperation(ctx, name, "credentials migrate")
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, operation, "credentials migrate") }()
	slot, err := m.Store.LoadSlot(name)
	if err != nil {
		return nil, err
	}
	dir, err := m.Store.Path("slots", name)
	if err != nil {
		return nil, err
	}
	stopped, err := lock.TryAcquire(filepath.Join(dir, "runner.lock"), false)
	if err != nil {
		return nil, fmt.Errorf("%s is running or busy; run 'farrow mac stop %s' before migrating credentials: %w", name, name, err)
	}
	defer func() { retErr = lock.JoinRelease(retErr, stopped, "credentials migrate stopped VM") }()
	if status, rpcErr := m.status(ctx, slot); rpcErr == nil && status.State != "stopped" {
		return nil, fmt.Errorf("%s runtime is %s; run 'farrow mac stop %s' before migrating credentials", name, status.State, name)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if completed, err := m.finishPublishedCredentialMigration(ctx, slot.InstanceID); err != nil {
		return nil, CredentialErrorForSlot(name, err)
	} else if completed {
		return &CredentialMigration{Slot: name, State: "migrated"}, nil
	}
	if oldRunner == "" {
		oldRunner, err = m.previousCredentialRunner(ctx, slot.InstanceID)
		if err != nil {
			return nil, CredentialErrorForSlot(name, err)
		}
	}
	if !filepath.IsAbs(oldRunner) {
		return nil, errors.New("--from must be the absolute path to the original signed runner or Farrow Mac.app")
	}
	oldRunner = filepath.Clean(oldRunner)
	if info, err := os.Stat(oldRunner); err == nil && info.IsDir() && strings.HasSuffix(oldRunner, ".app") {
		oldRunner = filepath.Join(oldRunner, "Contents", "MacOS", "farrow-mac-runner")
	}
	oldRunner, err = executableFile(oldRunner)
	if err != nil {
		return nil, fmt.Errorf("original runner is unavailable; preserve or restore the original signed app: %w", err)
	}
	oldInfo, err := os.Stat(oldRunner)
	if err != nil {
		return nil, err
	}
	currentInfo, err := os.Stat(m.Runner.Binary)
	if err != nil {
		return nil, err
	}
	if os.SameFile(oldInfo, currentInfo) {
		return nil, errors.New("--from selects the current runner; select the original signed runner that created the saved password")
	}
	currentRunner, currentHash, err := m.Runner.retainCredentialRunner(ctx, m.Store.Root)
	if err != nil {
		return nil, fmt.Errorf("could not retain the new signed credential owner; Keychain was not changed: %w", err)
	}
	originalRunner, originalHash, err := (Runner{Binary: oldRunner}).retainCredentialRunner(ctx, m.Store.Root)
	if err != nil {
		return nil, fmt.Errorf("could not retain the original signed credential owner; Keychain was not changed: %w", err)
	}
	// Legacy installations also need a durable recovery reference before the
	// original item is removed. Never replace an already published new owner
	// when resuming an interrupted migration.
	record, err := readCredentialRunnerRecord(m.Store.Root, slot.InstanceID)
	if err != nil {
		return nil, err
	}
	if record == nil && originalHash != "" {
		if err := saveCredentialRunnerRecord(m.Store.Root, slot.InstanceID, credentialRunnerRecord{Hash: originalHash}); err != nil {
			return nil, fmt.Errorf("could not save credential recovery reference; Keychain was not changed: %w", err)
		}
	}
	service := secretService(m.Store.Root)
	old := runnerCredentialOwner{originalRunner, service, slot.InstanceID}
	current := runnerCredentialOwner{currentRunner, service, slot.InstanceID}
	backup := runnerCredentialOwner{originalRunner, service + ".migration", slot.InstanceID}
	m.progress("Migrating %s saved GUI password with the original runner", name)
	if err := migrateCredential(ctx, old, current, backup, func() error {
		return saveCredentialRunnerRecord(m.Store.Root, slot.InstanceID, credentialRunnerRecord{Hash: currentHash, MigrationFrom: originalHash})
	}); err != nil {
		return nil, CredentialErrorForSlot(name, err)
	}
	if err := saveCredentialRunnerRecord(m.Store.Root, slot.InstanceID, credentialRunnerRecord{Hash: currentHash}); err != nil {
		return nil, fmt.Errorf("credential migrated successfully, but recovery reference cleanup failed; repeat the same migration command to finish cleanup: %w", err)
	}
	return &CredentialMigration{Slot: name, State: "migrated"}, nil
}

type credentialOwner interface {
	get(context.Context) (string, error)
	set(context.Context, string) error
	delete(context.Context) error
}
type runnerCredentialOwner struct {
	runner            Runner
	service, instance string
}

func (o runnerCredentialOwner) get(ctx context.Context) (string, error) {
	return o.runner.passwordAtService(ctx, o.service, o.instance)
}
func (o runnerCredentialOwner) set(ctx context.Context, password string) error {
	return o.runner.setPasswordAtService(ctx, o.service, o.instance, password)
}
func (o runnerCredentialOwner) delete(ctx context.Context) error {
	return o.runner.deletePasswordAtService(ctx, o.service, o.instance)
}

func secretMissing(err error) bool {
	var secret *SecretError
	return errors.As(err, &secret) && secret.Code == "keychain:-25300"
}

func migrateCredential(ctx context.Context, old, current, backup credentialOwner, publish func() error) error {
	password, originalErr := old.get(ctx)
	backedUp, backupErr := backup.get(ctx)
	if originalErr != nil {
		if backupErr != nil {
			return fmt.Errorf("original runner could not read the saved password or migration backup; nothing was changed: %w", originalErr)
		}
		password = backedUp
		// A previous process may have written the new item before exiting.
		// Reading it alone does not prove ownership: Keychain read permission
		// can differ from deletion permission. Recreate it under the new owner.
		if verified, err := current.get(ctx); err == nil {
			if verified != password {
				return errors.New("current password conflicts with the migration backup; both items were preserved")
			}
		} else if !secretMissing(err) {
			return fmt.Errorf("could not verify the interrupted migration; encrypted backup was preserved: %w", err)
		}
	}
	if password == "" {
		return errors.New("original runner returned an empty password; nothing was changed")
	}
	if backupErr == nil {
		if backedUp != password {
			return errors.New("migration backup conflicts with the original password; both items were preserved")
		}
	} else if secretMissing(backupErr) {
		if err := backup.set(ctx, password); err != nil {
			return fmt.Errorf("could not create an encrypted migration backup; original item was preserved: %w", err)
		}
		verified, err := backup.get(ctx)
		if err != nil || verified != password {
			return errors.New("could not verify the encrypted migration backup; original item was preserved")
		}
	} else {
		return fmt.Errorf("could not read the encrypted migration backup; original item was preserved: %w", backupErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := old.delete(ctx); err != nil {
		// On a resumed migration the main item may already belong to the new
		// signer. Only delete through that signer after verifying its contents.
		verified, readErr := current.get(ctx)
		if readErr != nil || verified != password {
			return rollbackCredential(old, current, password, "remove original item", err, false)
		}
		if removeErr := current.delete(ctx); removeErr != nil {
			return rollbackCredential(old, current, password, "remove original item", errors.Join(err, removeErr), false)
		}
	}
	if err := current.set(ctx, password); err != nil {
		return rollbackCredential(old, current, password, "create new item", err, true)
	}
	verified, err := current.get(ctx)
	if err != nil {
		return rollbackCredential(old, current, password, "verify new item", err, true)
	}
	if verified != password {
		return rollbackCredential(old, current, password, "verify new item", errors.New("saved password verification did not match"), true)
	}
	if err := ctx.Err(); err != nil {
		return rollbackCredential(old, current, password, "verify new item", err, true)
	}
	// Filesystem publication is part of the migration, not post-processing.
	// Keep the encrypted backup until the new owner and recovery source can
	// both be found after a process exit or a failed metadata write.
	if publish != nil {
		if err := publish(); err != nil {
			return fmt.Errorf("could not save the new credential owner; encrypted migration backup was preserved; repeat the same migration command: %w", err)
		}
	}
	return removeMigrationBackup(ctx, backup)
}

func removeMigrationBackup(ctx context.Context, backup credentialOwner) error {
	if err := backup.delete(ctx); err != nil {
		return fmt.Errorf("new runner password was verified, but encrypted migration backup cleanup failed; keep the original runner and repeat the same migration command to finish cleanup: %w", err)
	}
	return nil
}

func rollbackCredential(old, current credentialOwner, password, stage string, cause error, newMayExist bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The original may still exist after a failed delete. Avoid touching its
	// ownership when we can already prove it retained the exact password.
	if !newMayExist {
		if verified, err := old.get(ctx); err == nil && verified == password {
			return fmt.Errorf("credential migration failed during %s; original password is preserved: %w", stage, cause)
		}
	}
	if newMayExist {
		if err := current.delete(ctx); err != nil {
			return fmt.Errorf("credential migration failed during %s and could not remove the new item; encrypted migration backup was preserved; keep both signed runners and repeat the same migration command: %w", stage, errors.Join(cause, err))
		}
	}
	if err := old.set(ctx, password); err != nil {
		return fmt.Errorf("credential migration failed during %s and rollback failed; encrypted migration backup was preserved; keep the original signed runner and repeat the same migration command: %w", stage, errors.Join(cause, err))
	}
	verified, err := old.get(ctx)
	if err != nil || verified != password {
		return fmt.Errorf("credential migration failed during %s; rollback could not be verified; keep the original signed runner for recovery: %w", stage, errors.Join(cause, err))
	}
	return fmt.Errorf("credential migration failed during %s; original password was restored: %w", stage, cause)
}
