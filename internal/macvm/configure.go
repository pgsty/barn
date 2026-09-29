package macvm

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/lock"
)

// ConfigureOptions change a machine's settings. Hardware, network and shares
// change only while the machine is stopped; clipboard may change anytime and
// applies at the next start.
type ConfigureOptions struct {
	CPU         int
	MemoryBytes int64
	Share       []Share
	Unshare     []string
	Clipboard   *bool
	Subnet      string
}

func (o ConfigureOptions) empty() bool {
	return o.CPU == 0 && o.MemoryBytes == 0 && len(o.Share) == 0 && len(o.Unshare) == 0 && o.Clipboard == nil && o.Subnet == ""
}

func (o ConfigureOptions) needsStopped() bool {
	return o.CPU != 0 || o.MemoryBytes != 0 || len(o.Share) != 0 || len(o.Unshare) != 0 || o.Subnet != ""
}

// checkExistingOptions refuses create-time options that an existing machine
// does not already have, so up never silently ignores a request.
func checkExistingOptions(machine *Machine, options CreateOptions) error {
	var changed []string
	next := "barn mac configure " + machine.Name
	if options.CPU != 0 && options.CPU != machine.CPU {
		changed, next = append(changed, "--cpu"), next+fmt.Sprintf(" --cpu %d", options.CPU)
	}
	if options.MemoryBytes != 0 && options.MemoryBytes != machine.MemoryBytes {
		changed, next = append(changed, "--memory"), next+" --memory "+memoryArgument(options.MemoryBytes)
	}
	if len(options.Shares) != 0 {
		for _, share := range options.Shares {
			if !containsShare(machine.Shares, share) {
				changed = append(changed, "--share")
				next += " --share " + share.Name + "=" + share.Path
				if share.ReadOnly {
					next += ":ro"
				}
			}
		}
	}
	if options.Clipboard != nil && *options.Clipboard != machine.Clipboard {
		changed = append(changed, "--clipboard")
		next += " --clipboard " + map[bool]string{true: "on", false: "off"}[*options.Clipboard]
	}
	if options.Subnet != "" && options.Subnet != "auto" && options.Subnet != machine.Network.Subnet {
		changed, next = append(changed, "--subnet"), next+" --subnet "+options.Subnet
	}
	// The account and disk capacity are fixed for a machine's lifetime, and
	// recreate keeps them: another value needs another machine. A different
	// macOS comes from a new base.
	fixed := false
	if options.User != "" && options.User != machine.User {
		changed, fixed = append(changed, "--user"), true
	}
	if options.DiskBytes != 0 && options.DiskBytes != machine.DiskBytes {
		changed, fixed = append(changed, "--disk"), true
	}
	if len(changed) == 0 && options.Setup.IPSW == "" {
		return nil
	}
	switch {
	case fixed:
		next = "create another machine for these settings: barn mac up NEW-NAME"
	case options.Setup.IPSW != "":
		changed = append(changed, "--ipsw")
		next = "barn mac image update --ipsw " + options.Setup.IPSW + ", then barn mac recreate " + machine.Name + " --update"
	}
	return failure.New(failure.Conflict, fmt.Errorf("%s already exists, so %s would not apply; its configuration and data were preserved", machine.Name, strings.Join(changed, ", "))).
		Because("mac_configuration_conflict").Then(next)
}

func containsShare(shares []Share, share Share) bool {
	for _, existing := range shares {
		if existing == share {
			return true
		}
	}
	return false
}

func memoryArgument(memory int64) string {
	if memory > 0 && memory%(1<<30) == 0 {
		return fmt.Sprintf("%dG", memory/(1<<30))
	}
	if memory > 0 && memory%(1<<20) == 0 {
		return fmt.Sprintf("%dM", memory/(1<<20))
	}
	return strconv.FormatInt(memory, 10)
}

