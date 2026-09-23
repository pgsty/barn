package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/pgsty/farrow/internal/execx"
	"github.com/pgsty/farrow/internal/failure"
	privatevm "github.com/pgsty/farrow/internal/private"
)

// ErrCancelled is the one command-layer cancellation identity.
var ErrCancelled = errors.New("cancelled")

// errNoDeployment is every applied-state command run before the first up.
var errNoDeployment error = failure.New(failure.Conflict, errors.New("no deployment state found")).Because("no_deployment").Then("farrow up")

type commandOutcome struct {
	payload  any
	text     func(io.Writer, io.Writer) error
	streamed bool
}

type typedCommandError interface {
	error
	commandFailure() commandFailure
	exitCode() int
	commandPayload() any
}

type commandBoundaryError struct {
	failure commandFailure
	code    int
	cause   error
	payload any
	text    func(io.Writer, io.Writer) error
	silent  bool
}

func (err *commandBoundaryError) Error() string { return err.failure.Message }
func (err *commandBoundaryError) Unwrap() error { return err.cause }
func (err *commandBoundaryError) commandFailure() commandFailure {
	return err.failure
}
func (err *commandBoundaryError) exitCode() int       { return err.code }
func (err *commandBoundaryError) commandPayload() any { return err.payload }

func (err *commandBoundaryError) setOperationID(id string) {
	if err.failure.OperationID == "" {
		err.failure.OperationID = id
	}
}

// newCommandError is the one way a command reports failure. A class carried
// by err (see internal/failure) wins; fallback applies only when err is
// unclassified, so inner packages decide what they know for certain.
func newCommandError(fallback int, err error) *commandBoundaryError {
	code, reason, next := classifyError(err, fallback)
	return &commandBoundaryError{
		failure: commandFailure{Error: exitCategory(code), Reason: reason, Message: err.Error(), Next: next, Command: commandDetailOf(err)},
		code:    code, cause: err,
	}
}

func (err *commandBoundaryError) withOperation(id string) *commandBoundaryError {
	err.setOperationID(id)
	return err
}

func (err *commandBoundaryError) withPayload(payload any) *commandBoundaryError {
	err.payload = payload
	return err
}

// withText renders the failure's own stdout text; the error line still goes
// to stderr unless quiet is also set.
func (err *commandBoundaryError) withText(text func(io.Writer, io.Writer) error) *commandBoundaryError {
	err.text = text
	return err
}

func (err *commandBoundaryError) quiet() *commandBoundaryError {
	err.silent = true
	return err
}

func newUsageError(err error) error      { return newCommandError(exitUsage, err) }
func newConflictError(err error) error   { return newCommandError(exitConflict, err) }
func newRuntimeError(err error) error    { return newCommandError(exitRuntime, err) }
func newCapabilityError(err error) error { return newCommandError(exitCapability, err) }
func newIntegrityError(err error) error  { return newCommandError(exitIntegrity, err) }

// newRemoteExitError passes a guest program's own exit status through; the
// guest already printed its output, so Farrow adds no error line.
func newRemoteExitError(code int, err error, payload any) *commandBoundaryError {
	return (&commandBoundaryError{failure: commandFailure{Error: "remote_exit", Message: err.Error()}, code: code, cause: err, payload: payload}).quiet()
}

// classifyError maps an error to an exit code, a stable reason, and a next
// action. Only error identity is consulted, never message text.
func classifyError(err error, fallback int) (code int, reason, next string) {
	class, reason, next := failure.Classify(err)
	if class != "" {
		return classExit(class), reason, next
	}
	var preflight *privatevm.NetworkPreflightError
	var capability *privatevm.CapabilityError
	switch {
	case errors.Is(err, ErrCancelled) || errors.Is(err, context.Canceled):
		return exitCancelled, "", next
	case errors.As(err, &preflight):
		return preflight.Report.ExitCode, "network_preflight", next
	case privatevm.IsolatedPartial(err) != nil:
		return exitPartial, "", next
	case errors.As(err, &capability):
		return exitCapability, "", next
	case errors.Is(err, syscall.ENOSPC):
		return exitResource, "disk_full", next
	}
	return fallback, "", next
}

