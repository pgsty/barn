package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/config"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/hostconfig"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/linuxvm"
	netpreflight "github.com/pgsty/barn/internal/network/preflight"
	"github.com/pgsty/barn/internal/network/subnet"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/sshconfig"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/version"
	"golang.org/x/term"
)

type lifecycleOptions struct {
	ResourceInputs []config.ResourceResolution
	Applied        *spec.Resolved
	vmInputs
	Force            bool
	DeletePersistent bool
	Purge            bool
	NoWait           bool
	Parallelism      *int
	Rollback         bool
	ConfigPath       string
	Repository       string
	Mirror           bool
}

type persistentDeleteError struct{ err error }

func (e *persistentDeleteError) Error() string { return "delete persistent disks: " + e.err.Error() }
func (e *persistentDeleteError) Unwrap() error { return e.err }

type lifecyclePartialFailure struct {
	commandFailure
	Failures   []linuxvm.NodeFailure `json:"failures"`
	RolledBack []string              `json:"rolled_back,omitempty"`
}

type sshConfigReconciler interface {
	InstallSSHConfig(context.Context, string, string) (sshconfig.Result, error)
	RemoveSSHConfig(string, string) (sshconfig.Result, error)
}

type lifecycleResult struct {
	Resources    []config.ResourceResolution `json:"resources,omitempty"`
	retryOptions lifecycleRetryOptions
	Source       string `json:"source,omitempty"`
	Error        string `json:"error,omitempty"`
	linuxvm.Status
	SSHConfig  *sshconfig.Result     `json:"ssh_config,omitempty"`
	Warnings   []lifecycleWarning    `json:"warnings,omitempty"`
	Failures   []linuxvm.NodeFailure `json:"failures,omitempty"`
	RolledBack []string              `json:"rolled_back,omitempty"`
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
	return warnings
}

// planSize prints an inventory size as written: 128 GiB rather than 128.0 GiB,
// switching to TiB from 1024 GiB.
func planSize(value int64) string {
	return strings.Replace(progressBytes(value), ".0 ", " ", 1)
}

// planDataDisks names the data disks plan will create, which the inventory
// often implies (every node gets /data unless vm_disks says otherwise).
func planDataDisks(resolved spec.Resolved, selected []string) string {
	wanted := make(map[string]bool, len(selected))
	for _, name := range selected {
		wanted[name] = true
	}
	var names, descriptions []string
	for _, node := range resolved.Nodes {
		if len(wanted) != 0 && !wanted[node.Name] {
			continue
		}
		disks := make([]string, 0, len(node.Disks))
		for _, disk := range node.Disks {
			filesystem := disk.Filesystem
			if filesystem == "" {
				filesystem = "auto"
			}
			description := fmt.Sprintf("%s %s (%s fs", disk.Mount, planSize(disk.Size), filesystem)
			if disk.Persistent {
				description += ", persistent"
			}
			disks = append(disks, description+")")
		}
		description := strings.Join(disks, ", ")
		if description == "" {
			description = "none"
		}
		names, descriptions = append(names, node.Name), append(descriptions, description)
	}
	if len(descriptions) == 0 {
		return "none"
	}
	if slices.Equal(descriptions[1:], descriptions[:len(descriptions)-1]) {
		if len(descriptions) == 1 {
			return descriptions[0]
		}
		return descriptions[0] + " on each node"
	}
	for index := range descriptions {
		descriptions[index] = names[index] + ": " + descriptions[index]
	}
	return strings.Join(descriptions, "; ")
}

