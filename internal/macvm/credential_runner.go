package macvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pgsty/farrow/internal/fsutil"
)

// Keep the signed Keychain owner with the installation, independently of app
// replacement. Only native credential operations use it; VMs use the new app.
type credentialRunnerRecord struct {
	Hash          string `json:"sha256"`
	MigrationFrom string `json:"migration_from,omitempty"`
}

func runnerApp(binary string) string {
	app := filepath.Dir(filepath.Dir(filepath.Dir(binary)))
	if strings.HasSuffix(app, ".app") && filepath.Base(binary) == "farrow-mac-runner" && filepath.Base(filepath.Dir(binary)) == "MacOS" && filepath.Base(filepath.Dir(filepath.Dir(binary))) == "Contents" {
		return app
	}
	return ""
}

func credentialRecordPath(root, instance string) (string, error) {
	if !validUUID(instance) {
		return "", errors.New("invalid credential instance identity")
	}
	return (&Store{Root: root}).Path("credentials", instance+".json")
}

func runnerDigest(binary string) (string, error) {
	data, err := os.ReadFile(binary)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func verifyCredentialApp(ctx context.Context, app, hash string) error {
	binary := filepath.Join(app, "Contents", "MacOS", "farrow-mac-runner")
	if err := noSymlinks(binary); err != nil {
		return err
	}
	actual, err := runnerDigest(binary)
	if err != nil {
		return err
	}
	if actual != hash {
		return errors.New("retained credential runner identity changed; original copy was preserved")
	}
	if err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", app).Run(); err != nil {
		return fmt.Errorf("verify retained credential owner signature: %w", err)
	}
	return nil
}

func readCredentialRunnerRecord(root, instance string) (*credentialRunnerRecord, error) {
	path, err := credentialRecordPath(root, instance)
	if err != nil {
		return nil, err
	}
	var record credentialRunnerRecord
	if err := readJSON(path, &record); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	// An empty owner is used only while migrating to a standalone runner.
	// Keep the previous app reference until its encrypted backup is removed.
	if (record.Hash != "" && !validSHA256(record.Hash)) ||
		(record.MigrationFrom != "" && !validSHA256(record.MigrationFrom)) ||
		(record.Hash == "" && record.MigrationFrom == "") {
		return nil, errors.New("invalid saved credential runner identity")
	}
	return &record, nil
}

func retainedCredentialRunner(ctx context.Context, root, hash string) (Runner, error) {
	if !validSHA256(hash) {
		return Runner{}, errors.New("invalid saved credential runner identity")
	}
	app, err := (&Store{Root: root}).Path("credentials", "owners", hash, "Farrow Mac.app")
	if err != nil {
		return Runner{}, err
	}
	if err := verifyCredentialApp(ctx, app, hash); err != nil {
		return Runner{}, fmt.Errorf("saved credential owner is unavailable; restore its original copy or use credentials migrate --from: %w", err)
	}
	return Runner{Binary: filepath.Join(app, "Contents", "MacOS", "farrow-mac-runner")}, nil
}

func (r Runner) credentialRunner(ctx context.Context, root, instance string) (Runner, error) {
	record, err := readCredentialRunnerRecord(root, instance)
	if err != nil {
		return Runner{}, err
	}
	if record == nil || record.Hash == "" {
		return r, nil
	}
	return retainedCredentialRunner(ctx, root, record.Hash)
}

func saveCredentialRunnerRecord(root, instance string, record credentialRunnerRecord) error {
	path, err := credentialRecordPath(root, instance)
	if err != nil {
		return err
	}
	if record.Hash == "" && record.MigrationFrom == "" {
		if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		return fsutil.SyncDir(filepath.Dir(path))
	}
	if _, err := (&Store{Root: root}).mkdir("credentials"); err != nil {
		return err
	}
	return writeJSON(path, record)
}

func (r Runner) preserveCredentialRunner(ctx context.Context, root, instance string) (Runner, error) {
	owner, hash, err := r.retainCredentialRunner(ctx, root)
	if err != nil {
		return Runner{}, err
	}
	if hash != "" {
		if err := saveCredentialRunnerRecord(root, instance, credentialRunnerRecord{Hash: hash}); err != nil {
			return Runner{}, err
		}
	}
	return owner, nil
}

