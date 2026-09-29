package macvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/lock"
	"golang.org/x/sys/unix"
)

// SetupOptions prepare a base: the unbooted macOS restore every machine is
// cloned from. The installer comes only from Apple's servers or a local file.
type SetupOptions struct {
	IPSW      string
	DiskBytes int64
	// Update prepares the newest macOS 27 Apple offers instead of the pinned
	// release.
	Update bool
	// Confirm approves a download from Apple before it starts. Nil approves.
	Confirm func(DownloadPlan) error
}

// DownloadPlan describes the Apple download setup is about to begin.
type DownloadPlan struct {
	Version     string `json:"version"`
	Build       string `json:"build"`
	URL         string `json:"url"`
	SizeBytes   int64  `json:"size_bytes"`
	ResumeBytes int64  `json:"resume_bytes"`
	FreeBytes   int64  `json:"free_bytes"`
}

// Setup prepares the default base without creating a machine.
func (m *Manager) Setup(ctx context.Context, options SetupOptions) (base *BaseImage, retErr error) {
	if err := m.requireRunner(ctx); err != nil {
		return nil, err
	}
	held, err := m.acquireState(ctx, "mac setup")
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac setup") }()
	config, err := m.ensureConfig(held)
	if err != nil {
		return nil, err
	}
	return m.setupLocked(ctx, held, config, options)
}

type setupPlan struct {
	diskBytes  int64
	spec       IPSWSpec
	metadata   RestoreMetadata
	sourcePath string
	readyBase  *BaseImage
}

func availableBytes(path string) (int64, error) {
	for {
		var stat unix.Statfs_t
		err := unix.Statfs(path, &stat)
		if err == nil {
			return int64(stat.Bavail) * int64(stat.Bsize), nil
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, unix.ENOENT) || parent == path {
			return 0, err
		}
		path = parent
	}
}

