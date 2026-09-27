package macvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pgsty/farrow/internal/lock"
)

type Manager struct {
	Store    *Store
	Runner   Runner
	Progress io.Writer
}
type SetupOptions struct {
	IPSW, Subnet string
	Update       bool
	DiskBytes    int64
}
type InitOptions struct {
	SetupOptions
	User        string
	CPU         int
	MemoryBytes int64
	NoWait      bool
}
type SlotView struct {
	Slot
	IP           string         `json:"ip,omitempty"`
	MAC          string         `json:"mac,omitempty"`
	Runtime      *RuntimeStatus `json:"runtime,omitempty"`
	SSH          string         `json:"ssh,omitempty"`
	SSHError     string         `json:"ssh_error,omitempty"`
	RuntimeError string         `json:"runtime_error,omitempty"`
	// These measure only disk.asif. DiskBytes remains guest-visible capacity;
	// nil means no disk file is present, not a measured zero-byte allocation.
	DiskFileBytes      *int64 `json:"disk_file_bytes,omitempty"`
	DiskAllocatedBytes *int64 `json:"disk_allocated_bytes,omitempty"`
}
type Status struct {
	SchemaVersion int            `json:"schema_version"`
	Root          string         `json:"root"`
	Prepared      bool           `json:"prepared"`
	Network       *NetworkConfig `json:"network,omitempty"`
	Slots         []SlotView     `json:"slots"`
}

func (m *Manager) progress(format string, args ...any) {
	if m.Progress != nil {
		_, _ = fmt.Fprintf(m.Progress, format+"\n", args...)
	}
}
func (m *Manager) requireRunner() error {
	if err := HostSupported(); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return errors.New("run farrow mac as your normal macOS login user; only its network helper runs as root")
	}
	if m.Runner.Binary == "" {
		binary, err := FindRunner()
		if err != nil {
			return err
		}
		m.Runner.Binary = binary
	}
	m.Runner.Progress = m.Progress
	return nil
}
func (m *Manager) socket(name string, create bool) (string, error) {
	dir, err := RuntimeDir(m.Store.Root, create)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".sock"), nil
}
func (m *Manager) status(ctx context.Context, slot *Slot) (RuntimeStatus, error) {
	socket, err := m.socket(slot.Name, false)
	if err != nil {
		return RuntimeStatus{}, err
	}
	probe, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return m.Runner.RPC(probe, socket, slot.InstanceID, "status", false)
}

func (m *Manager) List(ctx context.Context) (Status, error) {
	result := Status{SchemaVersion: 1, Root: m.Store.Root, Slots: []SlotView{}}
	config, err := m.Store.LoadConfig()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if config != nil {
		result.Network = &config.Network
		result.Prepared = config.DefaultBaseID != ""
	}
	slots, err := m.Store.ListSlots()
	if err != nil {
		return result, err
	}
	for _, slot := range slots {
		view := SlotView{Slot: slot}
		if config != nil {
			pref, _ := config.Preference(slot.Name)
			view.IP = pref.IP
			view.MAC = pref.MAC
		}
		if slot.InstanceID != "" {
			diskPath, err := m.Store.Path("slots", slot.Name, "disk.asif")
			if err != nil {
				return result, err
			}
			fileBytes, allocatedBytes, err := regularFileUsage(diskPath)
			if err == nil {
				view.DiskFileBytes = &fileBytes
				view.DiskAllocatedBytes = &allocatedBytes
			} else if !errors.Is(err, os.ErrNotExist) {
				return result, err
			}
		}
		result.Slots = append(result.Slots, view)
	}
	// Each view owns its runtime result. Probe the fixed slots concurrently so
	// an unavailable guest does not extend the other guest's status wait.
	var pending sync.WaitGroup
	for i := range result.Slots {
		view := &result.Slots[i]
		if view.InstanceID == "" {
			continue
		}
		var pref SlotPreference
		if config != nil {
			pref, _ = config.Preference(view.Name)
		}
		pending.Add(1)
		go func() {
			defer pending.Done()
			m.inspectRuntime(ctx, view, pref)
		}()
	}
	pending.Wait()
	return result, nil
}

