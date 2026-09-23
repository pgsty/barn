package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pgsty/farrow/internal/config"
	"github.com/pgsty/farrow/internal/doctor"
	"github.com/pgsty/farrow/internal/execx"
	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/hostconfig"
	"github.com/pgsty/farrow/internal/identity"
	"github.com/pgsty/farrow/internal/image"
	"github.com/pgsty/farrow/internal/naming"
	darwinnet "github.com/pgsty/farrow/internal/network/darwin"
	linuxnet "github.com/pgsty/farrow/internal/network/linux"
	netpreflight "github.com/pgsty/farrow/internal/network/preflight"
	"github.com/pgsty/farrow/internal/network/subnet"
	privatevm "github.com/pgsty/farrow/internal/private"
	"github.com/pgsty/farrow/internal/provision"
	"github.com/pgsty/farrow/internal/spec"
	"github.com/pgsty/farrow/internal/sshconfig"
	"github.com/pgsty/farrow/internal/sshkeys"
	"github.com/pgsty/farrow/internal/state"
	"github.com/pgsty/farrow/internal/version"
	"github.com/pgsty/farrow/internal/vm"
	"golang.org/x/term"
)

const (
	exitOK         = 0
	exitRuntime    = 1
	exitUsage      = 2
	exitCapability = 3
	exitConflict   = 4
	exitPartial    = 5
	exitResource   = 6
	exitIntegrity  = 7
	exitCancelled  = 130

	// Compatibility expiry: user-network-state-v0 in CONTRIBUTING.md#compatibility-expiry.
	legacyDeploymentMessage = "this deployment predates the fixed-IP redesign; preserve any needed disks, then move or remove the selected FARROW_HOME and run `farrow setup && farrow up`"
)

type lifecycleOptions struct {
	Force            bool
	DeletePersistent bool
	Purge            bool
	NoWait           bool
	Rollback         bool
	ConfigPath       string
	Repository       string
	Mirror           bool
}

func configurationWarnings(resolved spec.Resolved) []string {
	warnings := make([]string, 0)
	if resolved.Network == "private" && resolved.Private != nil {
		if layout, err := subnet.Parse(resolved.Private.CIDR); err == nil {
			if warning := layout.Warning(); warning != "" {
				warnings = append(warnings, warning)
			}
		}
	}
	for _, node := range resolved.Nodes {
		for _, forward := range node.Forwards {
			address := net.ParseIP(forward.Bind)
			if address != nil && !address.IsLoopback() {
				warnings = append(warnings, fmt.Sprintf("node %s exposes host TCP %s:%d beyond loopback; this may make guest port %d reachable from other machines", node.Name, forward.Bind, forward.Host, forward.Guest))
			}
		}
	}
	return warnings
}

func printWarnings(out io.Writer, warnings []string) {
	for _, warning := range warnings {
		warningf(out, "%s", warning)
	}
}

func printImageStatusWarning(out io.Writer, entry image.Entry) {
	if entry.Status == "deprecated" {
		warningf(out, "image %s/%s (%s) is deprecated and EOL; use only for isolated compatibility testing", entry.Alias, entry.Arch, entry.Release)
	} else if entry.Status != "" && entry.Status != "supported" {
		warningf(out, "image %s/%s (%s) has status %s, not supported; use only with the corresponding test/risk acceptance", entry.Alias, entry.Arch, entry.Release, entry.Status)
	}
}

func lifecycleImageArch(resolved spec.Resolved) string {
	if resolved.Arch != "" {
		return resolved.Arch
	}
	return runtime.GOARCH
}

func confirmDestructive(force, interactive bool, action string, input io.Reader, output io.Writer) error {
	if force {
		return nil
	}
	if !interactive {
		return fmt.Errorf("%s requires --force when stdin is not a TTY", action)
	}
	if _, err := fmt.Fprintf(output, "Type %q to confirm: ", action); err != nil {
		return fmt.Errorf("write %s confirmation prompt: %w", action, err)
	}
	line, err := bufio.NewReader(io.LimitReader(input, 256)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read %s confirmation: %w", action, err)
	}
	switch typed := strings.TrimSpace(line); typed {
	case action:
		return nil
	case "":
		return fmt.Errorf("%w: no confirmation was entered; nothing was changed", ErrCancelled)
	default:
		return fmt.Errorf("%w: typed %q, expected %q; nothing was changed", ErrCancelled, typed, action)
	}
}

func confirmCLIAction(force bool, action string, stderr io.Writer) error {
	resume := suspendProgress(stderr)
	defer resume()
	return confirmDestructive(force, term.IsTerminal(int(os.Stdin.Fd())), action, os.Stdin, stderr)
}

func withReadinessTimeout(base time.Duration, resolved spec.Resolved) time.Duration {
	wait, err := resolved.SSHWaitTimeout()
	if err != nil || wait > time.Duration(1<<63-1)-5*time.Minute {
		return base
	}
	if candidate := wait + 5*time.Minute; candidate > base {
		return candidate
	}
	return base
}

type sudoRunner struct {
	base                execx.Runner
	preserveEnvironment []string
}

func (r sudoRunner) Run(ctx context.Context, binary string, args ...string) (execx.Result, error) {
	if r.base == nil {
		return execx.Result{}, errors.New("sudo runner has no base runner")
	}
	sudoArgs := []string{"-n"}
	if len(r.preserveEnvironment) > 0 {
		sudoArgs = append(sudoArgs, "--preserve-env="+strings.Join(r.preserveEnvironment, ","))
	}
	sudoArgs = append(sudoArgs, "--", binary)
	sudoArgs = append(sudoArgs, args...)
	return r.base.Run(ctx, "/usr/bin/sudo", sudoArgs...)
}

type darwinNetworkInstaller interface {
	InstallModeNetwork(context.Context, string, string, string, string, string, bool) (darwinnet.InstallReport, error)
}

func installDarwinNetwork(ctx context.Context, installer darwinNetworkInstaller, archive, interfaceID, arch, mode, cidr string, apply bool) (darwinnet.InstallReport, error) {
	return installer.InstallModeNetwork(ctx, archive, interfaceID, arch, mode, cidr, apply)
}

type networkOptions struct {
	Action      string
	CIDR        string
	Mode        string
	Archive     string
	InterfaceID string
	Apply       bool
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func runNetwork(parent context.Context, options networkOptions, stderr io.Writer) (commandOutcome, error) {
	command := options.Action
	if command == "install" || command == "uninstall" {
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			return commandOutcome{}, newCommandError(exitCapability, errors.New("network install/uninstall supports native Linux and macOS"))
		}
		var layout subnet.Layout
		vmnetMode := "host"
		if command == "install" {
			vmnetMode = options.Mode
			if vmnetMode == "" {
				vmnetMode = "host"
			}
			if vmnetMode != "host" && vmnetMode != "shared" {
				return commandOutcome{}, newUsageError(fmt.Errorf("--mode must be host or shared, got %q", vmnetMode))
			}
			if runtime.GOOS == "linux" && vmnetMode != "host" {
				return commandOutcome{}, newUsageError(errors.New("--mode is a macOS socket_vmnet option; Linux uses farrow0"))
			}
			if runtime.GOOS != "darwin" && (options.Archive != "" || options.InterfaceID != "") {
				return commandOutcome{}, newUsageError(errors.New("--archive and --interface-id are macOS-only"))
			}
			networkCIDR := options.CIDR
			if networkCIDR == "" {
				networkCIDR = subnet.DefaultCIDR
			}
			var layoutErr error
			layout, layoutErr = subnet.Parse(networkCIDR)
			if layoutErr != nil {
				return commandOutcome{}, newUsageError(layoutErr)
			}
		}
		baseRunner := execx.OSRunner{Timeout: 2 * time.Minute, OutputLimit: 1 << 20}
		ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
		defer cancel()
		if command == "install" {
			addresses := withoutRecordedAddresses(layout.StaticAddresses())
			preflightProgress := startProgress(ctx, stderr, "Validating the fixed-IP network plan")
			preflightReport := netpreflight.Run(ctx, netpreflight.Request{OS: runtime.GOOS, Arch: runtime.GOARCH, Purpose: netpreflight.Install, Layout: layout, Addresses: addresses}, netpreflight.Probe{Runner: baseRunner})
			preflightProgress.Stop(nil)
			if errors.Is(parent.Err(), context.Canceled) {
				return commandOutcome{}, ErrCancelled
			}
			if !preflightReport.Ready {
				return commandOutcome{}, newCommandError(preflightReport.ExitCode, setupFindingError(preflightReport)).withPayload(preflightReport)
			}
		}
		privilege := &sudoSession{base: baseRunner, stderr: stderr, scope: "network command"}
		defer privilege.close()
		reason := "read the protected host-network ownership state for the plan"
		if options.Apply {
			reason = "apply the reviewed host-network plan"
		}
		if err := privilege.ensure(ctx, reason); err != nil {
			return commandOutcome{}, newCommandError(exitCapability, err)
		}
		rootRunner := setupRootRunner(baseRunner)
		if runtime.GOOS == "darwin" {
			executor := darwinnet.Executor{User: baseRunner, Root: rootRunner, InUse: deploymentInUse}
			if command == "install" {
				progressItem := startProgress(ctx, stderr, "Installing the Darwin fixed-IP network")
				report, err := installDarwinNetwork(ctx, executor, options.Archive, options.InterfaceID, runtime.GOARCH, vmnetMode, layout.CIDR(), options.Apply)
				progressItem.Stop(err)
				if errors.Is(parent.Err(), context.Canceled) {
					return commandOutcome{}, ErrCancelled
				}
				if err != nil {
					if errors.Is(err, darwinnet.ErrVMNetSharingBusy) {
						return commandOutcome{}, newConflictError(err)
					}
					return commandOutcome{}, newCommandError(exitRuntime, err)
				}
				for _, warning := range report.Warnings {
					warningf(stderr, "%s", warning)
				}
				return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
					bestEffortf(stdout, "action: %s\napplied: %t\n", report.Action, report.Applied)
					for _, path := range sortedMapKeys(report.Targets) {
						bestEffortf(stdout, "%s %s\n", path, report.Targets[path])
					}
					if !report.Applied && report.Action != "none" {
						bestEffortln(stdout, "rerun with --yes using the same --archive and --interface-id; Farrow will request sudo when needed")
						return nil
					}
					return nil
				}}, nil
			}
			progressItem := startProgress(ctx, stderr, "Removing the Darwin fixed-IP network")
			report, err := executor.Uninstall(ctx, options.Apply)
			progressItem.Stop(err)
			if errors.Is(parent.Err(), context.Canceled) {
				return commandOutcome{}, ErrCancelled
			}
			if err != nil {
				return commandOutcome{}, newCommandError(exitRuntime, err)
			}
			if report.Recovered {
				warningf(stderr, "network.json was unavailable; ownership was confirmed from the installed interface, launchd plist, and binary digests")
			}
			return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
				for _, path := range report.RemoveFiles {
					bestEffortf(stdout, "remove file %s\n", path)
				}
				for _, path := range report.RemoveDirs {
					bestEffortf(stdout, "rmdir %s\n", path)
				}
				bestEffortf(stdout, "applied: %t\n", report.Applied)
				return nil
			}}, nil
		}
		executor := linuxnet.Executor{User: baseRunner, Root: rootRunner, InUse: deploymentInUse}
		if command == "install" {
			linuxConfig, configErr := linuxnet.ConfigForCIDR(layout.CIDR())
			if configErr != nil {
				return commandOutcome{}, newUsageError(configErr)
			}
			progressItem := startProgress(ctx, stderr, "Installing the Linux fixed-IP network")
			report, err := executor.InstallConfig(ctx, linuxConfig, options.Apply)
			progressItem.Stop(err)
			if errors.Is(parent.Err(), context.Canceled) {
				return commandOutcome{}, ErrCancelled
			}
			if err != nil {
				return commandOutcome{}, newCommandError(exitRuntime, err)
			}
			for _, warning := range report.Warnings {
				warningf(stderr, "%s", warning)
			}
			return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
				for _, directory := range report.Plan.Directories {
					bestEffortf(stdout, "directory %s %s %s\n", directory.Path, directory.Owner, directory.Mode)
				}
				for _, file := range report.Plan.Files {
					bestEffortf(stdout, "file %s %s %s\n", file.Path, file.Owner, file.Mode)
				}
				for _, phase := range report.Plan.Phases {
					bestEffortf(stdout, "phase %s\n", phase.Name)
					for _, action := range phase.Commands {
						bestEffortf(stdout, "  %s\n", execx.Display(action.Binary, action.Args...))
					}
				}
				bestEffortf(stdout, "applied: %t\n", report.Applied)
				if !report.Applied {
					bestEffortln(stdout, "rerun with --yes after reviewing this exact plan; Farrow will request sudo when needed")
					return nil
				}
				return nil
			}}, nil
		}
		progressItem := startProgress(ctx, stderr, "Removing the Linux fixed-IP network")
		report, err := executor.Uninstall(ctx, options.Apply)
		progressItem.Stop(err)
		if errors.Is(parent.Err(), context.Canceled) {
			return commandOutcome{}, ErrCancelled
		}
		if err != nil {
			return commandOutcome{}, newCommandError(exitRuntime, err)
		}
		return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
			for _, path := range report.Plan.RemoveFiles {
				bestEffortf(stdout, "remove file %s\n", path)
			}
			for _, directory := range report.Plan.RemoveDirectories {
				bestEffortf(stdout, "rmdir %s\n", directory)
			}
			bestEffortf(stdout, "applied: %t\n", report.Applied)
			if !report.Applied {
				bestEffortln(stdout, "rerun with --yes after reviewing this exact plan; Farrow will request sudo when needed")
				return nil
			}
			return nil
		}}, nil
	}
	if command != "status" {
		return commandOutcome{}, newUsageError(fmt.Errorf("unknown network action %q", command))
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	cidr := options.CIDR
	if cidr == "" {
		cidr = subnet.DefaultCIDR
	}
	layout, err := subnet.Parse(cidr)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	baseRunner := execx.OSRunner{Timeout: 5 * time.Second, OutputLimit: 1 << 20}
	progressItem := startProgress(ctx, stderr, "Inspecting the host network")
	preflightReport := netpreflight.Run(ctx, netpreflight.Request{OS: runtime.GOOS, Arch: runtime.GOARCH, Purpose: netpreflight.Inspect, Layout: layout}, netpreflight.Probe{Runner: baseRunner})
	installedReadable := preflightReport.Installation.Status == "exact" || preflightReport.Installation.Status == "protected"
	if options.CIDR == "" && installedReadable && preflightReport.Installation.CIDR != "" && preflightReport.Installation.CIDR != layout.CIDR() {
		installedLayout, installedErr := subnet.Parse(preflightReport.Installation.CIDR)
		if installedErr == nil {
			preflightReport = netpreflight.Run(ctx, netpreflight.Request{OS: runtime.GOOS, Arch: runtime.GOARCH, Purpose: netpreflight.Inspect, Layout: installedLayout}, netpreflight.Probe{Runner: baseRunner})
		}
	}
	doctorReport := (doctor.Probe{}).Run(ctx)
	checks := make([]doctor.Check, 0, 4)
	hasError := !preflightReport.Ready
	for _, check := range doctorReport.Checks {
		if check.Name == "kvm" || check.Name == "linux-family" || check.Name == "linux-network-owner" || check.Name == "bridge-helper" {
			checks = append(checks, check)
			if check.Status == doctor.Error {
				hasError = true
			}
		}
	}
	progressItem.Stop(nil)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	result := struct {
		OS        string              `json:"os"`
		Arch      string              `json:"arch"`
		Preflight netpreflight.Report `json:"preflight"`
		Checks    []doctor.Check      `json:"checks,omitempty"`
	}{OS: doctorReport.OS, Arch: doctorReport.Arch, Preflight: preflightReport, Checks: checks}
	renderText := func(stdout, _ io.Writer) error {
		readiness := "ready"
		if !preflightReport.Ready {
			readiness = "not ready"
		}
		textField(stdout, 9, "network", fmt.Sprintf("%s (%s, %s)", preflightReport.CIDR, installationWords(preflightReport.Installation.Status), readiness))
		for _, finding := range preflightReport.Findings {
			line := finding.Evidence
			if finding.Severity == netpreflight.Error && finding.Fix != "" {
				line += "; fix: " + finding.Fix
			}
			bestEffortf(stdout, "[%s] %s\n", finding.Severity, line)
		}
		for _, check := range checks {
			bestEffortf(stdout, "[%s] %s: %s\n", check.Status, check.Name, check.Evidence)
		}
		return nil
	}
	if hasError {
		code := exitCapability
		if preflightReport.ExitCode != 0 {
			code = preflightReport.ExitCode
		}
		return commandOutcome{}, newCommandError(code, errors.New("network status is not ready")).withPayload(result).withText(renderText).quiet()
	}
	return commandOutcome{payload: result, text: renderText}, nil
}

