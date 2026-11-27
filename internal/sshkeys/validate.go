package sshkeys

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// SSHArtifactError identifies a trust-artifact integrity failure separately
// from an unavailable guest runtime so CLI callers can return the integrity
// exit code without parsing error strings.
type SSHArtifactError struct {
	Err error
}

func (e *SSHArtifactError) Error() string {
	if e == nil || e.Err == nil {
		return "SSH artifact integrity failure"
	}
	return "SSH artifact integrity failure: " + e.Err.Error()
}

func (e *SSHArtifactError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ValidateSSHArtifacts verifies the exact deployment SSH trust artifacts used
// by guest integrations. The keys directory and files must be owned by the
// current user, have their fixed modes, and be neither symbolic nor hard
// links. Files are opened relative to the verified directory descriptor with
// O_NOFOLLOW so validation cannot silently switch to another final object.
func ValidateSSHArtifacts(root string) (privateKey, knownHosts string, err error) {
	defer func() {
		if err == nil {
			return
		}
		var integrityError *SSHArtifactError
		if !errors.As(err, &integrityError) {
			err = &SSHArtifactError{Err: err}
		}
	}()
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", "", errors.New("SSH root is not a clean absolute path")
	}
	keysDir := filepath.Join(root, "keys")
	keysInfo, err := os.Lstat(keysDir)
	if err != nil || !keysInfo.IsDir() || keysInfo.Mode()&os.ModeSymlink != 0 || keysInfo.Mode().Perm() != 0o700 {
		return "", "", errors.New("SSH keys directory is missing or unsafe")
	}
	if err := validateSSHOwner(keysInfo, false); err != nil {
		return "", "", fmt.Errorf("SSH keys directory is unsafe: %w", err)
	}
	canonicalRoot, rootErr := filepath.EvalSymlinks(root)
	canonicalKeys, keysErr := filepath.EvalSymlinks(keysDir)
	if rootErr != nil || keysErr != nil || canonicalKeys != filepath.Join(canonicalRoot, "keys") {
		return "", "", errors.New("SSH keys directory escapes the deployment root")
	}

	descriptor, err := unix.Open(keysDir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return "", "", fmt.Errorf("open SSH keys directory: %w", err)
	}
	directory := os.NewFile(uintptr(descriptor), keysDir)
	if directory == nil {
		_ = unix.Close(descriptor)
		return "", "", errors.New("open SSH keys directory handle")
	}
	defer func() {
		// The no-follow directory descriptor is read-only and exists only to
		// bind later identity checks to the validated keys directory.
		_ = directory.Close()
	}()
	openedDirectory, err := directory.Stat()
	if err != nil || !os.SameFile(keysInfo, openedDirectory) || !openedDirectory.IsDir() || openedDirectory.Mode().Perm() != 0o700 {
		return "", "", errors.New("SSH keys directory identity changed while opening")
	}
	if err := validateSSHOwner(openedDirectory, false); err != nil {
		return "", "", fmt.Errorf("opened SSH keys directory is unsafe: %w", err)
	}

	privateKey = filepath.Join(keysDir, "id_ed25519")
	knownHosts = filepath.Join(keysDir, "known_hosts")
	for _, pathname := range []string{privateKey, knownHosts} {
		minimumSize := int64(1)
		// accept-new records the first host key. An owned empty trust file is
		// normal after up --no-wait; an empty private key is still invalid.
		if pathname == knownHosts {
			minimumSize = 0
		}
		if err := validateSSHArtifact(directory, pathname, minimumSize); err != nil {
			return "", "", err
		}
	}
	return privateKey, knownHosts, nil
}

func validateSSHOwner(info os.FileInfo, requireSingleLink bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("ownership does not match the current user")
	}
	if requireSingleLink && stat.Nlink != 1 {
		return errors.New("link count is not one")
	}
	return nil
}

func validateSSHArtifact(directory *os.File, pathname string, minimumSize int64) error {
	before, err := os.Lstat(pathname)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() < minimumSize {
		return fmt.Errorf("SSH artifact is missing or unsafe: %s", pathname)
	}
	if err := validateSSHOwner(before, true); err != nil {
		return fmt.Errorf("SSH artifact is unsafe %s: %w", pathname, err)
	}
	descriptor, err := unix.Openat(int(directory.Fd()), filepath.Base(pathname), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open SSH artifact %s: %w", pathname, err)
	}
	handle := os.NewFile(uintptr(descriptor), pathname)
	if handle == nil {
		_ = unix.Close(descriptor)
		return fmt.Errorf("open SSH artifact handle: %s", pathname)
	}
	opened, statErr := handle.Stat()
	after, lstatErr := os.Lstat(pathname)
	closeErr := handle.Close()
	if statErr != nil || lstatErr != nil || closeErr != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 || !after.Mode().IsRegular() || after.Mode().Perm() != 0o600 || opened.Size() < minimumSize || opened.Size() != before.Size() || after.Size() != opened.Size() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		return fmt.Errorf("SSH artifact identity changed while opening: %s", pathname)
	}
	if err := validateSSHOwner(opened, true); err != nil {
		return fmt.Errorf("opened SSH artifact is unsafe %s: %w", pathname, err)
	}
	if err := validateSSHOwner(after, true); err != nil {
		return fmt.Errorf("SSH artifact changed after opening %s: %w", pathname, err)
	}
	return nil
}
