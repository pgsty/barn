package macvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/lock"
)

type BaseImage struct {
	SchemaVersion     int       `json:"schema_version"`
	ID                string    `json:"id"`
	Version           string    `json:"version"`
	Build             string    `json:"build"`
	HardwareModelHash string    `json:"hardware_model_hash"`
	InstallerSHA256   string    `json:"installer_sha256"`
	State             string    `json:"state"`
	DiskBytes         int64     `json:"disk_bytes"`
	RecipeVersion     int       `json:"recipe_version"`
	CreatedAt         time.Time `json:"created_at"`
	LastError         string    `json:"last_error,omitempty"`
}

type ImageEntry struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Path    string `json:"path"`
	State   string `json:"state"`
	Version string `json:"version"`
	Build   string `json:"build"`
	Detail  string `json:"detail,omitempty"`
	// SizeBytes is the total file length; it is not guest-visible capacity.
	SizeBytes int64 `json:"size_bytes"`
	// AllocatedBytes sums filesystem blocks. Shared APFS blocks or hard links
	// can be counted repeatedly, so this is not exclusive physical usage.
	AllocatedBytes       int64    `json:"allocated_bytes"`
	VirtualCapacityBytes int64    `json:"virtual_capacity_bytes,omitempty"`
	References           []string `json:"references"`
	Default              bool     `json:"default"`
}