// installationWords turns the preflight installation status into words.
func installationWords(status string) string {
	switch status {
	case "exact", "protected":
		return "installed"
	case "partial", "invalid":
		return "partially installed"
	default:
		return "not installed"
	}
}

const structuredCommandCaptureLimit = 4 << 20

type boundedCapture struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (capture *boundedCapture) Write(data []byte) (int, error) {
	written := len(data)
	remaining := capture.limit - capture.buffer.Len()
	if remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		_, _ = capture.buffer.Write(data[:remaining])
	}
	if remaining < len(data) {
		capture.truncated = true
	}
	return written, nil
}

func (capture *boundedCapture) String() string { return capture.buffer.String() }

type remoteCommandResult struct {
	Command         string   `json:"command"`
	Interactive     bool     `json:"interactive"`
	SessionStream   string   `json:"session_stream,omitempty"`
	Node            string   `json:"node"`
	User            string   `json:"user"`
	Host            string   `json:"host"`
	Port            uint16   `json:"port"`
	Arguments       []string `json:"arguments"`
	Success         bool     `json:"success"`
	ExitCode        int      `json:"exit_code"`
	DurationMS      int64    `json:"duration_ms"`
	Stdout          string   `json:"stdout,omitempty"`
	Stderr          string   `json:"stderr,omitempty"`
	StdoutTruncated bool     `json:"stdout_truncated,omitempty"`
	StderrTruncated bool     `json:"stderr_truncated,omitempty"`
	Error           string   `json:"error,omitempty"`
	AuditError      string   `json:"audit_error,omitempty"`
}

func executeSSHProcess(ctx context.Context, commandName, node, user, host string, port uint16, sshPath string, sshArgs, arguments []string, stdout, stderr io.Writer) (remoteCommandResult, error) {
	result := remoteCommandResult{
		Command: commandName, Node: node, User: user, Host: host, Port: port,
		Arguments: append([]string(nil), arguments...), ExitCode: -1,
	}
	sshCommand := exec.CommandContext(ctx, sshPath, sshArgs...)
	sshCommand.Stdin = os.Stdin
	structured := structuredOutput(stdout)
	interactiveStructured := structured && len(arguments) == 0
	var capturedStdout, capturedStderr boundedCapture
	if interactiveStructured {
		result.Interactive = true
		result.SessionStream = "stderr"
		sshCommand.Stdout = rawWriter(stderr)
		sshCommand.Stderr = rawWriter(stderr)
	} else if structured {
		capturedStdout.limit = structuredCommandCaptureLimit
		capturedStderr.limit = structuredCommandCaptureLimit
		sshCommand.Stdout = &capturedStdout
		sshCommand.Stderr = &capturedStderr
	} else {
		sshCommand.Stdout = rawWriter(stdout)
		sshCommand.Stderr = rawWriter(stderr)
	}
	started := time.Now()
	err := runSSHChild(sshCommand)
	result.DurationMS = time.Since(started).Milliseconds()
	result.Stdout = capturedStdout.String()
	result.Stderr = capturedStderr.String()
	result.StdoutTruncated = capturedStdout.truncated
	result.StderrTruncated = capturedStderr.truncated
	if err == nil {
		result.Success = true
		result.ExitCode = 0
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
	} else {
		result.Error = err.Error()
	}
	return result, err
}

func runSSHChild(command *exec.Cmd) error {
	if err := command.Start(); err != nil {
		return err
	}
	// Start first so the exec'd ssh process receives the terminal's normal
	// signal disposition. While it owns the foreground session, Farrow ignores
	// SIGINT: raw-mode Ctrl-C is an SSH byte, and non-raw Ctrl-C belongs to the
	// child process group rather than local command cancellation. SIGTERM still
	// reaches the command context and CommandContext terminates the child.
	// signal.Ignore drops every earlier Notify registration for SIGINT, and
	// signal.Reset would only restore the default disposition, so the
	// interrupt channel is re-armed explicitly once the child has exited.
	signal.Ignore(os.Interrupt)
	defer signal.Notify(interruptSignals, os.Interrupt)
	return command.Wait()
}

// splitRemoteInvocation separates the optional node selector from the remote
// command. With a literal --, everything before it is the selector and must
// be nothing or one known node, so a misspelled node is a usage error rather
// than a command run on the control node. Without --, the first argument is
// the node only when it names one; anything else is the remote command.
func splitRemoteInvocation(arguments []string, resolved spec.Resolved) (string, []string, bool, error) {
	known := func(name string) bool {
		for _, candidate := range resolved.Nodes {
			if candidate.Name == name {
				return true
			}
		}
		return false
	}
	for index, argument := range arguments {
		if argument != "--" {
			continue
		}
		head, command := arguments[:index], arguments[index+1:]
		switch {
		case len(head) == 0:
			return "", command, false, nil
		case len(head) > 1:
			return "", nil, false, fmt.Errorf("at most one node may precede --, got %s", strings.Join(head, " "))
		case !known(head[0]):
			return "", nil, false, privatevm.UnknownNodeError(head[0], "deployment", resolved.Nodes)
		}
		return head[0], command, false, nil
	}
	if len(arguments) > 0 && known(arguments[0]) {
		return arguments[0], arguments[1:], false, nil
	}
	// A near-miss node name is a typo, not a command to run on the default node.
	if len(arguments) > 0 {
		if suggestion := naming.Closest(arguments[0], sortedNodeNames(resolved)); suggestion != "" {
			return "", nil, false, failure.New(failure.Usage, fmt.Errorf("unknown node %q (did you mean %s?)", arguments[0], suggestion)).Then("to run it on the default node, put -- before it")
		}
	}
	return "", arguments, len(arguments) > 0, nil
}

func remoteCommandText(commandName string, arguments []string) string {
	// Keep ssh's shell syntax and the existing single-string exec shorthand.
	// An exec argv must survive OpenSSH's string-valued request intact.
	if commandName == "exec" && len(arguments) > 1 {
		return vm.QuoteRemote(arguments)
	}
	return strings.Join(arguments, " ")
}

