// Package macvm manages Barn's macOS guest machines on Apple Silicon.
// It never reads the Linux inventory or Linux VM state.
package macvm

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/identity"
	"github.com/pgsty/barn/internal/lock"
)

const (
	// SchemaVersion describes config.json and each machine's state.json.
	SchemaVersion = 2
	// imageSchemaVersion describes installer and base metadata, unchanged
	// since the first layout so prepared bases remain reusable.
	imageSchemaVersion = 1

	DefaultCPU               = 4
	DefaultMemoryBytes int64 = 8 << 30
	DefaultDiskBytes   int64 = 100 << 30
	DefaultMachine           = "mac1"

	minimumCPU               = 2
	minimumMemoryBytes int64 = 4 << 30
	minimumDiskBytes   int64 = 32 << 30
)

// Config holds installation-wide state. Each machine owns everything else.
type Config struct {
	SchemaVersion  int    `json:"schema_version"`
	InstallationID string `json:"installation_id"`
	DefaultBaseID  string `json:"default_base_id,omitempty"`
	// SSHConfigOff records ssh-config --remove: lifecycle commands leave
	// ~/.ssh alone until ssh-config --install.
	SSHConfigOff bool      `json:"ssh_config_off,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// MachineNetwork is the private network the runner creates for one machine:
// the host takes the gateway and the guest MAC receives the reserved address.
type MachineNetwork struct {
	Subnet  string `json:"subnet"`
	Gateway string `json:"gateway"`
	Address string `json:"address"`
}

// Share is one host directory shown in the guest under
// /Volumes/My Shared Files/<name>.
type Share struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

// Machine is the durable record of one macOS guest. State records the latest
// completed or pending lifecycle stage; Initialized becomes true only after
// SSH and sudo readiness succeeded once.
type Machine struct {
	SchemaVersion   int            `json:"schema_version"`
	Name            string         `json:"name"`
	InstanceID      string         `json:"instance_id"`
	BaseID          string         `json:"base_id"`
	State           string         `json:"state"`
	User            string         `json:"user"`
	Initialized     bool           `json:"initialized"`
	CPU             int            `json:"cpu"`
	MemoryBytes     int64          `json:"memory_bytes"`
	DiskBytes       int64          `json:"disk_bytes"`
	MAC             string         `json:"mac"`
	Network         MachineNetwork `json:"network"`
	Shares          []Share        `json:"shares,omitempty"`
	Clipboard       bool           `json:"clipboard"`
	Version         string         `json:"version,omitempty"`
	Build           string         `json:"build,omitempty"`
	ObservedVersion string         `json:"observed_version,omitempty"`
	ObservedBuild   string         `json:"observed_build,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at,omitempty"`
	LastError       string         `json:"last_error,omitempty"`
}

type recreateJournal struct {
	Schema int     `json:"schema"`
	Name   string  `json:"name"`
	Phase  string  `json:"phase"`
	Backup string  `json:"backup"`
	Old    Machine `json:"old"`
}

// HostKeyAlias pins the guest SSH host key to the instance, not its address.
func (m *Machine) HostKeyAlias() string { return "barn-mac-" + strings.ToLower(m.InstanceID) }

var namePattern = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidName accepts short lowercase names usable as directory, host and SSH
// alias components: a letter first, then letters, digits or inner hyphens.
func ValidName(name string) bool { return namePattern.MatchString(name) }

func invalidName(name string) error {
	return failure.New(failure.Usage, fmt.Errorf("invalid machine name %q: use lowercase letters, digits and inner hyphens, starting with a letter (at most 32 characters)", name))
}

// Store construction and reads never create directories. Root always names
// the mac directory, not BARN_HOME itself.
type Store struct{ Root string }

func NewStore(barnHome string) (*Store, error) {
	if barnHome == "" {
		return nil, errors.New("barn home must be specified")
	}
	// Resolve aliases only through BARN_HOME. Barn-owned paths below it must
	// still be real directories/files, checked by noSymlinks and Store.Path.
	absolute, err := fsutil.CanonicalPath(barnHome)
	if err != nil {
		return nil, err
	}
	s := &Store{Root: filepath.Join(absolute, "mac")}
	if err := noSymlinks(s.Root); err != nil {
		return nil, err
	}
	return s, nil
}

func NewInstanceID() (string, error) { return identity.NewUUID() }

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	b, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil && len(b) == 16
}

func safeID(value string) bool {
	if value == "" || len(value) > 180 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// Path refuses lexical traversal and symlink components, including the root.
// Files are private to the invoking user; this is not a defence against a
// malicious process running as that same user swapping paths concurrently.
func (s *Store) Path(parts ...string) (string, error) {
	if s.Root == "" || !filepath.IsAbs(s.Root) {
		return "", errors.New("mac store root must be absolute")
	}
	for _, part := range parts {
		if part == "" || filepath.IsAbs(part) {
			return "", errors.New("mac path components must be nonempty and relative")
		}
		for _, component := range strings.Split(filepath.ToSlash(part), "/") {
			if component == ".." || component == "." || component == "" {
				return "", errors.New("mac path traversal is not permitted")
			}
		}
	}
	path := filepath.Join(append([]string{s.Root}, parts...)...)
	if path != s.Root {
		within, err := fsutil.IsWithin(s.Root, path)
		if err != nil || !within {
			return "", errors.New("mac path escapes data root")
		}
	}
	if err := noSymlinks(path); err != nil {
		return "", err
	}
	return path, nil
}

func noSymlinks(path string) error {
	for current := filepath.Clean(path); current != "/" && current != "."; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("mac state path is a symlink: %s", current)
		}
	}
	return nil
}

func (s *Store) mkdir(parts ...string) (string, error) {
	path, err := s.Path(parts...)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) lockPath() (string, error) { return s.Path("runtime", "state.lock") }

func (s *Store) checkLock(held *lock.File) error {
	path, err := s.lockPath()
	if err != nil {
		return err
	}
	return held.ValidateExclusive(path)
}

func readJSON(path string, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return fmt.Errorf("mac metadata must be a regular file of at most 1 MiB: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid trailing content in %s", path)
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(path, append(data, '\n'), 0o600)
}

// schemaOf reads and validates the metadata header before a strict decode.
func schemaOf(path string) (int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return 0, fmt.Errorf("mac metadata must be a regular file of at most 1 MiB: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	return header.SchemaVersion, nil
}

func (c *Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported mac config schema %d", c.SchemaVersion)
	}
	if !validUUID(c.InstallationID) {
		return errors.New("invalid mac installation UUID")
	}
	if c.DefaultBaseID != "" && !safeID(c.DefaultBaseID) {
		return errors.New("invalid default base ID")
	}
	return nil
}

func NewConfig() (*Config, error) {
	id, err := NewInstanceID()
	if err != nil {
		return nil, err
	}
	return &Config{SchemaVersion: SchemaVersion, InstallationID: id, CreatedAt: time.Now().UTC()}, nil
}

// LoadConfig returns os.ErrNotExist before the first setup.
func (s *Store) LoadConfig() (*Config, error) {
	path, err := s.Path("config.json")
	if err != nil {
		return nil, err
	}
	schema, err := schemaOf(path)
	if err != nil {
		return nil, err
	}
	if schema != SchemaVersion {
		return nil, fmt.Errorf("unsupported mac metadata schema %d; expected %d", schema, SchemaVersion)
	}
	var c Config
	if err := readJSON(path, &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) SaveConfig(held *lock.File, c *Config) error {
	if err := s.checkLock(held); err != nil {
		return err
	}
	if c == nil {
		return errors.New("mac config is nil")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if old, err := s.LoadConfig(); err == nil {
		if c.InstallationID != old.InstallationID {
			return errors.New("mac installation identity cannot be changed in place")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if c.DefaultBaseID != "" {
		base, err := s.LoadBase(c.DefaultBaseID)
		if err != nil {
			return err
		}
		if base.State != "ready" {
			return errors.New("only a ready base can become the default")
		}
	}
	if _, err := s.mkdir(); err != nil {
		return err
	}
	path, err := s.Path("config.json")
	if err != nil {
		return err
	}
	return writeJSON(path, c)
}

// MachinePath names a machine's private directory.
func (s *Store) MachinePath(name string) (string, error) {
	if !ValidName(name) {
		return "", invalidName(name)
	}
	return s.Path("slots", name)
}

func (s *Store) machineFile(name, file string) (string, error) {
	if !ValidName(name) {
		return "", invalidName(name)
	}
	return s.Path("slots", name, file)
}

// LoadMachine returns os.ErrNotExist for an unknown machine. A directory that
// holds files but no state is refused, never treated as a fresh machine.
func (s *Store) LoadMachine(name string) (*Machine, error) {
	recovered, err := s.recoverRecreate(name)
	if err != nil {
		return nil, err
	}
	if recovered != nil {
		return recovered, nil
	}
	if err := s.cleanupDeletedMachines(); err != nil {
		return nil, err
	}
	return s.loadMachine(name)
}

func (s *Store) loadMachine(name string) (*Machine, error) {
	path, err := s.machineFile(name, "state.json")
	if err != nil {
		return nil, err
	}
	schema, err := schemaOf(path)
	if errors.Is(err, os.ErrNotExist) {
		entries, dirErr := os.ReadDir(filepath.Dir(path))
		if dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
			return nil, dirErr
		}
		// Creation writes the password first, through a dot-named temporary
		// file; that alone is not a machine.
		for _, entry := range entries {
			if entry.Name() != "password" && !strings.HasPrefix(entry.Name(), ".") {
				return nil, failure.New(failure.Conflict, fmt.Errorf("%s has files but no state; its directory was preserved for recovery: %s: %w", name, filepath.Dir(path), errStateless)).
					Then("barn mac destroy " + name)
			}
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if schema != SchemaVersion {
		return nil, fmt.Errorf("unsupported mac metadata schema %d; expected %d", schema, SchemaVersion)
	}
	var machine Machine
	if err := readJSON(path, &machine); err != nil {
		return nil, err
	}
	if err := validateMachine(&machine); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if machine.Name != name {
		return nil, errors.New("machine name does not match its directory")
	}
	return &machine, nil
}

const deletingMachinePrefix = ".deleting-"

// cleanupDeletedMachines finishes a machine removal whose process died after
// renaming the slot away from its public name. A live runner keeps its lock in
// the renamed directory, so recovery never removes files from an active VM.
func (s *Store) cleanupDeletedMachines() error {
	slots, err := s.Path("slots")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(slots)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), deletingMachinePrefix) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := strings.TrimPrefix(entry.Name(), deletingMachinePrefix)
		if !ValidName(name) {
			continue
		}
		path := filepath.Join(slots, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("deleting machine slot is not a directory: %s", path)
		}
		lockPath := filepath.Join(path, "runner.lock")
		if _, lockErr := os.Lstat(lockPath); lockErr == nil {
			proof, acquireErr := lock.TryAcquire(lockPath, false)
			if errors.Is(acquireErr, lock.ErrBusy) {
				continue
			}
			if acquireErr != nil {
				return acquireErr
			}
			if releaseErr := proof.Release(); releaseErr != nil {
				return releaseErr
			}
		} else if !errors.Is(lockErr, os.ErrNotExist) {
			return lockErr
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return fsutil.SyncDir(slots)
}

func validateMachine(m *Machine) error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported machine schema %d", m.SchemaVersion)
	}
	if !ValidName(m.Name) {
		return invalidName(m.Name)
	}
	if !validUUID(m.InstanceID) || !safeID(m.BaseID) {
		return errors.New("invalid instance UUID or base ID")
	}
	if !ValidUsername(m.User) {
		return errors.New("guest user must be an ordinary valid administrator username")
	}
	switch m.State {
	case "prepared", "starting", "running", "ready", "stopping", "stopped", "failed":
	default:
		return fmt.Errorf("invalid machine state %q", m.State)
	}
	if m.State == "ready" && !m.Initialized {
		return errors.New("a ready machine must have completed initialization")
	}
	if m.CPU < minimumCPU || m.MemoryBytes < minimumMemoryBytes || m.DiskBytes < minimumDiskBytes {
		return errors.New("resources are below the supported macOS minimum")
	}
	if !validMAC(m.MAC) {
		return errors.New("network MAC must be a locally administered unicast address")
	}
	if err := m.Network.Validate(); err != nil {
		return err
	}
	return validateShares(m.Shares)
}

func (s *Store) SaveMachine(held *lock.File, m *Machine) error {
	if m == nil {
		return errors.New("machine is nil")
	}
	if err := validateMachine(m); err != nil {
		return err
	}
	// Only the shared lock may create an instance reference. A machine
	// operation may update its own record without blocking image preparation.
	shared := s.checkLock(held) == nil
	if !shared {
		path, err := s.Path("runtime", m.Name+".operation.lock")
		if err != nil {
			return err
		}
		if err := held.ValidateExclusive(path); err != nil {
			return err
		}
	}
	// Use the recovery-free reader here. A caller holding the shared state
	// lock may be in the middle of a durable recreate journal, in which case
	// the public slot is intentionally absent until this write creates it.
	if old, err := s.loadMachine(m.Name); err == nil {
		if old.InstanceID != m.InstanceID || old.BaseID != m.BaseID {
			return errors.New("instance identity and base are immutable; recreate the machine to replace them")
		}
	} else if !shared {
		return fmt.Errorf("a machine operation cannot create an instance reference: %w", err)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	base, err := s.LoadBase(m.BaseID)
	if err != nil {
		return err
	}
	if base.State != "ready" {
		return errors.New("a machine must reference a ready base")
	}
	if _, err := s.mkdir("slots", m.Name); err != nil {
		return err
	}
	path, err := s.machineFile(m.Name, "state.json")
	if err != nil {
		return err
	}
	m.UpdatedAt = time.Now().UTC()
	return writeJSON(path, m)
}

// errStateless marks a machine directory that lost its state record, such as
// one left by an interrupted deletion. Only destroy acts on it.
var errStateless = errors.New("no machine record")

// ListMachines returns every machine sorted by name. Unknown directory names
// are ignored; an unreadable machine is an error so status never hides it.
func (s *Store) ListMachines() ([]Machine, error) { return s.listMachines(false) }

// usableMachines skips directories without a state record, which cannot run
// and must not block commands for the other machines.
func (s *Store) usableMachines() ([]Machine, error) { return s.listMachines(true) }

func (s *Store) listMachines(skipStateless bool) ([]Machine, error) {
	root, err := s.Path("slots")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []Machine{}, nil
	}
	if err != nil {
		return nil, err
	}
	machines := make([]Machine, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !ValidName(entry.Name()) {
			continue
		}
		machine, err := s.LoadMachine(entry.Name())
		if errors.Is(err, os.ErrNotExist) || skipStateless && errors.Is(err, errStateless) {
			continue
		}
		if err != nil {
			return nil, err
		}
		machines = append(machines, *machine)
	}
	sort.Slice(machines, func(i, j int) bool { return machines[i].Name < machines[j].Name })
	return machines, nil
}

// DeleteMachine removes one machine's directory. Callers first prove the
// runner stopped. Shared images stay in place.
func (s *Store) DeleteMachine(held *lock.File, name string) error {
	if err := s.checkLock(held); err != nil {
		return err
	}
	path, err := s.MachinePath(name)
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	trash := filepath.Join(parent, deletingMachinePrefix+name)
	if _, err := os.Lstat(trash); err == nil {
		if err := s.cleanupDeletedMachines(); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(path, trash); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := fsutil.SyncDir(parent); err != nil {
		return err
	}
	if err := os.RemoveAll(trash); err != nil {
		return err
	}
	if _, err := os.Stat(parent); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fsutil.SyncDir(parent)
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

// DefaultMachine picks the machine a command without a name acts on: the only
// machine, or mac1. With several machines and no mac1, the caller must choose.
func (s *Store) DefaultMachine() (string, error) {
	machines, err := s.usableMachines()
	if err != nil {
		return "", err
	}
	switch len(machines) {
	case 0:
		return DefaultMachine, nil
	case 1:
		return machines[0].Name, nil
	}
	names := make([]string, 0, len(machines))
	for _, machine := range machines {
		if machine.Name == DefaultMachine {
			return DefaultMachine, nil
		}
		names = append(names, machine.Name)
	}
	return "", failure.New(failure.Usage, fmt.Errorf("choose a machine: %s", strings.Join(names, ", ")))
}