// BaseImageID includes the restore build, layout recipe, hardware model and
// capacity. New image versions never reuse the path of an older ready base.
func BaseImageID(build string, diskBytes int64, hardwareModelHash string, recipeVersion int) (string, error) {
	if !safeID(build) || diskBytes < 32<<30 || !validSHA256(hardwareModelHash) || recipeVersion < 1 {
		return "", errors.New("invalid base image identity inputs")
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\n%d\n%s\nasif-overlay-v%d", build, diskBytes, hardwareModelHash, recipeVersion)))
	return fmt.Sprintf("%s-%s", build, hex.EncodeToString(hash[:12])), nil
}

func (s *Store) BasePath(id string) (string, error) {
	if !safeID(id) {
		return "", errors.New("invalid mac base image ID")
	}
	return s.Path("images", "base", id)
}

func (s *Store) LoadBase(id string) (*BaseImage, error) {
	dir, err := s.BasePath(id)
	if err != nil {
		return nil, err
	}
	path, err := s.Path("images", "base", id, "metadata.json")
	if err != nil {
		return nil, err
	}
	var base BaseImage
	if err := readJSON(path, &base); err != nil {
		return nil, err
	}
	if err := base.Validate(); err != nil {
		return nil, fmt.Errorf("base %s: %w", dir, err)
	}
	if base.ID != id {
		return nil, errors.New("base image ID does not match its directory")
	}
	return &base, nil
}

func (b *BaseImage) Validate() error {
	if b.SchemaVersion != imageSchemaVersion || !safeID(b.ID) || !safeID(b.Build) || b.Version == "" {
		return errors.New("invalid mac base metadata")
	}
	if b.DiskBytes < 32<<30 || b.RecipeVersion < 1 {
		return errors.New("invalid mac base disk capacity or recipe version")
	}
	if !validSHA256(b.HardwareModelHash) || !validSHA256(b.InstallerSHA256) {
		return errors.New("base hardware model and installer require SHA256 digests")
	}
	switch b.State {
	case "restoring", "ready", "failed":
	default:
		return fmt.Errorf("invalid base image state %q", b.State)
	}
	return nil
}

func (s *Store) SaveBase(held *lock.File, base *BaseImage) error {
	if err := s.checkLock(held); err != nil {
		return err
	}
	if base == nil {
		return errors.New("base metadata is nil")
	}
	if err := base.Validate(); err != nil {
		return err
	}
	if old, err := s.LoadBase(base.ID); err == nil {
		if old.State == "ready" && *old != *base {
			return errors.New("a ready base is immutable; prepare a new base ID")
		}
		if old.Build != base.Build || old.Version != base.Version || old.DiskBytes != base.DiskBytes || old.HardwareModelHash != base.HardwareModelHash || old.InstallerSHA256 != base.InstallerSHA256 || old.RecipeVersion != base.RecipeVersion {
			return errors.New("base identity cannot be changed in place")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := s.mkdir("images", "base", base.ID); err != nil {
		return err
	}
	path, err := s.Path("images", "base", base.ID, "metadata.json")
	if err != nil {
		return err
	}
	return writeJSON(path, base)
}

func (s *Store) imageLockPath(kind, id string) (string, error) {
	if !safeID(id) || (kind != "base" && kind != "installer") {
		return "", errors.New("invalid mac image lock identity")
	}
	return s.Path("runtime", kind+"-"+id+".lock")
}

// LockBase pins an image while restoring or using it, independently of
// machine references. Lock order is global state first, then base/installer locks.
func (s *Store) LockBase(ctx context.Context, id string) (*lock.File, error) {
	if _, err := s.mkdir("runtime"); err != nil {
		return nil, err
	}
	path, err := s.imageLockPath("base", id)
	if err != nil {
		return nil, err
	}
	return lock.Acquire(ctx, path, false)
}

// ImageList includes only installers and bases, never machine overlays. File
// length, allocated filesystem blocks and guest-visible capacity are separate.
func (s *Store) ImageList() ([]ImageEntry, error) {
	result := []ImageEntry{}
	refs := map[string][]string{}
	machines, err := s.ListMachines()
	if err != nil {
		return nil, err
	}
	for _, machine := range machines {
		refs[machine.BaseID] = append(refs[machine.BaseID], machine.Name)
	}
	defaultID := ""
	if cfg, err := s.LoadConfig(); err == nil {
		defaultID = cfg.DefaultBaseID
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	baseRoot, err := s.Path("images", "base")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(baseRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue // Finder metadata such as .DS_Store
		}
		if !entry.IsDir() {
			return nil, fmt.Errorf("unexpected entry in base directory: %s", entry.Name())
		}
		base, err := s.LoadBase(entry.Name())
		if errors.Is(err, os.ErrNotExist) && safeID(entry.Name()) {
			// Interrupted before its metadata was written: list it so that
			// prune can remove it, instead of failing every image command.
			path, _ := s.BasePath(entry.Name())
			size, allocated, err := directoryFileUsage(path)
			if err != nil {
				return nil, err
			}
			result = append(result, ImageEntry{Kind: "base", ID: entry.Name(), Path: path, State: "incomplete", SizeBytes: size, AllocatedBytes: allocated,
				References: append([]string{}, refs[entry.Name()]...), Default: entry.Name() == defaultID})
			continue
		}
		if err != nil {
			return nil, err
		}
		path, _ := s.BasePath(base.ID)
		size, allocated, err := directoryFileUsage(path)
		if err != nil {
			return nil, err
		}
		references := append([]string{}, refs[base.ID]...)
		result = append(result, ImageEntry{Kind: "base", ID: base.ID, Path: path, State: base.State, Version: base.Version, Build: base.Build, SizeBytes: size, AllocatedBytes: allocated, VirtualCapacityBytes: base.DiskBytes, References: references, Default: base.ID == defaultID})
	}
	installerRoot, err := s.Path("images", "ipsw")
	if err != nil {
		return nil, err
	}
	entries, err = os.ReadDir(installerRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		partial := strings.HasSuffix(name, ".ipsw.partial")
		if !partial && !strings.HasSuffix(name, ".ipsw") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil, fmt.Errorf("installer is not a regular file: %s", name)
		}
		path, err := s.Path("images", "ipsw", name)
		if err != nil {
			return nil, err
		}
		size, allocated, err := regularFileUsage(path)
		if err != nil {
			return nil, err
		}
		build := strings.TrimSuffix(strings.TrimSuffix(name, ".partial"), ".ipsw")
		if !safeID(build) {
			return nil, fmt.Errorf("invalid installer cache filename: %s", name)
		}
		state, version, detail := "downloading", "", ""
		if !partial {
			var installer Installer
			if err := readJSON(path+".json", &installer); err != nil {
				state, detail = "damaged", fmt.Sprintf("installer metadata unavailable: %v", err)
			} else if installer.SchemaVersion != imageSchemaVersion || installer.Build != build || installer.Version == "" || installer.SizeBytes != size || !validSHA256(installer.SHA256) {
				state, detail = "damaged", "installer metadata does not match its cache file"
			} else if installer.VerificationError != "" {
				state, version, detail = "damaged", installer.Version, installer.VerificationError
			} else {
				state, version = "ready", installer.Version
			}
		}
		result = append(result, ImageEntry{Kind: "installer", ID: build, Path: path, State: state, Version: version, Build: build, Detail: detail, SizeBytes: size, AllocatedBytes: allocated, References: []string{}})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind == result[j].Kind {
			return result[i].Path < result[j].Path
		}
		return result[i].Kind < result[j].Kind
	})
	return result, nil
}

func directoryFileUsage(path string) (size, allocated int64, err error) {
	err = filepath.WalkDir(path, func(child string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink inside mac base image")
		}
		if entry.IsDir() {
			return nil
		}
		fileBytes, blockBytes, err := regularFileUsage(child)
		if err != nil {
			return err
		}
		const maxInt64 = int64(1<<63 - 1)
		if fileBytes > maxInt64-size || blockBytes > maxInt64-allocated {
			return errors.New("mac image size exceeds supported counter range")
		}
		size += fileBytes
		allocated += blockBytes
		return nil
	})
	return size, allocated, err
}