func runPrivateSSH(parent context.Context, commandName string, args []string, resolved spec.Resolved, stdout, stderr io.Writer) (commandOutcome, error) {
	node, command, implicitCommand, err := splitRemoteInvocation(args, resolved)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	if implicitCommand {
		warningf(stderr, "treating %q as a remote command; write farrow %s [node] -- command", args[0], commandName)
	}
	if commandName == "exec" && len(command) == 0 {
		return commandOutcome{}, newUsageError(errors.New("exec requires a remote command: farrow exec [node] -- command [args...]"))
	}
	manager := privatevm.Manager{FarrowVersion: version.Version}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	connection, err := manager.Connection(ctx, node)
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return commandOutcome{}, newCommandError(exitCapability, err)
	}
	sshArgs := vm.SSHArgsForInstance(connection.User, connection.PrivateKey, connection.KnownHosts, connection.HostKeyAlias, connection.Port)
	if sshArgs == nil {
		return commandOutcome{}, newCommandError(exitIntegrity, errors.New("resolved SSH user is unsafe"))
	}
	if len(command) != 0 {
		sshArgs = append(sshArgs, remoteCommandText(commandName, command))
	}
	debugf(stderr, "ssh mode=private node=%s user=%s host=%s port=%d arguments=%d", connection.Node, connection.User, connection.Host, connection.Port, len(command))
	result, runErr := executeSSHProcess(ctx, commandName, connection.Node, connection.User, connection.Host, connection.Port, sshPath, sshArgs, command, stdout, stderr)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	outcome := commandOutcome{payload: result}
	if runErr != nil {
		var exitError *exec.ExitError
		if errors.As(runErr, &exitError) {
			code := exitError.ExitCode()
			return commandOutcome{}, newRemoteExitError(code, fmt.Errorf("remote command exited with status %d", code), result)
		}
		return commandOutcome{}, newRuntimeError(runErr)
	}
	return outcome, nil
}

func runSSH(ctx context.Context, commandName string, args []string, stdout, stderr io.Writer) (commandOutcome, error) {
	resolved, err := currentDeploymentResolved()
	if err != nil {
		return commandOutcome{}, deploymentReadError(err)
	}
	if resolved.Network != "private" {
		return commandOutcome{}, newConflictError(errors.New(legacyDeploymentMessage))
	}
	return runPrivateSSH(ctx, commandName, args, resolved, stdout, stderr)
}

func printProvisionReport(stdout, stderr io.Writer, report provision.Report) {
	textField(stdout, 10, "script", report.Script.Name)
	textField(stdout, 10, "sha256", report.Script.SHA256)
	textField(stdout, 10, "bytes", report.Script.Size)
	textField(stdout, 10, "sudo", report.Sudo)
	textField(stdout, 10, "parallel", report.Parallelism)
	for _, result := range report.Results {
		state := "failed"
		if result.Success {
			state = "success"
		}
		bestEffortf(stdout, "%-16s %s  exit=%d  duration=%dms\n", result.Node, statusValue(stdout, state), result.ExitCode, result.DurationMS)
		if result.Stdout != "" {
			bestEffortf(stdout, "--- %s stdout ---\n%s", result.Node, result.Stdout)
			if !strings.HasSuffix(result.Stdout, "\n") {
				bestEffortln(stdout)
			}
		}
		if result.Stderr != "" {
			bestEffortf(stderr, "--- %s stderr ---\n%s", result.Node, result.Stderr)
			if !strings.HasSuffix(result.Stderr, "\n") {
				bestEffortln(stderr)
			}
		}
		if result.StdoutTruncated {
			bestEffortf(stderr, "[%s] stdout was truncated at the bounded capture limit\n", result.Node)
		}
		if result.StderrTruncated {
			bestEffortf(stderr, "[%s] stderr was truncated at the bounded capture limit\n", result.Node)
		}
		if result.Error != "" {
			bestEffortf(stderr, "[%s] %s\n", result.Node, result.Error)
		}
	}
	bestEffortf(stdout, "%s %d successful, %d failed, %dms total\n", styled(stdout, ansiBold, "provisioned"), report.Successful, report.Failed, report.DurationMS)
}

func provisionConnectionExit(err error) int {
	var artifactError *sshkeys.SSHArtifactError
	if errors.As(err, &artifactError) {
		return exitIntegrity
	}
	return exitRuntime
}

// purgeDeployment removes what destroy deliberately preserves: keys and the
// deployment state document. Images stay cached; persistent disks were already
// deleted by the forced --purge destroy.
func purgeDeployment(ctx context.Context) error {
	manager := privatevm.Manager{FarrowVersion: version.Version}
	if _, err := manager.PurgeKeys(ctx, true); err != nil {
		return err
	}
	root, err := state.ResolveDataRoot()
	if err != nil {
		return err
	}
	for _, name := range []string{"state.json", "events.jsonl"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, directory := range []string{"nodes", "disks", "guest-warnings"} {
		if err := os.Remove(filepath.Join(root, directory)); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			return err
		}
	}
	return nil
}

// deploymentInUse refuses privileged network teardown while any recorded
// node of the single deployment is provably (or ambiguously) live.
func deploymentInUse(ctx context.Context) error {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return nil
	}
	store := state.Store{Root: root}
	entries, err := os.ReadDir(filepath.Join(root, "nodes"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	audit := privatevm.RuntimeIdentityAuditor(execx.OSRunner{Timeout: 15 * time.Second, OutputLimit: 1 << 20}, time.Second)
	for _, entry := range entries {
		node, readErr := store.ReadNode(entry.Name())
		if readErr != nil {
			continue
		}
		observation, auditErr := audit(ctx, node)
		if auditErr != nil {
			return fmt.Errorf("node %s runtime audit is inconclusive (%v); stop the deployment first", entry.Name(), auditErr)
		}
		if observation.Live {
			return fmt.Errorf("node %s is running; stop the deployment first", node.Node)
		}
	}
	return nil
}

// withoutRecordedAddresses drops addresses that belong to this deployment's
// own recorded nodes, so preflight does not report a running lab as a
// conflict.
func withoutRecordedAddresses(addresses []string) []string {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return addresses
	}
	deploymentState, err := (state.Store{Root: root}).ReadDeployment()
	if err != nil {
		return addresses
	}
	recorded := make(map[string]struct{}, len(deploymentState.Resolved.Nodes))
	for _, node := range deploymentState.Resolved.Nodes {
		recorded[node.Address] = struct{}{}
	}
	filtered := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if _, mine := recorded[address]; mine {
			continue
		}
		filtered = append(filtered, address)
	}
	return filtered
}

type provisionOptions struct {
	ScriptPath  string
	Sudo        bool
	Parallelism int
	Timeout     time.Duration
}

func runProvision(parent context.Context, options provisionOptions, nodes []string, stderr io.Writer) (commandOutcome, error) {
	if options.ScriptPath == "" {
		return commandOutcome{}, newUsageError(errors.New("--script is required"))
	}
	if options.Parallelism < 1 || options.Parallelism > provision.MaxParallelism {
		return commandOutcome{}, newUsageError(fmt.Errorf("provision parallelism must be 1..%d", provision.MaxParallelism))
	}
	if options.Timeout <= 0 || options.Timeout > 24*time.Hour {
		return commandOutcome{}, newUsageError(errors.New("provision timeout must be greater than zero and no more than 24h"))
	}
	script, err := provision.LoadScript(options.ScriptPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return commandOutcome{}, newUsageError(err)
		}
		return commandOutcome{}, newCommandError(exitUsage, err)
	}
	operationID, err := identity.NewUUID()
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	deployment, err := privatevm.Open()
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	ctx, cancel := context.WithTimeout(parent, options.Timeout)
	defer cancel()
	deploymentLock, err := privatevm.AcquireLock(ctx, deployment, false)
	if err != nil {
		return commandOutcome{}, newConflictError(err)
	}
	locked := true
	defer func() {
		if locked {
			_ = deploymentLock.Release()
		}
	}()
	deploymentState, err := (state.Store{Root: deployment.Root}).ReadDeployment()
	if err != nil {
		return commandOutcome{}, newConflictError(fmt.Errorf("provision requires an existing running deployment: %w", err))
	}
	resolved := deploymentState.Resolved

	targets := make([]provision.Target, 0)
	selectedNames := make([]string, 0)
	var recordEvent func(context.Context, string, string, string) error
	if resolved.Network == "private" {
		manager := privatevm.Manager{FarrowVersion: version.Version, OperationID: operationID, Nodes: append([]string(nil), nodes...)}
		connections, connectionErr := manager.ConnectionsLocked(ctx, deployment, deploymentLock)
		if connectionErr != nil {
			return commandOutcome{}, newCommandError(provisionConnectionExit(connectionErr), connectionErr)
		}
		for _, connection := range connections {
			if connection.Host != "127.0.0.1" {
				return commandOutcome{}, newCommandError(exitIntegrity, fmt.Errorf("refuse non-loopback provision endpoint for node %s", connection.Node))
			}
			targets = append(targets, provision.Target{Node: connection.Node, User: connection.User, Port: connection.Port, PrivateKey: connection.PrivateKey, KnownHosts: connection.KnownHosts, HostKeyAlias: connection.HostKeyAlias})
			selectedNames = append(selectedNames, connection.Node)
		}
		recordEvent = manager.RecordEvent
	} else {
		return commandOutcome{}, newCommandError(exitConflict, fmt.Errorf("unsupported deployment network %q", resolved.Network))
	}

	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return commandOutcome{}, newCommandError(exitCapability, err)
	}
	sshPath, err = filepath.Abs(sshPath)
	if err != nil {
		return commandOutcome{}, newCommandError(exitCapability, err)
	}
	startMessage := fmt.Sprintf("script_sha256=%s bytes=%d sudo=%t parallel=%d targets=%s", script.SHA256, script.Size, options.Sudo, options.Parallelism, strings.Join(selectedNames, ","))
	if err := recordEvent(ctx, "provision", "info", "starting "+startMessage); err != nil {
		return commandOutcome{}, newCommandError(exitRuntime, fmt.Errorf("refuse provision without an auditable event append: %w", err)).withOperation(operationID)
	}
	debugf(stderr, "provision operation_id=%s targets=%s timeout=%s parallel=%d sudo=%t script_sha256=%s", operationID, strings.Join(selectedNames, ","), options.Timeout, options.Parallelism, options.Sudo, script.SHA256)
	progressItem := startProgress(ctx, stderr, fmt.Sprintf("Provisioning %d node(s)", len(targets)))
	report, err := (provision.Executor{
		Runner: provision.SSHRunner{SSHPath: sshPath}, Parallelism: options.Parallelism, OperationID: operationID,
	}).Execute(ctx, script, targets, options.Sudo)
	progressItem.Stop(err)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	resultParts := make([]string, 0, len(report.Results))
	for _, result := range report.Results {
		resultParts = append(resultParts, fmt.Sprintf("%s=exit:%d,duration:%dms", result.Node, result.ExitCode, result.DurationMS))
	}
	level := "info"
	if report.Failed != 0 {
		level = "error"
	}
	auditContext, auditCancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	auditErr := recordEvent(auditContext, "provision", level, fmt.Sprintf("completed script_sha256=%s successful=%d failed=%d results=%s", script.SHA256, report.Successful, report.Failed, strings.Join(resultParts, ";")))
	auditCancel()
	if auditErr != nil {
		report.AuditError = "remote execution completed but its audit event could not be appended: " + auditErr.Error()
	}
	releaseErr := deploymentLock.Release()
	locked = false
	if releaseErr != nil {
		if report.AuditError != "" {
			report.AuditError += "; "
		}
		report.AuditError += "release deployment lock: " + releaseErr.Error()
	}
	renderText := func(stdout, stderr io.Writer) error { printProvisionReport(stdout, stderr, report); return nil }
	outcome := commandOutcome{payload: report, text: renderText}
	if report.AuditError != "" {
		return commandOutcome{}, newCommandError(exitRuntime, errors.New(report.AuditError)).withOperation(operationID).withPayload(report).withText(renderText)
	}
	if report.Failed == 0 {
		return outcome, nil
	}
	if report.Successful > 0 {
		return commandOutcome{}, newCommandError(exitPartial, errors.New("provision partially failed")).withPayload(report).withText(renderText).quiet()
	}
	if len(report.Results) == 1 {
		code := report.Results[0].ExitCode
		if code > 0 && code != 255 {
			return commandOutcome{}, newRemoteExitError(code, fmt.Errorf("remote provision exited with status %d", code), report).withText(renderText)
		}
	}
	return commandOutcome{}, newCommandError(exitRuntime, errors.New("provision failed")).withPayload(report).withText(renderText).quiet()
}

