package macvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/lock"
)

// HostVMLimit is Apple's limit on concurrently running macOS guests per host,
// shared with every other virtualization tool and with macOS installation.
const HostVMLimit = 2

// Manager runs Mac machine operations for one Store. The CLI supplies the
// progress, confirmation and privilege hooks; nil hooks are silent defaults.
type Manager struct {
	Store  *Store
	Runner Runner
	// Report receives progress events. Nil discards them.
	Report activity.Reporter
	// SSHHome selects the home whose ~/.ssh holds the Mac SSH fragment. Empty
	// uses the current user's home.
	SSHHome string

	hostRoutes func(context.Context) ([]DarwinRoute, error)
	hostLimits func(context.Context) (int, int64, error)
	protocolOK bool
}

// Status is the whole Mac environment as ls reports it.
type Status struct {
	SchemaVersion int           `json:"schema_version"`
	Root          string        `json:"root"`
	Prepared      bool          `json:"prepared"`
	Base          *ImageRef     `json:"base,omitempty"`
	Machines      []MachineView `json:"machines"`
	Running       int           `json:"running"`
	Limit         int           `json:"limit"`
}

// Outcome is what a lifecycle command reports for one machine.
type Outcome struct {
	Name     string   `json:"name"`
	Action   string   `json:"action"`
	State    string   `json:"state"`
	Ready    bool     `json:"ready"`
	Address  string   `json:"address,omitempty"`
	User     string   `json:"user,omitempty"`
	Version  string   `json:"version,omitempty"`
	Build    string   `json:"build,omitempty"`
	Forced   bool     `json:"forced,omitempty"`
	Window   bool     `json:"window,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Outcomes wraps commands that accept several machines.
type Outcomes struct {
	Machines []Outcome `json:"machines"`
}

func outcomeOf(machine *Machine, action string) Outcome {
	state := machine.State
	if state == "ready" {
		state = "running"
	}
	version, build := machine.Version, machine.Build
	if machine.ObservedBuild != "" {
		version, build = machine.ObservedVersion, machine.ObservedBuild
	}
	return Outcome{Name: machine.Name, Action: action, State: state, Ready: machine.State == "ready", Address: machine.Network.Address, User: machine.User, Version: version, Build: build}
}

// CreateOptions apply only when up creates a machine. Each is refused for an
// existing machine whose configuration differs, so nothing is silently ignored.
type CreateOptions struct {
	User        string
	CPU         int
	MemoryBytes int64
	DiskBytes   int64
	Shares      []Share
	Clipboard   *bool
	Subnet      string
	Setup       SetupOptions
}

// StartOptions control one boot.
type StartOptions struct {
	NoWait   bool
	Recovery bool
	// Window shows the desktop as soon as the VM runs.
	Window bool
}

type UpOptions struct {
	CreateOptions
	StartOptions
}

func (m *Manager) report(phase, format string, args ...any) {
	m.Report.Report(activity.Event{Phase: phase, Message: fmt.Sprintf(format, args...)})
}

func (m *Manager) requireRunner(ctx context.Context) error {
	if err := HostSupported(); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return failure.New(failure.Usage, errors.New("run barn mac as your normal macOS login user, not as root")).Because("mac_root")
	}
	if m.Runner.Binary == "" {
		binary, err := FindRunner()
		if err != nil {
			return err
		}
		m.Runner.Binary = binary
	}
	if m.Runner.Progress == nil {
		m.Runner.Progress = NewProgressWriter(m.Report)
	}
	if !m.protocolOK {
		if err := m.Runner.CheckProtocol(ctx); err != nil {
			return err
		}
		m.protocolOK = true
	}
	return nil
}

func (m *Manager) socket(name string, create bool) (string, error) {
	dir, err := RuntimeDir(m.Store.Root, create)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".sock"), nil
}

// List reports every machine without creating, downloading or starting
// anything. Runtime and SSH checks run concurrently.
func (m *Manager) List(ctx context.Context, probeSSH bool) (Status, error) {
	result := Status{SchemaVersion: SchemaVersion, Root: m.Store.Root, Machines: []MachineView{}, Limit: HostVMLimit}
	config, err := m.Store.LoadConfig()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if config != nil && config.DefaultBaseID != "" {
		if base, err := m.Store.LoadBase(config.DefaultBaseID); err == nil && base.State == "ready" {
			result.Prepared = true
			result.Base = &ImageRef{Version: base.Version, Build: base.Build, BaseID: base.ID}
		}
	}
	machines, err := m.Store.ListMachines()
	if err != nil {
		return result, err
	}
	for i := range machines {
		view := newMachineView(&machines[i])
		if path, err := m.Store.machineFile(machines[i].Name, "disk.asif"); err == nil {
			if _, allocated, err := regularFileUsage(path); err == nil {
				view.Disk.AllocatedBytes = &allocated
			}
		}
		result.Machines = append(result.Machines, view)
	}
	var pending sync.WaitGroup
	for i := range result.Machines {
		pending.Add(1)
		go func(view *MachineView) {
			defer pending.Done()
			m.inspectRuntime(ctx, view, probeSSH)
		}(&result.Machines[i])
	}
	pending.Wait()
	for _, view := range result.Machines {
		if view.State == "running" || view.State == "starting" || view.State == "stopping" {
			result.Running++
		}
	}
	return result, nil
}

// Machine loads one machine, naming how to create it when absent.
func (m *Manager) Machine(name string) (*Machine, error) {
	machine, err := m.Store.LoadMachine(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, failure.New(failure.Conflict, fmt.Errorf("no Mac machine named %s", name)).Because("mac_machine_absent").Then("barn mac up " + name)
	}
	return machine, err
}

// Up creates the machine when needed, starts it and waits for SSH. It never
// changes an existing machine: create-time options that differ are refused.
func (m *Manager) Up(ctx context.Context, name string, options UpOptions) (outcome Outcome, retErr error) {
	if err := m.requireRunner(ctx); err != nil {
		return outcome, err
	}
	operation, err := m.acquireOperation(ctx, name, "mac up "+name)
	if err != nil {
		return outcome, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, operation, "mac up") }()
	machine, err := m.Store.LoadMachine(name)
	action := "started"
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Refuse before creating anything when the new machine could not run.
		if err := m.checkVMLimit(ctx, name); err != nil {
			return outcome, err
		}
		machine, err = m.create(ctx, operation, name, options.CreateOptions)
		action = "created"
	case err == nil:
		err = checkExistingOptions(machine, options.CreateOptions)
	}
	if err != nil {
		return outcome, err
	}
	return m.startLocked(ctx, operation, machine, options.StartOptions, action)
}

// create writes a new machine: base, network, identity, password and overlay.
// A failed overlay clone leaves a prepared record that the next up resumes.
func (m *Manager) create(ctx context.Context, operation *lock.File, name string, options CreateOptions) (*Machine, error) {
	if options.User != "" && !ValidUsername(options.User) {
		return nil, failure.New(failure.Usage, fmt.Errorf("invalid macOS administrator username %q", options.User))
	}
	if err := validateShares(options.Shares); err != nil {
		return nil, failure.New(failure.Usage, err)
	}
	for _, share := range options.Shares {
		if err := checkShareSource(share); err != nil {
			return nil, err
		}
	}
	cpu, memory := DefaultCPU, DefaultMemoryBytes
	if options.CPU != 0 {
		cpu = options.CPU
	}
	if options.MemoryBytes != 0 {
		memory = options.MemoryBytes
	}
	if err := m.checkHostResources(ctx, cpu, memory); err != nil {
		return nil, err
	}
	// A bad or taken --subnet fails before a long macOS preparation; the
	// network is allocated again under the state lock below.
	requested := options.Subnet
	if requested == "auto" {
		requested = ""
	}
	if _, err := m.allocateNetwork(ctx, requested, name); err != nil {
		return nil, err
	}
	held, err := m.acquireState(ctx, "mac up "+name)
	if err != nil {
		return nil, err
	}
	machine, err := func() (*Machine, error) {
		config, err := m.ensureConfig(held)
		if err != nil {
			return nil, err
		}
		setup := options.Setup
		setup.DiskBytes = options.DiskBytes
		base, err := m.setupLocked(ctx, held, config, setup)
		if err != nil {
			return nil, err
		}
		network, err := m.allocateNetwork(ctx, requested, name)
		if err != nil {
			return nil, err
		}
		mac, err := newMAC()
		if err != nil {
			return nil, err
		}
		id, err := NewInstanceID()
		if err != nil {
			return nil, err
		}
		user := options.User
		if user == "" {
			user = DefaultUsername()
		}
		clipboard := true
		if options.Clipboard != nil {
			clipboard = *options.Clipboard
		}
		machine := &Machine{SchemaVersion: SchemaVersion, Name: name, InstanceID: id, BaseID: base.ID, State: "prepared", User: user,
			CPU: cpu, MemoryBytes: memory, DiskBytes: base.DiskBytes, MAC: mac, Network: network, Shares: options.Shares,
			Clipboard: clipboard, Version: base.Version, Build: base.Build, CreatedAt: time.Now().UTC()}
		// The password exists before the record, so a machine never lacks it.
		password, err := NewPassword()
		if err != nil {
			return nil, err
		}
		if err := m.Store.SavePassword(name, password); err != nil {
			return nil, err
		}
		if err := m.Store.SaveMachine(held, machine); err != nil {
			return nil, err
		}
		return machine, nil
	}()
	if releaseErr := held.Release(); releaseErr != nil {
		err = errors.Join(err, releaseErr)
	}
	if err != nil {
		return nil, err
	}
	m.report("prepare", "Creating %s from macOS %s (%s)", name, machine.Version, machine.Build)
	if err := m.cloneMachine(ctx, machine); err != nil {
		machine.State, machine.LastError = "failed", err.Error()
		return nil, errors.Join(err, m.Store.SaveMachine(operation, machine))
	}
	return machine, nil
}

func (m *Manager) ensureConfig(held *lock.File) (*Config, error) {
	config, err := m.Store.LoadConfig()
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return config, err
	}
	config, err = NewConfig()
	if err != nil {
		return nil, err
	}
	return config, m.Store.SaveConfig(held, config)
}

// cloneMachine creates the overlay disk, auxiliary storage and a fresh machine
// identifier from the base. A partial clone of a never-booted machine is
// discarded; a machine that ever booted is never re-cloned.
func (m *Manager) cloneMachine(ctx context.Context, machine *Machine) error {
	dir, err := m.Store.MachinePath(machine.Name)
	if err != nil {
		return err
	}
	files := []string{"disk.asif", "machine-id.bin", "auxiliary-storage.bin"}
	count := 0
	for _, name := range files {
		if _, err = os.Stat(filepath.Join(dir, name)); err == nil {
			count++
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if count == len(files) {
		return nil
	}
	if _, err = os.Stat(filepath.Join(dir, "boot-requested")); err == nil || machine.Initialized {
		return failure.New(failure.Integrity, fmt.Errorf("%s booted before but its disk or identity files are missing; its directory was preserved for recovery: %s", machine.Name, dir)).Because("mac_machine_damaged")
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
				return errors.Join(err, held.Release())
			}
		}
		if err = held.Release(); err != nil {
			return err
		}
	}
	base, err := m.Store.BasePath(machine.BaseID)
	if err != nil {
		return err
	}
	return m.Runner.Call(ctx, nil, nil, "clone", "--base", base, "--slot", dir)
}

// runInput is the runner's one line of stdin. It carries the first-boot
// password, so it never appears in arguments, logs or progress.
type runInput struct {
	Name      string        `json:"name"`
	Subtitle  string        `json:"subtitle,omitempty"`
	Provision *runProvision `json:"provision,omitempty"`
	Shares    []Share       `json:"shares,omitempty"`
	Guest     *runGuest     `json:"guest,omitempty"`
	Clipboard bool          `json:"clipboard,omitempty"`
	Window    bool          `json:"window,omitempty"`
}

type runProvision struct {
	Username string `json:"username"`
	Password string `json:"password"`
	FullName string `json:"full_name"`
}

// runGuest lets the runner reach the guest over pinned SSH for clipboard
// sharing and the desktop Restart command. SSH is the argv up to and
// including user@host; the runner appends only fixed guest commands.
type runGuest struct {
	SSH        []string `json:"ssh"`
	KnownHosts string   `json:"known_hosts"`
}

func (m *Manager) startLocked(ctx context.Context, operation *lock.File, machine *Machine, options StartOptions, action string) (outcome Outcome, retErr error) {
	defer func() {
		if retErr != nil && !errors.Is(retErr, errReadinessStopped) && ctx.Err() == nil {
			machine.LastError = retErr.Error()
			_ = m.Store.SaveMachine(operation, machine)
		}
	}()
	password := ""
	if !machine.Initialized {
		if err := m.cloneMachine(ctx, machine); err != nil {
			return outcome, err
		}
		var err error
		if password, err = m.Store.Password(machine.Name); err != nil {
			return outcome, err
		}
	} else if !options.NoWait && !options.Recovery {
		if err := m.checkSSHMaterial(machine); err != nil {
			return outcome, err
		}
	}
	launch, err := m.shouldLaunch(ctx, machine)
	if err != nil {
		return outcome, err
	}
	window := options.Window || options.Recovery
	if launch {
		if err := m.launch(ctx, operation, machine, password, options.Recovery, window); err != nil {
			return outcome, err
		}
	} else {
		if options.Recovery {
			return outcome, failure.New(failure.Conflict, fmt.Errorf("%s is running; recoveryOS needs a fresh boot", machine.Name)).Then("barn mac stop " + machine.Name)
		}
		if action == "started" {
			action = "running"
		}
		if window {
			if err := m.showWindow(ctx, machine); err != nil {
				return outcome, err
			}
		}
	}
	if machine.State != "ready" || launch {
		machine.State = "running"
		if err := m.Store.SaveMachine(operation, machine); err != nil {
			return outcome, err
		}
	}
	if options.NoWait || options.Recovery {
		outcome = outcomeOf(machine, action)
		outcome.Window = window
		return outcome, nil
	}
	observation, err := m.waitForSSH(ctx, operation, machine, password)
	if err != nil {
		return outcome, err
	}
	machine.Initialized, machine.State, machine.LastError = true, "ready", ""
	machine.ObservedVersion, machine.ObservedBuild = observation.Version, observation.Build
	if err := m.Store.SaveMachine(operation, machine); err != nil {
		return outcome, err
	}
	outcome = outcomeOf(machine, action)
	outcome.Window = window
	if warning := m.refreshSSHConfig(); warning != "" {
		outcome.Warnings = append(outcome.Warnings, warning)
	}
	return outcome, nil
}

// launch starts a detached runner for a machine proven stopped.
func (m *Manager) launch(ctx context.Context, operation *lock.File, machine *Machine, password string, recovery, window bool) error {
	if err := m.checkVMLimit(ctx, machine.Name); err != nil {
		return err
	}
	if err := m.checkNetworkFree(ctx, machine); err != nil {
		return err
	}
	for _, share := range machine.Shares {
		if err := checkShareSource(share); err != nil {
			return failure.WithNext(err, fmt.Sprintf("barn mac configure %s --unshare %s", machine.Name, share.Name))
		}
	}
	basePath, err := m.Store.BasePath(machine.BaseID)
	if err != nil {
		return err
	}
	dir, err := m.Store.MachinePath(machine.Name)
	if err != nil {
		return err
	}
	socket, err := m.socket(machine.Name, true)
	if err != nil {
		return err
	}
	args := []string{"run", "--base", basePath, "--slot", dir, "--socket", socket, "--instance", machine.InstanceID,
		"--mac", machine.MAC, "--gateway", machine.Network.Gateway, "--address", machine.Network.Address,
		"--cpu", strconv.Itoa(machine.CPU), "--memory", strconv.FormatInt(machine.MemoryBytes, 10)}
	if recovery {
		args = append(args, "--recovery")
	}
	connection, err := m.connection(machine)
	if err != nil {
		return err
	}
	version := machine.Version
	if machine.ObservedVersion != "" {
		version = machine.ObservedVersion
	}
	input := runInput{Name: machine.Name, Subtitle: fmt.Sprintf("macOS %s · %s@%s", version, machine.User, machine.Network.Address),
		Shares: machine.Shares, Clipboard: machine.Clipboard, Window: window,
		Guest: &runGuest{SSH: append([]string{SSHPath}, connection.OpenSSHArgs(false)...), KnownHosts: connection.KnownHosts}}
	if !machine.Initialized {
		input.Provision = &runProvision{Username: machine.User, Password: password, FullName: machine.User}
	}
	machine.State = "starting"
	if err := m.Store.SaveMachine(operation, machine); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "boot-requested"), []byte(machine.InstanceID+"\n"), 0o600); err != nil {
		return err
	}
	m.report("guest-ready", "Starting %s", machine.Name)
	_, err = m.Runner.Launch(ctx, args, input, filepath.Join(dir, "runner.log"), socket, machine.InstanceID)
	return err
}

// checkVMLimit names this store's running machines when Apple's limit would
// refuse another boot. Guests of other tools still count; the runner reports
// that case itself.
func (m *Manager) checkVMLimit(ctx context.Context, starting string) error {
	machines, err := m.Store.usableMachines()
	if err != nil {
		return err
	}
	var running []string
	for i := range machines {
		if machines[i].Name == starting {
			continue
		}
		if status, err := m.status(ctx, &machines[i]); err == nil && status.State != "stopped" {
			running = append(running, machines[i].Name)
		}
	}
	if len(running) < HostVMLimit {
		return nil
	}
	return failure.New(failure.Resource, fmt.Errorf("%s are running; macOS allows %d macOS virtual machines at a time", strings.Join(running, " and "), HostVMLimit)).
		Because("mac_vm_limit").Then("barn mac stop " + running[len(running)-1])
}

func (m *Manager) showWindow(ctx context.Context, machine *Machine) error {
	socket, err := m.socket(machine.Name, false)
	if err != nil {
		return err
	}
	if _, err := m.Runner.RPC(ctx, socket, machine.InstanceID, "open", false); err != nil {
		return fmt.Errorf("cannot show the %s desktop: %w", machine.Name, err)
	}
	return nil
}

// Start boots existing, initialized machines. Several machines start
// concurrently; each result is reported in the order requested.
func (m *Manager) Start(ctx context.Context, names []string, options StartOptions) (Outcomes, error) {
	if err := m.requireRunner(ctx); err != nil {
		return Outcomes{}, err
	}
	return m.each(ctx, names, func(ctx context.Context, name string) (Outcome, error) {
		return m.startOne(ctx, name, options)
	})
}

func (m *Manager) startOne(ctx context.Context, name string, options StartOptions) (outcome Outcome, retErr error) {
	operation, err := m.acquireOperation(ctx, name, "mac start "+name)
	if err != nil {
		return outcome, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, operation, "mac start") }()
	machine, err := m.Machine(name)
	if err != nil {
		return outcome, err
	}
	if !machine.Initialized && !options.Recovery {
		return outcome, failure.New(failure.Conflict, fmt.Errorf("%s has not finished its first boot", name)).Because("mac_not_initialized").Then("barn mac up " + name)
	}
	return m.startLocked(ctx, operation, machine, options, "started")
}

// each runs one operation per machine concurrently and joins the failures.
func (m *Manager) each(ctx context.Context, names []string, run func(context.Context, string) (Outcome, error)) (Outcomes, error) {
	results := make([]Outcome, len(names))
	errs := make([]error, len(names))
	var pending sync.WaitGroup
	for i, name := range names {
		pending.Add(1)
		go func(i int, name string) {
			defer pending.Done()
			results[i], errs[i] = run(ctx, name)
			if errs[i] != nil && len(names) > 1 {
				errs[i] = fmt.Errorf("%s: %w", name, errs[i])
			}
		}(i, name)
	}
	pending.Wait()
	report := Outcomes{Machines: []Outcome{}}
	for i := range names {
		if errs[i] == nil {
			report.Machines = append(report.Machines, results[i])
		}
	}
	err := errors.Join(errs...)
	if err != nil && len(report.Machines) > 0 {
		err = failure.New(failure.Partial, err)
	}
	return report, err
}

// stopGrace bounds a normal shutdown before the VM is powered off.
var stopGrace = 2 * time.Minute

// Stop shuts machines down normally, powering off one that does not finish
// within two minutes. force powers off at once.
func (m *Manager) Stop(ctx context.Context, names []string, force bool) (Outcomes, error) {
	if err := m.requireRunner(ctx); err != nil {
		return Outcomes{}, err
	}
	return m.each(ctx, names, func(ctx context.Context, name string) (Outcome, error) {
		return m.stopOne(ctx, name, force)
	})
}

func (m *Manager) stopOne(ctx context.Context, name string, force bool) (outcome Outcome, retErr error) {
	held, err := m.acquireStopOperation(ctx, name, "mac stop "+name)
	if err != nil {
		return outcome, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac stop") }()
	machine, err := m.Machine(name)
	if err != nil {
		return outcome, err
	}
	stopped, forced, err := m.stopRuntime(ctx, machine, force)
	if err != nil {
		return outcome, err
	}
	machine.State = "stopped"
	if !machine.Initialized {
		machine.State = "prepared"
	}
	if err := m.Store.SaveMachine(held, machine); err != nil {
		return outcome, err
	}
	action := "already_stopped"
	switch {
	case stopped && force:
		action = "powered_off"
	case stopped:
		action = "stopped"
	}
	outcome = outcomeOf(machine, action)
	outcome.Forced = forced
	return outcome, nil
}

// stopRuntime returns whether a running VM was stopped and whether it had to
// be powered off. Success always means the runner lock proved the stop.
func (m *Manager) stopRuntime(ctx context.Context, machine *Machine, force bool) (stopped, forced bool, err error) {
	return m.stopRuntimeWith(ctx, machine, force, m.shutdownGuestSSH)
}

func (m *Manager) stopRuntimeWith(ctx context.Context, machine *Machine, force bool, shutdown func(context.Context, *Machine) error) (stopped, forced bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	if _, err := m.status(ctx, machine); err != nil {
		// The runner owns an exclusive lock for its lifetime. Prove it is free
		// before treating an unavailable socket as a stopped VM.
		proof, err := m.stoppedRunnerLock(machine)
		if err != nil {
			return false, false, fmt.Errorf("the %s runner is not responding, so a stop cannot be confirmed: %w", machine.Name, err)
		}
		if proof != nil {
			if err := proof.Release(); err != nil {
				return false, false, err
			}
		}
		return false, false, ctx.Err()
	}
	socket, err := m.socket(machine.Name, false)
	if err != nil {
		return false, false, err
	}
	if !force {
		m.report("stop", "Shutting down %s", machine.Name)
		var shutdownErr error
		if machine.Initialized {
			shutdownErr = shutdown(ctx, machine)
			if err := ctx.Err(); err != nil {
				return false, false, err
			}
		}
		if !machine.Initialized || shutdownErr != nil {
			if _, err := m.Runner.RPC(ctx, socket, machine.InstanceID, "stop", false); err != nil {
				// Neither macOS nor the VM took the request; still power off
				// after the grace period, as a normal stop promises.
				reason := strings.ReplaceAll(errors.Join(shutdownErr, err).Error(), "\n", "; ")
				m.report("stop", "%s did not accept a normal shutdown (%s); waiting %s before powering it off", machine.Name, reason, stopGrace)
			}
		}
		done, err := m.waitStopped(ctx, machine, stopGrace)
		if err != nil || done {
			return done, false, err
		}
		m.report("stop", "%s did not shut down within %s; powering it off", machine.Name, stopGrace)
	}
	if _, err := m.Runner.RPC(ctx, socket, machine.InstanceID, "stop", true); err != nil {
		// The VM may have finished stopping between the checks.
		if done, waitErr := m.waitStopped(ctx, machine, 5*time.Second); waitErr == nil && done {
			return true, !force, nil
		}
		return false, false, err
	}
	done, err := m.waitStopped(ctx, machine, 30*time.Second)
	if err == nil && !done {
		err = fmt.Errorf("%s did not power off within 30 seconds; inspect barn mac logs %s", machine.Name, machine.Name)
	}
	return done, true, err
}

// waitStopped polls until the runner exits and its lock proves the stop.
func (m *Manager) waitStopped(ctx context.Context, machine *Machine, limit time.Duration) (bool, error) {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := m.status(ctx, machine)
		if err != nil || status.State == "stopped" {
			proof, lockErr := m.stoppedRunnerLock(machine)
			if lockErr == nil {
				if proof != nil {
					if err := proof.Release(); err != nil {
						return false, err
					}
				}
				return true, nil
			}
			if !errors.Is(lockErr, lock.ErrBusy) {
				return false, lockErr
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-ticker.C:
		}
	}
}

// Restart stops a machine normally and starts it again, so configuration
// changes take effect.
func (m *Manager) Restart(ctx context.Context, name string, options StartOptions) (Outcome, error) {
	if err := m.requireRunner(ctx); err != nil {
		return Outcome{}, err
	}
	machine, err := m.Machine(name)
	if err != nil {
		return Outcome{}, err
	}
	if !machine.Initialized {
		return Outcome{}, failure.New(failure.Conflict, fmt.Errorf("%s has not finished its first boot", name)).Because("mac_not_initialized").Then("barn mac up " + name)
	}
	stopped, err := m.stopOne(ctx, name, false)
	if err != nil {
		return Outcome{}, err
	}
	outcome, err := m.startOne(ctx, name, options)
	if err == nil {
		outcome.Action, outcome.Forced = "restarted", stopped.Forced
	}
	return outcome, err
}

// Open shows the desktop, starting an initialized machine first when needed.
func (m *Manager) Open(ctx context.Context, name string) (outcome Outcome, retErr error) {
	if err := m.requireRunner(ctx); err != nil {
		return outcome, err
	}
	machine, err := m.Machine(name)
	if err != nil {
		return outcome, err
	}
	if status, err := m.status(ctx, machine); err == nil && status.State == "running" {
		if err := m.showWindow(ctx, machine); err != nil {
			return outcome, err
		}
		outcome = outcomeOf(machine, "opened")
		outcome.State, outcome.Window = "running", true
		return outcome, nil
	}
	if !machine.Initialized {
		return outcome, failure.New(failure.Conflict, fmt.Errorf("%s has not finished its first boot", name)).Because("mac_not_initialized").Then("barn mac up " + name + " --open")
	}
	outcome, err = m.startOne(ctx, name, StartOptions{NoWait: true, Window: true})
	if err == nil {
		outcome.Action = "opened"
	}
	return outcome, err
}

// Destroy powers machines off and removes their directories. Shared images
// and other machines are untouched.
func (m *Manager) Destroy(ctx context.Context, names []string) (Outcomes, error) {
	if err := m.requireRunner(ctx); err != nil {
		return Outcomes{}, err
	}
	report := Outcomes{Machines: []Outcome{}}
	var errs []error
	for _, name := range names {
		outcome, err := m.destroyOne(ctx, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		report.Machines = append(report.Machines, outcome)
	}
	if warning := m.refreshSSHConfig(); warning != "" && len(report.Machines) > 0 {
		report.Machines[len(report.Machines)-1].Warnings = append(report.Machines[len(report.Machines)-1].Warnings, warning)
	}
	err := errors.Join(errs...)
	if err != nil && len(report.Machines) > 0 {
		err = failure.New(failure.Partial, err)
	}
	return report, err
}

func (m *Manager) destroyOne(ctx context.Context, name string) (outcome Outcome, retErr error) {
	operation, err := m.acquireStopOperation(ctx, name, "mac destroy "+name)
	if err != nil {
		return outcome, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, operation, "mac destroy") }()
	machine, err := m.Store.LoadMachine(name)
	if errors.Is(err, os.ErrNotExist) {
		return Outcome{Name: name, Action: "absent", State: "absent"}, nil
	}
	if errors.Is(err, errStateless) {
		// Without a record no runner can have started it; the runner lock
		// taken during removal proves that none holds its files.
		held, err := m.acquireState(ctx, "mac destroy "+name)
		if err != nil {
			return outcome, err
		}
		defer func() { retErr = lock.JoinRelease(retErr, held, "mac destroy") }()
		if err := m.removeMachineFiles(held, &Machine{Name: name}); err != nil {
			return outcome, err
		}
		return Outcome{Name: name, Action: "destroyed", State: "absent"}, nil
	}
	if err != nil {
		return outcome, err
	}
	if _, _, err := m.stopRuntime(ctx, machine, true); err != nil {
		return outcome, err
	}
	held, err := m.acquireState(ctx, "mac destroy "+name)
	if err != nil {
		return outcome, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac destroy") }()
	if err := m.removeMachineFiles(held, machine); err != nil {
		return outcome, err
	}
	return Outcome{Name: name, Action: "destroyed", State: "absent", Address: machine.Network.Address}, nil
}

// removeMachineFiles deletes a stopped machine's directory while holding its
// runner lock, so no runner can start on the files being removed.
func (m *Manager) removeMachineFiles(held *lock.File, machine *Machine) (retErr error) {
	dir, err := m.Store.MachinePath(machine.Name)
	if err != nil {
		return err
	}
	proof, err := lock.TryAcquire(filepath.Join(dir, "runner.lock"), false)
	if err != nil {
		return fmt.Errorf("%s became active before removal: %w", machine.Name, err)
	}
	defer func() { retErr = lock.JoinRelease(retErr, proof, "stopped machine removal") }()
	return m.Store.DeleteMachine(held, machine.Name)
}

// RecreateOptions choose the base and the first boot of the new instance.
type RecreateOptions struct {
	Update bool
	StartOptions
}

// Recreate replaces a machine with a fresh instance of the same name,
// settings, network and account. Update selects the current default base.
func (m *Manager) Recreate(ctx context.Context, name string, options RecreateOptions) (outcome Outcome, retErr error) {
	if err := m.requireRunner(ctx); err != nil {
		return outcome, err
	}
	operation, err := m.acquireStopOperation(ctx, name, "mac recreate "+name)
	if err != nil {
		return outcome, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, operation, "mac recreate") }()
	old, err := m.Machine(name)
	if err != nil {
		return outcome, err
	}
	base, err := m.Store.LoadBase(old.BaseID)
	if options.Update {
		base, err = m.updatedBase(old)
	}
	if err != nil {
		return outcome, err
	}
	if err := m.validateBase(base); err != nil {
		return outcome, fmt.Errorf("cannot recreate %s: %w", name, err)
	}
	// Check what the new instance needs before anything is erased.
	for _, share := range old.Shares {
		if err := checkShareSource(share); err != nil {
			return outcome, err
		}
	}
	if _, _, err := m.stopRuntime(ctx, old, true); err != nil {
		return outcome, err
	}
	if err := m.checkNetworkFree(ctx, old); err != nil {
		return outcome, err
	}
	held, err := m.acquireState(ctx, "mac recreate "+name)
	if err != nil {
		return outcome, err
	}
	machine, err := func() (*Machine, error) {
		if err := m.removeMachineFiles(held, old); err != nil {
			return nil, err
		}
		id, err := NewInstanceID()
		if err != nil {
			return nil, err
		}
		machine := *old
		machine.InstanceID, machine.BaseID, machine.State, machine.Initialized = id, base.ID, "prepared", false
		machine.DiskBytes, machine.Version, machine.Build = base.DiskBytes, base.Version, base.Build
		machine.ObservedVersion, machine.ObservedBuild, machine.LastError = "", "", ""
		machine.CreatedAt = time.Now().UTC()
		password, err := NewPassword()
		if err != nil {
			return nil, err
		}
		if err := m.Store.SavePassword(name, password); err != nil {
			return nil, err
		}
		return &machine, m.Store.SaveMachine(held, &machine)
	}()
	if releaseErr := held.Release(); releaseErr != nil {
		err = errors.Join(err, releaseErr)
	}
	if err != nil {
		return outcome, err
	}
	if err := m.cloneMachine(ctx, machine); err != nil {
		machine.State, machine.LastError = "failed", err.Error()
		return outcome, errors.Join(err, m.Store.SaveMachine(operation, machine))
	}
	outcome, err = m.startLocked(ctx, operation, machine, options.StartOptions, "recreated")
	return outcome, err
}

// updatedBase is the default base's macOS at the machine's own disk capacity,
// which recreate keeps; another capacity needs its base prepared first.
func (m *Manager) updatedBase(machine *Machine) (*BaseImage, error) {
	config, err := m.Store.LoadConfig()
	if err != nil {
		return nil, err
	}
	if config.DefaultBaseID == "" {
		return nil, failure.New(failure.Conflict, errors.New("no prepared default base")).Then("barn mac image update")
	}
	base, err := m.Store.LoadBase(config.DefaultBaseID)
	if err != nil || base.DiskBytes == machine.DiskBytes {
		return base, err
	}
	if ready := m.readyBaseFor(base.Build, machine.DiskBytes); ready != nil {
		return ready, nil
	}
	return nil, failure.New(failure.Conflict, fmt.Errorf("%s has a %d GiB disk, but macOS %s (%s) is prepared only at %d GiB", machine.Name, machine.DiskBytes>>30, base.Version, base.Build, base.DiskBytes>>30)).
		Then(fmt.Sprintf("barn mac setup --disk %dG, then barn mac recreate %s --update", machine.DiskBytes>>30, machine.Name))
}

// validateBase checks a base's small metadata and required files before a
// machine is replaced. The runner still validates the actual disk format.
func (m *Manager) validateBase(base *BaseImage) error {
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

// Password returns the guest login password saved at creation.
func (m *Manager) Password(name string) (string, error) {
	if _, err := m.Machine(name); err != nil {
		return "", err
	}
	return m.Store.Password(name)
}

// Connection returns the OpenSSH parameters for a running, initialized guest.
func (m *Manager) Connection(ctx context.Context, name string) (Connection, error) {
	machine, err := m.Machine(name)
	if err != nil {
		return Connection{}, err
	}
	if !machine.Initialized {
		return Connection{}, failure.New(failure.Conflict, fmt.Errorf("%s has not finished its first boot", name)).Because("mac_not_initialized").Then("barn mac up " + name)
	}
	if err := m.checkSSHMaterial(machine); err != nil {
		return Connection{}, err
	}
	status, err := m.status(ctx, machine)
	if err != nil || status.State != "running" {
		return Connection{}, failure.New(failure.Conflict, fmt.Errorf("%s is not running", name)).Because("mac_not_running").Then("barn mac start " + name)
	}
	return m.connection(machine)
}
