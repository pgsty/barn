// Package macvm manages Farrow's macOS guest machines on Apple Silicon.
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

	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/identity"
	"github.com/pgsty/farrow/internal/lock"
)

const (
	// SchemaVersion describes config.json and each machine's state.json.
	SchemaVersion = 2
	// legacySchemaVersion is the two-slot layout written before named machines.
	legacySchemaVersion = 1
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

// HostKeyAlias pins the guest SSH host key to the instance, not its address.
func (m *Machine) HostKeyAlias() string { return "farrow-mac-" + strings.ToLower(m.InstanceID) }

var namePattern = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidName accepts short lowercase names usable as directory, host and SSH
// alias components: a letter first, then letters, digits or inner hyphens.
func ValidName(name string) bool { return namePattern.MatchString(name) }

func invalidName(name string) error {
	return failure.New(failure.Usage, fmt.Errorf("invalid machine name %q: use lowercase letters, digits and inner hyphens, starting with a letter (at most 32 characters)", name))
}

// ErrLegacyState marks data written by the two-slot layout. Every command
// except migrate refuses it so nothing is reinterpreted by accident.
var ErrLegacyState = failure.New(failure.Conflict, errors.New("this Mac data uses the earlier two-slot layout")).
	Because("mac_legacy_state").Then("farrow mac migrate")

// Store construction and reads never create directories. Root always names
// the mac directory, not FARROW_HOME itself.
type Store struct{ Root string }

func NewStore(farrowHome string) (*Store, error) {
	if farrowHome == "" {
		return nil, errors.New("farrow home must be specified")
	}
	absolute, err := filepath.Abs(farrowHome)
	if err != nil {
		return nil, err
	}
	// macOS exposes /tmp and /var through system symlinks. Canonicalize only
	// these well-known ancestors; symlinks inside the selected home stay errors.
	for _, systemPath := range []string{"/tmp", "/var"} {
		if strings.HasPrefix(absolute, systemPath+"/") {
			if resolved, err := filepath.EvalSymlinks(systemPath); err == nil {
				absolute = filepath.Join(resolved, strings.TrimPrefix(absolute, systemPath+"/"))
			}
		}
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

// schemaOf reads only the schema number, so a legacy document is recognized
// before a strict decode rejects its different fields.
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

// LoadConfig returns os.ErrNotExist before the first setup and ErrLegacyState
// for the earlier two-slot layout.
func (s *Store) LoadConfig() (*Config, error) {
	path, err := s.Path("config.json")
	if err != nil {
		return nil, err
	}
	schema, err := schemaOf(path)
	if err != nil {
		return nil, err
	}
	if schema == legacySchemaVersion {
		return nil, ErrLegacyState
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

// MachinePath names a machine's private directory. The historical "slots"
// directory keeps existing instances in place.
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
					Then("farrow mac destroy " + name)
			}
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if schema == legacySchemaVersion {
		return nil, ErrLegacyState
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
	if old, err := s.LoadMachine(m.Name); err == nil {
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
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fsutil.SyncDir(filepath.Dir(path))
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