func (m *Manager) Init(ctx context.Context, name string, options InitOptions) (slot *Slot, retErr error) {
	if err := m.requireRunner(); err != nil {
		return nil, err
	}
	operation, err := m.acquireOperation(ctx, name, "mac init "+name)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, operation, "mac init") }()
	held, err := m.acquireState(ctx, "mac init "+name)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac init") }()
	return m.initLocked(ctx, held, name, options, nil)
}

func (m *Manager) initLocked(ctx context.Context, held *lock.File, name string, options InitOptions, base *BaseImage) (*Slot, error) {
	name, err := NormalizeSlot(name)
	if err != nil {
		return nil, err
	}
	if existing, err := m.Store.LoadSlot(name); err == nil {
		if err := m.checkExistingOptions(existing, options); err != nil {
			return nil, err
		}
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if options.User != "" && !ValidUsername(options.User) {
		return nil, fmt.Errorf("invalid macOS administrator username %q", options.User)
	}
	if options.CPU != 0 && options.CPU < 2 {
		return nil, errors.New("macOS requires at least 2 CPUs")
	}
	if options.MemoryBytes != 0 && options.MemoryBytes < 4<<30 {
		return nil, errors.New("macOS requires at least 4 GiB memory")
	}
	if base == nil {
		options.SetupOptions, err = m.setupOptionsForSlot(name, options.SetupOptions)
		if err != nil {
			return nil, err
		}
		base, err = m.setupLocked(ctx, held, options.SetupOptions, false)
		if err != nil {
			return nil, err
		}
	}
	config, err := m.Store.LoadConfig()
	if err != nil {
		return nil, err
	}
	pref, err := config.Preference(name)
	if err != nil {
		return nil, err
	}
	username := options.User
	if username == "" {
		username = DefaultUsername()
	}
	if !ValidUsername(username) {
		return nil, fmt.Errorf("invalid macOS administrator username %q", username)
	}
	if options.CPU > 0 {
		pref.CPU = options.CPU
	}
	if options.MemoryBytes > 0 {
		pref.MemoryBytes = options.MemoryBytes
	}
	if pref.CPU < 2 || pref.MemoryBytes < 4<<30 {
		return nil, errors.New("macOS requires at least 2 CPUs and 4 GiB memory")
	}
	id, err := NewInstanceID()
	if err != nil {
		return nil, err
	}
	password, err := NewPassword()
	if err != nil {
		return nil, err
	}
	if err = m.Runner.SetPassword(ctx, m.Store.Root, id, password); err != nil {
		return nil, err
	}
	slot := &Slot{SchemaVersion: 1, Name: name, InstanceID: id, BaseID: base.ID, State: "created", User: username, CPU: pref.CPU, MemoryBytes: pref.MemoryBytes, DiskBytes: base.DiskBytes, Version: base.Version, Build: base.Build, PasswordRef: id, CreatedAt: time.Now().UTC()}
	if err = m.Store.SaveSlot(held, slot); err != nil {
		_ = m.Runner.DeletePassword(ctx, m.Store.Root, id)
		return nil, err
	}
	if err = m.cloneSlot(ctx, slot); err != nil {
		slot.State = "failed"
		slot.LastError = err.Error()
		return nil, errors.Join(err, m.Store.SaveSlot(held, slot))
	}
	// Keep resource choices after destroy. Use the actual base capacity, since
	// reset may supply a prepared base independently of setup options.
	for i := range config.Slots {
		if config.Slots[i].Name == name {
			config.Slots[i].CPU = slot.CPU
			config.Slots[i].MemoryBytes = slot.MemoryBytes
			config.Slots[i].DiskBytes = slot.DiskBytes
			config.Slots[i].ResourcesConfigured = true
			break
		}
	}
	if err = m.Store.SaveConfig(held, config); err != nil {
		return slot, fmt.Errorf("slot created but could not save resource preferences: %w", err)
	}
	return slot, nil
}

func (m *Manager) cloneSlot(ctx context.Context, slot *Slot) error {
	dir, err := m.Store.SlotPath(slot.Name)
	if err != nil {
		return err
	}
	files := []string{"disk.asif", "machine-id.bin", "auxiliary-storage.bin"}
	count := 0
	for _, name := range files {
		path, pathErr := m.Store.Path("slots", slot.Name, name)
		if pathErr != nil {
			return pathErr
		}
		if _, err = os.Stat(path); err == nil {
			count++
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if count == len(files) {
		return nil
	}
	if _, err = os.Stat(filepath.Join(dir, "boot-requested")); err == nil || slot.Initialized {
		return errors.New("previously booted slot is missing disk or identity files; preserve it for recovery or explicitly reset")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if count > 0 {
		held, err := lock.TryAcquire(filepath.Join(dir, "runner.lock"), false)
		if err != nil {
			return err
		}
		for _, name := range files {
			if err = os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				_ = held.Release()
				return err
			}
		}
		if err = held.Release(); err != nil {
			return err
		}
	}
	base, err := m.Store.BasePath(slot.BaseID)
	if err != nil {
		return err
	}
	return m.Runner.Call(ctx, nil, nil, "clone", "--base", base, "--slot", dir)
}

func (m *Manager) Up(ctx context.Context, name string, options InitOptions, startOnly bool) (slot *Slot, retErr error) {
	if err := m.requireRunner(); err != nil {
		return nil, err
	}
	if startOnly {
		existing, err := m.Store.LoadSlot(name)
		if err != nil || !existing.Initialized {
			return nil, fmt.Errorf("%s is absent or not initialized; run farrow mac up %s", name, name)
		}
	}
	operation, err := m.acquireOperation(ctx, name, "mac up "+name)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, operation, "mac up") }()
	held, err := m.acquireState(ctx, "mac up "+name)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac up") }()
	slot, err = m.Store.LoadSlot(name)
	wasExisting := err == nil
	if startOnly {
		if err != nil || !slot.Initialized {
			return nil, fmt.Errorf("%s is absent or not initialized; run farrow mac up %s", name, name)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		slot, err = m.initLocked(ctx, held, name, options, nil)
	}
	if err != nil {
		return nil, err
	}
	if !startOnly && wasExisting {
		if err := m.checkExistingOptions(slot, options); err != nil {
			return slot, err
		}
	}
	defer func() {
		if retErr != nil && slot != nil && !errors.Is(retErr, errReadinessStopped) {
			slot.LastError = retErr.Error()
			_ = m.Store.SaveSlot(operation, slot)
		}
	}()
	config, err := m.Store.LoadConfig()
	if err != nil {
		return nil, err
	}
	pref, err := config.Preference(name)
	if err != nil {
		return nil, err
	}
	if err = m.ensureNetwork(ctx, config); err != nil {
		return slot, err
	}
	password := ""
	if !slot.Initialized {
		password, err = m.Runner.Password(ctx, m.Store.Root, slot.InstanceID)
		if err != nil {
			return slot, CredentialErrorForSlot(name, err)
		}
		if err = m.cloneSlot(ctx, slot); err != nil {
			return slot, err
		}
	}
	if err := held.Release(); err != nil {
		return slot, err
	}
	if slot.Initialized && !options.NoWait {
		if err := m.checkSSHMaterial(slot); err != nil {
			return slot, err
		}
	}
	launch, err := m.shouldLaunch(ctx, slot)
	if err != nil {
		return slot, err
	}
	if launch {
		basePath, err := m.Store.BasePath(slot.BaseID)
		if err != nil {
			return slot, err
		}
		slotPath, err := m.Store.SlotPath(name)
		if err != nil {
			return slot, err
		}
		socket, err := m.socket(name, true)
		if err != nil {
			return slot, err
		}
		networkSocket, err := m.networkSocket(config)
		if err != nil {
			return slot, err
		}
		args := []string{"run", "--base", basePath, "--slot", slotPath, "--socket", socket, "--instance", slot.InstanceID, "--mac", pref.MAC, "--network-socket", networkSocket, "--network-slot", strings.TrimPrefix(name, "mac"), "--cpu", strconv.Itoa(slot.CPU), "--memory", strconv.FormatInt(slot.MemoryBytes, 10)}
		var input any
		if !slot.Initialized {
			input = map[string]any{"provision": true, "username": slot.User, "password": password}
		}
		slot.State = "starting"
		if err = m.Store.SaveSlot(operation, slot); err != nil {
			return slot, err
		}
		if err = os.WriteFile(filepath.Join(slotPath, "boot-requested"), []byte(slot.InstanceID+"\n"), 0600); err != nil {
			return slot, err
		}
		if _, err = m.Runner.Launch(ctx, args, input, filepath.Join(slotPath, "runner.log"), socket, slot.InstanceID); err != nil {
			return slot, err
		}
	}
	if !slot.Initialized {
		slot.State = "provisioning"
	} else {
		slot.State = "running"
	}
	if err = m.Store.SaveSlot(operation, slot); err != nil {
		return slot, err
	}
	if options.NoWait {
		return slot, nil
	}
	observation, err := m.waitForSSH(ctx, operation, slot, pref, password)
	if err != nil {
		return slot, err
	}
	slot.Initialized = true
	slot.State = "ready"
	slot.LastError = ""
	slot.ObservedVersion = observation.Version
	slot.ObservedBuild = observation.Build
	if err = m.Store.SaveSlot(operation, slot); err != nil {
		return slot, err
	}
	return slot, nil
}

func (m *Manager) stopRuntime(ctx context.Context, slot *Slot, force bool) error {
	return m.stopRuntimeWithShutdown(ctx, slot, force, m.shutdownGuestSSH)
}

func (m *Manager) stopRuntimeWithShutdown(ctx context.Context, slot *Slot, force bool, shutdown func(context.Context, *Slot) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := m.status(ctx, slot); err != nil {
		// The runner owns an exclusive slot lock for its lifetime. Prove it is
		// free before treating an unavailable socket as a stopped VM.
		probe, err := m.stoppedRunnerLock(slot)
		if err != nil {
			return fmt.Errorf("runner is not responding; cannot establish a safe stopped state: %w", err)
		}
		if err = probe.Release(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
	socket, err := m.socket(slot.Name, false)
	if err != nil {
		return err
	}
	var shutdownErr error
	if slot.Initialized && !force {
		shutdownErr = shutdown(ctx, slot)
		if err := ctx.Err(); err != nil {
			return err
		}
		if shutdownErr != nil {
			m.progress("Guest SSH shutdown failed; requesting shutdown through the VM: %v", shutdownErr)
		}
	}
	if force || !slot.Initialized || shutdownErr != nil {
		if _, err = m.Runner.RPC(ctx, socket, slot.InstanceID, "stop", force); err != nil {
			return errors.Join(shutdownErr, err)
		}
	}
	timeout := time.NewTimer(2 * time.Minute)
	defer timeout.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		status, err := m.status(ctx, slot)
		if err != nil || status.State == "stopped" {
			proof, lockErr := m.stoppedRunnerLock(slot)
			if lockErr == nil {
				if err = proof.Release(); err != nil {
					return err
				}
				break
			}
			if !errors.Is(lockErr, lock.ErrBusy) {
				return lockErr
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return errors.Join(shutdownErr, fmt.Errorf("%s did not stop in 2 minutes; retry farrow mac stop %s --force to explicitly power off", slot.Name, slot.Name))
		case <-ticker.C:
		}
	}
	return nil
}

func (m *Manager) stopLocked(ctx context.Context, held *lock.File, slot *Slot, force bool) error {
	if err := m.stopRuntime(ctx, slot, force); err != nil {
		return err
	}
	slot.State = "stopped"
	return m.Store.SaveSlot(held, slot)
}

func (m *Manager) Stop(ctx context.Context, name string, force bool) (slot *Slot, retErr error) {
	if err := m.requireRunner(); err != nil {
		return nil, err
	}
	held, err := m.acquireStopOperation(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac stop") }()
	slot, err = m.Store.LoadSlot(name)
	if err != nil {
		return nil, err
	}
	return slot, m.stopLocked(ctx, held, slot, force)
}

func (m *Manager) Open(ctx context.Context, name string) error {
	if err := m.requireRunner(); err != nil {
		return err
	}
	slot, err := m.Store.LoadSlot(name)
	if err != nil {
		return err
	}
	socket, err := m.socket(name, false)
	if err != nil {
		return err
	}
	_, err = m.Runner.RPC(ctx, socket, slot.InstanceID, "open", false)
	if err != nil {
		return fmt.Errorf("cannot open %s desktop: %w; run farrow mac up %s, then farrow mac open %s", name, err, name, name)
	}
	return nil
}

func (m *Manager) Destroy(ctx context.Context, name string, reset, update bool) (result *Slot, retErr error) {
	if err := m.requireRunner(); err != nil {
		return nil, err
	}
	operation, err := m.acquireOperation(ctx, name, "mac destroy/reset "+name)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, operation, "mac destroy/reset") }()
	held, err := m.acquireState(ctx, "mac destroy/reset "+name)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac destroy/reset") }()
	slot, err := m.Store.LoadSlot(name)
	if errors.Is(err, os.ErrNotExist) && !reset {
		return &Slot{Name: name, State: "empty"}, nil
	}
	if err != nil {
		return nil, err
	}
	var base *BaseImage
	if reset {
		base, err = m.Store.LoadBase(slot.BaseID)
		if err != nil {
			return nil, err
		}
	}
	if update {
		cfg, err := m.Store.LoadConfig()
		if err != nil {
			return nil, err
		}
		base, err = m.Store.LoadBase(cfg.DefaultBaseID)
		if err != nil {
			return nil, err
		}
	}
	if reset && base.DiskBytes != slot.DiskBytes {
		return nil, fmt.Errorf("cannot reset %s: selected base capacity is %d GiB, but this instance uses %d GiB; prepare a base with matching capacity before reset", name, base.DiskBytes>>30, slot.DiskBytes>>30)
	}
	if reset {
		if err := m.validateResetBase(base); err != nil {
			return nil, fmt.Errorf("cannot reset %s: %w", name, err)
		}
	}
	if err = m.stopRuntime(ctx, slot, false); err != nil {
		return nil, err
	}
	dir, err := m.Store.SlotPath(name)
	if err != nil {
		return nil, err
	}
	stoppedProof, err := lock.TryAcquire(filepath.Join(dir, "runner.lock"), false)
	if err != nil {
		return nil, fmt.Errorf("slot became active before deletion: %w", err)
	}
	defer func() { retErr = lock.JoinRelease(retErr, stoppedProof, "stopped slot deletion") }()
	// Keep the instance and its keychain entry together if credential removal
	// fails. No broad root deletion is used.
	if err = m.Runner.DeletePassword(ctx, m.Store.Root, slot.InstanceID); err != nil {
		return nil, CredentialErrorForSlot(name, err)
	}
	if err = m.Store.DeleteSlot(held, name); err != nil {
		return nil, err
	}
	if !reset {
		return &Slot{Name: name, State: "empty"}, nil
	}
	return m.initLocked(ctx, held, name, InitOptions{User: slot.User, CPU: slot.CPU, MemoryBytes: slot.MemoryBytes}, base)
}

// Check the small metadata and required files before destroying the current
// guest. Native clone still validates the actual image format; this preflight
// catches missing or incomplete bases without hashing an entire system disk.
func (m *Manager) validateResetBase(base *BaseImage) error {
	if base.State != "ready" {
		return fmt.Errorf("base %s is %s, not ready", base.ID, base.State)
	}
	for _, name := range []string{"disk.asif", "hardware-model.bin", "auxiliary-storage.bin", "machine-id.bin", "base.json"} {
		path, err := m.Store.Path("images", "base", base.ID, name)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("base %s is incomplete: %w", base.ID, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("base %s requires a nonempty regular %s", base.ID, name)
		}
		if name == "base.json" {
			var metadata map[string]any
			if err := readJSON(path, &metadata); err != nil {
				return err
			}
			ready, readyOK := metadata["ready"].(bool)
			firstBoot, firstBootOK := metadata["first_boot"].(bool)
			if !readyOK || !ready || !firstBootOK || firstBoot {
				return fmt.Errorf("base %s is not a completed, unbooted restore", base.ID)
			}
		}
	}
	return nil
}