// Copy and verify code before touching any Keychain item or publishing its owner.
func (r Runner) retainCredentialRunner(ctx context.Context, root string) (Runner, string, error) {
	source := runnerApp(r.Binary)
	// Custom standalone runners retain the explicit migration protocol.
	if source == "" {
		return r, "", nil
	}
	hash, err := runnerDigest(r.Binary)
	if err != nil {
		return Runner{}, "", err
	}
	if err := verifyCredentialApp(ctx, source, hash); err != nil {
		return Runner{}, "", err
	}
	store := &Store{Root: root}
	owners, err := store.mkdir("credentials", "owners")
	if err != nil {
		return Runner{}, "", err
	}
	target, err := store.Path("credentials", "owners", hash)
	if err != nil {
		return Runner{}, "", err
	}
	app := filepath.Join(target, "Farrow Mac.app")
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		stage, err := os.MkdirTemp(owners, ".stage-")
		if err != nil {
			return Runner{}, "", err
		}
		defer func() { _ = os.RemoveAll(stage) }()
		stagedApp := filepath.Join(stage, "Farrow Mac.app")
		if err := exec.CommandContext(ctx, "/usr/bin/ditto", source, stagedApp).Run(); err != nil {
			return Runner{}, "", fmt.Errorf("retain original signed credential owner: %w", err)
		}
		if err := verifyCredentialApp(ctx, stagedApp, hash); err != nil {
			return Runner{}, "", err
		}
		if err := os.Rename(stage, target); err != nil {
			if _, existsErr := os.Stat(target); existsErr != nil {
				return Runner{}, "", err
			}
		}
	} else if err != nil {
		return Runner{}, "", err
	}
	if err := verifyCredentialApp(ctx, app, hash); err != nil {
		return Runner{}, "", err
	}
	if err := fsutil.SyncDir(owners); err != nil {
		return Runner{}, "", err
	}
	return Runner{Binary: filepath.Join(app, "Contents", "MacOS", "farrow-mac-runner")}, hash, nil
}

// A process can exit after deleting the backup but before clearing its recovery
// reference. Finish that publication without trying to read the retired owner.
func (m *Manager) finishPublishedCredentialMigration(ctx context.Context, instance string) (bool, error) {
	record, err := readCredentialRunnerRecord(m.Store.Root, instance)
	if err != nil || record == nil || record.MigrationFrom == "" {
		return false, err
	}
	original, err := retainedCredentialRunner(ctx, m.Store.Root, record.MigrationFrom)
	if err != nil {
		return false, err
	}
	if _, err := original.passwordAtService(ctx, secretService(m.Store.Root)+".migration", instance); err == nil {
		return false, nil
	} else if !secretMissing(err) {
		return false, err
	}
	owner, err := m.Runner.credentialRunner(ctx, m.Store.Root, instance)
	if err != nil {
		return false, err
	}
	if _, err := owner.passwordAtService(ctx, secretService(m.Store.Root), instance); err != nil {
		return false, err
	}
	if err := saveCredentialRunnerRecord(m.Store.Root, instance, credentialRunnerRecord{Hash: record.Hash}); err != nil {
		return false, fmt.Errorf("credential is ready, but recovery reference cleanup failed; repeat the same migration command: %w", err)
	}
	ownerHash, err := runnerDigest(owner.Binary)
	if err != nil {
		return false, err
	}
	currentHash, err := runnerDigest(m.Runner.Binary)
	return ownerHash == currentHash, err
}

func (m *Manager) previousCredentialRunner(ctx context.Context, instance string) (string, error) {
	record, err := readCredentialRunnerRecord(m.Store.Root, instance)
	if err != nil {
		return "", err
	}
	if record != nil && record.MigrationFrom != "" {
		original, err := retainedCredentialRunner(ctx, m.Store.Root, record.MigrationFrom)
		if err != nil {
			return "", err
		}
		_, err = original.passwordAtService(ctx, secretService(m.Store.Root)+".migration", instance)
		if err == nil {
			return original.Binary, nil
		} else if !secretMissing(err) {
			return "", err
		}
		// The backup may already have been removed before a process exit.
		// Use the verified new owner instead of reviving the old identity.
	}
	saved, err := m.Runner.credentialRunner(ctx, m.Store.Root, instance)
	if err == nil && saved.Binary != m.Runner.Binary {
		return saved.Binary, nil
	}
	if err != nil {
		return "", err
	}
	app := runnerApp(m.Runner.Binary)
	if app == "" {
		return "", errors.New("no retained credential owner; specify --from with the original signed runner")
	}
	candidates, err := filepath.Glob(filepath.Join(filepath.Dir(app), ".mac-app-previous.*", "Farrow Mac.app"))
	if err != nil {
		return "", err
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, ea := os.Stat(filepath.Dir(candidates[i]))
		b, eb := os.Stat(filepath.Dir(candidates[j]))
		return ea == nil && (eb != nil || a.ModTime().After(b.ModTime()))
	})
	for _, candidate := range candidates {
		binary := filepath.Join(candidate, "Contents", "MacOS", "farrow-mac-runner")
		hash, err := runnerDigest(binary)
		if err != nil {
			continue
		}
		if err := verifyCredentialApp(ctx, candidate, hash); err != nil {
			continue
		}
		// Read only the requested instance. A locked/denied Keychain must not
		// trigger a sweep through older programs or discard its diagnostic.
		_, err = (Runner{Binary: binary}).Password(ctx, m.Store.Root, instance)
		if err == nil {
			return binary, nil
		}
		var secret *SecretError
		if !errors.As(err, &secret) || (secret.Code != "keychain:-25244" && secret.Code != "keychain:-25300") {
			return "", err
		}
	}
	return "", errors.New("original signed credential owner was not found; specify --from with the original Farrow Mac.app")
}