// Configure changes a machine's settings, never its disk, account or
// identity. Changes apply at the next start.
func (m *Manager) Configure(ctx context.Context, name string, options ConfigureOptions) (outcome Outcome, retErr error) {
	if options.empty() {
		return outcome, failure.New(failure.Usage, errors.New("specify at least one setting to change"))
	}
	if options.CPU != 0 && options.CPU < minimumCPU {
		return outcome, failure.New(failure.Usage, fmt.Errorf("macOS needs at least %d CPUs", minimumCPU))
	}
	if options.MemoryBytes != 0 && options.MemoryBytes < minimumMemoryBytes {
		return outcome, failure.New(failure.Usage, fmt.Errorf("macOS needs at least %d GiB of memory", minimumMemoryBytes>>30))
	}
	if err := m.requireRunner(ctx); err != nil {
		return outcome, err
	}
	operation, err := m.acquireOperation(ctx, name, "mac configure "+name)
	if err != nil {
		return outcome, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, operation, "mac configure") }()
	held, err := m.acquireState(ctx, "mac configure "+name)
	if err != nil {
		return outcome, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac configure state") }()
	machine, err := m.Machine(name)
	if err != nil {
		return outcome, err
	}
	if options.needsStopped() {
		if status, err := m.status(ctx, machine); err == nil && status.State != "stopped" {
			return outcome, failure.New(failure.Conflict, fmt.Errorf("%s is %s; stop it to change its hardware, network or shared folders", name, status.State)).
				Because("mac_running").Then("barn mac stop " + name)
		}
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}
		proof, err := m.stoppedRunnerLock(machine)
		if err != nil {
			return outcome, err
		}
		if proof != nil {
			defer func() { retErr = lock.JoinRelease(retErr, proof, "mac configure stopped proof") }()
		}
	}
	if options.CPU != 0 {
		machine.CPU = options.CPU
	}
	if options.MemoryBytes != 0 {
		machine.MemoryBytes = options.MemoryBytes
	}
	if options.CPU != 0 || options.MemoryBytes != 0 {
		if err := m.checkHostResources(ctx, machine.CPU, machine.MemoryBytes); err != nil {
			return outcome, err
		}
	}
	if len(options.Share) != 0 || len(options.Unshare) != 0 {
		for _, share := range options.Share {
			if err := checkShareSource(share); err != nil {
				return outcome, err
			}
		}
		if machine.Shares, err = mergeShares(machine.Shares, options.Share, options.Unshare); err != nil {
			return outcome, err
		}
	}
	if options.Clipboard != nil {
		machine.Clipboard = *options.Clipboard
	}
	subnetChanged := false
	if options.Subnet != "" {
		requested := options.Subnet
		if requested == "auto" {
			requested = ""
		}
		network, err := m.allocateNetwork(ctx, requested, name)
		if err != nil {
			return outcome, err
		}
		subnetChanged = network != machine.Network
		machine.Network = network
	}
	if err := m.Store.SaveMachine(held, machine); err != nil {
		return outcome, err
	}
	outcome = outcomeOf(machine, "configured")
	outcome.State = displayState(machine)
	if status, err := m.status(ctx, machine); err == nil && status.State != "stopped" {
		outcome.State = "running"
		outcome.Warnings = append(outcome.Warnings, "the change applies at the next start: barn mac restart "+name)
	}
	if subnetChanged {
		if warning := m.refreshSSHConfig(); warning != "" {
			outcome.Warnings = append(outcome.Warnings, warning)
		}
	}
	return outcome, nil
}

// checkHostResources keeps a machine within this host's logical CPUs and
// installed memory, read from the host rather than current free memory.
func (m *Manager) checkHostResources(ctx context.Context, cpu int, memory int64) error {
	if cpu < minimumCPU {
		return failure.New(failure.Usage, fmt.Errorf("macOS needs at least %d CPUs", minimumCPU))
	}
	if memory < minimumMemoryBytes {
		return failure.New(failure.Usage, fmt.Errorf("macOS needs at least %d GiB of memory", minimumMemoryBytes>>30))
	}
	limits := m.hostLimits
	if limits == nil {
		limits = hostLimits
	}
	maxCPU, maxMemory, err := limits(ctx)
	if err != nil {
		return err
	}
	return validateHostResources(cpu, memory, maxCPU, maxMemory)
}

func hostLimits(ctx context.Context) (int, int64, error) {
	output, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "hw.logicalcpu", "hw.memsize").Output()
	if err != nil {
		return 0, 0, fmt.Errorf("read host CPU and memory limits: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		return 0, 0, errors.New("could not determine host CPU and memory limits")
	}
	cpu, cpuErr := strconv.Atoi(fields[0])
	memory, memoryErr := strconv.ParseInt(fields[1], 10, 64)
	if cpuErr != nil || memoryErr != nil || cpu < 1 || memory < 1 {
		return 0, 0, errors.New("invalid host CPU or memory limits")
	}
	return cpu, memory, nil
}

func validateHostResources(cpu int, memory int64, maxCPU int, maxMemory int64) error {
	if memory%(1<<20) != 0 {
		return failure.New(failure.Usage, fmt.Errorf("memory must be a whole number of MiB, not %d bytes", memory))
	}
	if cpu > maxCPU {
		return failure.New(failure.Usage, fmt.Errorf("%d CPUs exceeds this host's %d logical CPUs", cpu, maxCPU))
	}
	if memory > maxMemory {
		return failure.New(failure.Usage, fmt.Errorf("%.1f GiB of memory exceeds this host's %.1f GiB", float64(memory)/(1<<30), float64(maxMemory)/(1<<30)))
	}
	return nil
}