func lifecycleReadsConfig(command string) bool {
	return command == "up" || command == "plan" || command == "recreate" || command == "reload"
}

func loadLifecycleConfig(command, configPath string) (spec.Resolved, string, error) {
	if !lifecycleReadsConfig(command) {
		return spec.Resolved{}, "", nil
	}
	file, path, err := config.Discover("", configPath)
	if errors.Is(err, config.ErrNoConfig) {
		return spec.Resolved{}, "", nil
	}
	if err != nil {
		return spec.Resolved{}, "", err
	}
	resolved, err := file.Resolve()
	if err != nil {
		return spec.Resolved{}, "", err
	}
	return resolved, path, nil
}

func currentDeploymentResolved() (spec.Resolved, error) {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return spec.Resolved{}, err
	}
	deploymentState, err := (state.Store{Root: root}).ReadDeployment()
	if err != nil {
		return spec.Resolved{}, fmt.Errorf("read deployment state %s: %w", filepath.Join(root, "state.json"), err)
	}
	return deploymentState.Resolved, nil
}

func deploymentReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return newCommandError(exitConflict, errNoDeployment)
	}
	return newCommandError(exitIntegrity, err)
}

// catalogOrigin names where the active catalog came from in words a user
// recognizes: the built-in copy, a repository host, or a local file.
func catalogOrigin(source string) string {
	switch {
	case source == "embedded", source == "":
		return "built-in"
	case strings.HasPrefix(source, "local:"):
		return "local"
	}
	if parsed, err := url.Parse(source); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return source
}

func printPrivateStatus(out io.Writer, status privatevm.Status) {
	printStatusTable(out, status, true)
}

func printStatusTable(out io.Writer, status privatevm.Status, errors bool) {
	if len(status.Nodes) != 0 {
		rows := make([][]string, 0, len(status.Nodes))
		for _, node := range status.Nodes {
			phase := string(node.State)
			if node.Error != "" {
				phase += " (error)"
			} else if len(node.Warnings) > 0 && node.State == state.Running {
				phase += " (limited)"
			} else if node.Runtime != "" && node.Runtime != phase && node.Runtime != "inactive" {
				phase += " (" + node.Runtime + ")"
			}
			if node.Accel == "tcg" {
				phase += " (tcg/" + node.GuestArch + ")"
			}
			rows = append(rows, []string{node.Name, phase, node.Address, node.Image, fmt.Sprint(node.CPUs), fmt.Sprintf("%.1f GiB", float64(node.Memory)/float64(spec.GiB))})
		}
		printTable(out, []string{"NAME", "STATE", "ADDRESS", "IMAGE", "CPU", "MEMORY"}, rows, 1)
		for _, node := range status.Nodes {
			if errors && node.Error != "" {
				bestEffortf(out, "%s: %s\n", node.Name, node.Error)
			}
			if verboseOutput(out) {
				bestEffortf(out, "%s: SSH %s:%d, %s/%s, PID %d\n", node.Name, node.SSHHost, node.SSHPort, node.GuestArch, node.Accel, node.ProcessID)
			}
		}
	}
	if status.Message != "" {
		bestEffortln(out, status.Message)
	}
	if status.Note != "" {
		textField(out, 6, "note", status.Note)
	}
}

// Only a successful SSH readiness probe justifies a ready label.
func allNodesReady(status privatevm.Status) bool {
	if len(status.Nodes) == 0 {
		return false
	}
	for _, node := range status.Nodes {
		if !node.Ready || node.Error != "" || node.State != state.Running || (node.Runtime != "" && node.Runtime != "running") {
			return false
		}
	}
	return true
}

type persistentDeleteError struct{ err error }

func (e *persistentDeleteError) Error() string { return "delete persistent disks: " + e.err.Error() }
func (e *persistentDeleteError) Unwrap() error { return e.err }

type lifecyclePartialFailure struct {
	commandFailure
	Failures   []privatevm.NodeFailure `json:"failures"`
	RolledBack []string                `json:"rolled_back,omitempty"`
}

type sshConfigReconciler interface {
	InstallSSHConfig(context.Context, string, string) (sshconfig.Result, error)
	RemoveSSHConfig(string, string) (sshconfig.Result, error)
}

func lifecycleSSHConfigAction(command string, deploymentHasNodes bool) string {
	switch command {
	case "up", "start", "restart", "reload", "recreate":
		return "install"
	case "destroy", "purge":
		if deploymentHasNodes {
			return "install"
		}
		return "remove"
	default:
		return ""
	}
}

func destroyLeavesDeploymentNodes(resolved spec.Resolved, selected []string) bool {
	return len(selected) != 0 && len(selected) < len(resolved.Nodes)
}

func reconcileLifecycleSSHConfig(ctx context.Context, command string, deploymentHasNodes bool, reconciler sshConfigReconciler) (*sshconfig.Result, error) {
	switch lifecycleSSHConfigAction(command, deploymentHasNodes) {
	case "install":
		result, err := reconciler.InstallSSHConfig(ctx, "farrow", "")
		return &result, err
	case "remove":
		result, err := reconciler.RemoveSSHConfig("farrow", "")
		return &result, err
	default:
		return nil, nil
	}
}

func fullDeploymentSSHManager(manager privatevm.Manager) privatevm.Manager {
	manager.Nodes = nil
	return manager
}

func classifyPrivateLifecycleError(err error, operationID string) error {
	boundary := newCommandError(exitRuntime, err).withOperation(operationID)
	var preflight *privatevm.NetworkPreflightError
	if errors.As(err, &preflight) {
		boundary.withPayload(preflight.Report)
	}
	if partial := privatevm.IsolatedPartial(err); partial != nil {
		boundary.withPayload(lifecyclePartialFailure{commandFailure: boundary.failure, Failures: partial.Failures, RolledBack: partial.RolledBack})
	}
	return boundary
}

type lifecycleResult struct {
	retryOptions lifecycleRetryOptions
	Source       string `json:"source,omitempty"`
	Error        string `json:"error,omitempty"`
	privatevm.Status
	SSHConfig  *sshconfig.Result       `json:"ssh_config,omitempty"`
	Warnings   []lifecycleWarning      `json:"warnings,omitempty"`
	Failures   []privatevm.NodeFailure `json:"failures,omitempty"`
	RolledBack []string                `json:"rolled_back,omitempty"`
}

