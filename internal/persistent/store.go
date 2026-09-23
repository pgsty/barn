// Package persistent owns deployment data disks which survive node
// destruction.  The store is intentionally small and strict: every retained
// disk has one typed ownership marker, deterministic paths, and no unowned
// entries are tolerated.
package persistent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/naming"
)

const (
	Schema       = 1
	rootName     = "disks"
	markerName   = "ownership.json"
	diskFileName = "disk.qcow2"
)

// Identity is the immutable attachment contract for one persistent disk.
// Size is deliberately exact: recreate never grows, shrinks, or silently
// repurposes a retained disk.
type Identity struct {
	Node       string
	Name       string
	Serial     string
	Size       int64
	Mount      string
	Filesystem string
}

// Record is the on-disk ownership marker.  Path and OwnerUID bind the marker
// to the exact Farrow-owned file and local account.
type Record struct {
	Schema      int       `json:"schema"`
	Node        string    `json:"node"`
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Serial      string    `json:"serial"`
	Size        int64     `json:"size_bytes"`
	Mount       string    `json:"mount"`
	Filesystem  string    `json:"filesystem,omitempty"`
	OwnerUID    int       `json:"owner_uid"`
	PreservedAt time.Time `json:"preserved_at"`
}

func key(node, name string) string { return node + "\x00" + name }

func validateIdentity(identity Identity) error {
	if !naming.ValidNodeName(identity.Node) || !naming.ValidNodeName(identity.Name) {
		return errors.New("persistent disk node/name identity is invalid")
	}
	if identity.Serial == "" || identity.Size <= 0 || !filepath.IsAbs(identity.Mount) || filepath.Clean(identity.Mount) != identity.Mount || identity.Mount == "/" {
		return errors.New("persistent disk serial, size, or mount identity is invalid")
	}
	return nil
}

func rootPath(root string) string {
	return filepath.Join(root, rootName)
}

func paths(root string, identity Identity) (directory, diskPath, markerPath string, err error) {
	if err = validateIdentity(identity); err != nil {
		return "", "", "", err
	}
	directory = filepath.Join(rootPath(root), identity.Node, identity.Name)
	diskPath = filepath.Join(directory, diskFileName)
	markerPath = filepath.Join(directory, markerName)
	inside, withinErr := fsutil.IsWithin(root, diskPath)
	if withinErr != nil || !inside {
		return "", "", "", errors.New("persistent disk path escapes the deployment root")
	}
	return directory, diskPath, markerPath, nil
}

func owned(info os.FileInfo, mode os.FileMode, directory bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return false
	}
	if directory {
		return info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == mode
	}
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == mode && stat.Nlink == 1
}

func validateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !owned(info, 0o700, true) {
		return fmt.Errorf("persistent disk directory is missing or unsafe: %s", path)
	}
	return nil
}

func validateFile(path string, maxSize int64) error {
	info, err := os.Lstat(path)
	if err != nil || !owned(info, 0o600, false) || (maxSize > 0 && info.Size() > maxSize) {
		return fmt.Errorf("persistent disk file is missing or unsafe: %s", path)
	}
	return nil
}

func validateCanonicalWithin(root, target string) error {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve persistent deployment root: %w", err)
	}
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("resolve persistent target: %w", err)
	}
	inside, err := fsutil.IsWithin(canonicalRoot, canonicalTarget)
	if err != nil || !inside {
		return fmt.Errorf("persistent target resolves outside the deployment root: %s", target)
	}
	return nil
}

// ValidateSource also accepts read-only group/other bits left by older
// qemu-img creation under umask 022/027. Preserve narrows them before publishing
// into the retained store, whose disk and marker modes remain exactly 0600.
func ValidateSource(root, source string) error {
	inside, err := fsutil.IsWithin(root, source)
	if err != nil || !inside {
		return errors.New("persistent source disk escapes the deployment root")
	}
	info, err := os.Lstat(source)
	if err != nil || !(owned(info, 0o600, false) || owned(info, 0o640, false) || owned(info, 0o644, false)) {
		return fmt.Errorf("persistent source disk is missing or unsafe: %s", source)
	}
	return validateCanonicalWithin(root, source)
}

