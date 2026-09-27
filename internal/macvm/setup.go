package macvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/farrow/internal/lock"
	"golang.org/x/sys/unix"
)

func (m *Manager) Setup(ctx context.Context, options SetupOptions) (base *BaseImage, retErr error) {
	if err := m.requireRunner(); err != nil {
		return nil, err
	}
	held, err := m.acquireState(ctx, "mac setup/image update")
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac setup") }()
	return m.setupLocked(ctx, held, options, options.upgradesNetwork())
}

// Only explicit network setup may replace an installed helper. Image updates
// preserve a compatible helper so active guests do not block image cache reuse.
func (options SetupOptions) upgradesNetwork() bool { return !options.Update }

func (m *Manager) configuration(ctx context.Context, held *lock.File, subnet string) (*Config, error) {
	config, err := m.Store.LoadConfig()
	if err == nil {
		if subnet != "" && subnet != config.Network.Subnet {
			return m.reselectPendingSubnet(ctx, held, config, subnet, m.networkInstallationExists, selectHostSubnet)
		}
		return config, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	selected, err := selectHostSubnet(ctx, subnet)
	if err != nil {
		return nil, err
	}
	config, err = NewConfig(selected)
	if err != nil {
		return nil, err
	}
	if err = m.Store.SaveConfig(held, config); err != nil {
		return nil, err
	}
	return config, nil
}

func selectHostSubnet(ctx context.Context, subnet string) (string, error) {
	routes, err := exec.CommandContext(ctx, "/usr/sbin/netstat", "-rn", "-f", "inet").Output()
	if err != nil {
		return "", err
	}
	occupied, err := ParseDarwinRoutes(string(routes))
	if err != nil {
		return "", err
	}
	return SelectSubnet(subnet, occupied)
}

// A canceled first authorization may leave local preferences behind. Only an
// installation with no guest/base artifacts and positive evidence that the
// privileged installation does not exist can change these unused addresses.
func (m *Manager) reselectPendingSubnet(ctx context.Context, held *lock.File, config *Config, subnet string, installed func(context.Context, *Config) (bool, error), selectSubnet func(context.Context, string) (string, error)) (*Config, error) {
	if err := m.Store.checkLock(held); err != nil {
		return nil, err
	}
	if config.DefaultBaseID != "" {
		return nil, fmt.Errorf("mac subnet is already fixed at %s by its prepared base", config.Network.Subnet)
	}
	slots, err := m.Store.ListSlots()
	if err != nil {
		return nil, err
	}
	for _, slot := range slots {
		if slot.InstanceID != "" {
			return nil, fmt.Errorf("mac subnet is already fixed at %s by %s", config.Network.Subnet, slot.Name)
		}
	}
	basePath, err := m.Store.Path("images", "base")
	if err != nil {
		return nil, err
	}
	bases, err := os.ReadDir(basePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(bases) != 0 {
		return nil, errors.New("mac subnet cannot change while base image artifacts exist")
	}
	exists, err := installed(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("cannot prove the previous Mac network was never installed: %w", err)
	}
	if exists {
		return nil, fmt.Errorf("mac subnet is already installed at %s; existing network configuration is preserved", config.Network.Subnet)
	}
	selected, err := selectSubnet(ctx, subnet)
	if err != nil {
		return nil, err
	}
	return m.savePendingSubnet(held, config, selected)
}

func (m *Manager) savePendingSubnet(held *lock.File, config *Config, subnet string) (*Config, error) {
	if err := m.Store.checkLock(held); err != nil {
		return nil, err
	}
	prefix, err := ValidateSubnet(subnet)
	if err != nil {
		return nil, err
	}
	next := *config
	next.Slots = append([]SlotPreference(nil), config.Slots...)
	next.Network.Subnet, next.Network.Gateway = prefix.String(), subnetAddress(prefix, 1).String()
	for i := range next.Slots {
		address := byte(10)
		if next.Slots[i].Name == "mac2" {
			address = 11
		}
		next.Slots[i].IP = subnetAddress(prefix, address).String()
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	path, err := m.Store.Path("config.json")
	if err != nil {
		return nil, err
	}
	// SaveConfig intentionally prohibits changes to established reservations.
	// Its sole exception is this guarded, never-installed initial configuration.
	if err := writeJSON(path, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

type setupPlan struct {
	diskBytes  int64
	spec       IPSWSpec
	metadata   RestoreMetadata
	sourcePath string
	readyBase  *BaseImage
}

func availableSetupBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

// preflightSetup performs read-only validation before configuration is saved or
// administrator authorization is requested. A reusable base needs no download
// space, installer cache, or new restore.
func (m *Manager) preflightSetup(ctx context.Context, options SetupOptions, available func(string) (uint64, error)) (setupPlan, error) {
	plan := setupPlan{diskBytes: options.DiskBytes, spec: DefaultIPSW()}
	if options.DiskBytes != 0 && options.DiskBytes < 32<<30 {
		return plan, errors.New("mac disk must be at least 32 GiB")
	}
	if options.Subnet != "" {
		if _, err := ValidateSubnet(options.Subnet); err != nil {
			return plan, err
		}
	}
	config, err := m.Store.LoadConfig()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return plan, err
	}
	var defaultBase *BaseImage
	if config != nil && config.DefaultBaseID != "" {
		defaultBase, err = m.Store.LoadBase(config.DefaultBaseID)
		if err != nil {
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
	if options.IPSW != "" {
		path, err := filepath.Abs(options.IPSW)
		if err != nil {
			return plan, err
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return plan, fmt.Errorf("read local IPSW: %w", err)
		}
		// Reject devices and named pipes before Open can block on them.
		info, err := os.Stat(path)
		if err != nil {
			return plan, fmt.Errorf("read local IPSW: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return plan, errors.New("local IPSW must be a nonempty regular file")
		}
		file, err := os.Open(path)
		if err != nil {
			return plan, fmt.Errorf("read local IPSW: %w", err)
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil {
			return plan, errors.Join(statErr, closeErr)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return plan, errors.New("local IPSW must be a nonempty regular file")
		}
		if err = m.Runner.Call(ctx, nil, &plan.metadata, "metadata", "--ipsw", path); err != nil {
			return plan, err
		}
		if !strings.HasPrefix(plan.metadata.Version, "27.") || !safeID(plan.metadata.Build) || !validSHA256(plan.metadata.HardwareModelHash) {
			return plan, errors.New("local IPSW must provide compatible macOS 27 metadata and hardware model")
		}
		plan.sourcePath = path
		if plan.metadata.Build != plan.spec.Build {
			plan.spec = IPSWSpec{Version: plan.metadata.Version, Build: plan.metadata.Build, SizeBytes: info.Size()}
		} else if info.Size() != plan.spec.SizeBytes {
			return plan, fmt.Errorf("local IPSW size does not match the pinned %s image", plan.spec.Build)
		}
	} else if options.Update {
		image, err := m.discoverRestoreImage(ctx)
		if err != nil {
			return plan, err
		}
		discovered, plan.metadata = &image, image.RestoreMetadata
	} else if defaultBase != nil && defaultBase.DiskBytes == plan.diskBytes {
		plan.readyBase = defaultBase
	}
	if plan.metadata.HardwareModelHash != "" {
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
	// Apple's online catalog can prefer a different hardware model from the
	// exact same local IPSW. Keep a verified, supported base for that build;
	// changing the catalog's preferred model does not require a new restore.
	if discovered != nil && plan.readyBase == nil && defaultBase != nil && defaultBase.State == "ready" && defaultBase.Build == plan.metadata.Build && defaultBase.Version == plan.metadata.Version && defaultBase.DiskBytes == plan.diskBytes && defaultBase.RecipeVersion == 1 {
		if err := m.validateResetBase(defaultBase); err != nil {
			return plan, fmt.Errorf("prepared base is not reusable: %w", err)
		}
		path, err := m.Store.Path("images", "base", defaultBase.ID, "hardware-model.bin")
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
		if hardware.Hash != defaultBase.HardwareModelHash {
			return plan, errors.New("cached hardware model does not match the saved base identity")
		}
		if hardware.Supported {
			plan.readyBase = defaultBase
		}
	}
	if plan.readyBase != nil {
		if err := m.validateResetBase(plan.readyBase); err != nil {
			return plan, fmt.Errorf("prepared base is not reusable: %w", err)
		}
		return plan, nil
	}
	free, err := available(m.Store.Root)
	if err != nil {
		return plan, err
	}
	if free < 50<<30 {
		return plan, errors.New("base preparation requires at least 50 GiB free; about 100 GiB is recommended for restore and both slots")
	}
	if discovered != nil {
		plan.spec, err = discovered.installerSpec(ctx)
		if err != nil {
			return plan, err
		}
	}
	return plan, nil
}

func compatibleSetupBase(base *BaseImage, metadata RestoreMetadata, diskBytes int64) bool {
	return base.Build == metadata.Build && base.Version == metadata.Version && base.HardwareModelHash == metadata.HardwareModelHash && base.DiskBytes == diskBytes && base.RecipeVersion == 1
}

func (m *Manager) setupLocked(ctx context.Context, held *lock.File, options SetupOptions, allowNetworkUpgrade bool) (*BaseImage, error) {
	ensure := m.ensureNetworkForPreparation
	if allowNetworkUpgrade {
		ensure = m.ensureNetworkForSetup
	}
	return m.setupWithChecks(ctx, held, options, ensure, availableSetupBytes)
}

func (m *Manager) setupWithChecks(ctx context.Context, held *lock.File, options SetupOptions, ensure func(context.Context, *Config) error, available func(string) (uint64, error)) (*BaseImage, error) {
	plan, err := m.preflightSetup(ctx, options, available)
	if err != nil {
		return nil, err
	}
	config, err := m.configuration(ctx, held, options.Subnet)
	if err != nil {
		return nil, err
	}
	if err = ensure(ctx, config); err != nil {
		return nil, err
	}
	if plan.readyBase != nil {
		if config.DefaultBaseID != plan.readyBase.ID {
			config.DefaultBaseID = plan.readyBase.ID
			if err := m.Store.SaveConfig(held, config); err != nil {
				return nil, err
			}
		}
		m.progress("macOS %s (%s): compatible base already ready; no download or restore needed", plan.readyBase.Version, plan.readyBase.Build)
		return plan.readyBase, nil
	}
	spec, metadata, diskBytes := plan.spec, plan.metadata, plan.diskBytes
	var installer *Installer
	progressTime := time.Time{}
	var progressBytes int64
	downloadOptions := DownloadOptions{Progress: func(done, total int64) {
		now := time.Now()
		elapsed := now.Sub(progressTime)
		if progressTime.IsZero() || elapsed >= 10*time.Second || done == total {
			if !progressTime.IsZero() && done > progressBytes && done < total {
				rate := float64(done-progressBytes) / elapsed.Seconds()
				eta := time.Duration(float64(total-done)/rate) * time.Second
				m.progress("IPSW: %.2f / %.2f GiB, %.1f MiB/s, about %s remaining", float64(done)/(1<<30), float64(total)/(1<<30), rate/(1<<20), eta.Round(time.Second))
			} else {
				m.progress("IPSW: %.2f / %.2f GiB", float64(done)/(1<<30), float64(total)/(1<<30))
			}
			progressTime, progressBytes = now, done
		}
	}}
	if plan.sourcePath != "" {
		installer, err = m.Store.ImportIPSW(ctx, plan.sourcePath, spec, downloadOptions)
	} else {
		m.progress("Preparing macOS %s (%s) from Apple", spec.Version, spec.Build)
		installer, err = m.Store.DownloadIPSW(ctx, spec, downloadOptions)
	}
	if err != nil {
		return nil, err
	}
	if err = m.Runner.Call(ctx, nil, &metadata, "metadata", "--ipsw", installer.Path); err != nil {
		return nil, err
	}
	if metadata.Build != installer.Build || !strings.HasPrefix(metadata.Version, "27.") {
		return nil, errors.New("restore image version/build does not match pinned installer")
	}
	id, err := BaseImageID(metadata.Build, diskBytes, metadata.HardwareModelHash, 1)
	if err != nil {
		return nil, err
	}
	base, loadErr := m.Store.LoadBase(id)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return nil, loadErr
	}
	if base != nil && !compatibleSetupBase(base, metadata, diskBytes) {
		return nil, errors.New("cached base metadata does not match its requested build, hardware, capacity and recipe")
	}
	if base == nil {
		base = &BaseImage{SchemaVersion: 1, ID: id, Version: metadata.Version, Build: metadata.Build, HardwareModelHash: metadata.HardwareModelHash, InstallerSHA256: installer.SHA256, State: "restoring", DiskBytes: diskBytes, RecipeVersion: 1, CreatedAt: time.Now().UTC()}
	}
	if base.State != "ready" {
		baseLock, err := m.Store.LockBase(ctx, id)
		if err != nil {
			return nil, err
		}
		err = func() (retErr error) {
			defer func() { retErr = lock.JoinRelease(retErr, baseLock, "base restore") }()
			base.State = "restoring"
			base.LastError = ""
			if err = m.Store.SaveBase(held, base); err != nil {
				return err
			}
			path, err := m.Store.BasePath(id)
			if err != nil {
				return err
			}
			// An interrupted installer cannot resume. Only its known, unpublished
			// artifacts are discarded; a ready base never enters this branch.
			for _, name := range []string{"disk.asif", "hardware-model.bin", "auxiliary-storage.bin", "machine-id.bin", "base.json"} {
				p, err := m.Store.Path("images", "base", id, name)
				if err != nil {
					return err
				}
				if err = os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			m.progress("Restoring base %s; Apple installer uses a host macOS VM slot", id)
			var result RestoreMetadata
			if err = m.Runner.Call(ctx, nil, &result, "restore", "--ipsw", installer.Path, "--base", path, "--cpu", strconv.Itoa(DefaultCPU), "--memory", strconv.FormatInt(DefaultMemoryBytes, 10), "--disk-size", strconv.FormatInt(diskBytes, 10)); err != nil {
				base.State = "failed"
				base.LastError = err.Error()
				return errors.Join(err, m.Store.SaveBase(held, base))
			}
			if result.Build != base.Build {
				return errors.New("restored base build mismatch")
			}
			base.State = "ready"
			return m.Store.SaveBase(held, base)
		}()
		if err != nil {
			return nil, err
		}
	}
	config.DefaultBaseID = base.ID
	if err = m.Store.SaveConfig(held, config); err != nil {
		return nil, err
	}
	return base, nil
}

func (m *Manager) setupOptionsForSlot(name string, options SetupOptions) (SetupOptions, error) {
	if options.DiskBytes != 0 {
		return options, nil
	}
	config, err := m.Store.LoadConfig()
	if errors.Is(err, os.ErrNotExist) {
		options.DiskBytes = DefaultDiskBytes
		return options, nil
	}
	if err != nil {
		return options, err
	}
	pref, err := config.Preference(name)
	if err != nil {
		return options, err
	}
	if pref.ResourcesConfigured || pref.DiskBytes != DefaultDiskBytes || pref.CPU != DefaultCPU || pref.MemoryBytes != DefaultMemoryBytes || config.DefaultBaseID == "" {
		options.DiskBytes = pref.DiskBytes
		return options, nil
	}
	base, err := m.Store.LoadBase(config.DefaultBaseID)
	if err != nil {
		return options, err
	}
	options.DiskBytes = base.DiskBytes
	return options, nil
}
