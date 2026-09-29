// Package sshkeys owns the deployment SSH key material: one Ed25519 pair and
// one known_hosts file under <root>/keys, generated on demand and removed only
// through the bounded purge below.
package sshkeys

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/fsutil"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// PurgeAction records one planned or applied key-file removal.
type PurgeAction struct {
	Path    string `json:"path"`
	Applied bool   `json:"applied"`
}

// PurgeReport lists the exact artifacts a purge plans or removed.
type PurgeReport struct {
	Apply   bool          `json:"apply"`
	Actions []PurgeAction `json:"actions"`
}

// PurgeStateError reports a safe, expected precondition failure: retained
// node artifacts or disks must be destroyed before keys can be purged.
type PurgeStateError struct{ Reason string }

func (e *PurgeStateError) Error() string { return "refuse key purge: " + e.Reason }

// PurgeIntegrityError reports a path, ownership, or mode invariant that makes
// deletion unsafe.
type PurgeIntegrityError struct{ Reason string }

func (e *PurgeIntegrityError) Error() string { return "key purge integrity check failed: " + e.Reason }

func purgeIntegrity(reason string, args ...any) error {
	return &PurgeIntegrityError{Reason: fmt.Sprintf(reason, args...)}
}

func ensureModeDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("unsafe directory %s", path)
	}
	return os.Chmod(path, 0o700)
}

// Existing guests trust the original identity, even when both local key files
// have been lost. Generating another pair would make recovery harder.
func EnsureExistingKeys(ctx context.Context, runner execx.Runner, root string) (string, string, string, error) {
	privateKey := filepath.Join(root, "keys", "id_ed25519")
	if _, err := os.Lstat(privateKey); errors.Is(err, os.ErrNotExist) {
		return "", "", "", fmt.Errorf("deployment private SSH key is missing; restore %s from backup to preserve access to existing guests", privateKey)
	} else if err != nil {
		return "", "", "", err
	}
	return EnsureKeys(ctx, runner, root)
}

// EnsureKeys creates <root>/keys with an Ed25519 pair and known_hosts when
// absent, normalizes modes, and returns the private key path, known_hosts
// path, and public key text.
func EnsureKeys(ctx context.Context, runner execx.Runner, root string) (string, string, string, error) {
	directory := filepath.Join(root, "keys")
	if err := ensureModeDirectory(directory); err != nil {
		return "", "", "", err
	}
	privateKey := filepath.Join(directory, "id_ed25519")
	publicKey := privateKey + ".pub"
	knownHosts := filepath.Join(directory, "known_hosts")
	if _, err := os.Lstat(privateKey); errors.Is(err, os.ErrNotExist) {
		if _, publicErr := os.Lstat(publicKey); !errors.Is(publicErr, os.ErrNotExist) {
			return "", "", "", fmt.Errorf("deployment private SSH key is missing; restore %s from backup to preserve access to existing guests", privateKey)
		}
		sshKeygen, lookErr := exec.LookPath("ssh-keygen")
		if lookErr != nil {
			return "", "", "", lookErr
		}
		if _, runErr := runner.Run(ctx, sshKeygen, "-q", "-t", "ed25519", "-N", "", "-C", "barn", "-f", privateKey); runErr != nil {
			return "", "", "", runErr
		}
	} else if err != nil {
		return "", "", "", err
	}
	if _, err := os.Lstat(knownHosts); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(knownHosts, nil, 0o600); err != nil {
			return "", "", "", err
		}
	} else if err != nil {
		return "", "", "", err
	}
	if err := restorePublicKey(privateKey, publicKey); err != nil {
		return "", "", "", err
	}
	for path, mode := range map[string]os.FileMode{privateKey: 0o600, publicKey: 0o644, knownHosts: 0o600} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return "", "", "", fmt.Errorf("deployment key file is unsafe: %s", path)
		}
		if err := os.Chmod(path, mode); err != nil {
			return "", "", "", err
		}
	}
	publicBytes, err := os.ReadFile(publicKey)
	if err != nil {
		return "", "", "", err
	}
	return privateKey, knownHosts, strings.TrimSpace(string(publicBytes)), nil
}

// The public half is derived data. Recover it without invoking ssh-keygen or
// replacing the private identity already trusted by running guests.
func restorePublicKey(privateKey, publicKey string) error {
	if _, err := os.Lstat(publicKey); !errors.Is(err, os.ErrNotExist) {
		return err // Existing files are checked by EnsureKeys below.
	}
	info, err := os.Lstat(privateKey)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return fmt.Errorf("cannot restore public SSH key: private key is unsafe: %s", privateKey)
	}
	if err := validateSSHOwner(info, true); err != nil {
		return fmt.Errorf("cannot restore public SSH key: %w", err)
	}
	handle, err := os.OpenFile(privateKey, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }() // Read-only identity source.
	opened, err := handle.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return fmt.Errorf("private SSH key changed while opening: %s", privateKey)
	}
	data, err := io.ReadAll(io.LimitReader(handle, 1<<20))
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return fmt.Errorf("cannot restore public SSH key from %s: %w; restore the original valid private key from backup", privateKey, err)
	}
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " barn\n"
	if err := fsutil.AtomicCreate(publicKey, []byte(public), 0o644); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("restore public SSH key: %w", err)
	}
	return nil
}

var purgeAllowlist = map[string]struct{}{
	"id_ed25519":      {},
	"id_ed25519.pub":  {},
	"known_hosts":     {},
	"known_hosts.old": {},
}

// PurgeKeys plans (and with apply removes) exactly the allowlisted key
// artifacts under <root>/keys, then the emptied directory itself. The caller
// is responsible for proving no node artifacts or retained disks remain.
func PurgeKeys(root string, apply bool) (PurgeReport, error) {
	report := PurgeReport{Apply: apply, Actions: []PurgeAction{}}
	keysDir := filepath.Join(root, "keys")
	info, err := os.Lstat(keysDir)
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, purgeIntegrity("inspect keys directory: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return report, purgeIntegrity("keys path is not a real directory: %s", keysDir)
	}
	if inside, withinErr := fsutil.IsWithin(root, keysDir); withinErr != nil || !inside {
		return report, purgeIntegrity("keys directory escapes the deployment root: %s", keysDir)
	}
	entries, err := os.ReadDir(keysDir)
	if err != nil {
		return report, purgeIntegrity("read keys directory: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		name := entry.Name()
		if _, ok := purgeAllowlist[name]; !ok {
			return report, purgeIntegrity("unexpected key-directory entry %q", name)
		}
		path := filepath.Join(keysDir, name)
		entryInfo, statErr := os.Lstat(path)
		if statErr != nil || !entryInfo.Mode().IsRegular() {
			return report, purgeIntegrity("key artifact is not a regular file: %s", path)
		}
		report.Actions = append(report.Actions, PurgeAction{Path: path})
	}
	if !apply {
		return report, nil
	}
	for index := range report.Actions {
		if err := os.Remove(report.Actions[index].Path); err != nil {
			return report, purgeIntegrity("remove key artifact %s: %v", report.Actions[index].Path, err)
		}
		report.Actions[index].Applied = true
	}
	if err := os.Remove(keysDir); err != nil {
		return report, purgeIntegrity("remove emptied keys directory: %v", err)
	}
	return report, fsutil.SyncDir(root)
}