func printWarnings(out io.Writer, warnings []string) {
	for _, warning := range warnings {
		warningf(out, "%s", warning)
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
		return fmt.Errorf("%s requires --yes (or --force) when stdin is not a terminal", action)
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

func lifecycleReadsConfig(command string) bool {
	return command == "up" || command == "plan" || command == "recreate" || command == "reload" || command == "resize"
}

func loadLifecycleConfig(command, configPath string, nodes []string, input vmInputs) (spec.Resolved, string, []config.ResourceResolution, error) {
	if !lifecycleReadsConfig(command) {
		return spec.Resolved{}, "", nil, nil
	}
	file, path, err := config.Discover("", configPath, config.InventoryOptions{Resources: input.Resources, Nodes: nodes})
	if errors.Is(err, config.ErrNoConfig) {
		return spec.Resolved{}, "", nil, nil
	}
	if err != nil {
		return spec.Resolved{}, "", nil, err
	}
	resolved, err := file.Resolve()
	if err != nil {
		return spec.Resolved{}, "", nil, err
	}
	if err := file.CheckArchOverride(nodes, input.Image, input.Arch, input.Sources["arch"]); err != nil {
		return spec.Resolved{}, "", nil, err
	}
	return resolved, path, resourceInputSources(file.Resources, input, nodes), nil
}

func currentDeploymentResolved() (spec.Resolved, error) {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return spec.Resolved{}, err
	}
	deploymentState, err := (state.Store{Root: root}).ReadDeployment()
	if err != nil {
		if errors.Is(err, state.ErrInvalidDeploymentNetwork) {
			return spec.Resolved{}, err
		}
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

func printPrivateStatus(out io.Writer, status linuxvm.Status) {
	printStatusTable(out, status, true)
}

func printStatusTable(out io.Writer, status linuxvm.Status, errors bool) {
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
			root := "-"
			if node.RootDisk > 0 {
				root = planSize(node.RootDisk)
			}
			rows = append(rows, []string{node.Name, node.Address, phase, node.Image, compactSpec(node.CPUs, node.Memory), root})
		}
		printTable(out, []string{"NAME", "ADDRESS", "STATE", "IMAGE", "SPEC", "ROOT"}, rows, 2)
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
		textField(out, 5, "note", status.Note)
	}
}

// Only a successful SSH readiness probe justifies a ready label.
func allNodesReady(status linuxvm.Status) bool {
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
		result, err := reconciler.InstallSSHConfig(ctx, "barn", "")
		return &result, err
	case "remove":
		result, err := reconciler.RemoveSSHConfig("barn", "")
		return &result, err
	default:
		return nil, nil
	}
}

func fullDeploymentSSHManager(manager linuxvm.Manager) linuxvm.Manager {
	manager.Nodes = nil
	return manager
}

func classifyPrivateLifecycleError(err error, operationID string) error {
	boundary := newCommandError(exitRuntime, err).withOperation(operationID)
	var preflight *linuxvm.NetworkPreflightError
	if errors.As(err, &preflight) {
		boundary = boundary.withPayload(preflight.Report)
	}
	if partial := linuxvm.IsolatedPartial(err); partial != nil {
		boundary = boundary.withPayload(lifecyclePartialFailure{commandFailure: boundary.failure, Failures: partial.Failures, RolledBack: partial.RolledBack})
	}
	return boundary
}

func buildRetryOptions(command, repository string, options lifecycleOptions) lifecycleRetryOptions {
	retryOptions := lifecycleRetryOptions{Repository: repository, NoWait: options.NoWait, Rollback: options.Rollback, Parallelism: options.Parallelism, vmInputs: options.vmInputs}
	if repository == image.DefaultRepositoryURL && strings.TrimSpace(options.Repository) == "" && !options.Mirror && strings.TrimSpace(os.Getenv("BARN_REPO")) == "" {
		retryOptions.Repository = "" // a retry finds the default by itself
	}
	if command == "start" || command == "restart" {
		// Resume an interrupted start without changing its guest-convergence
		// semantics, or stopping successful peers for a second time.
		retryOptions.Action = "start"
	}
	if command == "resize" {
		// A partial resize must retry the resource operation itself. Falling back
		// to `up` would silently ignore the inventory resource change.
		retryOptions.Action = "resize"
	}
	return retryOptions
}

func runPlanCommand(parent context.Context, manager linuxvm.Manager, resolved spec.Resolved, nodes []string, source string, options lifecycleOptions, retryOptions lifecycleRetryOptions, operationID string) (commandOutcome, error) {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	plan, err := manager.Plan(ctx, resolved)
	if err != nil {
		return commandOutcome{}, classifyPrivateLifecycleError(err, operationID)
	}
	plan.Source = source
	applyCommand := func(action string, selected []string) string {
		return lifecycleNodeCommand(action, source, selected, retryOptions)
	}
	return commandOutcome{payload: struct {
		linuxvm.LifecyclePlan
		Resources []config.ResourceResolution `json:"resources"`
	}{plan, options.ResourceInputs}, text: func(stdout, _ io.Writer) error {
		renderPlanText(stdout, plan, resolved, nodes, options, applyCommand)
		return nil
	}}, nil
}