// regularFileUsage uses lstat so a replaced/symlink disk is never followed.
// Darwin and Linux both express st_blocks in 512-byte units, regardless of
// the filesystem's allocation block size. This is a point-in-time measure.
func regularFileUsage(path string) (size, allocated int64, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, 0, fmt.Errorf("mac image must be a regular non-symlink file: %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	const maxInt64 = int64(1<<63 - 1)
	if !ok || info.Size() < 0 || stat.Blocks < 0 || stat.Blocks > maxInt64/512 {
		return 0, 0, fmt.Errorf("filesystem allocation is unavailable or invalid: %s", path)
	}
	return info.Size(), stat.Blocks * 512, nil
}

// PruneReport lists unused images. Removed is empty for a dry run.
type PruneReport struct {
	Candidates     []ImageEntry `json:"candidates"`
	Removed        []string     `json:"removed"`
	ReclaimedBytes int64        `json:"reclaimed_bytes"`
	Applied        bool         `json:"applied"`
}

// PruneImages lists, and with apply removes, bases no machine references and
// that are not the default. installers adds idle IPSW caches and partial
// downloads. A dry run changes nothing.
func (m *Manager) PruneImages(ctx context.Context, installers, apply bool) (report PruneReport, retErr error) {
	held, err := m.acquireState(ctx, "mac image prune")
	if err != nil {
		return report, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac image prune") }()
	return m.Store.PruneImages(held, installers, apply)
}

// PruneImages requires the global state lock across listing and removal. It
// preserves referenced and default bases and skips any busy image lock.
func (s *Store) PruneImages(held *lock.File, installers, apply bool) (PruneReport, error) {
	report := PruneReport{Candidates: []ImageEntry{}, Removed: []string{}, Applied: apply}
	if err := s.checkLock(held); err != nil {
		return report, err
	}
	entries, err := s.ImageList()
	if err != nil {
		return report, err
	}
	for _, entry := range entries {
		if entry.Kind == "base" && (entry.Default || len(entry.References) > 0) {
			continue
		}
		if entry.Kind == "installer" && !installers {
			continue
		}
		report.Candidates = append(report.Candidates, entry)
		report.ReclaimedBytes += entry.AllocatedBytes
		if !apply {
			continue
		}
		lockPath, err := s.imageLockPath(entry.Kind, entry.ID)
		if err != nil {
			return report, err
		}
		imageLock, err := lock.TryAcquire(lockPath, false)
		if errors.Is(err, lock.ErrBusy) {
			continue
		}
		if err != nil {
			return report, err
		}
		err = func() (retErr error) {
			defer func() { retErr = lock.JoinRelease(retErr, imageLock, "mac image prune") }()
			// Recheck containment immediately before removal. RemoveAll never
			// follows child symlinks; ImageList also rejects unexpected children.
			rel, err := filepath.Rel(s.Root, entry.Path)
			if err != nil {
				return err
			}
			path, err := s.Path(rel)
			if err != nil {
				return err
			}
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			if entry.Kind == "installer" {
				if err := os.Remove(path + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			return fsutil.SyncDir(filepath.Dir(path))
		}()
		if err != nil {
			return report, err
		}
		report.Removed = append(report.Removed, entry.Path)
	}
	return report, nil
}