func runPrivateCommand(parent context.Context, command string, resolved spec.Resolved, nodes []string, repository, source string, force, deletePersistent, purge, noWait, rollback bool, stderr io.Writer) (commandOutcome, error) {
	retryOptions := lifecycleRetryOptions{Repository: repository, NoWait: noWait, Rollback: rollback}
	if command == "start" || command == "restart" {
		// Resume an interrupted start without changing its guest-convergence
		// semantics, or stopping successful peers for a second time.
		retryOptions.Action = "start"
	}
	operationID := ""
	if command != "status" && command != "plan" {
		var err error
		parent, operationID, err = operationContext(parent)
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
	}
	var progressItem *progress
	manager := privatevm.Manager{FarrowVersion: version.Version, OperationID: operationID, Repository: repository, NoWait: noWait, RollbackFailed: rollback, Nodes: append([]string(nil), nodes...)}
	manager.Progress = deferredProgressReporter(&progressItem)
	if command == "plan" {
		ctx, cancel := context.WithTimeout(parent, time.Minute)
		defer cancel()
		plan, err := manager.Plan(ctx, resolved)
		if err != nil {
			return commandOutcome{}, classifyPrivateLifecycleError(err, operationID)
		}
		plan.Source = source
		applyCommand := func(command string) string {
			if source != "" && source != "applied deployment state" {
				command += " -f " + shellQuote(source)
			}
			return command
		}
		return commandOutcome{payload: plan, text: func(stdout, _ io.Writer) error {
			textField(stdout, 12, "images", strings.Join(plan.Images, ", "))
			textField(stdout, 12, "resources", fmt.Sprintf("%d vCPU, %.1f GiB RAM, %.1f GiB virtual disk capacity", plan.CPUs, float64(plan.Memory)/float64(spec.GiB), float64(plan.Storage)/float64(spec.GiB)))
			for _, change := range plan.Changes {
				textField(stdout, 12, "change", change)
			}
			if plan.Blocked != "" {
				textField(stdout, 12, "blocked", plan.Blocked)
			}
			textField(stdout, 12, "checks", "host capabilities and free addresses are checked by farrow up")
			switch plan.Action {
			case "none":
				bestEffortf(stdout, "no changes: %d node(s) (%s) match the applied inventory\n", len(plan.Nodes), strings.Join(plan.Nodes, ", "))
				return nil
			case "repair":
				textField(stdout, 12, "repair", strings.Join(plan.Nodes, ", ")+"  (state was left mid-transition; run: farrow status)")
			}
			if len(plan.Start) != 0 {
				textField(stdout, 12, "start", strings.Join(plan.Start, ", ")+"  (apply: "+applyCommand("farrow up")+")")
			}
			if len(plan.Create) != 0 {
				textField(stdout, 12, "create", strings.Join(plan.Create, ", ")+"  (apply: "+applyCommand("farrow up")+")")
			}
			if len(plan.Recreate) != 0 {
				detail := strings.Join(plan.Recreate, ", ")
				if plan.Blocked == "" {
					detail += "  (apply: " + applyCommand("farrow recreate "+strings.Join(plan.Recreate, " ")) + ")"
				}
				textField(stdout, 12, "recreate", detail)
			} else if plan.Action == "recreate" && plan.Blocked == "" {
				textField(stdout, 12, "recreate", "deployment-level settings changed  (apply: "+applyCommand("farrow recreate")+")")
			}
			if len(plan.Missing) != 0 {
				textField(stdout, 12, "missing", strings.Join(plan.Missing, ", ")+"  (not in the inventory; remove with: farrow destroy "+strings.Join(plan.Missing, " ")+")")
			}
			if plan.Destructive {
				textField(stdout, 12, "disks", "root and non-persistent data disks will be replaced; persistent data disks are retained")
			}
			return nil
		}}, nil
	}
	timeout := 15 * time.Second
	var operation func(context.Context) (privatevm.Status, error)
	switch command {
	case "up":
		timeout = 0
		operation = func(ctx context.Context) (privatevm.Status, error) { return manager.Up(ctx, resolved) }
	case "start":
		timeout, operation = withReadinessTimeout(10*time.Minute, resolved), manager.Start
	case "stop":
		timeout, operation = 5*time.Minute, manager.Stop
	case "restart":
		timeout, operation = withReadinessTimeout(15*time.Minute, resolved), manager.Restart
	case "reload":
		timeout = 0
		operation = func(ctx context.Context) (privatevm.Status, error) { return manager.Reload(ctx, resolved) }
	case "recreate":
		if !force {
			names := nodes
			if len(names) == 0 {
				for _, node := range resolved.Nodes {
					names = append(names, node.Name)
				}
			}
			bestEffortf(stderr, "recreate: %s; root and non-persistent data disks will be replaced; persistent data disks stay\n", strings.Join(names, ", "))
		}
		if err := confirmCLIAction(force, "recreate", stderr); err != nil {
			if errors.Is(err, ErrCancelled) {
				return commandOutcome{}, err
			}
			return commandOutcome{}, newUsageError(err)
		}
		timeout = 0
		operation = func(ctx context.Context) (privatevm.Status, error) { return manager.RecreateResolved(ctx, resolved) }
	case "status":
		operation = manager.Status
	case "destroy", "purge":
		if command == "purge" {
			if len(nodes) != 0 {
				return commandOutcome{}, newUsageError(errors.New("purge does not accept node selectors"))
			}
			deletePersistent, purge = true, true
		} else {
			if len(nodes) != 0 && (deletePersistent || purge) {
				return commandOutcome{}, newUsageError(errors.New("--delete-persistent and --purge apply to whole-deployment destroy only"))
			}
			if !force {
				// State the exact scope before the typed confirmation.
				bestEffortln(stderr, destroyScope(resolved, nodes, deletePersistent, purge))
			}
			if err := confirmCLIAction(force, "destroy", stderr); err != nil {
				if errors.Is(err, ErrCancelled) {
					return commandOutcome{}, err
				}
				return commandOutcome{}, newUsageError(err)
			}
		}
		timeout = 10 * time.Minute
		operation = func(ctx context.Context) (privatevm.Status, error) {
			if len(nodes) != 0 {
				return manager.DestroyNodes(ctx)
			}
			status, err := manager.Destroy(ctx)
			if err != nil || !deletePersistent {
				return status, err
			}
			deleted, err := manager.DeletePersistent(ctx)
			if err != nil {
				return status, &persistentDeleteError{err: err}
			}
			status.Message = fmt.Sprintf("destroyed node artifacts; explicitly deleted %d persistent data disk(s); image cache and keys preserved", len(deleted))
			return status, nil
		}
	default:
		return commandOutcome{}, newUsageError(fmt.Errorf("unsupported private command %q", command))
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	} else {
		// Downloads bound inactivity themselves; each guest has its own readiness deadline.
		ctx, cancel = context.WithCancel(parent)
	}
	defer cancel()
	debugf(stderr, "lifecycle=%s mode=private timeout=%s operation_id=%s nodes=%d", command, timeout, operationID, len(resolved.Nodes))
	if command != "status" {
		progressItem = startProgress(ctx, stderr, lifecycleMessage(command))
	}
	status, err := operation(ctx)
	status.OperationID = operationID
	if errors.Is(parent.Err(), context.Canceled) {
		progressItem.Stop(ErrCancelled)
		if startupCommand(command) {
			textField(stderr, 10, "resume", lifecycleRetryCommand(source, nodes, retryOptions))
		}
		if command != "status" {
			recordCancelledLifecycle(parent, manager, command, status, err, stderr)
		}
		return commandOutcome{}, ErrCancelled
	}
	lifecycleSucceeded := err == nil
	deploymentHasNodes := command != "purge" && (command != "destroy" || destroyLeavesDeploymentNodes(resolved, nodes))
	if noWait && err == nil {
		status.Message = strings.TrimSpace(status.Message + "; guest readiness and metadata refresh skipped (--no-wait)")
	}
	reconciledSSHConfig, warnings := finishLifecycleIntegrations(ctx, command, deploymentHasNodes, noWait,
		lifecycleRetryCommand(source, nodes, retryOptions), status, err, fullDeploymentSSHManager(manager), deferredProgressReporter(&progressItem))
	if lifecycleSucceeded && (command == "purge" || command == "destroy" && purge) {
		if purgeErr := purgeDeployment(ctx); purgeErr != nil {
			if err == nil {
				err = fmt.Errorf("destroy succeeded but purge failed: %w", purgeErr)
			} else {
				err = fmt.Errorf("%v; deployment purge also failed: %w", err, purgeErr)
			}
		} else {
			status.Message = "destroyed node artifacts and persistent data disks; purged deployment keys and state; images remain cached"
		}
	}
	if errors.Is(parent.Err(), context.Canceled) {
		progressItem.Stop(ErrCancelled)
		if startupCommand(command) {
			textField(stderr, 10, "resume", lifecycleRetryCommand(source, nodes, retryOptions))
		}
		if command != "status" {
			recordCancelledLifecycle(parent, manager, command, status, err, stderr)
		}
		return commandOutcome{}, ErrCancelled
	}
	progressItem.Stop(err)
	if err != nil {
		var partial *privatevm.PartialError
		isPartial := errors.As(err, &partial)
		if isPartial || len(status.Nodes) != 0 && command == "status" {
			result := lifecycleResult{retryOptions: retryOptions, Source: source, Error: "partial", Status: status, SSHConfig: reconciledSSHConfig, Warnings: warnings}
			result.Message = err.Error()
			if isPartial {
				result.Failures = partial.Failures
				result.RolledBack = partial.RolledBack
			}
			failure := newCommandError(exitPartial, err).withOperation(operationID).withPayload(result).withText(func(stdout, _ io.Writer) error {
				printLifecycleResult(stdout, command, result, noWait)
				return nil
			})
			if isPartial {
				// The node table carries the detail; stderr keeps one line.
				failure.failure.Message = fmt.Sprintf("%d of %d node(s) failed", len(partial.Failures), partial.Total)
			}
			return commandOutcome{}, failure
		}
		return commandOutcome{}, classifyPrivateLifecycleError(err, operationID)
	}
	if verboseOutput(stderr) && (command == "up" || command == "reload") {
		suggestHostsPublication(resolved, stderr)
	}
	if command == "up" || command == "reload" || command == "start" || command == "restart" || command == "recreate" {
		seen := make(map[string]struct{})
		guestArch := lifecycleImageArch(resolved)
		if service, serviceErr := imageService(repository, false, nil); serviceErr == nil {
			if session, sessionErr := service.OpenCatalog(); sessionErr == nil {
				for _, node := range resolved.Nodes {
					alias := node.Image
					if alias == "" {
						alias = resolved.Image
					}
					if _, duplicate := seen[alias]; duplicate {
						continue
					}
					seen[alias] = struct{}{}
					if entry, entryErr := session.LookupArch(ctx, alias, guestArch); entryErr == nil {
						printImageStatusWarning(stderr, entry)
					}
				}
			}
		}
	}
	result := lifecycleResult{retryOptions: retryOptions, Source: source, Status: status, SSHConfig: reconciledSSHConfig, Warnings: warnings}
	return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
		printLifecycleResult(stdout, command, result, noWait)
		return nil
	}}, nil
}

// recordCancelledLifecycle keeps the audit trail honest after an interrupt:
// the engine may have changed node state before the signal landed, so the
// event is appended on a context that outlives the cancellation, exactly as
// provision does for its completion event.
func recordCancelledLifecycle(parent context.Context, manager privatevm.Manager, command string, status privatevm.Status, err error, stderr io.Writer) {
	message := "cancelled by signal"
	if err != nil {
		message += ": " + err.Error()
	} else if status.Message != "" {
		message += " after " + status.Message
	}
	audit, cancelAudit := context.WithTimeout(context.WithoutCancel(parent), lifecycleEventTimeout)
	defer cancelAudit()
	if eventErr := manager.RecordEvent(audit, command, "error", message); eventErr != nil {
		warningf(stderr, "cancelled %s could not be recorded in events.jsonl: %v", command, eventErr)
	}
}

// runPurgeCommand is the no-confirmation, whole-deployment disposal path. A
// valid deployment goes through the normal identity-checked destroy engine.
// When an earlier whole destroy already removed state.json, the residual path
// can still delete strictly inventoried persistent disks and allowlisted keys.
// A missing state document never authorizes deletion of retained node
// artifacts, whose process and path identity can no longer be proven.
func runPurgeCommand(parent context.Context, stderr io.Writer) (commandOutcome, error) {
	resolved, err := currentDeploymentResolved()
	if err == nil {
		if resolved.Network != "private" {
			return commandOutcome{}, newConflictError(errors.New(legacyDeploymentMessage))
		}
		return runPrivateCommand(parent, "purge", resolved, nil, "", "applied deployment state", true, true, true, false, false, stderr)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return commandOutcome{}, newCommandError(exitIntegrity, fmt.Errorf("read deployment state for purge: %w", err))
	}

	root, rootErr := state.ResolveDataRoot()
	if rootErr != nil {
		return commandOutcome{}, newCommandError(exitIntegrity, rootErr)
	}
	rootInfo, statErr := os.Lstat(root)
	rootExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return commandOutcome{}, newCommandError(exitRuntime, fmt.Errorf("inspect deployment root for purge: %w", statErr))
	}
	if rootExists {
		stat, ok := rootInfo.Sys().(*syscall.Stat_t)
		if !ok || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm()&0o022 != 0 || int(stat.Uid) != os.Geteuid() {
			return commandOutcome{}, newCommandError(exitIntegrity, fmt.Errorf("deployment root is not a current-user directory protected from group/world writes: %s", root))
		}
	} else {
		status := privatevm.Status{Message: "no deployment to purge; image cache and host network remain installed"}
		result := lifecycleResult{Status: status}
		return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
			printPrivateStatus(stdout, status)
			return nil
		}}, nil
	}

	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	manager := privatevm.Manager{FarrowVersion: version.Version}
	deleted, deleteErr := manager.DeletePersistent(ctx)
	if deleteErr != nil {
		return commandOutcome{}, newCommandError(exitIntegrity, fmt.Errorf("deployment state is missing; refuse unsafe residual purge: %w", deleteErr))
	}
	if purgeErr := purgeDeployment(ctx); purgeErr != nil {
		return commandOutcome{}, newCommandError(exitRuntime, fmt.Errorf("purge residual deployment data: %w", purgeErr))
	}

	sshResult, sshErr := (privatevm.Manager{FarrowVersion: version.Version}).RemoveSSHConfig("farrow", "")
	status := privatevm.Status{Message: fmt.Sprintf("purged residual deployment data and %d persistent data disk(s); image cache and host network remain installed", len(deleted))}
	result := lifecycleResult{Status: status, SSHConfig: &sshResult}
	if sshErr != nil {
		result.Warnings = append(result.Warnings, sshIntegrationWarning("remove", sshErr))
	}
	return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
		printLifecycleResult(stdout, "purge", result, false)
		if sshResult.Changed {
			textField(stdout, 6, "ssh", fmt.Sprintf("%s %s", sshResult.Action, sshResult.Fragment))
		}
		return nil
	}}, nil
}