func renderPlanText(stdout io.Writer, plan linuxvm.LifecyclePlan, resolved spec.Resolved, nodes []string, options lifecycleOptions, applyCommand func(string, []string) string) {
	printPlanTable(stdout, plan, resolved, options.Applied)
	printResourceCalculations(stdout, options.ResourceInputs)
	planState, planMessage := planSummary(plan)
	printResultSummary(stdout, "plan", planState, planMessage)
	textField(stdout, 12, "images", strings.Join(plan.Images, ", "))
	textField(stdout, 12, "resources", fmt.Sprintf("%d vCPU, %s RAM, %s virtual disk capacity", plan.CPUs, planSize(plan.Memory), planSize(plan.Storage)))
	textField(stdout, 12, "data", planDataDisks(resolved, plan.Nodes))
	for _, change := range plan.Changes {
		textField(stdout, 12, "change", change)
	}
	if plan.Blocked != "" {
		textField(stdout, 12, "blocked", plan.Blocked)
	}
	switch plan.Action {
	case "none":
		return
	case "repair":
		textField(stdout, 12, "repair", strings.Join(plan.Nodes, ", ")+"  (left mid-transition; apply: "+applyCommand("up", nodes)+")")
	}
	for _, group := range []struct {
		label string
		names []string
	}{{"start", plan.Start}, {"create", plan.Create}} {
		if len(group.names) == 0 {
			continue
		}
		detail := strings.Join(group.names, ", ")
		if plan.Blocked == "" && (plan.Action == "create" || plan.Action == "converge" || plan.Action == "start" || plan.Action == "repair") {
			detail += "  (apply: " + applyCommand("up", nodes) + ")"
		} else if plan.Blocked == "" && plan.WholeDeployment {
			detail += "  (included in whole-deployment recreation)"
		}
		textField(stdout, 12, group.label, detail)
	}
	if len(plan.Recreate) != 0 {
		detail := strings.Join(plan.Recreate, ", ")
		if plan.Blocked == "" {
			selected := plan.Recreate
			if plan.WholeDeployment {
				selected = nodes
			}
			detail += "  (apply: " + applyCommand("recreate", selected) + ")"
		}
		textField(stdout, 12, "recreate", detail)
	} else if plan.Action == "recreate" && plan.Blocked == "" {
		textField(stdout, 12, "recreate", "deployment-level settings changed  (apply: "+applyCommand("recreate", nodes)+")")
	}
	if len(plan.Resize) != 0 {
		detail := strings.Join(plan.Resize, ", ")
		if plan.Blocked == "" && plan.Action == "resize" {
			detail += "  (apply: " + applyCommand("resize", plan.Resize) + ")"
		}
		textField(stdout, 12, "resize", detail)
	}
	if len(plan.Missing) != 0 {
		textField(stdout, 12, "missing", strings.Join(plan.Missing, ", ")+"  (not in the inventory; remove with: barn destroy "+strings.Join(plan.Missing, " ")+")")
	}
	if plan.Destructive {
		textField(stdout, 12, "disks", "root and non-persistent data disks will be replaced; persistent data disks are retained")
	}
}

