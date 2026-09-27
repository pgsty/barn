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

	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/lock"
)

func (m *Manager) checkExistingOptions(slot *Slot, options InitOptions) error {
	var changed []string
	if options.CPU != 0 && options.CPU != slot.CPU {
		changed = append(changed, "--cpu")
	}
	if options.MemoryBytes != 0 && options.MemoryBytes != slot.MemoryBytes {
		changed = append(changed, "--memory")
	}
	if options.DiskBytes != 0 && options.DiskBytes != slot.DiskBytes {
		changed = append(changed, "--disk")
	}
	if options.User != "" && options.User != slot.User {
		changed = append(changed, "--user")
	}
	if options.IPSW != "" {
		changed = append(changed, "--ipsw")
	}
	if options.Update {
		changed = append(changed, "--update")
	}
	if options.Subnet != "" {
		config, err := m.Store.LoadConfig()
		if err != nil {
			return err
		}
		if options.Subnet != config.Network.Subnet {
			changed = append(changed, "--subnet")
		}
	}
	if len(changed) == 0 {
		return nil
	}
	message := fmt.Errorf("%s already exists; %s would not be applied. Its configuration and data were preserved", slot.Name, strings.Join(changed, ", "))
	next := "farrow mac ls; use farrow mac image update to prepare a different base"
	if (options.CPU != 0 && options.CPU != slot.CPU) || (options.MemoryBytes != 0 && options.MemoryBytes != slot.MemoryBytes) {
		next = "farrow mac stop " + slot.Name + ", then farrow mac configure " + slot.Name
		if options.CPU != 0 {
			next += fmt.Sprintf(" --cpu %d", options.CPU)
		}
		if options.MemoryBytes != 0 {
			next += " --memory " + resourceMemoryArgument(options.MemoryBytes)
		}
	}
	return failure.New(failure.Conflict, message).Because("mac_configuration_conflict").Then(next)
}

func resourceMemoryArgument(memory int64) string {
	if memory > 0 && memory%(1<<30) == 0 {
		return fmt.Sprintf("%dG", memory/(1<<30))
	}
	if memory > 0 && memory%(1<<20) == 0 {
		return fmt.Sprintf("%dM", memory/(1<<20))
	}
	return strconv.FormatInt(memory, 10)
}

// Configure changes only stopped-instance compute resources. Disk layout,
// guest account and base identity are never changed by this operation.
func (m *Manager) Configure(ctx context.Context, name string, cpu int, memory int64) (slot *Slot, retErr error) {
	normalized, err := NormalizeSlot(name)
	if err != nil {
		return nil, err
	}
	name = normalized
	if cpu == 0 && memory == 0 {
		return nil, failure.New(failure.Usage, errors.New("specify --cpu or --memory"))
	}
	if cpu != 0 && cpu < 2 {
		return nil, failure.New(failure.Usage, errors.New("macOS requires at least 2 CPUs"))
	}
	if memory != 0 && memory < 4<<30 {
		return nil, failure.New(failure.Usage, errors.New("macOS requires at least 4 GiB memory"))
	}
	if err := m.requireRunner(); err != nil {
		return nil, err
	}
	operation, err := m.acquireOperation(ctx, name, "mac configure "+name)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, operation, "mac configure") }()
	held, err := m.acquireState(ctx, "mac configure "+name)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac configure state") }()
	slot, err = m.Store.LoadSlot(name)
	if err != nil {
		return nil, err
	}
	status, statusErr := m.status(ctx, slot)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if statusErr == nil && status.State != "stopped" {
		return nil, failure.New(failure.Conflict, fmt.Errorf("%s is %s; resources can only change while stopped", name, status.State)).Then("farrow mac stop " + name)
	}
	proof, err := m.stoppedRunnerLock(slot)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, proof, "mac configure stopped proof") }()
	config, err := m.Store.LoadConfig()
	if err != nil {
		return nil, err
	}
	if cpu != 0 {
		slot.CPU = cpu
	}
	if memory != 0 {
		slot.MemoryBytes = memory
	}
	limits, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "hw.logicalcpu", "hw.memsize").Output()
	if err != nil {
		return nil, fmt.Errorf("read host resource limits before configuring %s: %w", name, err)
	}
	if err = validateHostResources(slot.CPU, slot.MemoryBytes, string(limits)); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	for i := range config.Slots {
		if config.Slots[i].Name == name {
			config.Slots[i].CPU = slot.CPU
			config.Slots[i].MemoryBytes = slot.MemoryBytes
			config.Slots[i].ResourcesConfigured = true
		}
	}
	if err = m.Store.SaveSlot(held, slot); err != nil {
		return nil, err
	}
	if err = m.Store.SaveConfig(held, config); err != nil {
		return slot, fmt.Errorf("resources saved for %s, but could not update its defaults: %w", name, err)
	}
	return slot, nil
}

func (m *Manager) stoppedRunnerLock(slot *Slot) (*lock.File, error) {
	dir, err := m.Store.SlotPath(slot.Name)
	if err != nil {
		return nil, err
	}
	// Created slots need no runtime file. Avoid creating one during status.
	path := filepath.Join(dir, "runner.lock")
	if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
		bootPath, pathErr := m.Store.Path("slots", slot.Name, "boot-requested")
		if pathErr != nil {
			return nil, pathErr
		}
		_, bootErr := os.Stat(bootPath)
		if slot.Initialized || bootErr == nil {
			return nil, fmt.Errorf("%s previously booted but its runner lock is missing; stopped state is unconfirmed", slot.Name)
		}
		if !errors.Is(bootErr, os.ErrNotExist) {
			return nil, bootErr
		}
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	proof, err := lock.TryAcquire(path, false)
	if err != nil {
		return nil, fmt.Errorf("%s runtime is unresponsive or still active; stopped state is unconfirmed: %w", slot.Name, err)
	}
	return proof, nil
}

// These are conservative physical-host bounds, not a replacement for native
// VZ configuration validation. Read total installed memory, not fluctuating
// free memory, so unrelated activity does not change saved resource policy.
func validateHostResources(cpu int, memory int64, limits string) error {
	fields := strings.Fields(limits)
	if len(fields) != 2 {
		return errors.New("could not determine host CPU and memory limits; resources were not changed")
	}
	maxCPU, cpuErr := strconv.Atoi(fields[0])
	maxMemory, memoryErr := strconv.ParseInt(fields[1], 10, 64)
	if cpuErr != nil || memoryErr != nil || maxCPU < 1 || maxMemory < 1 {
		return errors.New("invalid host CPU or memory limits; resources were not changed")
	}
	if cpu > maxCPU {
		return failure.New(failure.Usage, fmt.Errorf("requested %d CPUs exceeds this host's %d logical CPUs; resources were not changed", cpu, maxCPU))
	}
	if memory > maxMemory {
		return failure.New(failure.Usage, fmt.Errorf("requested %.1f GiB exceeds this host's %.1f GiB memory; resources were not changed", float64(memory)/(1<<30), float64(maxMemory)/(1<<30)))
	}
	return nil
}
