// Package macvm manages Farrow's independent two-slot macOS environment.
// It intentionally does not use Linux inventory or Linux VM state.
package macvm

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/identity"
	"github.com/pgsty/farrow/internal/lock"
)

const (
	SchemaVersion            = 1
	DefaultCPU               = 4
	DefaultMemoryBytes int64 = 8 << 30
	DefaultDiskBytes   int64 = 100 << 30
)

type NetworkConfig struct {
	Subnet    string `json:"subnet"`
	Gateway   string `json:"gateway"`
	NetworkID string `json:"network_id"`
}

type SlotPreference struct {
	Name        string `json:"name"`
	IP          string `json:"ip"`
	MAC         string `json:"mac"`
	CPU         int    `json:"cpu"`
	MemoryBytes int64  `json:"memory_bytes"`
	DiskBytes   int64  `json:"disk_bytes"`
	// Set after creating an instance so later setup does not replace its saved
	// resource preferences, including an explicitly chosen default capacity.
	ResourcesConfigured bool `json:"resources_configured,omitempty"`
}

type Config struct {
	SchemaVersion  int              `json:"schema_version"`
	InstallationID string           `json:"installation_id"`
	Network        NetworkConfig    `json:"network"`
	Slots          []SlotPreference `json:"slots"`
	DefaultBaseID  string           `json:"default_base_id,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
}

// Slot describes an instance. IP and MAC belong to Config's stable slot.
// State records the latest completed or pending lifecycle stage; Initialized
// only becomes true after the SSH and sudo readiness checks have succeeded.
type Slot struct {
	SchemaVersion   int       `json:"schema_version"`
	Name            string    `json:"name"`
	InstanceID      string    `json:"instance_id,omitempty"`
	BaseID          string    `json:"base_id,omitempty"`
	State           string    `json:"state"`
	User            string    `json:"user,omitempty"`
	Initialized     bool      `json:"initialized"`
	CPU             int       `json:"cpu,omitempty"`
	MemoryBytes     int64     `json:"memory_bytes,omitempty"`
	DiskBytes       int64     `json:"disk_bytes,omitempty"`
	Version         string    `json:"version,omitempty"`
	Build           string    `json:"build,omitempty"`
	ObservedVersion string    `json:"observed_version,omitempty"`
	ObservedBuild   string    `json:"observed_build,omitempty"`
	PasswordRef     string    `json:"password_ref,omitempty"`
	CreatedAt       time.Time `json:"created_at,omitempty"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
}

// Store construction and reads never create directories. Root always names
// the isolated mac directory, not FARROW_HOME itself.
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

func NormalizeSlot(name string) (string, error) {
	switch name {
	case "", "1", "mac1":
		return "mac1", nil
	case "2", "mac2":
		return "mac2", nil
	default:
		return "", fmt.Errorf("invalid macOS slot %q: use mac1 or mac2", name)
	}
}

func NewInstanceID() (string, error) { return identity.NewUUID() }

func NewConfig(subnet string) (*Config, error) {
	p, err := ValidateSubnet(subnet)
	if err != nil {
		return nil, err
	}
	id, err := NewInstanceID()
	if err != nil {
		return nil, err
	}
	networkID, err := NewInstanceID()
	if err != nil {
		return nil, err
	}
	c := &Config{SchemaVersion: SchemaVersion, InstallationID: id, Network: NetworkConfig{Subnet: p.String(), Gateway: subnetAddress(p, 1).String(), NetworkID: networkID}, CreatedAt: time.Now().UTC()}
	for i, name := range []string{"mac1", "mac2"} {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		b[0] = b[0]&0xfc | 0x02 // locally administered, unicast
		// Keep the two reservations distinct even if the random bytes coincide.
		b[5] = b[5]&0xfe | byte(i)
		c.Slots = append(c.Slots, SlotPreference{Name: name, IP: subnetAddress(p, byte(10+i)).String(), MAC: net.HardwareAddr(b).String(), CPU: DefaultCPU, MemoryBytes: DefaultMemoryBytes, DiskBytes: DefaultDiskBytes})
	}
	return c, nil
}

func (c *Config) Preference(name string) (SlotPreference, error) {
	n, err := NormalizeSlot(name)
	if err != nil {
		return SlotPreference{}, err
	}
	for _, p := range c.Slots {
		if p.Name == n {
			return p, nil
		}
	}
	return SlotPreference{}, fmt.Errorf("missing preference for %s", n)
}

func (c *Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported mac config schema %d", c.SchemaVersion)
	}
	if !validUUID(c.InstallationID) || !validUUID(c.Network.NetworkID) {
		return errors.New("invalid mac installation or network UUID")
	}
	p, err := ValidateSubnet(c.Network.Subnet)
	if err != nil {
		return err
	}
	if c.Network.Gateway != subnetAddress(p, 1).String() {
		return errors.New("mac gateway must be the .1 address of its subnet")
	}
	if len(c.Slots) != 2 {
		return errors.New("mac config must contain exactly mac1 and mac2")
	}
	seen := map[string]bool{}
	for i, name := range []string{"mac1", "mac2"} {
		pref, err := c.Preference(name)
		if err != nil {
			return err
		}
		if pref.IP != subnetAddress(p, byte(i+10)).String() {
			return fmt.Errorf("%s IP must be its reserved .%d address", name, i+10)
		}
		mac, err := net.ParseMAC(pref.MAC)
		if err != nil || len(mac) != 6 || mac[0]&3 != 2 {
			return fmt.Errorf("%s MAC must be a locally administered unicast address", name)
		}
		if seen[mac.String()] {
			return errors.New("mac slot MAC addresses must be distinct")
		}
		seen[mac.String()] = true
		if pref.CPU < 2 || pref.MemoryBytes < 4<<30 || pref.DiskBytes < 32<<30 {
			return fmt.Errorf("%s resources are below the supported macOS minimum", name)
		}
	}
	if c.DefaultBaseID != "" && !safeID(c.DefaultBaseID) {
		return errors.New("invalid default base ID")
	}
	return nil
}

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