func buildLifecycleOperation(
	command string,
	manager *linuxvm.Manager,
	resolved spec.Resolved,
	nodes []string,
	options lifecycleOptions,
	operationID string,
	stderr io.Writer,
) (time.Duration, func(context.Context) (linuxvm.Status, error), error) {
	switch command {
	case "up":
		return 0, func(ctx context.Context) (linuxvm.Status, error) { return manager.Up(ctx, resolved) }, nil
	case "start":
		return withReadinessTimeout(10*time.Minute, resolved), manager.Start, nil
	case "stop":
		return 5 * time.Minute, manager.Stop, nil
	case "restart":
		return withReadinessTimeout(15*time.Minute, resolved), manager.Restart, nil
	case "reload":
		return 0, func(ctx context.Context) (linuxvm.Status, error) { return manager.Reload(ctx, resolved) }, nil
	case "resize":
		return 0, func(ctx context.Context) (linuxvm.Status, error) { return manager.Resize(ctx, resolved) }, nil
	case "status":
		return 15 * time.Second, manager.Status, nil
	case "recreate":
		// Resolve the exact replacement scope before asking: the prompt names
		// every applied node the operation destroys, and an inventory that omits
		// a node is refused before anyone is asked to confirm.
		targets, scopeErr := manager.RecreateTargets(resolved)
		if scopeErr != nil {
			return 0, nil, classifyPrivateLifecycleError(scopeErr, operationID)
		}
		if !options.Force && term.IsTerminal(int(os.Stdin.Fd())) {
			bestEffortf(stderr, "recreate: %s; root and non-persistent data disks will be replaced; persistent data disks stay\n", strings.Join(targets, ", "))
		}
		if err := confirmCLIAction(options.Force, "recreate", stderr); err != nil {
			if errors.Is(err, ErrCancelled) {
				return 0, nil, err
			}
			return 0, nil, newUsageError(err)
		}
		return 0, func(ctx context.Context) (linuxvm.Status, error) { return manager.RecreateResolved(ctx, resolved) }, nil
	case "destroy", "purge":
		deletePersistent, purge := options.DeletePersistent, options.Purge
		if command == "purge" {
			if len(nodes) != 0 {
				return 0, nil, newUsageError(errors.New("purge does not accept node selectors"))
			}
			deletePersistent, purge = true, true
		} else {
			if len(nodes) != 0 && (deletePersistent || purge) {
				return 0, nil, newUsageError(errors.New("--delete-persistent and --purge apply to whole-deployment destroy only"))
			}
			if !options.Force && term.IsTerminal(int(os.Stdin.Fd())) {
				bestEffortln(stderr, destroyScope(resolved, nodes, deletePersistent, purge))
			}
			if err := confirmCLIAction(options.Force, "destroy", stderr); err != nil {
				if errors.Is(err, ErrCancelled) {
					return 0, nil, err
				}
				return 0, nil, newUsageError(err)
			}
		}
		if command == "purge" || purge {
			// Terminal disposal removes every disk immediately afterward, so skip
			// guest OS graceful shutdown and power off QEMU directly via QMP quit.
			manager.ImmediateStop = true
		}
		return 10 * time.Minute, func(ctx context.Context) (linuxvm.Status, error) {
			if len(nodes) != 0 {
				return manager.DestroyNodes(ctx)
			}
			status, err := manager.Destroy(ctx)
			if err != nil || !deletePersistent {
				return status, err
			}
			reportLifecycleNodes(manager.Progress, status.Nodes, activity.Event{
				Phase: command, State: "cleaning", Message: "removing persistent data disks",
			})
			deleted, err := manager.DeletePersistent(ctx)
			if err != nil {
				return status, &persistentDeleteError{err: err}
			}
			status.Message = fmt.Sprintf("destroyed node artifacts; explicitly deleted %d persistent data disk(s); image cache and keys preserved", len(deleted))
			reportLifecycleNodes(manager.Progress, status.Nodes, activity.Event{
				Phase: command, State: "removed", Message: "node and persistent disks removed", Done: true,
			})
			return status, nil
		}, nil
	default:
		return 0, nil, newUsageError(fmt.Errorf("unsupported private command %q", command))
	}
}

func runPrivateCommand(parent context.Context, command string, resolved spec.Resolved, nodes []string, repository, source string, options lifecycleOptions, stderr io.Writer) (commandOutcome, error) {
	retryOptions := buildRetryOptions(command, repository, options)
	operationID := ""
	if command != "status" && command != "plan" {
		var err error
		parent, operationID, err = operationContext(parent)
		if err != nil {
			return commandOutcome{}, newRuntimeError(err)
		}
	}
	var progressItem *progress
	manager := linuxvm.Manager{BarnVersion: version.Version, OperationID: operationID, Repository: repository, NoWait: options.NoWait, Parallelism: options.Parallelism, RollbackFailed: options.Rollback, Nodes: append([]string(nil), nodes...)}
	report := deferredProgressReporter(&progressItem)
	resourceNodes := make(map[string]spec.Node, len(resolved.Nodes))
	for _, node := range resolved.Nodes {
		resourceNodes[node.Name] = node
	}
	manager.Progress = func(event activity.Event) {
		if node, ok := resourceNodes[event.Node]; ok && event.CPUs == 0 {
			event.CPUs, event.Memory = node.CPUs, node.Memory
		}
		report.Report(event)
	}
	if command == "plan" {
		return runPlanCommand(parent, manager, resolved, nodes, source, options, retryOptions, operationID)
	}
	timeout, operation, err := buildLifecycleOperation(command, &manager, resolved, nodes, options, operationID, stderr)
	if err != nil {
		return commandOutcome{}, err
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
		progressItem = startLifecycleProgress(ctx, stderr, command)
	}
	status, err := operation(ctx)
	err = lifecycleOverrideRecovery(err, source, nodes, retryOptions)
	status.OperationID = operationID
	if errors.Is(parent.Err(), context.Canceled) {
		return handleCancelledLifecycle(parent, progressItem, manager, command, source, nodes, retryOptions, status, err, stderr)
	}
	lifecycleSucceeded := err == nil
	deploymentHasNodes := command != "purge" && (command != "destroy" || destroyLeavesDeploymentNodes(resolved, nodes))
	if options.NoWait && err == nil {
		status.Message = strings.TrimSpace(status.Message + "; guest readiness and metadata refresh skipped (--no-wait)")
	}
	metadataRetry := lifecycleRetryCommand(source, nodes, retryOptions)
	if command == "destroy" {
		// The selected nodes no longer exist. Retrying up with their names can
		// recreate them, and the inventory may still name the former control.
		metadataRetry = "update the inventory to match the remaining nodes and control role, then run " + lifecycleRetryCommand(source, nil, retryOptions)
	}
	reconciledSSHConfig, warnings := finishLifecycleIntegrations(ctx, command, deploymentHasNodes, options.NoWait,
		metadataRetry, status, err, fullDeploymentSSHManager(manager), deferredProgressReporter(&progressItem))
	if lifecycleSucceeded && (command == "purge" || command == "destroy" && options.Purge) {
		reportLifecycleNodes(manager.Progress, status.Nodes, activity.Event{
			Phase: command, State: "cleaning", Message: "removing deployment keys and state",
		})
		if purgeErr := (linuxvm.Manager{BarnVersion: version.Version}).PurgeDeployment(ctx); purgeErr != nil {
			if err == nil {
				err = fmt.Errorf("destroy succeeded but purge failed: %w", purgeErr)
			} else {
				err = fmt.Errorf("%v; deployment purge also failed: %w", err, purgeErr)
			}
		} else {
			status.Message = "destroyed node artifacts and persistent data disks; purged deployment keys and state; images remain cached"
			reportLifecycleNodes(manager.Progress, status.Nodes, activity.Event{
				Phase: command, State: "purged", Message: "node, disks, keys and state removed", Done: true,
			})
		}
	}
	if errors.Is(parent.Err(), context.Canceled) {
		return handleCancelledLifecycle(parent, progressItem, manager, command, source, nodes, retryOptions, status, err, stderr)
	}
	progressItem.Stop(err)
	if err != nil {
		return lifecycleFailureOutcome(command, operationID, source, options, retryOptions, status, reconciledSSHConfig, warnings, err)
	}
	if verboseOutput(stderr) && (command == "up" || command == "reload") {
		suggestHostsPublication(resolved, stderr)
	}
	if startupCommand(command) {
		warnLifecycleImages(ctx, resolved, repository, stderr)
	}
	result := lifecycleResult{Resources: options.ResourceInputs, retryOptions: retryOptions, Source: source, Status: status, SSHConfig: reconciledSSHConfig, Warnings: warnings}
	return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
		printLifecycleResult(stdout, command, result, options.NoWait)
		return nil
	}}, nil
}