// preflightSetup validates read-only: it selects a reusable base, or names
// the installer to restore from. A reusable base needs no download or space.
func (m *Manager) preflightSetup(ctx context.Context, config *Config, options SetupOptions) (setupPlan, error) {
	plan := setupPlan{diskBytes: options.DiskBytes, spec: DefaultIPSW()}
	if options.DiskBytes != 0 && options.DiskBytes < minimumDiskBytes {
		return plan, failure.New(failure.Usage, fmt.Errorf("the macOS disk must be at least %d GiB", minimumDiskBytes>>30))
	}
	var defaultBase *BaseImage
	if config.DefaultBaseID != "" {
		var err error
		if defaultBase, err = m.Store.LoadBase(config.DefaultBaseID); err != nil {
			return plan, err
		}
		if plan.diskBytes == 0 {
			plan.diskBytes = defaultBase.DiskBytes
		}
	}
	if plan.diskBytes == 0 {
		plan.diskBytes = DefaultDiskBytes
	}
	var discovered *restoreDiscovery
	switch {
	case options.IPSW != "":
		path, size, err := localIPSW(options.IPSW)
		if err != nil {
			return plan, err
		}
		if err = m.Runner.Call(ctx, nil, &plan.metadata, "metadata", "--ipsw", path); err != nil {
			return plan, err
		}
		if !strings.HasPrefix(plan.metadata.Version, "27.") || !safeID(plan.metadata.Build) || !validSHA256(plan.metadata.HardwareModelHash) {
			return plan, failure.New(failure.Usage, errors.New("the local IPSW is not a macOS 27 restore image this Mac supports"))
		}
		plan.sourcePath = path
		if plan.metadata.Build != plan.spec.Build {
			plan.spec = IPSWSpec{Version: plan.metadata.Version, Build: plan.metadata.Build, SizeBytes: size}
		} else if size != plan.spec.SizeBytes {
			return plan, failure.New(failure.Integrity, fmt.Errorf("the local IPSW size does not match Apple's %s restore image", plan.spec.Build))
		}
	case options.Update:
		image, err := m.discoverRestoreImage(ctx)
		if err != nil {
			return plan, err
		}
		discovered, plan.metadata = &image, image.RestoreMetadata
	default:
		// Another capacity keeps the default base's macOS: a base prepared by
		// image update must not fall back to the pinned build.
		build := plan.spec.Build
		if defaultBase != nil {
			build = defaultBase.Build
		}
		if defaultBase != nil && defaultBase.State == "ready" && defaultBase.DiskBytes == plan.diskBytes {
			plan.readyBase = defaultBase
		} else if base := m.readyBaseFor(build, plan.diskBytes); base != nil {
			plan.readyBase = base
		} else if build != plan.spec.Build {
			image, err := m.discoverRestoreImage(ctx)
			if err != nil {
				return plan, err
			}
			if image.Build != build {
				return plan, failure.New(failure.Conflict, fmt.Errorf("the default base is macOS %s (%s), but Apple now offers %s (%s), so a %d GiB base of %s cannot be prepared", defaultBase.Version, build, image.Version, image.Build, plan.diskBytes>>30, build)).
					Then("barn mac image update, then retry")
			}
			discovered, plan.metadata = &image, image.RestoreMetadata
		}
	}
	if plan.readyBase == nil && plan.metadata.HardwareModelHash != "" {
		id, err := BaseImageID(plan.metadata.Build, plan.diskBytes, plan.metadata.HardwareModelHash, 1)
		if err != nil {
			return plan, err
		}
		base, err := m.Store.LoadBase(id)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return plan, err
		}
		if base != nil && !compatibleSetupBase(base, plan.metadata, plan.diskBytes) {
			return plan, errors.New("cached base metadata does not match its requested build, hardware, capacity and recipe")
		}
		if base != nil && base.State == "ready" {
			plan.readyBase = base
		}
	}
	// Apple's online catalog can prefer another hardware model for the same
	// build. A verified, supported base of that build needs no new restore.
	if discovered != nil && plan.readyBase == nil {
		if base := m.readyBaseFor(plan.metadata.Build, plan.diskBytes); base != nil {
			path, err := m.Store.Path("images", "base", base.ID, "hardware-model.bin")
			if err != nil {
				return plan, err
			}
			var hardware struct {
				Supported bool   `json:"supported"`
				Hash      string `json:"hardware_model_sha256"`
			}
			if err := m.Runner.Call(ctx, nil, &hardware, "hardware", "--path", path); err != nil {
				return plan, fmt.Errorf("check cached hardware compatibility: %w", err)
			}
			if hardware.Hash == base.HardwareModelHash && hardware.Supported {
				plan.readyBase = base
			}
		}
	}
	if plan.readyBase != nil {
		if err := m.validateBase(plan.readyBase); err != nil {
			return plan, fmt.Errorf("the prepared base is not reusable: %w", err)
		}
		return plan, nil
	}
	if discovered != nil {
		var err error
		if plan.spec, err = discovered.installerSpec(ctx); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

// readyBaseFor returns a ready base of a build and capacity, if one exists.
func (m *Manager) readyBaseFor(build string, diskBytes int64) *BaseImage {
	root, err := m.Store.Path("images", "base")
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		base, err := m.Store.LoadBase(entry.Name())
		if err == nil && base.State == "ready" && base.Build == build && base.DiskBytes == diskBytes && base.RecipeVersion == 1 && m.validateBase(base) == nil {
			return base
		}
	}
	return nil
}

func localIPSW(raw string) (string, int64, error) {
	path, err := filepath.Abs(raw)
	if err != nil {
		return "", 0, err
	}
	if path, err = filepath.EvalSymlinks(path); err != nil {
		return "", 0, failure.New(failure.Usage, fmt.Errorf("read local IPSW: %w", err))
	}
	// Refuse devices and named pipes before opening can block on them.
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, failure.New(failure.Usage, fmt.Errorf("read local IPSW: %w", err))
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", 0, failure.New(failure.Usage, errors.New("the local IPSW must be a nonempty regular file"))
	}
	return path, info.Size(), nil
}

