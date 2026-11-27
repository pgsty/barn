package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/config"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/hostconfig"
	"github.com/pgsty/barn/internal/identity"
	"github.com/pgsty/barn/internal/linuxvm"
	"github.com/pgsty/barn/internal/provision"
	setuphost "github.com/pgsty/barn/internal/setup"
	"github.com/pgsty/barn/internal/sshconfig"
	"github.com/pgsty/barn/internal/sshkeys"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/version"
	"github.com/pgsty/barn/internal/vm"
)

const structuredCommandCaptureLimit = 4 << 20

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
	var capturedStdout, capturedStderr *execx.LimitedBuffer
	if interactiveStructured {
		result.Interactive = true
		result.SessionStream = "stderr"
		sshCommand.Stdout = rawWriter(stderr)
		sshCommand.Stderr = rawWriter(stderr)
	} else if structured {
		capturedStdout = execx.NewLimitedBuffer(structuredCommandCaptureLimit)
		capturedStderr = execx.NewLimitedBuffer(structuredCommandCaptureLimit)
		sshCommand.Stdout = capturedStdout
		sshCommand.Stderr = capturedStderr
	} else {
		sshCommand.Stdout = rawWriter(stdout)
		sshCommand.Stderr = rawWriter(stderr)
	}
	started := time.Now()
	err := runSSHChild(sshCommand)
	result.DurationMS = time.Since(started).Milliseconds()
	if capturedStdout != nil {
		result.Stdout = capturedStdout.String()
		result.StdoutTruncated = capturedStdout.Truncated()
	}
	if capturedStderr != nil {
		result.Stderr = capturedStderr.String()
		result.StderrTruncated = capturedStderr.Truncated()
	}
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
	// signal disposition. While it owns the foreground session, Barn ignores
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

// splitRemoteArguments accepts only an optional selector before the first --.
// Everything after it belongs to the remote command, including later -- tokens.
func splitRemoteArguments(arguments []string, commandPath, selector string, requireCommand bool) ([]string, []string, error) {
	head, remote := arguments, []string(nil)
	index := slices.Index(arguments, "--")
	if index >= 0 {
		head, remote = arguments[:index], arguments[index+1:]
	}
	if len(head) > 1 {
		if index < 0 {
			return nil, nil, fmt.Errorf("remote commands require --: %s [%s] -- command [args...]", commandPath, selector)
		}
		return nil, nil, fmt.Errorf("at most one %s may precede --", selector)
	}
	if len(remote) > 0 && remote[0] == "" {
		return nil, nil, errors.New("remote command must start with a non-empty argument")
	}
	if requireCommand && len(remote) == 0 {
		return nil, nil, fmt.Errorf("exec requires a remote command after --: %s [%s] -- command [args...]", commandPath, selector)
	}
	return head, remote, nil
}

func remoteCommandText(commandName string, arguments []string) string {
	// Keep ssh's shell syntax and the existing single-string exec shorthand.
	// An exec argv must survive OpenSSH's string-valued request intact.
	if commandName == "exec" && len(arguments) > 1 {
		return vm.QuoteRemote(arguments)
	}
	return strings.Join(arguments, " ")
}