func handleCancelledLifecycle(parent context.Context, progressItem *progress, manager linuxvm.Manager, command, source string, nodes []string, retryOptions lifecycleRetryOptions, status linuxvm.Status, err error, stderr io.Writer) (commandOutcome, error) {
	progressItem.Stop(ErrCancelled)
	if lifecycleCanRetry(command) {
		textField(stderr, 10, "resume", lifecycleRetryCommand(source, nodes, retryOptions))
	}
	if command != "status" {
		recordCancelledLifecycle(parent, manager, command, status, err, stderr)
	}
	return commandOutcome{}, ErrCancelled
}

func lifecycleFailureOutcome(command, operationID, source string, options lifecycleOptions, retryOptions lifecycleRetryOptions, status linuxvm.Status, sshCfg *sshconfig.Result, warnings []lifecycleWarning, err error) (commandOutcome, error) {
	var partial *linuxvm.PartialError
	isPartial := errors.As(err, &partial)
	if !isPartial && (len(status.Nodes) == 0 || command != "status") {
		return commandOutcome{}, classifyPrivateLifecycleError(err, operationID)
	}
	result := lifecycleResult{Resources: options.ResourceInputs, retryOptions: retryOptions, Source: source, Error: "partial", Status: status, SSHConfig: sshCfg, Warnings: warnings}
	result.Message = err.Error()
	if isPartial {
		result.Failures = partial.Failures
		result.RolledBack = partial.RolledBack
	}
	failure := newCommandError(exitPartial, err).withOperation(operationID)
	// A global error joined to node failures keeps its own class and line.
	result.Error = failure.failure.Error
	if linuxvm.IsolatedPartial(err) != nil {
		// The node table carries the detail; stderr keeps one line.
		failure.failure.Message = fmt.Sprintf("%d of %d node(s) failed", len(partial.Failures), partial.Total)
	}
	failure = failure.withPayload(result).withText(func(stdout, _ io.Writer) error {
		printLifecycleResult(stdout, command, result, options.NoWait)
		return nil
	})
	return commandOutcome{}, failure
}