func decodeMarker(path string) (Record, error) {
	if err := validateFile(path, 16<<10); err != nil {
		return Record{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("decode persistent disk ownership marker: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Record{}, errors.New("persistent disk ownership marker contains trailing JSON")
	}
	return record, nil
}

func recordIdentity(record Record) Identity {
	return Identity{Node: record.Node, Name: record.Name, Serial: record.Serial, Size: record.Size, Mount: record.Mount, Filesystem: record.Filesystem}
}

func validateRecord(root string, record Record) error {
	if record.Schema != Schema || record.OwnerUID != os.Geteuid() || record.PreservedAt.IsZero() {
		return errors.New("persistent disk ownership marker schema, owner, or timestamp is invalid")
	}
	directory, expectedDisk, _, err := paths(root, recordIdentity(record))
	if err != nil {
		return err
	}
	if record.Path != expectedDisk || filepath.Dir(record.Path) != directory {
		return errors.New("persistent disk ownership marker path is not canonical")
	}
	if err := validateFile(record.Path, 0); err != nil {
		return err
	}
	return validateCanonicalWithin(root, record.Path)
}

func sameIdentity(record Record, expected Identity) bool {
	return record.Node == expected.Node && record.Name == expected.Name && record.Serial == expected.Serial && record.Size == expected.Size && record.Mount == expected.Mount
}

// Inventory performs a complete fail-closed walk of the retained-disk store.
// An unexpected entry anywhere in the tree makes the whole store unusable.
func Inventory(deploymentRoot string) ([]Record, error) {
	root := rootPath(deploymentRoot)
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := validateDirectory(root); err != nil {
		return nil, err
	}
	nodes, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0)
	for _, nodeEntry := range nodes {
		if !naming.ValidNodeName(nodeEntry.Name()) {
			return nil, fmt.Errorf("persistent disk store has unexpected node entry %q", nodeEntry.Name())
		}
		nodeDir := filepath.Join(root, nodeEntry.Name())
		if err := validateDirectory(nodeDir); err != nil {
			return nil, err
		}
		disks, err := os.ReadDir(nodeDir)
		if err != nil {
			return nil, err
		}
		if len(disks) == 0 {
			return nil, fmt.Errorf("persistent disk node directory is unexpectedly empty: %s", nodeDir)
		}
		for _, diskEntry := range disks {
			if !naming.ValidNodeName(diskEntry.Name()) {
				return nil, fmt.Errorf("persistent disk store has unexpected disk entry %q", diskEntry.Name())
			}
			diskDir := filepath.Join(nodeDir, diskEntry.Name())
			if err := validateDirectory(diskDir); err != nil {
				return nil, err
			}
			entries, err := os.ReadDir(diskDir)
			if err != nil {
				return nil, err
			}
			if len(entries) != 2 || entries[0].Name() != diskFileName || entries[1].Name() != markerName {
				return nil, fmt.Errorf("persistent disk directory contains unexpected artifacts: %s", diskDir)
			}
			record, err := decodeMarker(filepath.Join(diskDir, markerName))
			if err != nil {
				return nil, err
			}
			if record.Node != nodeEntry.Name() || record.Name != diskEntry.Name() {
				return nil, errors.New("persistent disk marker identity differs from its directory")
			}
			if err := validateRecord(deploymentRoot, record); err != nil {
				return nil, err
			}
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Node == records[j].Node {
			return records[i].Name < records[j].Name
		}
		return records[i].Node < records[j].Node
	})
	return records, nil
}

// ValidateDesired verifies all retained disks against the complete desired
// persistent-disk set. Missing records are allowed for first creation; extra
// or semantically different records are rejected.
func ValidateDesired(root string, desired []Identity) (map[string]Record, error) {
	expected := make(map[string]Identity, len(desired))
	for _, identity := range desired {
		if err := validateIdentity(identity); err != nil {
			return nil, err
		}
		identityKey := key(identity.Node, identity.Name)
		if _, duplicate := expected[identityKey]; duplicate {
			return nil, errors.New("duplicate desired persistent disk identity")
		}
		expected[identityKey] = identity
	}
	records, err := Inventory(root)
	if err != nil {
		return nil, err
	}
	found := make(map[string]Record, len(records))
	for _, record := range records {
		identityKey := key(record.Node, record.Name)
		identity, ok := expected[identityKey]
		if !ok {
			return nil, fmt.Errorf("retained disk %s/%s is not present as persistent in desired configuration", record.Node, record.Name)
		}
		if !sameIdentity(record, identity) {
			return nil, fmt.Errorf("retained disk %s/%s has incompatible size, mount, or serial", record.Node, record.Name)
		}
		found[identityKey] = record
	}
	return found, nil
}

// Find returns a previously retained compatible disk, if any, while still
// validating the complete desired set and store.
func Find(root string, desired []Identity, identity Identity) (Record, bool, error) {
	found, err := ValidateDesired(root, desired)
	if err != nil {
		return Record{}, false, err
	}
	record, ok := found[key(identity.Node, identity.Name)]
	return record, ok, nil
}

func makeDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return validateDirectory(path)
}