func runLifecycleCommand(ctx context.Context, command string, options lifecycleOptions, nodes []string, stderr io.Writer) (_ commandOutcome, returnErr error) {
	if command != "status" && command != "plan" {
		var id string
		var err error
		ctx, id, err = operationContext(ctx)
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		recordOperationPhase(ctx, id, command, "begin", nil, stderr)
		defer func() {
			if failure, ok := returnErr.(interface{ setOperationID(string) }); ok {
				failure.setOperationID(id)
			}
			if returnErr != nil || !(command == "purge" || command == "destroy" && options.Purge) {
				recordOperationPhase(ctx, id, command, "complete", returnErr, stderr)
			}
		}()
		progressItem := startProgress(ctx, stderr, lifecycleMessage(command))
		defer func() { progressItem.Stop(returnErr) }()
	}
	if (options.DeletePersistent || options.Purge) && !options.Force && !term.IsTerminal(int(os.Stdin.Fd())) {
		// On a terminal the interactive destroy confirmation covers the
		// widened scope; without one, automation must state --force.
		return commandOutcome{}, newUsageError(errors.New("--delete-persistent and --purge require the separate --force destroy confirmation"))
	}
	if options.Purge {
		// A purge is a terminal disposal; retained persistent disks make no
		// sense past it.
		options.DeletePersistent = true
	}
	repository := options.Repository
	if command == "plan" || command == "up" || command == "reload" || command == "recreate" {
		var repositoryErr error
		repository, _, repositoryErr = image.ResolveRepository(options.Repository, options.Mirror)
		if repositoryErr != nil {
			return commandOutcome{}, newUsageError(repositoryErr)
		}
	}
	resolvedFile, source, err := loadLifecycleConfig(command, options.ConfigPath)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	hasConfig := source != ""
	persisted, persistedErr := currentDeploymentResolved()
	if persistedErr != nil && !errors.Is(persistedErr, os.ErrNotExist) {
		return commandOutcome{}, deploymentReadError(persistedErr)
	}
	switch {
	case persistedErr == nil && persisted.Network == "private":
		if !hasConfig {
			resolvedFile = persisted
			hasConfig = true
		}
	case persistedErr == nil && persisted.Network == "user":
		return commandOutcome{}, newConflictError(errors.New(legacyDeploymentMessage))
	}
	if !hasConfig && command == "up" && interactiveTextSession(stderr) {
		if _, setupErr := runSetupCommand(ctx, "", implicitSetupOptions(options, repository), outputFormatFor(stderr), verboseOutput(stderr), stderr); setupErr != nil {
			return commandOutcome{}, setupErr
		}
		resolvedFile, source, err = loadLifecycleConfig(command, options.ConfigPath)
		if err != nil {
			return commandOutcome{}, newUsageError(err)
		}
		hasConfig = source != ""
	}
	if !hasConfig {
		switch {
		case command == "destroy" && len(nodes) == 0:
			// Like purge, destroying nothing is already done.
			status := privatevm.Status{Message: "no deployment to destroy"}
			return commandOutcome{payload: lifecycleResult{Status: status}, text: func(stdout, _ io.Writer) error {
				printPrivateStatus(stdout, status)
				return nil
			}}, nil
		case !lifecycleReadsConfig(command):
			return commandOutcome{}, newCommandError(exitConflict, errNoDeployment)
		case command == "up":
			// Only a terminal session writes the default inventory itself.
			return commandOutcome{}, newCommandError(exitUsage, failure.WithNext(config.ErrNoConfig, "farrow setup --yes, then farrow up; or pass -f <file>"))
		default:
			return commandOutcome{}, newCommandError(exitUsage, config.ErrNoConfig)
		}
	}
	// Selectors are checked against the same specification the engine will
	// use, before host preflight, confirmation prompts, or any change.
	selectorSource := "inventory"
	if source == "" {
		selectorSource = "deployment"
	}
	if err := validateNodeSelectors(resolvedFile, nodes, selectorSource); err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	if source == "" {
		source = "applied deployment state"
	}
	if lifecycleReadsConfig(command) && !structuredOutput(stderr) && (verboseOutput(stderr) || source == "applied deployment state") {
		textField(stderr, 10, "inventory", source)
	}
	printWarnings(stderr, configurationWarnings(resolvedFile))
	outcome, err := runPrivateCommand(ctx, command, resolvedFile, nodes, repository, source, options.Force, options.DeletePersistent, options.Purge, options.NoWait, options.Rollback, stderr)
	canPrepareHost := needsHostSetup(command, err)
	var networkFailure *privatevm.NetworkPreflightError
	if !canPrepareHost && command == "up" && errors.As(err, &networkFailure) && errors.Is(persistedErr, os.ErrNotExist) && options.ConfigPath == "" {
		candidate := recognizeDefaultSetupTemplate(setupSelection{ConfigPath: source})
		canPrepareHost = candidate.Generated
	}
	if canPrepareHost && interactiveTextSession(stderr) {
		// Supply missing host dependencies or repair the owned network, then
		// retry the original operation with the same inventory and selectors.
		bestEffortf(stderr, "%s Preparing the host before starting nodes\n", styled(stderr, ansiCyan, "→"))
		setupOptions := implicitSetupOptions(options, repository)
		if source == "applied deployment state" {
			setupOptions.Applied = &resolvedFile
		}
		if _, setupErr := runSetupCommand(ctx, "", setupOptions, outputFormatFor(stderr), verboseOutput(stderr), stderr); setupErr != nil {
			return commandOutcome{}, setupErr
		}
		if source != "applied deployment state" {
			resolvedFile, source, err = loadLifecycleConfig(command, options.ConfigPath)
			if err != nil {
				return commandOutcome{}, newUsageError(err)
			}
		}
		outcome, err = runPrivateCommand(ctx, command, resolvedFile, nodes, repository, source, options.Force, options.DeletePersistent, options.Purge, options.NoWait, options.Rollback, stderr)
	}
	return outcome, err
}

func implicitSetupOptions(options lifecycleOptions, repository string) setupCLIOptions {
	return setupCLIOptions{FilePath: options.ConfigPath, Mode: "host", Repo: repository, Yes: true}
}