func warnLifecycleImages(ctx context.Context, resolved spec.Resolved, repository string, stderr io.Writer) {
	service, serviceErr := imageService(repository, false, nil)
	if serviceErr != nil {
		return
	}
	session, sessionErr := service.OpenCatalog()
	if sessionErr != nil {
		return
	}
	seen := make(map[string]struct{})
	guestArch := lifecycleImageArch(resolved)
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

// recordCancelledLifecycle keeps the audit trail honest after an interrupt:
// the engine may have changed node state before the signal landed, so the
// event is appended on a context that outlives the cancellation, exactly as
// provision does for its completion event.
func recordCancelledLifecycle(parent context.Context, manager linuxvm.Manager, command string, status linuxvm.Status, err error, stderr io.Writer) {
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

// confirmPurge asks only when --ask was explicitly requested, including when
// stdin is a pipe. A missing or negative answer cancels before any mutation.
func confirmPurge(ask bool, scope string, stdin io.Reader, stderr io.Writer) error {
	if !ask {
		return nil
	}
	if _, err := fmt.Fprintln(stderr, scope); err != nil {
		return fmt.Errorf("write purge scope: %w", err)
	}
	return confirmPlan("Purge this deployment? [y/N] ", false, stdin, stderr)
}

func purgeScope(resolved spec.Resolved) string {
	var scope strings.Builder
	scope.WriteString(strings.Replace(destroyScope(resolved, nil, true, true), "destroy scope:", "purge scope:", 1))
	for _, node := range resolved.Nodes {
		ref := node.Image
		if ref == "" {
			ref = resolved.Image
		}
		fmt.Fprintf(&scope, "\n  %s  %s  %s/%s  %d vCPU  %s RAM", node.Name, node.Address, ref, lifecycleImageArch(resolved), node.CPUs, planSize(node.Memory))
	}
	return scope.String()
}

// runPurgeCommand is the whole-deployment disposal path. A valid deployment
// goes through the normal identity-checked destroy engine. When an earlier
// whole destroy already removed state.json, the residual path can still delete
// strictly inventoried persistent disks and allowlisted keys. A missing state
// document never authorizes deletion of retained node artifacts, whose process
// and path identity can no longer be proven.
func runPurgeCommand(parent context.Context, ask bool, parallelism *int, stderr io.Writer) (_ commandOutcome, returnErr error) {
	resolved, err := currentDeploymentResolved()
	if err == nil {
		if err := confirmPurge(ask, purgeScope(resolved), os.Stdin, stderr); err != nil {
			return commandOutcome{}, err
		}
		return runPrivateCommand(parent, "purge", resolved, nil, "", "applied deployment state", lifecycleOptions{Force: true, DeletePersistent: true, Purge: true, Parallelism: parallelism}, stderr)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return commandOutcome{}, newCommandError(exitIntegrity, fmt.Errorf("read deployment state for purge: %w", err))
	}

	root, rootErr := state.ResolveDataRoot()
	if rootErr != nil {
		return commandOutcome{}, newCommandError(exitIntegrity, rootErr)
	}
	rootExists, verifyErr := linuxvm.VerifyResidualPurgeRoot(root)
	if verifyErr != nil {
		code := exitIntegrity
		if !rootExists {
			code = exitRuntime
		}
		return commandOutcome{}, newCommandError(code, verifyErr)
	}
	if !rootExists {
		status := linuxvm.Status{Message: "no deployment to purge; image cache and host network remain installed"}
		result := lifecycleResult{Status: status}
		return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
			printItemResult(stdout, "deployment", "unchanged", status.Message)
			return nil
		}}, nil
	}
	if err := confirmPurge(ask, fmt.Sprintf("purge scope: residual deployment data under %s, including retained persistent data disks and the deployment keys (images stay cached)", root), os.Stdin, stderr); err != nil {
		return commandOutcome{}, err
	}

	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	progressItem := startLifecycleProgress(ctx, stderr, "purge")
	defer func() { progressItem.Stop(returnErr) }()
	manager := linuxvm.Manager{BarnVersion: version.Version, Progress: progressItem.Report}
	progressItem.Report(activity.Event{Phase: "purge", Message: "Removing retained persistent disks"})
	deleted, deleteErr := manager.DeletePersistent(ctx)
	if deleteErr != nil {
		return commandOutcome{}, newCommandError(exitIntegrity, fmt.Errorf("deployment state is missing; refuse unsafe residual purge: %w", deleteErr))
	}
	progressItem.Report(activity.Event{Phase: "purge", Message: "Removing deployment keys and state"})
	if purgeErr := manager.PurgeDeployment(ctx); purgeErr != nil {
		return commandOutcome{}, newCommandError(exitRuntime, fmt.Errorf("purge residual deployment data: %w", purgeErr))
	}

	progressItem.Report(activity.Event{Phase: "purge", Message: "Removing the SSH client configuration"})
	sshResult, sshErr := manager.RemoveSSHConfig("barn", "")
	status := linuxvm.Status{Message: fmt.Sprintf("purged residual deployment data and %d persistent data disk(s); image cache and host network remain installed", len(deleted))}
	result := lifecycleResult{Status: status, SSHConfig: &sshResult}
	if sshErr != nil {
		result.Warnings = append(result.Warnings, sshIntegrationWarning("remove", sshErr))
	}
	return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
		printItemResult(stdout, "deployment", "purged", status.Message)
		textResult := result
		textResult.Message = ""
		printLifecycleResult(stdout, "purge", textResult, false)
		if sshResult.Changed {
			textField(stdout, 6, "ssh", fmt.Sprintf("%s %s", sshResult.Action, sshResult.Fragment))
		}
		return nil
	}}, nil
}

