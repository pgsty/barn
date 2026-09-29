package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pgsty/barn/internal/naming"
)

// Warnings are a disposable observation, separate from the strict node state
// and node artifacts that older releases must still be able to read and destroy.
type guestWarningCache struct {
	VMUUID     string         `json:"vm_uuid"`
	Generation uint64         `json:"generation"`
	SpecHash   string         `json:"spec_hash"`
	Warnings   []GuestWarning `json:"warnings"`
}

func (s Store) guestWarningsPath(node string) (string, error) {
	if !naming.ValidNodeName(node) {
		return "", fmt.Errorf("invalid node name %q", node)
	}
	return filepath.Join(s.Root, "guest-warnings", node+".json"), nil
}

func (s Store) readGuestWarnings(node NodeState) []GuestWarning {
	path, err := s.guestWarningsPath(node.Node)
	if err != nil {
		return nil // ReadNode already validated the name.
	}
	var cached guestWarningCache
	info, err := os.Lstat(filepath.Dir(path))
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			err = errors.New("guest warning cache is not a real directory")
		} else {
			err = strictRead(path, &cached)
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []GuestWarning{{Stage: "warning-state", Detail: "could not read saved guest warnings: " + err.Error()}}
	}
	if cached.VMUUID != node.VMUUID || cached.Generation != node.Generation || cached.SpecHash != node.SpecHash {
		return nil
	}
	return cached.Warnings
}

// WriteGuestWarnings never rewrites authoritative VM state. Its caller can
// report a cache failure without interrupting a usable guest.
func (s Store) WriteGuestWarnings(node NodeState, warnings []GuestWarning) error {
	if err := validateNode(node, node.Node); err != nil {
		return err
	}
	path, err := s.guestWarningsPath(node.Node)
	if err != nil {
		return err
	}
	if len(warnings) == 0 {
		info, err := os.Lstat(filepath.Dir(path))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("guest warning cache is not a real directory")
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := s.EnsureRoot(); err != nil {
		return err
	}
	if err := ensureDir(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeJSON(path, guestWarningCache{node.VMUUID, node.Generation, node.SpecHash, warnings})
}