func compatibleSetupBase(base *BaseImage, metadata RestoreMetadata, diskBytes int64) bool {
	return base.Build == metadata.Build && base.Version == metadata.Version && base.HardwareModelHash == metadata.HardwareModelHash && base.DiskBytes == diskBytes && base.RecipeVersion == 1
}

// setupLocked returns a ready base, restoring one when needed. A download
// from Apple begins only after Confirm approves its plan.
func (m *Manager) setupLocked(ctx context.Context, held *lock.File, config *Config, options SetupOptions) (*BaseImage, error) {
	plan, err := m.preflightSetup(ctx, config, options)
	if err != nil {
		return nil, err
	}
	if plan.readyBase != nil {
		if config.DefaultBaseID != plan.readyBase.ID && (config.DefaultBaseID == "" || options.Update || options.IPSW != "") {
			config.DefaultBaseID = plan.readyBase.ID
			if err := m.Store.SaveConfig(held, config); err != nil {
				return nil, err
			}
		}
		return plan.readyBase, nil
	}
	if err := m.checkVMLimit(ctx, ""); err != nil {
		return nil, fmt.Errorf("installing macOS needs one of the host's macOS VM slots: %w", err)
	}
	free, err := availableBytes(m.Store.Root)
	if err != nil {
		return nil, err
	}
	installer, err := m.obtainInstaller(ctx, plan, options, free)
	if err != nil {
		return nil, err
	}
	metadata := plan.metadata
	if err = m.Runner.Call(ctx, nil, &metadata, "metadata", "--ipsw", installer.Path); err != nil {
		return nil, err
	}
	if metadata.Build != installer.Build || !strings.HasPrefix(metadata.Version, "27.") {
		return nil, failure.New(failure.Integrity, errors.New("the restore image version or build does not match its installer"))
	}
	base, err := m.restoreBase(ctx, held, installer, metadata, plan.diskBytes)
	if err != nil {
		return nil, err
	}
	// Only the first base, image update and an explicit installer choose the
	// default; a base for another capacity leaves it alone.
	if config.DefaultBaseID == "" || options.Update || options.IPSW != "" {
		config.DefaultBaseID = base.ID
		if err = m.Store.SaveConfig(held, config); err != nil {
			return nil, err
		}
	}
	return base, nil
}