func runLifecycleCommand(ctx context.Context, command string, options lifecycleOptions, nodes []string, stderr io.Writer) (_ commandOutcome, returnErr error) {
	var err error
	if lifecycleReadsConfig(command) {
		options.vmInputs, err = options.vmInputs.resolve(true, true)
	}
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	if command != "status" && command != "plan" {
		var id string
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
		progressItem := startLifecycleProgress(ctx, stderr, command)
		defer func() { progressItem.Stop(returnErr) }()
	}
	if (options.DeletePersistent || options.Purge) && !options.Force && !term.IsTerminal(int(os.Stdin.Fd())) {
		return commandOutcome{}, newUsageError(errors.New("--delete-persistent and --purge require the separate --yes (or --force) destroy confirmation"))
	}
	if options.Purge {
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
	resolvedFile, source, persistedErr, outcome, done, err := resolveLifecycleTarget(ctx, command, &options, nodes, repository, stderr)
	if err != nil || done {
		return outcome, err
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
	if source == "" && lifecycleReadsConfig(command) {
		resolvedFile, options.ResourceInputs, err = applyStateResources(resolvedFile, nodes, options.vmInputs)
		if err != nil {
			return commandOutcome{}, newUsageError(err)
		}
	}
	resolvedFile = applyLifecycleOverrides(resolvedFile, nodes, options)
	if source == "" {
		source = "applied deployment state"
	}
	if lifecycleReadsConfig(command) && !structuredOutput(stderr) && (verboseOutput(stderr) || source == "applied deployment state") {
		textField(stderr, 10, "inventory", source)
	}
	printWarnings(stderr, configurationWarnings(resolvedFile))
	printLifecycleOverrides(stderr, options)
	if command != "plan" {
		printResourceCalculations(stderr, options.ResourceInputs)
	}
	outcome, err = runPrivateCommand(ctx, command, resolvedFile, nodes, repository, source, options, stderr)
	return maybeRetryAfterHostSetup(ctx, command, resolvedFile, nodes, repository, source, options, persistedErr, outcome, err, stderr)
}

func resolveLifecycleTarget(ctx context.Context, command string, options *lifecycleOptions, nodes []string, repository string, stderr io.Writer) (spec.Resolved, string, error, commandOutcome, bool, error) {
	resolvedFile, source, resources, err := loadLifecycleConfig(command, options.ConfigPath, nodes, options.vmInputs)
	options.ResourceInputs = resources
	if err != nil {
		return spec.Resolved{}, "", nil, commandOutcome{}, false, newUsageError(err)
	}
	hasConfig := source != ""
	persisted, persistedErr := currentDeploymentResolved()
	if persistedErr != nil && !errors.Is(persistedErr, os.ErrNotExist) {
		return spec.Resolved{}, "", persistedErr, commandOutcome{}, false, deploymentReadError(persistedErr)
	}
	if persistedErr == nil {
		options.Applied = &persisted
		if !hasConfig {
			resolvedFile = persisted
			hasConfig = true
		}
	}
	if !hasConfig && command == "up" && interactiveTextSession(stderr) {
		setupOptions := implicitSetupOptions(*options, repository)
		setupOptions.vmInputs = options.vmInputs
		if _, setupErr := runSetupCommand(ctx, "", setupOptions, outputFormatFor(stderr), verboseOutput(stderr), stderr); setupErr != nil {
			return spec.Resolved{}, "", persistedErr, commandOutcome{}, false, setupErr
		}
		resolvedFile, source, options.ResourceInputs, err = loadLifecycleConfig(command, options.ConfigPath, nodes, options.vmInputs)
		if err != nil {
			return spec.Resolved{}, "", persistedErr, commandOutcome{}, false, newUsageError(err)
		}
		hasConfig = source != ""
	}
	if hasConfig {
		return resolvedFile, source, persistedErr, commandOutcome{}, false, nil
	}
	switch {
	case command == "destroy" && len(nodes) == 0 && (options.DeletePersistent || options.Purge):
		return spec.Resolved{}, "", persistedErr, commandOutcome{}, false, newCommandError(exitConflict, failure.WithNext(errors.New("no deployment state found; retained persistent disks are removed by purge"), "barn purge"))
	case command == "destroy" && len(nodes) == 0:
		status := linuxvm.Status{Message: "no deployment to destroy"}
		return spec.Resolved{}, "", persistedErr, commandOutcome{payload: lifecycleResult{Status: status}, text: func(stdout, _ io.Writer) error {
			printItemResult(stdout, "deployment", "unchanged", status.Message)
			return nil
		}}, true, nil
	case !lifecycleReadsConfig(command):
		return spec.Resolved{}, "", persistedErr, commandOutcome{}, false, newCommandError(exitConflict, errNoDeployment)
	case command == "up":
		return spec.Resolved{}, "", persistedErr, commandOutcome{}, false, newCommandError(exitUsage, failure.WithNext(config.ErrNoConfig, "barn setup --yes, then barn up; or pass -f <file>"))
	default:
		return spec.Resolved{}, "", persistedErr, commandOutcome{}, false, newCommandError(exitUsage, config.ErrNoConfig)
	}
}

func maybeRetryAfterHostSetup(ctx context.Context, command string, resolvedFile spec.Resolved, nodes []string, repository, source string, options lifecycleOptions, persistedErr error, outcome commandOutcome, err error, stderr io.Writer) (commandOutcome, error) {
	canPrepareHost := needsHostSetup(command, err)
	var networkFailure *linuxvm.NetworkPreflightError
	if !canPrepareHost && command == "up" && errors.As(err, &networkFailure) && errors.Is(persistedErr, os.ErrNotExist) && options.ConfigPath == "" {
		candidate := recognizeDefaultSetupTemplate(setupSelection{ConfigPath: source})
		canPrepareHost = candidate.Generated
	}
	if !canPrepareHost || !interactiveTextSession(stderr) {
		return outcome, err
	}
	// Supply missing host dependencies or repair the owned network, then
	// retry the original operation with the same inventory and selectors.
	infof(stderr, "preparing the host before starting nodes")
	setupOptions := implicitSetupOptions(options, repository)
	if source == "applied deployment state" || options.vmInputs.any() || !recognizeDefaultSetupTemplate(setupSelection{ConfigPath: source}).Generated {
		setupOptions.Applied = &resolvedFile
	}
	if _, setupErr := runSetupCommand(ctx, "", setupOptions, outputFormatFor(stderr), verboseOutput(stderr), stderr); setupErr != nil {
		return commandOutcome{}, setupErr
	}
	if source != "applied deployment state" {
		var loadErr error
		resolvedFile, source, options.ResourceInputs, loadErr = loadLifecycleConfig(command, options.ConfigPath, nodes, options.vmInputs)
		if loadErr != nil {
			return commandOutcome{}, newUsageError(loadErr)
		}
		resolvedFile = applyLifecycleOverrides(resolvedFile, nodes, options)
	}
	return runPrivateCommand(ctx, command, resolvedFile, nodes, repository, source, options, stderr)
}

// implicitSetupOptions prepares the host from an interactive lifecycle command.
// It runs only on a terminal and never assumes --yes: the plan of package and
// network changes is shown and confirmed once before any privileged step, the
// same contract as an explicit `barn setup`.
func implicitSetupOptions(options lifecycleOptions, repository string) setupCLIOptions {
	return setupCLIOptions{FilePath: options.ConfigPath, Mode: "host", Repo: repository}
}

// Setup can supply missing executables or restore an intact Barn network.
// Destructive commands keep their explicit confirmation and are never replayed.
func needsHostSetup(command string, err error) bool {
	switch command {
	case "up", "start", "restart", "reload":
	default:
		return false
	}
	var qemuError *platform.QEMUUnavailableError
	if errors.As(err, &qemuError) && !qemuError.SetupSupported {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	var preflight *linuxvm.NetworkPreflightError
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
			return linuxvm.UnknownNodeError(name, where, resolved.Nodes)
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
		scope += "; also deletes persistent data disks and the deployment state (keys are preserved)"
	default:
		scope += "; removes the deployment state; persistent data disks and keys are preserved"
	}
	return scope
}

// suggestHostsPublication reminds the user once per up that declared host
// aliases are reachable by name only after `barn hosts install`. Best
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
	if strings.Contains(string(data), "# barn:begin") {
		return
	}
	warningf(stderr, "declared host aliases are not published; run `barn hosts install --yes` to add them to %s", target)
}