func classExit(class failure.Class) int {
	switch class {
	case failure.Usage:
		return exitUsage
	case failure.Capability:
		return exitCapability
	case failure.Conflict:
		return exitConflict
	case failure.Partial:
		return exitPartial
	case failure.Resource:
		return exitResource
	case failure.Integrity:
		return exitIntegrity
	case failure.Cancelled:
		return exitCancelled
	default:
		return exitRuntime
	}
}

// commandDetailOf exposes the external program behind a failure, if any.
func commandDetailOf(err error) *commandDetail {
	var commandErr *execx.CommandError
	if !errors.As(err, &commandErr) {
		return nil
	}
	detail := &commandDetail{
		Name: commandErr.Name(), Argv: append([]string{commandErr.Binary}, commandErr.Args...),
		ExitStatus: commandErr.ExitCode, Signal: commandErr.Signal, TimedOut: commandErr.Timeout > 0,
		Stderr: strings.Join(commandErr.StderrTail(stderrTailLines), "\n"),
	}
	return detail
}

// stderrTailLines bounds the external stderr shown under an error.
const stderrTailLines = 8

type commandOutcomeCollector struct {
	value *commandOutcome
}

type commandOutcomeContextKey struct{}

func collectCommandOutcome(ctx context.Context, outcome commandOutcome) error {
	collector, ok := ctx.Value(commandOutcomeContextKey{}).(*commandOutcomeCollector)
	if !ok || collector == nil {
		return errors.New("command outcome collector is missing")
	}
	if collector.value != nil {
		return errors.New("command produced more than one outcome")
	}
	collector.value = &outcome
	return nil
}

func renderCommandOutcome(outcome *commandOutcome, stdout, stderr io.Writer) int {
	if outcome == nil {
		return exitOK
	}
	if outcome.streamed {
		return exitOK
	}
	if structuredOutput(stdout) {
		return encodeJSON(stdout, stderr, outcome.payload)
	}
	if outcome.text == nil {
		return exitOK
	}
	if err := outcome.text(stdout, stderr); err != nil {
		errorf(stderr, "write command output: %v", err)
		return exitRuntime
	}
	if err := outputWriteError(stdout); err != nil {
		errorf(stderr, "write command output: %v", err)
		return exitRuntime
	}
	return exitOK
}

func noFileCompletions(command *cobra.Command, names ...string) {
	for _, name := range names {
		if _, exists := command.GetFlagCompletionFunc(name); exists {
			continue
		}
		if command.Flags().Lookup(name) == nil && command.PersistentFlags().Lookup(name) == nil {
			panic(fmt.Sprintf("register completion for missing flag %s on %s", name, command.CommandPath()))
		}
		if err := command.RegisterFlagCompletionFunc(name, cobra.NoFileCompletions); err != nil {
			panic(fmt.Sprintf("register completion for --%s on %s: %v", name, command.CommandPath(), err))
		}
	}
}