func cleanupEmptyParents(root, directory string) {
	_ = os.Remove(directory)
	_ = os.Remove(filepath.Dir(directory))
	_ = os.Remove(rootPath(root))
}

// Preserve atomically relocates a node-local disk into the deterministic
// retained store and publishes its ownership marker. Existing compatible
// retained disks are a no-op only when source already equals the retained path.
func Preserve(root string, identity Identity, source string) (Record, error) {
	if err := validateIdentity(identity); err != nil {
		return Record{}, err
	}
	records, err := Inventory(root)
	if err != nil {
		return Record{}, err
	}
	for _, existing := range records {
		if existing.Node != identity.Node || existing.Name != identity.Name {
			continue
		}
		if !sameIdentity(existing, identity) {
			return Record{}, fmt.Errorf("retained disk %s/%s ownership is incompatible", identity.Node, identity.Name)
		}
		if filepath.Clean(source) != existing.Path {
			// A destroy interrupted after its rename already preserved this
			// disk: the node-local source is gone and the record matches.
			if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
				return existing, nil
			}
			return Record{}, errors.New("compatible retained disk already exists at a different path")
		}
		return existing, nil
	}
	if err := ValidateSource(root, source); err != nil {
		return Record{}, err
	}
	directory, target, marker, err := paths(root, identity)
	if err != nil {
		return Record{}, err
	}
	if err := makeDirectory(rootPath(root)); err != nil {
		return Record{}, err
	}
	if err := makeDirectory(filepath.Dir(directory)); err != nil {
		return Record{}, err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return Record{}, fmt.Errorf("create persistent disk directory: %w", err)
	}
	if err := validateDirectory(directory); err != nil {
		return Record{}, err
	}
	if err := os.Rename(source, target); err != nil {
		cleanupEmptyParents(root, directory)
		return Record{}, fmt.Errorf("preserve persistent disk: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = os.Remove(marker)
			_ = os.Rename(target, source)
			cleanupEmptyParents(root, directory)
		}
	}()
	if err := os.Chmod(target, 0o600); err != nil {
		return Record{}, fmt.Errorf("normalize retained disk permissions: %w", err)
	}
	if err := fsutil.SyncDir(filepath.Dir(source)); err != nil {
		return Record{}, err
	}
	record := Record{Schema: Schema, Node: identity.Node, Name: identity.Name, Path: target, Serial: identity.Serial, Size: identity.Size, Mount: identity.Mount, Filesystem: identity.Filesystem, OwnerUID: os.Geteuid(), PreservedAt: time.Now().UTC()}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return Record{}, err
	}
	if err := fsutil.AtomicWrite(marker, append(data, '\n'), 0o600); err != nil {
		return Record{}, err
	}
	if err := validateRecord(root, record); err != nil {
		return Record{}, err
	}
	rollback = false
	return record, fsutil.SyncDir(directory)
}

// DeleteAll deletes only a fully validated deployment-owned retained store.
func DeleteAll(deploymentRoot string) ([]Record, error) {
	records, err := Inventory(deploymentRoot)
	if err != nil || len(records) == 0 {
		return records, err
	}
	for _, record := range records {
		directory := filepath.Dir(record.Path)
		if err := os.Remove(record.Path); err != nil {
			return nil, err
		}
		if err := os.Remove(filepath.Join(directory, markerName)); err != nil {
			return nil, err
		}
		if err := os.Remove(directory); err != nil {
			return nil, err
		}
	}
	root := rootPath(deploymentRoot)
	nodes, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		nodeDir := filepath.Join(root, node.Name())
		entries, readErr := os.ReadDir(nodeDir)
		if readErr != nil || len(entries) != 0 {
			return nil, fmt.Errorf("persistent node directory changed during deletion: %s", nodeDir)
		}
		if err := os.Remove(nodeDir); err != nil {
			return nil, err
		}
	}
	if err := os.Remove(root); err != nil {
		return nil, err
	}
	if err := fsutil.SyncDir(deploymentRoot); err != nil {
		return nil, err
	}
	return records, nil
}