func runSSH(parent context.Context, commandName string, args []string, stdout, stderr io.Writer) (commandOutcome, error) {
	head, command, err := splitRemoteArguments(args, "barn "+commandName, "node", commandName == "exec")
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	resolved, err := currentDeploymentResolved()
	if err != nil {
		return commandOutcome{}, deploymentReadError(err)
	}
	if err := validateNodeSelectors(resolved, head, "deployment"); err != nil {
		return commandOutcome{}, newUsageError(failure.WithNext(err, "barn status"))
	}
	node := ""
	if len(head) == 1 {
		node = head[0]
	}
	manager := linuxvm.Manager{BarnVersion: version.Version}
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

func printProvisionReport(stdout, stderr io.Writer, report provision.Report) {
	frozen := lifecycleProgressFrozen(stdout)
	if !frozen {
		rows := make([][]string, 0, len(report.Results))
		for _, result := range report.Results {
			state := "failed"
			if result.Success {
				state = "success"
			}
			rows = append(rows, []string{result.Node, result.Address, state, fmt.Sprintf("exit %d, %s", result.ExitCode, time.Duration(result.DurationMS)*time.Millisecond)})
		}
		printTable(stdout, []string{"NAME", "ADDRESS", "STATE", "MESSAGE"}, rows, 2)
		marker := "DONE"
		if report.Failed > 0 || report.AuditError != "" {
			marker = "FAILED"
		}
		completion := "[" + marker + "]"
		bestEffortf(stdout, "provision %d/%d nodes: %s %s\n", report.Successful, len(report.Results), elapsedLabel(time.Duration(report.DurationMS)*time.Millisecond), styledCompletionMarker(stdout, completion))
	}
	if verboseOutput(stdout) {
		textField(stdout, 10, "script", report.Script.Name)
		textField(stdout, 10, "sha256", report.Script.SHA256)
		textField(stdout, 10, "bytes", report.Script.Size)
		textField(stdout, 10, "sudo", report.Sudo)
		textField(stdout, 10, "parallel", report.Parallelism)
	}
	output := outputContextFrom(stdout)
	showOutput := verboseOutput(stdout) || output == nil || !output.stdoutTTY
	for _, result := range report.Results {
		if showOutput || !result.Success {
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
}

func provisionConnectionExit(err error) int {
	var artifactError *sshkeys.SSHArtifactError
	if errors.As(err, &artifactError) {
		return exitIntegrity
	}
	return exitRuntime
}

type provisionOptions struct {
	ScriptPath  string
	Sudo        bool
	Parallelism int
	Timeout     time.Duration
}

func runProvision(parent context.Context, options provisionOptions, nodes []string, stderr io.Writer) (_ commandOutcome, returnErr error) {
	if options.ScriptPath == "" {
		return commandOutcome{}, newUsageError(errors.New("--script is required"))
	}
	if options.Parallelism < 1 {
		return commandOutcome{}, newUsageError(errors.New("provision parallelism must be positive"))
	}
	if options.Timeout <= 0 {
		return commandOutcome{}, newUsageError(errors.New("provision timeout must be greater than zero"))
	}
	script, err := provision.LoadScript(options.ScriptPath)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	operationID, err := identity.NewUUID()
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	deployment, err := linuxvm.Open()
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	ctx, cancel := context.WithTimeout(parent, options.Timeout)
	defer cancel()
	deploymentLock, err := linuxvm.AcquireLock(ctx, deployment, false)
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
		if errors.Is(err, os.ErrNotExist) {
			return commandOutcome{}, newConflictError(fmt.Errorf("provision requires an existing running deployment: %w", err))
		}
		return commandOutcome{}, newCommandError(exitIntegrity, fmt.Errorf("read deployment state for provision: %w", err))
	}
	resolved := deploymentState.Resolved
	addresses := make(map[string]string, len(resolved.Nodes))
	for _, node := range resolved.Nodes {
		addresses[node.Name] = node.Address
	}
	manager := linuxvm.Manager{BarnVersion: version.Version, OperationID: operationID, Nodes: append([]string(nil), nodes...)}
	connections, connectionErr := manager.ConnectionsLocked(ctx, deployment, deploymentLock)
	if connectionErr != nil {
		return commandOutcome{}, newCommandError(provisionConnectionExit(connectionErr), connectionErr)
	}
	targets := make([]provision.Target, 0, len(connections))
	selectedNames := make([]string, 0, len(connections))
	for _, connection := range connections {
		if connection.Host != "127.0.0.1" {
			return commandOutcome{}, newCommandError(exitIntegrity, fmt.Errorf("will not provision node %s: its SSH endpoint is not on loopback", connection.Node))
		}
		targets = append(targets, provision.Target{Node: connection.Node, Address: addresses[connection.Node], User: connection.User, Port: connection.Port, PrivateKey: connection.PrivateKey, KnownHosts: connection.KnownHosts, HostKeyAlias: connection.HostKeyAlias})
		selectedNames = append(selectedNames, connection.Node)
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
	if err := manager.RecordEvent(ctx, "provision", "info", "starting "+startMessage); err != nil {
		return commandOutcome{}, newCommandError(exitRuntime, fmt.Errorf("will not provision without recording it in the event log: %w", err)).withOperation(operationID)
	}
	debugf(stderr, "provision operation_id=%s targets=%s timeout=%s parallel=%d sudo=%t script_sha256=%s", operationID, strings.Join(selectedNames, ","), options.Timeout, options.Parallelism, options.Sudo, script.SHA256)
	progressItem := startOperationProgress(ctx, stderr, fmt.Sprintf("Provisioning %d node(s)", len(targets)), "provision")
	defer func() { progressItem.Stop(returnErr) }()
	report, err := (provision.Executor{
		Runner: provision.SSHRunner{SSHPath: sshPath, Progress: progressItem.Report}, Parallelism: options.Parallelism, OperationID: operationID, Progress: progressItem.Report,
	}).Execute(ctx, script, targets, options.Sudo)
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
	auditErr := manager.RecordEvent(auditContext, "provision", level, fmt.Sprintf("completed script_sha256=%s successful=%d failed=%d results=%s", script.SHA256, report.Successful, report.Failed, strings.Join(resultParts, ";")))
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

type sshConfigOptions struct {
	Install bool
	Remove  bool
	Name    string
}

func sshConfigOutcome(result sshconfig.Result, name string) commandOutcome {
	return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
		phase := "unchanged"
		if result.Changed {
			phase = "applied"
		}
		printItemResult(stdout, "ssh-config", phase, result.Action+" SSH config "+name)
		textField(stdout, 10, "fragment", result.Fragment)
		textField(stdout, 10, "config", result.Config)
		return nil
	}}
}

func runSSHConfig(parent context.Context, options sshConfigOptions, nodes []string) (commandOutcome, error) {
	if options.Install && options.Remove {
		return commandOutcome{}, newUsageError(errors.New("--install and --remove are mutually exclusive"))
	}
	if options.Name == "" {
		options.Name = "barn"
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
	if _, resolveErr := currentDeploymentResolved(); resolveErr != nil {
		return commandOutcome{}, deploymentReadError(resolveErr)
	}
	manager := linuxvm.Manager{BarnVersion: version.Version, Nodes: append([]string(nil), nodes...)}
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

func removeSSHConfigFragment(name string) (sshconfig.Result, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sshconfig.Result{}, err
	}
	return sshconfig.Remove(home, name)
}

type sshConfigFailurePayload struct {
	commandFailure
	Partial bool             `json:"partial"`
	Result  sshconfig.Result `json:"result"`
}

func classifySSHConfigFailure(result sshconfig.Result, err error) error {
	partial := result.Changed && strings.HasSuffix(result.Action, "-partial")
	if partial {
		err = fmt.Errorf("SSH config operation partially changed owned state; retry is safe (action=%s fragment=%s config=%s): %w", result.Action, result.Fragment, result.Config, err)
	}
	boundary := newCommandError(exitRuntime, err)
	return boundary.withPayload(sshConfigFailurePayload{commandFailure: boundary.failure, Partial: partial, Result: result})
}

func runHosts(parent context.Context, action string, apply bool, configPath string, stderr io.Writer) (commandOutcome, error) {
	if action != hostconfig.ActionInstall && action != hostconfig.ActionUninstall {
		return commandOutcome{}, newUsageError(fmt.Errorf("unknown hosts action %q", action))
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	var entries []hostconfig.Entry
	var err error
	if action == hostconfig.ActionInstall {
		if configPath != "" {
			file, _, discoverErr := config.Discover("", configPath)
			if discoverErr != nil {
				return commandOutcome{}, newUsageError(discoverErr)
			}
			entries, err = file.HostEntries()
			if err != nil {
				return commandOutcome{}, newUsageError(err)
			}
		} else {
			if _, resolveErr := currentDeploymentResolved(); resolveErr != nil {
				return commandOutcome{}, deploymentReadError(resolveErr)
			}
			manager := linuxvm.Manager{BarnVersion: version.Version}
			entries, err = manager.HostEntries(ctx)
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return commandOutcome{}, newCommandError(exitConflict, errNoDeployment)
			}
			return commandOutcome{}, newCommandError(exitIntegrity, err)
		}
	}
	baseRunner := execx.OSRunner{Timeout: 30 * time.Second, OutputLimit: 1 << 20}
	executor := hostconfig.Executor{Root: setuphost.RootRunner(baseRunner)}
	debugf(stderr, "hosts action=%s entries=%d apply=%t", action, len(entries), apply)
	// Reading /etc/hosts needs no privilege: plan first, and on a terminal
	// without --yes show the plan and ask once.
	confirm := !apply && interactiveTextSession(stderr) && !structuredOutput(stderr)
	next := ""
	if !confirm {
		next = "barn hosts " + action
		if configPath != "" {
			next += " -f " + shellQuote(configPath)
		}
		next += " --yes"
	}
	report, err := executor.Execute(ctx, action, entries, false)
	if err != nil {
		return commandOutcome{}, newCommandError(exitRuntime, err)
	}
	if !report.Plan.Changed || !apply && !confirm {
		return hostsOutcome(report, next), nil
	}
	if confirm {
		if err := hostsOutcome(report, "").text(stderr, stderr); err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		question, defaultYes := "Apply this /etc/hosts change? [Y/n] ", true
		if action == hostconfig.ActionUninstall {
			question, defaultYes = "Remove the Barn block from /etc/hosts? [y/N] ", false
		}
		if err := confirmPlan(question, defaultYes, os.Stdin, stderr); err != nil {
			if errors.Is(err, ErrCancelled) {
				return commandOutcome{}, err
			}
			return commandOutcome{}, newRuntimeError(err)
		}
	}
	privilege := newSudoSession(baseRunner, stderr, "hosts command")
	defer privilege.Close()
	if _, _, err := ensureSetupHostsHelper(ctx, baseRunner, privilege, stderr); err != nil {
		return commandOutcome{}, newCommandError(exitRuntime, err)
	}
	if err := privilege.Ensure(ctx, "apply the reviewed /etc/hosts plan"); err != nil {
		return commandOutcome{}, newCommandError(exitCapability, err)
	}
	progressMessage := "Installing deployment host entries"
	if action == hostconfig.ActionUninstall {
		progressMessage = "Removing deployment host entries"
	}
	progressItem := startProgress(ctx, stderr, progressMessage)
	executor.Reviewed = &report.Plan
	report, err = executor.Execute(ctx, action, entries, true)
	progressItem.Stop(err)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	if err != nil {
		return commandOutcome{}, newCommandError(exitRuntime, err)
	}
	return hostsOutcome(report, ""), nil
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
		return commandOutcome{}, deploymentReadError(resolveErr)
	}
	node := requestedNode
	if options.Source == "events" {
		if node != "" {
			return commandOutcome{}, newUsageError(errors.New("--source events is the deployment-wide event log and does not accept a node"))
		}
		node = "deployment"
	}
	path, err := (linuxvm.Manager{BarnVersion: version.Version}).LogPath(requestedNode, options.Source)
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
	defer func() { _ = handle.Close() }()
	if structuredOutput(stdout) && !options.Follow {
		info, err := handle.Stat()
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		capture := execx.NewLimitedBuffer(structuredCommandCaptureLimit)
		if _, err := io.Copy(capture, handle); err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
		content := capture.String()
		return commandOutcome{payload: logResult{
			Node: node, Source: options.Source, Path: path, Content: content,
			Bytes: len(content), TotalBytes: info.Size(), Truncated: capture.Truncated(),
		}}, nil
	}
	if structuredOutput(stdout) {
		return followStructuredLog(ctx, handle, node, options.Source, path, stdout)
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
	infof(stderr, "following %s log for %s", options.Source, node)
	for {
		select {
		case <-ctx.Done():
			return commandOutcome{}, ErrCancelled
		case <-time.After(250 * time.Millisecond):
			if _, err := io.Copy(followed, handle); err != nil {
				return commandOutcome{}, newRuntimeError(err)
			}
		}
	}
}

func followStructuredLog(ctx context.Context, handle *os.File, node, source, path string, stdout io.Writer) (commandOutcome, error) {
	if err := encodeStreamOutput(stdout, logStreamRecord{Type: "start", Node: node, Source: source, Path: path}); err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	reader := bufio.NewReaderSize(handle, structuredLogRecordLimit)
	var sequence uint64
	for {
		chunk, continued, readErr := readStructuredLogChunk(reader)
		if chunk != "" {
			sequence++
			if err := encodeStreamOutput(stdout, logStreamRecord{
				Type: "line", Node: node, Source: source, Path: path, Sequence: sequence,
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
