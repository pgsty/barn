package macvm

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pgsty/barn/internal/fsutil"
)

// The guest's GUI password lives next to its SSH private key with the same
// 0600 protection. That key already grants passwordless sudo in the guest, so
// Keychain storage would add signing-identity and lock-state failures without
// protecting anything more.

func NewPassword() (string, error) {
	data := make([]byte, 30)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "Fr9!" + base64.RawURLEncoding.EncodeToString(data), nil
}

func (s *Store) passwordPath(name string) (string, error) { return s.machineFile(name, "password") }

func (s *Store) SavePassword(name, password string) error {
	if password == "" || strings.ContainsAny(password, "\r\n\x00") {
		return errors.New("guest password must be a nonempty single line")
	}
	path, err := s.passwordPath(name)
	if err != nil {
		return err
	}
	if _, err := s.mkdir("slots", name); err != nil {
		return err
	}
	return fsutil.AtomicWrite(path, []byte(password+"\n"), 0o600)
}

func (s *Store) Password(name string) (string, error) {
	path, err := s.passwordPath(name)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s has no saved GUI password; recreate the machine, or set one in the guest with barn mac exec %s -- sudo dscl . -passwd /Users/<user> <new-password>", name, name)
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 4096 {
		return "", fmt.Errorf("%s password file must be a private regular file: %s", name, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	password := strings.TrimSuffix(string(data), "\n")
	if password == "" {
		return "", fmt.Errorf("%s password file is empty: %s", name, path)
	}
	return password, nil
}
