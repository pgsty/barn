package macvm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/lock"
)

type recreateJournal struct {
	Schema int     `json:"schema"`
	Name   string  `json:"name"`
	Phase  string  `json:"phase"`
	Backup string  `json:"backup"`
	Old    Machine `json:"old"`
}

func (s *Store) recreateJournalPath(name string) (string, error) {
	if !ValidName(name) {
		return "", invalidName(name)
	}
	return s.Path("runtime", "recreate-"+name+".json")
}

func (s *Store) recreateBackupPath(name string) (string, error) {
	if !ValidName(name) {
		return "", invalidName(name)
	}
	slots, err := s.Path("slots")
	if err != nil {
		return "", err
	}
	return filepath.Join(slots, ".recreate-"+name), nil
}

func (s *Store) readRecreateJournal(name string) (recreateJournal, string, error) {
	path, err := s.recreateJournalPath(name)
	if err != nil {
		return recreateJournal{}, "", err
	}
	var journal recreateJournal
	if err := readJSON(path, &journal); err != nil {
		return recreateJournal{}, path, err
	}
	backup, err := s.recreateBackupPath(name)
	if err != nil {
		return recreateJournal{}, path, err
	}
	if journal.Schema != 1 || journal.Name != name || journal.Backup != backup {
		return recreateJournal{}, path, errors.New("mac recreate journal identity is invalid")
	}
	switch journal.Phase {
	case "moving", "moved", "new-ready":
	default:
		return recreateJournal{}, path, errors.New("mac recreate journal phase is invalid")
	}
	if err := validateMachine(&journal.Old); err != nil || journal.Old.Name != name {
		return recreateJournal{}, path, errors.New("mac recreate journal contains invalid old machine state")
	}
	return journal, path, nil
}