// obtainInstaller returns a verified IPSW: the cache, a clone or in-place use
// of the local file, or a resumable download from Apple after consent.
func (m *Manager) obtainInstaller(ctx context.Context, plan setupPlan, options SetupOptions, free int64) (*Installer, error) {
	// Installing macOS grows a base to about 30 GiB before any machine exists.
	const restoreBytes int64 = 40 << 30
	progress := m.downloadProgress(plan.spec)
	if plan.sourcePath != "" {
		if free < restoreBytes {
			return nil, lowSpace(free, restoreBytes)
		}
		m.report("image-verify", "Checking the local macOS %s (%s) restore image", plan.spec.Version, plan.spec.Build)
		return m.Store.ImportIPSW(ctx, plan.sourcePath, plan.spec, DownloadOptions{Progress: progress})
	}
	cached, err := m.Store.CachedInstaller(ctx, plan.spec)
	if err == nil {
		return cached, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	resume, err := m.Store.PartialBytes(plan.spec)
	if err != nil {
		return nil, err
	}
	download := DownloadPlan{Version: plan.spec.Version, Build: plan.spec.Build, URL: plan.spec.URL, SizeBytes: plan.spec.SizeBytes, ResumeBytes: resume, FreeBytes: free}
	if needed := plan.spec.SizeBytes - resume + restoreBytes; free < needed {
		return nil, lowSpace(free, needed)
	}
	if options.Confirm != nil {
		if err := options.Confirm(download); err != nil {
			return nil, err
		}
	}
	m.report("image-download", "Downloading macOS %s (%s) from Apple", plan.spec.Version, plan.spec.Build)
	return m.Store.DownloadIPSW(ctx, plan.spec, DownloadOptions{Progress: progress})
}

func lowSpace(free, needed int64) error {
	return failure.New(failure.Resource, fmt.Errorf("preparing macOS needs %.0f GiB free on this volume, but only %.0f GiB is available", float64(needed)/(1<<30), float64(free)/(1<<30))).
		Because("disk_full").Then("free disk space, or remove unused images with barn mac image prune --installers --yes")
}

func (m *Manager) downloadProgress(spec IPSWSpec) func(done, total int64) {
	started := time.Now()
	start := int64(-1)
	return func(done, total int64) {
		if start < 0 {
			start = done
		}
		m.Report.Report(activity.Event{Phase: "image-download", Message: fmt.Sprintf("Downloading macOS %s (%s) from Apple", spec.Version, spec.Build),
			Source: spec.URL, CurrentBytes: done, StartBytes: start, TotalBytes: total, StartedAt: started, Done: done == total})
	}
}

func (m *Manager) restoreBase(ctx context.Context, held *lock.File, installer *Installer, metadata RestoreMetadata, diskBytes int64) (*BaseImage, error) {
	id, err := BaseImageID(metadata.Build, diskBytes, metadata.HardwareModelHash, 1)
	if err != nil {
		return nil, err
	}
	base, err := m.Store.LoadBase(id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if base != nil && !compatibleSetupBase(base, metadata, diskBytes) {
		return nil, errors.New("cached base metadata does not match its requested build, hardware, capacity and recipe")
	}
	if base == nil {
		base = &BaseImage{SchemaVersion: imageSchemaVersion, ID: id, Version: metadata.Version, Build: metadata.Build, HardwareModelHash: metadata.HardwareModelHash,
			InstallerSHA256: installer.SHA256, State: "restoring", DiskBytes: diskBytes, RecipeVersion: 1, CreatedAt: time.Now().UTC()}
	}
	if base.State == "ready" {
		return base, nil
	}
	baseLock, err := m.Store.LockBase(ctx, id)
	if err != nil {
		return nil, err
	}
	err = func() (retErr error) {
		defer func() { retErr = lock.JoinRelease(retErr, baseLock, "base restore") }()
		base.State, base.LastError = "restoring", ""
		if err := m.Store.SaveBase(held, base); err != nil {
			return err
		}
		path, err := m.Store.BasePath(id)
		if err != nil {
			return err
		}
		// An interrupted installer cannot resume. Only its known, unpublished
		// artifacts are discarded; a ready base never reaches this point.
		for _, name := range []string{"disk.asif", "hardware-model.bin", "auxiliary-storage.bin", "machine-id.bin", "base.json"} {
			if err := os.Remove(filepath.Join(path, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		m.report("image-restore", "Installing macOS %s (%s)", metadata.Version, metadata.Build)
		var result RestoreMetadata
		if err := m.Runner.Call(ctx, nil, &result, "restore", "--ipsw", installer.Path, "--base", path, "--cpu", strconv.Itoa(DefaultCPU),
			"--memory", strconv.FormatInt(DefaultMemoryBytes, 10), "--disk-size", strconv.FormatInt(diskBytes, 10)); err != nil {
			base.State, base.LastError = "failed", err.Error()
			return errors.Join(err, m.Store.SaveBase(held, base))
		}
		if result.Build != base.Build {
			return failure.New(failure.Integrity, errors.New("the restored base reports a different build"))
		}
		base.State = "ready"
		return m.Store.SaveBase(held, base)
	}()
	if err != nil {
		return nil, err
	}
	return base, nil
}