// Setup can supply missing executables or restore an intact Farrow network.
// Destructive commands keep their explicit confirmation and are never replayed.
func needsHostSetup(command string, err error) bool {
	switch command {
	case "up", "start", "restart", "reload":
	default:
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	var preflight *privatevm.NetworkPreflightError
	if !errors.As(err, &preflight) {
		return false
	}
	if preflight.Report.CanRepair() {
		return true
	}
	for _, finding := range preflight.Report.Findings {
		if finding.Code == "installation.absent" && finding.Severity == netpreflight.Error {
			return true
		}
	}
	return false
}

func interactiveTextSession(stderr io.Writer) bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// validateNodeSelectors checks selectors against resolved; where names it
// for the user ("inventory" or "deployment").
func validateNodeSelectors(resolved spec.Resolved, nodes []string, where string) error {
	known := make(map[string]struct{}, len(resolved.Nodes))
	for _, node := range resolved.Nodes {
		known[node.Name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(nodes))
	for _, name := range nodes {
		if _, ok := known[name]; !ok {
			return privatevm.UnknownNodeError(name, where, resolved.Nodes)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("node %q is selected more than once", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// destroyScope states exactly what a destroy will remove so the typed
// confirmation covers a reviewed scope rather than a bare verb.
func destroyScope(resolved spec.Resolved, nodes []string, deletePersistent, purge bool) string {
	names := make([]string, 0, len(resolved.Nodes))
	for _, node := range resolved.Nodes {
		names = append(names, node.Name)
	}
	if len(nodes) != 0 {
		return fmt.Sprintf("destroy scope: node(s) %s are removed from the deployment; the other nodes stay", strings.Join(nodes, ", "))
	}
	scope := fmt.Sprintf("destroy scope: the whole deployment (%d node(s): %s)", len(names), strings.Join(names, ", "))
	switch {
	case purge:
		scope += "; also deletes persistent data disks, the deployment keys, and the deployment state (images stay cached)"
	case deletePersistent:
		scope += "; also deletes persistent data disks (keys and state are preserved)"
	default:
		scope += "; persistent data disks, keys, and state are preserved"
	}
	return scope
}

type sshConfigOptions struct {
	Install bool
	Remove  bool
	Name    string
}

func sshConfigOutcome(result sshconfig.Result, name string) commandOutcome {
	return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
		bestEffortf(stdout, "%s SSH config %s (changed=%t)\nfragment: %s\nconfig: %s\n", result.Action, name, result.Changed, result.Fragment, result.Config)
		return nil
	}}
}

func runSSHConfig(parent context.Context, options sshConfigOptions, nodes []string) (commandOutcome, error) {
	if options.Install && options.Remove {
		return commandOutcome{}, newUsageError(errors.New("--install and --remove are mutually exclusive"))
	}
	if options.Name == "" {
		options.Name = "farrow"
	}
	if options.Remove && len(nodes) != 0 {
		return commandOutcome{}, newUsageError(errors.New("ssh-config --remove removes the deployment fragment and does not accept node selectors"))
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	// Removal is intentionally state-independent: destroy preserves the
	// deployment state, and sshconfig.Remove itself deletes only the exact
	// marker-owned fragment and Include. This keeps rollback available
	// after resolved state or node artifacts are gone.
	if options.Remove {
		result, err := removeSSHConfigFragment(options.Name)
		if err != nil {
			return commandOutcome{}, classifySSHConfigFailure(result, err)
		}
		return sshConfigOutcome(result, options.Name), nil
	}
	resolved, resolveErr := currentDeploymentResolved()
	if resolveErr != nil {
		return commandOutcome{}, newCommandError(exitConflict, errNoDeployment)
	}
	if resolved.Network != "private" {
		return commandOutcome{}, newConflictError(errors.New(legacyDeploymentMessage))
	}
	manager := privatevm.Manager{FarrowVersion: version.Version, Nodes: append([]string(nil), nodes...)}
	if options.Install {
		result, err := manager.InstallSSHConfig(ctx, options.Name, "")
		if err != nil {
			return commandOutcome{}, classifySSHConfigFailure(result, err)
		}
		return sshConfigOutcome(result, options.Name), nil
	}
	text, err := manager.SSHConfig(ctx)
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	return commandOutcome{payload: map[string]string{"config": text}, text: func(stdout, _ io.Writer) error {
		bestEffortf(stdout, "%s", text)
		return nil
	}}, nil
}

// removeSSHConfigFragment is deliberately state-independent: destroy keeps
// the fragment on disk, and sshconfig.Remove itself deletes only the exact
// marker-owned fragment and Include line.
func removeSSHConfigFragment(name string) (sshconfig.Result, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sshconfig.Result{}, err
	}
	return sshconfig.Remove(home, name)
}

type sshConfigFailurePayload struct {
	Error   string           `json:"error"`
	Message string           `json:"message"`
	Partial bool             `json:"partial"`
	Result  sshconfig.Result `json:"result"`
}

func classifySSHConfigFailure(result sshconfig.Result, err error) error {
	partial := result.Changed && strings.HasSuffix(result.Action, "-partial")
	payload := sshConfigFailurePayload{Error: "ssh_config", Message: err.Error(), Partial: partial, Result: result}
	if partial {
		err = fmt.Errorf("SSH config operation partially changed owned state; retry is safe (action=%s fragment=%s config=%s): %w", result.Action, result.Fragment, result.Config, err)
		payload.Message = err.Error()
	}
	return newCommandError(exitRuntime, err).withPayload(payload)
}

func runHosts(parent context.Context, action string, apply bool, stderr io.Writer) (commandOutcome, error) {
	if action != hostconfig.ActionInstall && action != hostconfig.ActionUninstall {
		return commandOutcome{}, newUsageError(fmt.Errorf("unknown hosts action %q", action))
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	manager := privatevm.Manager{FarrowVersion: version.Version}
	var entries []hostconfig.Entry
	var err error
	if action == hostconfig.ActionInstall {
		resolved, resolveErr := currentDeploymentResolved()
		switch {
		case errors.Is(resolveErr, os.ErrNotExist):
			return commandOutcome{}, newCommandError(exitConflict, errNoDeployment)
		case resolveErr != nil:
			return commandOutcome{}, newCommandError(exitIntegrity, resolveErr)
		case resolved.Network != "private":
			return commandOutcome{}, newConflictError(errors.New(legacyDeploymentMessage))
		}
		entries, err = manager.HostEntries(ctx)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return commandOutcome{}, newCommandError(exitConflict, errNoDeployment)
			}
			return commandOutcome{}, newCommandError(exitIntegrity, err)
		}
	}
	baseRunner := execx.OSRunner{Timeout: 30 * time.Second, OutputLimit: 1 << 20}
	rootRunner := setupRootRunner(baseRunner)
	if apply {
		privilege := &sudoSession{base: baseRunner, stderr: stderr, scope: "hosts command"}
		defer privilege.close()
		if _, _, err := ensureSetupHostsHelper(ctx, baseRunner, privilege, stderr); err != nil {
			return commandOutcome{}, newCommandError(exitRuntime, err)
		}
		if err := privilege.ensure(ctx, "apply the reviewed /etc/hosts plan"); err != nil {
			return commandOutcome{}, newCommandError(exitCapability, err)
		}
	}
	debugf(stderr, "hosts action=%s entries=%d apply=%t", action, len(entries), apply)
	progressMessage := "Installing deployment host entries"
	if action == hostconfig.ActionUninstall {
		progressMessage = "Removing deployment host entries"
	}
	progressItem := startProgress(ctx, stderr, progressMessage)
	report, err := (hostconfig.Executor{Root: rootRunner}).Execute(ctx, action, entries, apply)
	progressItem.Stop(err)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	if err != nil {
		return commandOutcome{}, newCommandError(exitRuntime, err)
	}
	return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
		textField(stdout, 16, "action", statusValue(stdout, report.Plan.Action))
		textField(stdout, 16, "target", report.Plan.Target)
		textField(stdout, 16, "changed", report.Plan.Changed)
		textField(stdout, 16, "applied", report.Applied)
		textField(stdout, 16, "before sha256", report.Plan.BeforeSHA256)
		textField(stdout, 16, "after sha256", report.Plan.AfterSHA256)
		textField(stdout, 16, "helper", report.Plan.HelperPath)
		textField(stdout, 16, "helper sha256", report.Plan.HelperSHA256)
		for _, line := range report.Plan.Lines {
			bestEffortln(stdout, line)
		}
		if report.Plan.Changed && !report.Applied {
			bestEffortln(stdout, "rerun with --yes to apply this plan; Farrow will request sudo when needed")
			return nil
		}
		return nil
	}}, nil
}

type logResult struct {
	Node       string `json:"node"`
	Source     string `json:"source"`
	Path       string `json:"path"`
	Content    string `json:"content"`
	Bytes      int    `json:"bytes"`
	TotalBytes int64  `json:"total_bytes"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type logStreamRecord struct {
	Type      string `json:"type"`
	Node      string `json:"node"`
	Source    string `json:"source"`
	Path      string `json:"path"`
	Sequence  uint64 `json:"sequence,omitempty"`
	Content   string `json:"content,omitempty"`
	Continued bool   `json:"continued,omitempty"`
}

const structuredLogRecordLimit = 64 << 10

func readStructuredLogChunk(reader *bufio.Reader) (string, bool, error) {
	chunk, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return string(chunk), true, nil
	}
	return string(chunk), false, err
}

type logOptions struct {
	Source string
	Follow bool
}

func runLogs(parent context.Context, options logOptions, requestedNode string, stdout, stderr io.Writer) (commandOutcome, error) {
	if options.Source == "" {
		options.Source = "serial"
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	resolved, resolveErr := currentDeploymentResolved()
	if resolveErr != nil && (options.Source != "events" || !errors.Is(resolveErr, os.ErrNotExist)) {
		return commandOutcome{}, newCommandError(exitConflict, errNoDeployment)
	}
	if resolveErr == nil && resolved.Network != "private" {
		return commandOutcome{}, newConflictError(errors.New(legacyDeploymentMessage))
	}
	node := requestedNode
	if options.Source == "events" {
		if node != "" {
			return commandOutcome{}, newUsageError(errors.New("--source events is the deployment-wide event log and does not accept a node"))
		}
		node = "deployment"
	}
	path, err := (privatevm.Manager{FarrowVersion: version.Version}).LogPath(requestedNode, options.Source)
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	if node == "" {
		for _, candidate := range resolved.Nodes {
			if candidate.Control {
				node = candidate.Name
				break
			}
		}
	}
	handle, err := os.Open(path)
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	defer func() {
		// Log reads are read-only; content has already been consumed by the time
		// this descriptor is released.
		_ = handle.Close()
	}()
	if structuredOutput(stdout) && !options.Follow {
		info, err := handle.Stat()
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		capture := boundedCapture{limit: structuredCommandCaptureLimit}
		if _, err := io.Copy(&capture, handle); err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		content := capture.String()
		result := logResult{
			Node: node, Source: options.Source, Path: path, Content: content,
			Bytes: len(content), TotalBytes: info.Size(), Truncated: capture.truncated,
		}
		return commandOutcome{payload: result}, nil
	}
	if structuredOutput(stdout) {
		if err := encodeStreamOutput(stdout, logStreamRecord{Type: "start", Node: node, Source: options.Source, Path: path}); err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		reader := bufio.NewReaderSize(handle, structuredLogRecordLimit)
		var sequence uint64
		for {
			chunk, continued, readErr := readStructuredLogChunk(reader)
			if chunk != "" {
				sequence++
				if err := encodeStreamOutput(stdout, logStreamRecord{
					Type: "line", Node: node, Source: options.Source, Path: path, Sequence: sequence,
					Content: chunk, Continued: continued,
				}); err != nil {
					return commandOutcome{}, newRuntimeError(err)
				}
			}
			if readErr == nil {
				continue
			}
			if !errors.Is(readErr, io.EOF) {
				return commandOutcome{}, newRuntimeError(readErr)
			}
			select {
			case <-ctx.Done():
				return commandOutcome{}, ErrCancelled
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	if !options.Follow {
		content, err := io.ReadAll(handle)
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		result := logResult{Node: node, Source: options.Source, Path: path, Content: string(content), Bytes: len(content), TotalBytes: int64(len(content))}
		return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
			out := newLogTextWriter(stdout, options.Source)
			_, _ = out.Write(content)
			out.Flush()
			return nil
		}}, nil
	}
	followed := newLogTextWriter(stdout, options.Source)
	if _, err := io.Copy(followed, handle); err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	bestEffortf(stderr, "%s following %s log for %s\n", styled(stderr, ansiCyan, "→"), options.Source, node)
	for options.Follow {
		select {
		case <-ctx.Done():
			return commandOutcome{}, ErrCancelled
		case <-time.After(250 * time.Millisecond):
			if _, err := io.Copy(followed, handle); err != nil {
				return commandOutcome{}, newRuntimeError(err)
			}
		}
	}
	return commandOutcome{streamed: true}, nil
}

type validateResult struct {
	Valid    bool          `json:"valid"`
	Source   string        `json:"source"`
	SpecHash string        `json:"spec_hash"`
	Resolved spec.Resolved `json:"resolved"`
	Warnings []string      `json:"warnings,omitempty"`
}

func runValidate(filePath string) (commandOutcome, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	file, source, err := config.Discover(cwd, filePath)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	resolved, err := file.Resolve()
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	hash, err := spec.Hash(resolved)
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	warnings := configurationWarnings(resolved)
	result := validateResult{Valid: true, Source: source, SpecHash: hash, Resolved: resolved, Warnings: warnings}
	return commandOutcome{payload: result, text: func(stdout, stderr io.Writer) error {
		printWarnings(stderr, warnings)
		names := make([]string, 0, len(resolved.Nodes))
		for _, node := range resolved.Nodes {
			names = append(names, node.Name)
		}
		bestEffortf(stdout, "%s %s (%d node(s): %s)\n", statusValue(stdout, "valid:"), source, len(names), strings.Join(names, ", "))
		return nil
	}}, nil
}

type initOptions struct {
	Template string
	CIDR     string
	Output   string
	Force    bool
}

type initResult struct {
	Template string `json:"template"`
	Content  string `json:"content,omitempty"`
	Path     string `json:"path,omitempty"`
}

func runInit(options initOptions) (commandOutcome, error) {
	name := options.Template
	if name == "" {
		name = "meta"
	}
	data, err := config.Template(name, options.CIDR)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	warnings := make([]string, 0, 1)
	if options.CIDR != "" {
		if layout, parseErr := subnet.Parse(options.CIDR); parseErr == nil {
			if warning := layout.Warning(); warning != "" {
				warnings = append(warnings, warning)
			}
		}
	}
	if options.Output == "-" {
		result := initResult{Template: name, Content: string(data)}
		return commandOutcome{payload: result, text: func(stdout, stderr io.Writer) error {
			printWarnings(stderr, warnings)
			_, _ = stdout.Write(data)
			return nil
		}}, nil
	}
	target := options.Output
	if target == "" {
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			return commandOutcome{}, newRuntimeError(cwdErr)
		}
		target = filepath.Join(cwd, "farrow.yml")
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	if options.Force {
		err = fsutil.AtomicWrite(target, data, 0o600)
	} else {
		err = fsutil.AtomicCreate(target, data, 0o600)
	}
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return commandOutcome{}, newConflictError(fmt.Errorf("%s already exists; edit it, pass --force to replace it, or -o for another path", target))
		}
		return commandOutcome{}, newRuntimeError(err)
	}
	result := initResult{Template: name, Path: target}
	return commandOutcome{payload: result, text: func(stdout, stderr io.Writer) error {
		printWarnings(stderr, warnings)
		textField(stdout, 10, "template", name)
		textField(stdout, 10, "wrote", target)
		next := "farrow up"
		if options.Output != "" {
			next += " -f " + shellQuote(options.Output)
		}
		textField(stdout, 10, "next", next)
		return nil
	}}, nil
}

type imageOptions struct {
	Action         string
	Repository     string
	Mirror         bool
	Alias          string
	Arch           string
	Source         string
	Path           string
	ExpectedSHA256 string
	Name           string
	Boot           string
	SourceUser     string
	DryRun         bool
	Apply          bool
	AllowDowngrade bool
}

func runImage(parent context.Context, options imageOptions, stderr io.Writer) (commandOutcome, error) {
	switch options.Action {
	case "update":
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		var progressItem *progress
		service, err := imageService(options.Repository, options.Mirror, deferredProgressReporter(&progressItem))
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		progressItem = startProgress(ctx, stderr, "Updating the image catalog")
		result, err := service.UpdateCatalog(ctx)
		progressItem.Stop(err)
		if errors.Is(parent.Err(), context.Canceled) {
			return commandOutcome{}, ErrCancelled
		}
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
			if result.Updated {
				bestEffortf(stdout, "updated image catalog from revision %d to %d\n", result.PreviousVersion, result.ActiveVersion)
			} else {
				bestEffortf(stdout, "image catalog revision %d is current\n", result.ActiveVersion)
			}
			textField(stdout, 12, "repository", result.Repository)
			textField(stdout, 12, "source", result.Source)
			return nil
		}}, nil
	case "list":
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		service, err := imageService(options.Repository, options.Mirror, nil)
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		entries, manifestState, err := service.List(ctx)
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		payload := struct {
			Manifest image.ManifestState `json:"manifest"`
			Images   []image.Entry       `json:"images"`
		}{manifestState, entries}
		return commandOutcome{payload: payload, text: func(stdout, _ io.Writer) error {
			textField(stdout, 9, "catalog", fmt.Sprintf("revision %d (%s)", manifestState.ActiveVersion, catalogOrigin(manifestState.Source)))
			rows := make([][]string, 0, len(entries))
			for _, entry := range entries {
				rows = append(rows, []string{entry.Alias, entry.Release, entry.Arch, strings.Join(entry.Channels, ","), entry.Status})
			}
			printTable(stdout, []string{"ALIAS", "RELEASE", "ARCH", "CHANNELS", "STATUS"}, rows, 4)
			return nil
		}}, nil
	case "info", "pull":
		ctx, cancel := context.WithCancel(parent)
		defer cancel()
		var progressItem *progress
		service, err := imageService(options.Repository, options.Mirror, deferredProgressReporter(&progressItem))
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		var info image.Info
		if options.Action == "pull" {
			debugf(stderr, "image pull alias=%s (inactivity timeout; resumable)", options.Alias)
			progressItem = startProgress(ctx, stderr, fmt.Sprintf("Pulling image %s", options.Alias))
			if options.Arch == "" {
				info, err = service.PullAlias(ctx, options.Alias)
			} else {
				info, err = service.PullArch(ctx, options.Alias, options.Arch)
			}
		} else {
			if options.Arch == "" {
				info, err = service.Info(ctx, options.Alias)
			} else {
				info, err = service.InfoArch(ctx, options.Alias, options.Arch)
			}
		}
		progressItem.Stop(err)
		if errors.Is(parent.Err(), context.Canceled) {
			return commandOutcome{}, ErrCancelled
		}
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		printImageStatusWarning(stderr, info.Entry)
		return commandOutcome{payload: info, text: func(stdout, _ io.Writer) error {
			bestEffortf(stdout, "%s %s %s status=%s sha256:%s cached=%t\n", info.Entry.Alias, info.Entry.Release, info.Entry.Arch, info.Entry.Status, info.Entry.SHA256, info.Cached)
			if info.Path != "" {
				bestEffortf(stdout, "path: %s\n", info.Path)
				return nil
			}
			return nil
		}}, nil
	case "repo-scan", "repo-build", "repo-verify":
		root, err := filepath.Abs(options.Path)
		if err != nil {
			return commandOutcome{}, newUsageError(err)
		}
		if options.Action == "repo-scan" {
			report, scanErr := image.ScanRepository(root)
			if scanErr != nil {
				return commandOutcome{}, newCommandError(exitRuntime, scanErr)
			}
			return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
				textField(stdout, 12, "root", report.Root)
				textField(stdout, 12, "tracked", len(report.Tracked))
				textField(stdout, 12, "missing", len(report.Missing))
				textField(stdout, 12, "untracked", len(report.Untracked))
				textField(stdout, 12, "unsafe", len(report.Unsafe))
				return nil
			}}, nil
		}
		qemuImg, err := exec.LookPath("qemu-img")
		if err != nil {
			return commandOutcome{}, newCommandError(exitCapability, fmt.Errorf("repository %s requires qemu-img: %w", strings.TrimPrefix(options.Action, "repo-"), err))
		}
		ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
		defer cancel()
		builder := image.RepoBuilder{QEMUImg: qemuImg, Runner: execx.OSRunner{Timeout: 10 * time.Minute, OutputLimit: 1 << 20}}
		var result image.RepoBuildResult
		if options.Action == "repo-build" {
			result, err = builder.Build(ctx, root)
		} else {
			result, err = builder.Verify(ctx, root)
		}
		if err != nil {
			fallback := exitRuntime
			if options.Action == "repo-verify" {
				fallback = exitIntegrity
			}
			return commandOutcome{}, newCommandError(fallback, err)
		}
		return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
			textField(stdout, 12, "catalog", result.Path)
			textField(stdout, 12, "revision", result.Catalog.Version)
			textField(stdout, 12, "images", len(result.Catalog.Images))
			textField(stdout, 12, "bytes", result.Bytes)
			return nil
		}}, nil
	case "prune":
		if options.DryRun && options.Apply {
			return commandOutcome{}, newUsageError(errors.New("--dry-run and --yes are mutually exclusive"))
		}
		ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
		defer cancel()
		service, err := imageService(options.Repository, options.Mirror, nil)
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		progressItem := startProgress(ctx, stderr, "Scanning unreferenced image cache entries")
		report, err := service.PruneAll(ctx, options.Apply, func(refCtx context.Context) (map[string]struct{}, error) {
			return nodeImageReferences(service.DataRoot)
		})
		progressItem.Stop(err)
		if errors.Is(parent.Err(), context.Canceled) {
			return commandOutcome{}, ErrCancelled
		}
		if err != nil {
			return commandOutcome{}, newCommandError(exitRuntime, err)
		}
		return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
			if len(report.Items) == 0 {
				bestEffortf(stdout, "no unreferenced cache images\n")
				return nil
			}
			for _, item := range report.Items {
				action := "would delete"
				if item.Applied {
					action = "deleted"
				}
				if item.Digest == "" {
					bestEffortf(stdout, "%s %s (%d bytes, %s)\n", action, item.ImagePath, item.Bytes, item.Kind)
				} else {
					bestEffortf(stdout, "%s %s sha256:%s (%d bytes)\n", action, item.ImagePath, item.Digest, item.Bytes)
				}
			}
			return nil
		}}, nil
	case "sync":
		if options.Source == "" {
			return commandOutcome{}, newUsageError(errors.New("image sync requires a URL or path"))
		}
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		debugf(stderr, "image catalog sync source=%s allow_downgrade=%t", progressSource(options.Source), options.AllowDowngrade)
		var progressItem *progress
		service, err := imageService(options.Repository, options.Mirror, deferredProgressReporter(&progressItem))
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		progressItem = startProgress(ctx, stderr, "Activating the image catalog")
		state, err := service.SyncManifest(ctx, options.Source, options.AllowDowngrade)
		progressItem.Stop(err)
		if errors.Is(parent.Err(), context.Canceled) {
			return commandOutcome{}, ErrCancelled
		}
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		return commandOutcome{payload: state, text: func(stdout, _ io.Writer) error {
			bestEffortf(stdout, "activated image catalog revision %d (digest %s, key %s)\n", state.ActiveVersion, state.ActiveDigest, state.KeyID)
			return nil
		}}, nil
	case "reset":
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		service, err := imageService(options.Repository, options.Mirror, nil)
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		state, err := service.ResetManifest(ctx)
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		return commandOutcome{payload: state, text: func(stdout, _ io.Writer) error {
			bestEffortf(stdout, "image catalog reset to the built-in revision %d; downgrade protection still at %d\n", state.ActiveVersion, state.HighestVersion)
			return nil
		}}, nil
	case "import":
		if options.Path == "" {
			return commandOutcome{}, newUsageError(errors.New("image import requires a path"))
		}
		invalidAliasMetadata := (options.Name == "" && (options.Boot != "" || options.SourceUser != "")) || (options.Name != "" && (options.SourceUser == "" || (options.Boot != "bios" && options.Boot != "uefi")))
		if invalidAliasMetadata {
			return commandOutcome{}, newUsageError(errors.New("--name requires --boot bios|uefi and --source-user; alias metadata is immutable"))
		}
		ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
		defer cancel()
		service, err := imageService("", false, nil)
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		var path string
		var metadata image.Metadata
		var localEntry *image.Entry
		debugf(stderr, "image import source=%s expected_sha256=%s alias=%s", options.Path, options.ExpectedSHA256, options.Name)
		progressItem := startProgress(ctx, stderr, "Importing and verifying the image")
		if options.Name == "" {
			path, metadata, err = service.ImportFile(ctx, options.Path, options.ExpectedSHA256)
		} else {
			entry, importedPath, importedMetadata, importErr := service.ImportNamed(ctx, options.Path, options.ExpectedSHA256, options.Name, options.Boot, options.SourceUser)
			path, metadata, err = importedPath, importedMetadata, importErr
			if importErr == nil {
				localEntry = &entry
			}
		}
		progressItem.Stop(err)
		if errors.Is(parent.Err(), context.Canceled) {
			return commandOutcome{}, ErrCancelled
		}
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		result := struct {
			Path     string         `json:"path"`
			Metadata image.Metadata `json:"metadata"`
			Entry    *image.Entry   `json:"entry,omitempty"`
		}{Path: path, Metadata: metadata, Entry: localEntry}
		renderText := func(stdout, _ io.Writer) error {
			if path != "" {
				bestEffortf(stdout, "imported %s\nsha256 %s\n", path, metadata.Digest)
				return nil
			}
			return nil
		}
		return commandOutcome{payload: result, text: renderText}, nil
	default:
		return commandOutcome{}, newUsageError(fmt.Errorf("unknown image command %q", options.Action))
	}
}

// doctorCheckLabel is the word a person reads, chosen to match the consequence
// rather than the raw severity. A network finding is real, but it never changes
// the exit status, and printing "error" next to a successful exit is the kind of
// contradiction that teaches people to stop reading the output.
func doctorCheckLabel(check doctor.Check) string {
	if check.Class == doctor.ClassNetwork && check.Status == doctor.Error {
		return "blocked"
	}
	return string(check.Status)
}

func runDoctor(parent context.Context, stderr io.Writer) (commandOutcome, error) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	progressItem := startProgress(ctx, stderr, "Checking host capabilities")
	report := (doctor.Probe{}).Run(ctx)
	progressItem.Stop(nil)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	renderText := func(stdout, _ io.Writer) error {
		for _, check := range report.Checks {
			bestEffortf(stdout, "%s %-20s %s\n", statusCell(stdout, 10, doctorCheckLabel(check)), check.Name, check.Evidence)
			if check.Fix != "" {
				bestEffortf(stdout, "  fix: %s\n", check.Fix)
			}
		}
		if report.HasErrors() {
			bestEffortf(stdout, "this host cannot run Farrow guests until the errors above are resolved\n")
		} else {
			bestEffortf(stdout, "host compute capability is ready\n")
		}
		if !report.NetworkReady() {
			bestEffortf(stdout, "the host-global network is not ready; run `farrow setup` to prepare it\n")
		}
		return nil
	}
	if report.HasErrors() {
		return commandOutcome{}, newCommandError(exitCapability, errors.New("host compute capability is not ready")).withPayload(report).withText(renderText).quiet()
	}
	return commandOutcome{payload: report, text: renderText}, nil
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	prepared, preparedStdout, preparedStderr, err := prepareOutput(args, stdout, stderr)
	if err != nil {
		errorf(stderr, "%v", err)
		return exitUsage
	}
	if len(prepared) > 0 {
		debugf(preparedStderr, "command=%s format=%s stdout_tty=%t stderr_tty=%t", prepared[0], outputFormatFor(preparedStdout), writerTTY(preparedStdout), writerTTY(preparedStderr))
	}
	return executeCLI(ctx, prepared, preparedStdout, preparedStderr)
}

// interruptSignals is the one delivery channel for SIGINT and SIGTERM. It is
// package-level so a child that must own SIGINT for a while (an SSH session)
// can suspend and re-arm the same registration.
var interruptSignals = make(chan os.Signal, 1)

// interruptContext cancels the returned context on the first SIGINT or
// SIGTERM. The stop function unregisters the signals and cancels the context.
func interruptContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	signal.Notify(interruptSignals, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-interruptSignals:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(interruptSignals)
		cancel()
	}
}

func main() {
	ctx, stop := interruptContext(context.Background())
	code := runContext(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// suggestHostsPublication reminds the user once per up that declared host
// aliases are reachable by name only after `farrow hosts install`. Best
// effort: any read problem stays silent.
func suggestHostsPublication(resolved spec.Resolved, stderr io.Writer) {
	hasAliases := false
	for _, node := range resolved.Nodes {
		if len(node.Aliases) != 0 {
			hasAliases = true
		}
	}
	if !hasAliases {
		return
	}
	target, err := hostconfig.NativePath()
	if err != nil {
		return
	}
	data, err := os.ReadFile(target)
	if err != nil || len(data) > 1<<20 {
		return
	}
	if strings.Contains(string(data), "# farrow:begin") {
		return
	}
	warningf(stderr, "declared host aliases are not published; run `farrow hosts install --yes` to add them to %s", target)
}