func newRootCommand(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "farrow",
		Short: "Run reproducible local Linux virtual-machine labs",
		Long: `Farrow turns a Pigsty-compatible inventory into fixed-IP QEMU virtual
machines. It manages one deployment per user and keeps runtime state under
$FARROW_HOME (default ~/.farrow), so applied-state commands work from any
directory.

Configuration-aware commands use -f first, then discover farrow.yml,
farrow.yaml, pigsty.yml, or pigsty.yaml in the working directory. Once a
deployment exists, lifecycle commands can fall back to the last applied
inventory; validate always requires an inventory file.`,
		Example: `  farrow init                  # write an Ubuntu 24.04 inventory
  farrow plan                  # review images, resources, and changes
  farrow up                    # prepare the host if needed, then start the lab
  farrow status                # inspect the deployment from any directory
  farrow ssh                   # open the default/control node
  farrow --json status         # structured output for automation
  farrow setup                 # prepare the host explicitly`,
		SilenceErrors:         true,
		SilenceUsage:          true,
		DisableAutoGenTag:     true,
		DisableFlagsInUseLine: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	configureHelpOnly(root, "farrow requires a command", stdout, stderr)
	// --version is the conventional spelling of `farrow version`.
	showVersion := root.Flags().Bool("version", false, "print build version")
	greet := root.RunE
	root.RunE = func(command *cobra.Command, arguments []string) error {
		if *showVersion {
			return collectCommandOutcome(command.Context(), runVersionCommand())
		}
		return greet(command, arguments)
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return err })
	root.PersistentFlags().Bool("json", false, "emit JSON output")
	root.PersistentFlags().Bool("yaml", false, "emit YAML output")
	root.PersistentFlags().BoolP("verbose", "v", false, "emit detailed diagnostics to stderr")
	noFileCompletions(root, "json", "yaml", "verbose")
	// prepareOutput consumes the spelled-out presentation flags before Cobra
	// runs, so the persistent flags above mostly document them. Cobra still
	// parses shorthand combinations (-nv, -vv); honor those too.
	root.PersistentPreRun = func(command *cobra.Command, _ []string) {
		if verbose, err := command.Flags().GetBool("verbose"); err == nil && verbose {
			enableVerbose(stderr)
		}
	}

	root.AddGroup(
		&cobra.Group{ID: "setup", Title: "Setup:"},
		&cobra.Group{ID: "lifecycle", Title: "Lifecycle:"},
		&cobra.Group{ID: "access", Title: "Guest Access:"},
		&cobra.Group{ID: "images", Title: "Images:"},
		&cobra.Group{ID: "host", Title: "Host Management:"},
		&cobra.Group{ID: "advanced", Title: "Advanced:"},
	)

	setup := newSetupCommand(stdout, stderr)
	setup.Aliases = []string{"s"}
	setup.GroupID = "setup"
	initCommand := newInitCommand(stdout, stderr)
	initCommand.GroupID = "setup"
	validate := newValidateCommand(stdout, stderr)
	validate.Aliases = []string{"v"}
	validate.GroupID = "setup"
	root.AddCommand(setup, initCommand, validate)

	for _, item := range []struct{ name, short string }{
		{"plan", "Show the changes required to reach desired state"},
		{"up", "Create, start, and configure SSH for selected virtual machines"},
		{"start", "Start selected stopped virtual machines"},
		{"stop", "Stop selected running virtual machines"},
		{"restart", "Restart selected virtual machines"},
		{"reload", "Stop, re-read the configuration, and converge (stop + up)"},
		{"recreate", "Destroy and recreate selected virtual machines"},
		{"status", "Show deployment state"},
		{"destroy", "Destroy the deployment, or remove selected nodes from it"},
	} {
		command := newLifecycleCommand(item.name, item.short, stdout, stderr)
		command.GroupID = "lifecycle"
		root.AddCommand(command)
	}
	purge := newPurgeCommand(stdout, stderr)
	purge.GroupID = "lifecycle"
	root.AddCommand(purge)
	ssh := rawOperation(
		"ssh [node] [-- command [args...]]",
		"Open SSH or run a command in a guest",
		`Open an interactive SSH session to a node, or run a remote command exactly
as plain ssh would: everything after -- is joined and parsed by the remote
shell, so quotes, pipes, and semicolons work the way they do with ssh.
Flags before -- belong to Farrow; everything before -- must be nothing or one
known node; an unknown node before -- is refused.
Without --, a known first argument selects the node; otherwise all arguments
run as a command on the default node, with a warning. Use -- in scripts to
make the node/command boundary explicit.`,
		`  farrow ssh                          # open the default node
  farrow ssh meta                     # open one named node
  farrow ssh meta -- uptime           # run a remote command
  farrow ssh meta -- 'df -h /data; id'  # a shell line, quoted like plain ssh`,
		stdout, stderr, func(ctx context.Context, arguments []string, stdout, stderr io.Writer) (commandOutcome, error) {
			return runSSH(ctx, "ssh", arguments, stdout, stderr)
		})
	ssh.ValidArgsFunction = nodeCompletion(false, true)
	ssh.GroupID = "access"
	execCommand := rawOperation(
		"exec [node] -- <command> [args...]",
		"Run a command in a guest",
		`Run a remote command over the deployment SSH connection and pass its exit
status through. Multiple arguments after -- keep their argument boundaries,
including spaces, quotes and empty values. A single command string keeps the
shell shorthand; use sh -c for explicit shell expressions.
Everything before -- must be nothing or one known node.
For convenience, -- may be omitted: a known first argument selects the node;
otherwise the command runs on the default node, with a warning.`,
		`  farrow exec -- hostname
  farrow exec meta -- systemctl is-active postgresql
  farrow exec meta -- 'uptime; id'
  farrow exec meta -- sh -c 'exit 17'
  farrow --json exec meta -- uname -a`,
		stdout, stderr, func(ctx context.Context, arguments []string, stdout, stderr io.Writer) (commandOutcome, error) {
			return runSSH(ctx, "exec", arguments, stdout, stderr)
		})
	execCommand.ValidArgsFunction = nodeCompletion(false, true)
	execCommand.Aliases = []string{"ex"}
	execCommand.GroupID = "access"
	provision := newProvisionCommand(stdout, stderr)
	provision.GroupID = "access"
	sshConfig := newSSHConfigCommand(stdout, stderr)
	sshConfig.GroupID = "access"
	hosts := newHostsCommand(stdout, stderr)
	hosts.GroupID = "access"
	logs := newLogsCommand(stdout, stderr)
	logs.GroupID = "access"
	root.AddCommand(ssh, execCommand, logs, provision, sshConfig, hosts)

	update := newUpdateCommand(stdout, stderr)
	update.GroupID = "images"
	images := newImageCommand(stdout, stderr)
	images.GroupID = "images"
	repository := newRepoCommand(stdout, stderr)
	repository.GroupID = "images"
	root.AddCommand(update, images, repository)

	doctor := &cobra.Command{
		Use:     "doctor",
		Aliases: []string{"dt"},
		Short:   "Check host capabilities",
		Long: `Probe QEMU, native acceleration, firmware, OpenSSH, a short accelerated boot
test, and host-network readiness. Missing network setup is informational;
missing compute capability fails the check.`,
		Example: `  farrow doctor
  farrow --json doctor
  farrow doctor --verbose`,
		Args:    cobra.NoArgs,
		GroupID: "host",
		RunE: func(command *cobra.Command, _ []string) error {
			outcome, err := runDoctor(command.Context(), stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	network := newNetworkCommand(stdout, stderr)
	network.GroupID = "host"
	root.AddCommand(doctor, network)

	versionCommand := &cobra.Command{
		Use:     "version",
		Aliases: []string{"ver"},
		Short:   "Print build version",
		Long: `Print the Farrow version, source commit, build time, operating system, and
architecture.`,
		Example: `  farrow version
  farrow --json version`,
		Args:    cobra.NoArgs,
		GroupID: "advanced",
		RunE: func(command *cobra.Command, _ []string) error {
			return collectCommandOutcome(command.Context(), runVersionCommand())
		},
	}
	completion := newCompletionCommand(root, stdout, stderr)
	completion.GroupID = "advanced"
	root.AddCommand(versionCommand, completion)
	configureAliasDiscovery(root)
	completeInventoryFiles(root)
	return root
}

// completeInventoryFiles limits every --file completion to YAML inventories.
func completeInventoryFiles(command *cobra.Command) {
	if command.Flags().Lookup("file") != nil {
		_ = command.MarkFlagFilename("file", "yml", "yaml")
	}
	for _, child := range command.Commands() {
		completeInventoryFiles(child)
	}
}

func executeCLI(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	collector := &commandOutcomeCollector{}
	ctx = context.WithValue(ctx, commandOutcomeContextKey{}, collector)
	root := newRootCommand(stdout, stderr)
	root.SetArgs(arguments)
	if errors.Is(ctx.Err(), context.Canceled) {
		return reportCancelled(nil, stdout, stderr)
	}
	executed, err := root.ExecuteContextC(ctx)
	if errors.Is(ctx.Err(), context.Canceled) {
		return reportCancelled(err, stdout, stderr)
	}
	if writeErr := outputWriteError(stdout); writeErr != nil {
		errorf(stderr, "write command output: %v", writeErr)
		return exitRuntime
	}
	if err == nil {
		return renderCommandOutcome(collector.value, stdout, stderr)
	}
	if executed == nil {
		executed = root
	}
	if errors.Is(err, ErrCancelled) {
		return reportCancelled(err, stdout, stderr)
	}
	var typed typedCommandError
	if errors.As(err, &typed) {
		return renderTypedCommandError(typed, stdout, stderr)
	}
	if !structuredPayloadWritten(stdout) {
		if structuredOutput(stdout) {
			if code := encodeJSON(stdout, stderr, commandFailure{Error: "usage", Message: err.Error()}); code != exitOK {
				return code
			}
		}
	}
	errorf(stderr, "%v", err)
	if _, writeErr := fmt.Fprintf(stderr, "run '%s --help' for usage\n", executed.CommandPath()); writeErr != nil {
		return exitRuntime
	}
	return exitUsage
}

func renderTypedCommandError(typed typedCommandError, stdout, stderr io.Writer) int {
	failure := typed.commandFailure()
	if structuredOutput(stdout) && !structuredPayloadWritten(stdout) {
		payload := any(failure)
		if typed.commandPayload() != nil {
			payload = typed.commandPayload()
		}
		if code := encodeJSON(stdout, stderr, payload); code != exitOK {
			return code
		}
	} else if boundary, ok := typed.(*commandBoundaryError); ok && boundary.text != nil {
		if renderErr := boundary.text(stdout, stderr); renderErr != nil {
			errorf(stderr, "write command failure: %v", renderErr)
			return exitRuntime
		}
	}
	if boundary, ok := typed.(*commandBoundaryError); !ok || !boundary.silent {
		printFailure(stderr, failure)
	}
	return typed.exitCode()
}

// printFailure is the one text shape of a failed command: the error line, the
// failing program's stderr tail, the full argv under --verbose, and next.
func printFailure(stderr io.Writer, failure commandFailure) {
	errorf(stderr, "%s", failure.Message)
	if failure.Command != nil {
		if lines := strings.Split(failure.Command.Stderr, "\n"); failure.Command.Stderr != "" && len(lines) > 1 {
			for _, line := range lines {
				bestEffortf(stderr, "  %s\n", line)
			}
		}
		debugf(stderr, "command: %s", execx.Display(failure.Command.Argv[0], failure.Command.Argv[1:]...))
	}
	if failure.Next != "" {
		bestEffortf(stderr, "%s %s\n", styled(stderr, ansiDim, "next:"), failure.Next)
	}
}

func reportCancelled(err error, stdout, stderr io.Writer) int {
	message := ErrCancelled.Error()
	if err != nil && errors.Is(err, ErrCancelled) {
		message = err.Error()
	}
	if structuredOutput(stdout) && !structuredPayloadWritten(stdout) {
		if code := encodeJSON(stdout, stderr, commandFailure{Error: "cancelled", Message: message}); code != exitOK {
			return code
		}
	}
	bestEffortf(stderr, "%s %s\n", styled(stderr, ansiRed, "error:"), message)
	return exitCancelled
}

func exitCategory(code int) string {
	switch code {
	case exitUsage:
		return "usage"
	case exitCapability:
		return "capability"
	case exitConflict:
		return "conflict"
	case exitPartial:
		return "partial"
	case exitResource:
		return "resource"
	case exitIntegrity:
		return "integrity"
	case exitCancelled:
		return "cancelled"
	default:
		return "runtime"
	}
}