func (s *Store) removeMachineDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("mac recreate slot is unsafe: %s", path)
	}
	runnerLock := filepath.Join(path, "runner.lock")
	if _, err := os.Lstat(runnerLock); err == nil {
		proof, acquireErr := lock.TryAcquire(runnerLock, false)
		if errors.Is(acquireErr, lock.ErrBusy) {
			return fmt.Errorf("%s runner is still active", filepath.Base(path))
		}
		if acquireErr != nil {
			return acquireErr
		}
		if releaseErr := proof.Release(); releaseErr != nil {
			return releaseErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.RemoveAll(path)
}

// recoverRecreate repairs a process that died while replacing a machine. A
// retained backup always wins until the new machine has been fully cloned and
// the journal has been removed.
func (s *Store) recoverRecreate(name string) (_ *Machine, returnErr error) {
	journalPath, err := s.recreateJournalPath(name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(journalPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	journal, _, err := s.readRecreateJournal(name)
	if err != nil {
		return nil, err
	}
	public, err := s.MachinePath(name)
	if err != nil {
		return nil, err
	}
	_, publicErr := os.Lstat(public)
	lockPath, err := s.lockPath()
	if err != nil {
		return nil, err
	}
	recoveryLock, err := lock.TryAcquire(lockPath, false)
	if errors.Is(err, lock.ErrBusy) {
		if errors.Is(publicErr, os.ErrNotExist) {
			old := journal.Old
			return &old, nil
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = lock.JoinRelease(returnErr, recoveryLock, "mac recreate recovery lock") }()

	// Re-read after taking the lock: another reader may have won the race
	// between the first journal check and this acquisition.
	journal, _, err = s.readRecreateJournal(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	public, err = s.MachinePath(name)
	if err != nil {
		return nil, err
	}
	backup, err := s.recreateBackupPath(name)
	if err != nil {
		return nil, err
	}
	_, publicErr = os.Lstat(public)
	_, backupErr := os.Lstat(backup)
	if backupErr != nil && !errors.Is(backupErr, os.ErrNotExist) {
		return nil, backupErr
	}
	if journal.Phase == "new-ready" {
		if publicErr != nil {
			return nil, fmt.Errorf("mac recreate completed without a new machine: %w", publicErr)
		}
		if _, err := s.loadMachine(name); err != nil {
			return nil, fmt.Errorf("mac recreate new machine is not durable: %w", err)
		}
		// The new machine is durable. The old directory is now only cleanup
		// work, so an interrupted removal must never roll the new machine back.
		if backupErr == nil {
			if err := os.RemoveAll(backup); err != nil {
				return nil, err
			}
			if err := fsutil.SyncDir(filepath.Dir(backup)); err != nil {
				return nil, err
			}
		} else if !errors.Is(backupErr, os.ErrNotExist) {
			return nil, backupErr
		}
		if err := os.Remove(journalPath); err != nil {
			return nil, err
		}
		return nil, fsutil.SyncDir(filepath.Dir(journalPath))
	}
	if errors.Is(backupErr, os.ErrNotExist) {
		if publicErr == nil {
			if err := os.Remove(journalPath); err != nil {
				return nil, err
			}
			return nil, fsutil.SyncDir(filepath.Dir(journalPath))
		}
		return nil, errors.New("mac recreate journal lost both old backup and new machine")
	}
	if publicErr == nil {
		if err := s.removeMachineDirectory(public); err != nil {
			return nil, err
		}
	} else if !errors.Is(publicErr, os.ErrNotExist) {
		return nil, publicErr
	}
	if err := os.Rename(backup, public); err != nil {
		return nil, err
	}
	if err := os.Remove(journalPath); err != nil {
		return nil, err
	}
	if err := fsutil.SyncDir(filepath.Dir(public)); err != nil {
		return nil, err
	}
	return nil, fsutil.SyncDir(filepath.Dir(journalPath))
}

func (s *Store) BeginRecreate(held *lock.File, old *Machine) error {
	if err := s.checkLock(held); err != nil {
		return err
	}
	if old == nil {
		return errors.New("mac recreate old machine is invalid")
	}
	if err := validateMachine(old); err != nil {
		return fmt.Errorf("mac recreate old machine is invalid: %w", err)
	}
	journalPath, err := s.recreateJournalPath(old.Name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(journalPath); err == nil {
		return errors.New("mac recreate already has a pending journal")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	backup, err := s.recreateBackupPath(old.Name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(backup); err == nil {
		return errors.New("mac recreate backup already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := s.mkdir("runtime"); err != nil {
		return err
	}
	journal := recreateJournal{Schema: 1, Name: old.Name, Phase: "moving", Backup: backup, Old: *old}
	if err := writeJSON(journalPath, &journal); err != nil {
		return err
	}
	public, err := s.MachinePath(old.Name)
	if err != nil {
		return err
	}
	runner, err := lock.TryAcquire(filepath.Join(public, "runner.lock"), false)
	if err != nil {
		return err
	}
	defer func() { _ = runner.Release() }()
	if err := os.Rename(public, backup); err != nil {
		return err
	}
	if err := fsutil.SyncDir(filepath.Dir(public)); err != nil {
		return err
	}
	journal.Phase = "moved"
	return writeJSON(journalPath, &journal)
}

func (s *Store) RestoreRecreate(held *lock.File, name string) error {
	if err := s.checkLock(held); err != nil {
		return err
	}
	journal, journalPath, err := s.readRecreateJournal(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	public, err := s.MachinePath(name)
	if err != nil {
		return err
	}
	backupInfo, backupErr := os.Lstat(journal.Backup)
	publicInfo, publicErr := os.Lstat(public)
	if errors.Is(backupErr, os.ErrNotExist) {
		if publicErr == nil && publicInfo.IsDir() {
			if err := os.Remove(journalPath); err != nil {
				return err
			}
			return fsutil.SyncDir(filepath.Dir(journalPath))
		}
		return fmt.Errorf("mac recreate cannot restore %s: backup is missing", name)
	}
	if backupErr != nil {
		return backupErr
	}
	if !backupInfo.IsDir() || backupInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("mac recreate backup is unsafe: %s", journal.Backup)
	}
	if err := s.removeMachineDirectory(public); err != nil {
		return err
	}
	if err := os.Rename(journal.Backup, public); err != nil {
		return err
	}
	if err := os.Remove(journalPath); err != nil {
		return err
	}
	return fsutil.SyncDir(filepath.Dir(public))
}

func (s *Store) FinalizeRecreate(held *lock.File, name string) error {
	if err := s.checkLock(held); err != nil {
		return err
	}
	journal, journalPath, err := s.readRecreateJournal(name)
	if err != nil {
		return err
	}
	journal.Phase = "new-ready"
	if err := writeJSON(journalPath, &journal); err != nil {
		return err
	}
	if err := os.RemoveAll(journal.Backup); err != nil {
		return err
	}
	if err := fsutil.SyncDir(filepath.Dir(journal.Backup)); err != nil {
		return err
	}
	if err := os.Remove(journalPath); err != nil {
		return err
	}
	return fsutil.SyncDir(filepath.Dir(journalPath))
}