func (s *Store) LoadConfig() (*Config, error) {
	path, err := s.Path("config.json")
	if err != nil {
		return nil, err
	}
	var c Config
	if err := readJSON(path, &c); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			for _, name := range []string{"mac1", "mac2"} {
				dir, pathErr := s.SlotPath(name)
				if pathErr != nil {
					return nil, pathErr
				}
				entries, dirErr := os.ReadDir(dir)
				if dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
					return nil, dirErr
				}
				if len(entries) > 0 {
					return nil, errors.New("mac config is missing while slot artifacts remain; recover the saved network and installation identity before setup")
				}
			}
		}
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
		if c.InstallationID != old.InstallationID || c.Network != old.Network {
			return errors.New("mac installation and network identity cannot be changed in place")
		}
		for _, p := range old.Slots {
			next, _ := c.Preference(p.Name)
			if next.IP != p.IP || next.MAC != p.MAC {
				return errors.New("mac slot IP and MAC cannot be changed in place")
			}
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
	path, err := s.Path("config.json")
	if err != nil {
		return err
	}
	return writeJSON(path, c)
}

func (s *Store) SlotPath(name string) (string, error) {
	n, err := NormalizeSlot(name)
	if err != nil {
		return "", err
	}
	return s.Path("slots", n)
}

func (s *Store) LoadSlot(name string) (*Slot, error) {
	n, err := NormalizeSlot(name)
	if err != nil {
		return nil, err
	}
	path, err := s.Path("slots", n, "state.json")
	if err != nil {
		return nil, err
	}
	var slot Slot
	if err := readJSON(path, &slot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			entries, dirErr := os.ReadDir(filepath.Dir(path))
			if dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
				return nil, dirErr
			}
			if len(entries) > 0 {
				return nil, fmt.Errorf("%s state is missing but slot artifacts remain; preserve the directory and recover its state before init or image pruning", n)
			}
		}
		return nil, err
	}
	if err := validateSlot(&slot); err != nil {
		return nil, err
	}
	if slot.Name != n {
		return nil, errors.New("mac slot name does not match its directory")
	}
	return &slot, nil
}

func validateSlot(slot *Slot) error {
	if slot.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported mac slot schema %d", slot.SchemaVersion)
	}
	if slot.Name != "mac1" && slot.Name != "mac2" {
		return errors.New("invalid mac slot name")
	}
	if !validUUID(slot.InstanceID) || !safeID(slot.BaseID) {
		return errors.New("invalid mac instance UUID or base ID")
	}
	if !ValidUsername(slot.User) {
		return errors.New("mac slot user must be an ordinary valid administrator username")
	}
	if slot.PasswordRef != "" && slot.PasswordRef != slot.InstanceID {
		return errors.New("mac password reference must match its instance UUID")
	}
	switch slot.State {
	case "created", "starting", "running", "provisioning", "ready", "stopping", "stopped", "failed":
	default:
		return fmt.Errorf("invalid mac slot state %q", slot.State)
	}
	if slot.State == "ready" && !slot.Initialized {
		return errors.New("ready mac slot must have completed initialization")
	}
	if slot.CPU < 2 || slot.MemoryBytes < 4<<30 || slot.DiskBytes < 32<<30 {
		return errors.New("mac slot resources are below the supported minimum")
	}
	return nil
}

func (s *Store) SaveSlot(held *lock.File, slot *Slot) error {
	if slot == nil {
		return errors.New("mac slot is nil")
	}
	if err := validateSlot(slot); err != nil {
		return err
	}
	// Only the shared lock may create a base reference. A slot operation may
	// update the existing instance without blocking unrelated image preparation.
	shared := s.checkLock(held) == nil
	if !shared {
		path, err := s.Path("runtime", slot.Name+".operation.lock")
		if err != nil {
			return err
		}
		if err := held.ValidateExclusive(path); err != nil {
			return err
		}
	}
	if old, err := s.LoadSlot(slot.Name); err == nil {
		if old.InstanceID != slot.InstanceID || old.BaseID != slot.BaseID {
			return errors.New("mac instance identity and base are immutable; destroy the old instance before replacement")
		}
	} else if !shared {
		return fmt.Errorf("slot operation cannot create an instance reference: %w", err)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	base, err := s.LoadBase(slot.BaseID)
	if err != nil {
		return err
	}
	if base.State != "ready" {
		return errors.New("mac slot must reference a ready base")
	}
	if _, err := s.mkdir("slots", slot.Name); err != nil {
		return err
	}
	path, err := s.Path("slots", slot.Name, "state.json")
	if err != nil {
		return err
	}
	slot.UpdatedAt = time.Now().UTC()
	return writeJSON(path, slot)
}

func (s *Store) ListSlots() ([]Slot, error) {
	slots := make([]Slot, 0, 2)
	for _, name := range []string{"mac1", "mac2"} {
		slot, err := s.LoadSlot(name)
		if errors.Is(err, os.ErrNotExist) {
			slots = append(slots, Slot{SchemaVersion: SchemaVersion, Name: name, State: "empty"})
			continue
		}
		if err != nil {
			return nil, err
		}
		slots = append(slots, *slot)
	}
	return slots, nil
}

// DeleteSlot removes only this slot's directory. The caller must first stop
// and verify the runner. Stable reservations and shared images are preserved.
func (s *Store) DeleteSlot(held *lock.File, name string) error {
	if err := s.checkLock(held); err != nil {
		return err
	}
	path, err := s.SlotPath(name)
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
