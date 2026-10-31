package private

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/lock"
	"github.com/pgsty/barn/internal/process"
	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/qmp"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/sshkeys"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/vm"
)

var errResizeOffline = errors.New("online resize is unavailable")

type resizeGuestResources struct {
	CPUs   int
	Memory int64
}

func resizeTargetNode(value spec.Resolved, name string) (spec.Node, bool) {
	for _, node := range value.Nodes {
		if node.Name == name {
			return node, true
		}
	}
	return spec.Node{}, false
}

func resizeTargets(current, desired spec.Resolved, selected []string, store state.Store) (spec.Resolved, []string, error) {
	merged, err := applyNodeSelection(current, desired, selected)
	if err != nil {
		return spec.Resolved{}, nil, err
	}
	merged = materializeExistingForwardPorts(merged, current)
	hasState := func(name string) bool {
		_, readErr := store.ReadNode(name)
		return readErr == nil
	}
	diff := diffResolved(current, merged, hasState)
	if diff.EnvelopeChanged {
		return spec.Resolved{}, nil, failure.WithNext(fmt.Errorf("%w: deployment-level settings changed", ErrRecreateRequired), "barn recreate")
	}
	if len(diff.Removed) != 0 {
		return spec.Resolved{}, nil, failure.WithNext(fmt.Errorf("%w: %s", ErrNodesRemoved, strings.Join(diff.Removed, ", ")), "barn destroy "+strings.Join(diff.Removed, " "))
	}
	if len(diff.Changed) != 0 {
		return spec.Resolved{}, nil, fmt.Errorf("%w: node(s) %s changed outside CPU and memory; use `barn recreate %s`", ErrRecreateRequired, strings.Join(diff.Changed, ", "), strings.Join(diff.Changed, " "))
	}
	if len(diff.Create) != 0 {
		return spec.Resolved{}, nil, failure.WithNext(fmt.Errorf("resize cannot create node(s): %s", strings.Join(diff.Create, ", ")), "barn up "+strings.Join(diff.Create, " "))
	}
	if len(selected) == 0 {
		selected, err = selectedNodeNames(merged, nil)
		if err != nil {
			return spec.Resolved{}, nil, err
		}
	}
	resizeSet := nodeNameSet(diff.Resize)
	for _, name := range selected {
		if _, exists := resizeSet[name]; exists {
			continue
		}
		if _, exists := resizeTargetNode(current, name); !exists {
			return spec.Resolved{}, nil, fmt.Errorf("node %s has not been created; run `barn up %s` first", name, name)
		}
	}
	return merged, diff.Resize, nil
}

func resizeBaseInvocation(node state.NodeState) qemu.Invocation {
	if node.Phase != state.Running && node.PendingInvocation.Binary != "" {
		return node.PendingInvocation
	}
	return node.Invocation
}

func resizeCanBeOnline(resources qemu.Resources, target spec.Node) bool {
	if target.CPUs < resources.CPUs || target.Memory < resources.Memory {
		return false
	}
	if target.CPUs > resources.CPUs && (resources.MaxCPUs <= resources.CPUs || target.CPUs > resources.MaxCPUs) {
		return false
	}
	if target.Memory > resources.Memory && (resources.MaxMemory <= resources.Memory || target.Memory > resources.MaxMemory || resources.MemorySlots < 1) {
		return false
	}
	return target.CPUs != resources.CPUs || target.Memory != resources.Memory
}

func resizeDeploymentState(current state.DeploymentState, name string, target spec.Node, version string, now time.Time) (state.DeploymentState, error) {
	updated := current
	found := false
	for index := range updated.Resolved.Nodes {
		if updated.Resolved.Nodes[index].Name != name {
			continue
		}
		updated.Resolved.Nodes[index].CPUs = target.CPUs
		updated.Resolved.Nodes[index].Memory = target.Memory
		found = true
		break
	}
	if !found {
		return state.DeploymentState{}, fmt.Errorf("deployment has no node %s", name)
	}
	hash, err := spec.Hash(updated.Resolved)
	if err != nil {
		return state.DeploymentState{}, err
	}
	updated.Schema = state.DeploymentSchema
	if version != "" {
		updated.BarnVersion = version
	}
	updated.SpecHash = hash
	updated.UpdatedAt = now.UTC()
	return updated, nil
}

func commitResize(store state.Store, current state.DeploymentState, node state.NodeState, target spec.Node, next qemu.Invocation, online bool, version string, now time.Time) (state.NodeState, state.DeploymentState, error) {
	updatedDeployment, err := resizeDeploymentState(current, node.Node, target, version, now)
	if err != nil {
		return state.NodeState{}, state.DeploymentState{}, err
	}
	old := node
	if node.GuestSpecHash == "" {
		node.GuestSpecHash = node.SpecHash
	}
	node.SpecHash, err = spec.NodeHash(updatedDeployment.Resolved, node.Node)
	if err != nil {
		return state.NodeState{}, state.DeploymentState{}, err
	}
	if online {
		node.PendingInvocation = next
	} else {
		node.Invocation = next
		node.PendingInvocation = qemu.Invocation{}
	}
	node.UpdatedAt = now.UTC()
	if err := store.WriteNode(node); err != nil {
		return state.NodeState{}, state.DeploymentState{}, err
	}
	if err := store.WriteDeployment(updatedDeployment); err != nil {
		// A deployment write failure must not leave the node claiming a
		// resource hash that the applied deployment does not contain.
		if restoreErr := store.WriteNode(old); restoreErr != nil {
			return state.NodeState{}, state.DeploymentState{}, errors.Join(err, fmt.Errorf("restore node state after deployment write failure: %w", restoreErr))
		}
		return state.NodeState{}, state.DeploymentState{}, err
	}
	return node, updatedDeployment, nil
}

func qmpResizeClient() *qmp.Client { return &qmp.Client{Timeout: 5 * time.Second} }

func hotplugCPU(ctx context.Context, client *qmp.Client, socket string, node string, current, target int) error {
	if target <= current {
		return nil
	}
	slots, err := client.QueryHotpluggableCPUs(ctx, socket)
	if err != nil {
		return fmt.Errorf("%w: query hotpluggable CPUs: %v", errResizeOffline, err)
	}
	needed := target - current
	available := 0
	for _, slot := range slots {
		if slot.QOMPath == "" {
			count := slot.VCPUsCount
			if count < 1 {
				count = 1
			}
			available += count
		}
	}
	if available < needed {
		return errResizeOffline
	}
	added := 0
	for index, slot := range slots {
		if slot.QOMPath != "" {
			continue
		}
		count := slot.VCPUsCount
		if count < 1 {
			count = 1
		}
		if added+count > needed {
			continue
		}
		arguments := make(map[string]any, len(slot.Props)+2)
		for key, value := range slot.Props {
			arguments[key] = value
		}
		arguments["driver"] = slot.Type
		arguments["id"] = fmt.Sprintf("barn-resize-%s-cpu-%d", node, index)
		if err := client.DeviceAdd(ctx, socket, arguments); err != nil {
			return fmt.Errorf("%w: hotplug CPU %d: %v", errResizeOffline, current+added+count, err)
		}
		added += count
		if added == needed {
			return nil
		}
	}
	return errResizeOffline
}

func hotplugMemory(ctx context.Context, client *qmp.Client, socket string, node string, current, target int64) error {
	if target <= current {
		return nil
	}
	objectID := fmt.Sprintf("barn_resize_mem_%s_%d", node, time.Now().UnixNano())
	deviceID := fmt.Sprintf("barn_resize_dimm_%s_%d", node, time.Now().UnixNano())
	delta := target - current
	if err := client.ObjectAdd(ctx, socket, map[string]any{"qom-type": "memory-backend-ram", "id": objectID, "size": delta}); err != nil {
		return fmt.Errorf("%w: create hotplug memory backend: %v", errResizeOffline, err)
	}
	if err := client.DeviceAdd(ctx, socket, map[string]any{"driver": "pc-dimm", "id": deviceID, "memdev": objectID}); err != nil {
		_ = client.ObjectDel(ctx, socket, objectID)
		return fmt.Errorf("%w: hotplug memory: %v", errResizeOffline, err)
	}
	return nil
}

func guestResources(ctx context.Context, runner execx.Runner, sshPath, user, key, knownHosts string, node state.NodeState) (resizeGuestResources, error) {
	alias := vm.HostKeyAlias(node.VMUUID)
	args := vm.SSHArgsForInstance(user, key, knownHosts, alias, node.SSHPort, "nproc")
	if args == nil {
		return resizeGuestResources{}, errors.New("invalid SSH arguments while checking guest CPU count")
	}
	cpuResult, err := runner.Run(ctx, sshPath, args...)
	if err != nil {
		return resizeGuestResources{}, fmt.Errorf("read guest CPU count: %w", err)
	}
	cpus, err := strconv.Atoi(strings.TrimSpace(string(cpuResult.Stdout)))
	if err != nil || cpus < 1 {
		return resizeGuestResources{}, fmt.Errorf("guest returned invalid CPU count %q", strings.TrimSpace(string(cpuResult.Stdout)))
	}
	args = vm.SSHArgsForInstance(user, key, knownHosts, alias, node.SSHPort, "awk", "/^MemTotal:/ {print $2 * 1024}", "/proc/meminfo")
	if args == nil {
		return resizeGuestResources{}, errors.New("invalid SSH arguments while checking guest memory")
	}
	memoryResult, err := runner.Run(ctx, sshPath, args...)
	if err != nil {
		return resizeGuestResources{}, fmt.Errorf("read guest memory: %w", err)
	}
	memory, err := strconv.ParseInt(strings.TrimSpace(string(memoryResult.Stdout)), 10, 64)
	if err != nil || memory < 1 {
		return resizeGuestResources{}, fmt.Errorf("guest returned invalid memory value %q", strings.TrimSpace(string(memoryResult.Stdout)))
	}
	return resizeGuestResources{CPUs: cpus, Memory: memory}, nil
}

func waitGuestResources(ctx context.Context, runner execx.Runner, sshPath, user, key, knownHosts string, node state.NodeState, target spec.Node) error {
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var last resizeGuestResources
	var lastErr error
	for {
		last, lastErr = guestResources(waitCtx, runner, sshPath, user, key, knownHosts, node)
		if lastErr == nil && last.CPUs >= target.CPUs && guestMemoryReady(last.Memory, target.Memory) {
			return nil
		}
		if waitCtx.Err() != nil {
			if lastErr != nil {
				return fmt.Errorf("guest did not expose resized resources (last check: %w)", lastErr)
			}
			return fmt.Errorf("guest exposed %d CPUs and %s memory, want %d CPUs and about %s", last.CPUs, formatBytes(last.Memory), target.CPUs, formatBytes(target.Memory))
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func formatBytes(value int64) string {
	if value%(1024*1024*1024) == 0 {
		return fmt.Sprintf("%d GiB", value/(1024*1024*1024))
	}
	return fmt.Sprintf("%d MiB", value/(1024*1024))
}

func resizeNodeFailure(name string, err error) NodeFailure {
	stage := "resize"
	_, reason, next := failure.Classify(err)
	if strings.HasPrefix(next, "barn start ") {
		stage = "start"
	}
	return NodeFailure{Node: name, Stage: stage, Error: err.Error(), Reason: reason}
}

func guestMemoryReady(actual, target int64) bool {
	if actual >= target {
		return true
	}
	// Linux reserves a small amount of memory for the kernel and firmware, so
	// /proc/meminfo is normally below QEMU's exact -m value. Keep the check
	// strict enough to catch a DIMM that never became visible while allowing
	// that normal reservation.
	tolerance := target / 20
	if tolerance < 64*1024*1024 {
		tolerance = 64 * 1024 * 1024
	}
	return actual >= target-tolerance
}

func (m Manager) resizeOnline(ctx context.Context, desired spec.Node) (applied, guestChanged bool, returnErr error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return false, false, err
	}
	deploymentLock, err := acquireDeploymentLock(ctx, deploymentValue.Root, false, m.Progress)
	if err != nil {
		return false, false, err
	}
	defer func() { returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment resize lock") }()
	store := state.Store{Root: deploymentValue.Root}
	deploymentState, err := store.ReadDeployment()
	if err != nil {
		return false, false, err
	}
	node, err := store.ReadNode(desired.Name)
	if err != nil {
		return false, false, err
	}
	if node.Phase != state.Running {
		return false, false, nil
	}
	base := resizeBaseInvocation(node)
	resources, err := qemu.InvocationResources(base)
	if err != nil {
		return false, false, err
	}
	// A decrease is always an offline operation. Checking the applied desired
	// values first also avoids requiring guest SSH just to discover that a
	// previously hotplugged VM must be restarted.
	currentDefinition, ok := resizeTargetNode(deploymentState.Resolved, node.Node)
	if !ok || desired.CPUs < currentDefinition.CPUs || desired.Memory < currentDefinition.Memory {
		return false, false, nil
	}
	if !resizeCanBeOnline(resources, desired) {
		return false, false, nil
	}
	next, err := qemu.ConfigureResources(base, desired.CPUs, desired.Memory)
	if err != nil {
		return false, false, err
	}
	identity := process.Identity{PID: node.Process.PID, Executable: node.Process.Executable, Started: node.Process.Started, ArgvHash: node.Process.ArgvHash}
	if !process.MatchesLive(ctx, m.runner(), identity, node.Invocation) {
		return false, false, fmt.Errorf("node %s process identity does not match the recorded QEMU invocation", node.Node)
	}
	client := qmpResizeClient()
	lifecycle := vm.Lifecycle{Runner: m.runner(), QMP: client}
	if err := lifecycle.ValidateIdentity(ctx, node.Runtime.QMP, node.Node, node.VMUUID); err != nil {
		return false, false, fmt.Errorf("node %s QMP identity does not match: %w", node.Node, err)
	}
	memorySummary, err := client.QueryMemorySizeSummary(ctx, node.Runtime.QMP)
	if err != nil || memorySummary.BaseMemory < 1 || memorySummary.PluggedMemory < 0 {
		return false, false, nil
	}
	if memorySummary.BaseMemory > int64(^uint64(0)>>1)-memorySummary.PluggedMemory {
		return false, false, nil
	}
	resources.Memory = memorySummary.BaseMemory + memorySummary.PluggedMemory
	sshPath, err := m.lookPath("ssh")
	if err != nil {
		return false, false, err
	}
	privateKey, knownHosts, _, err := sshkeys.EnsureExistingKeys(ctx, m.runner(), deploymentValue.Root)
	if err != nil {
		return false, false, err
	}
	guest, err := guestResources(ctx, m.runner(), sshPath, deploymentState.Resolved.SSHUser, privateKey, knownHosts, node)
	if err != nil {
		return false, false, err
	}
	// An online resize may already have changed the running guest while the
	// recorded process invocation still describes its original argv. Use the
	// guest's live topology for the delta, while retaining max capacity from
	// the invocation for the capability check.
	resources.CPUs = guest.CPUs
	// The QMP total above is used for the hotplug delta. The guest value is
	// used only as a readiness probe because it includes kernel reservations.
	if !resizeCanBeOnline(resources, desired) {
		return false, false, nil
	}
	guestChanged = desired.CPUs > resources.CPUs || desired.Memory > resources.Memory
	if err := hotplugCPU(ctx, client, node.Runtime.QMP, node.Node, resources.CPUs, desired.CPUs); err != nil {
		if errors.Is(err, errResizeOffline) {
			return false, false, nil
		}
		return false, false, err
	}
	if err := hotplugMemory(ctx, client, node.Runtime.QMP, node.Node, resources.Memory, desired.Memory); err != nil {
		if errors.Is(err, errResizeOffline) {
			return false, false, nil
		}
		return false, false, err
	}
	if err := waitGuestResources(ctx, m.runner(), sshPath, deploymentState.Resolved.SSHUser, privateKey, knownHosts, node, desired); err != nil {
		return false, guestChanged, err
	}
	if _, _, err := commitResize(store, deploymentState, node, desired, next, true, m.BarnVersion, time.Now().UTC()); err != nil {
		return false, guestChanged, err
	}
	return true, false, nil
}

func (m Manager) resizeOffline(ctx context.Context, desired spec.Node) (mode string, returnErr error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return "", err
	}
	store := state.Store{Root: deploymentValue.Root}
	node, err := store.ReadNode(desired.Name)
	if err != nil {
		return "", err
	}
	if node.Phase != state.Running && node.Phase != state.Stopped && node.Phase != state.Prepared {
		return "", interruptedPhaseError(node)
	}
	base := resizeBaseInvocation(node)
	next, err := qemu.ConfigureResources(base, desired.CPUs, desired.Memory)
	if err != nil {
		return "", err
	}
	wasRunning := node.Phase == state.Running
	stopped := false
	resumeAfterError := func(err error) error {
		if err == nil || !stopped {
			return err
		}
		return failure.WithNext(fmt.Errorf("resizing node %s did not finish; the node remains stopped: %w", desired.Name, err), "barn start "+desired.Name)
	}
	if wasRunning {
		stopper := m
		stopper.Nodes = []string{desired.Name}
		if _, err := stopper.Stop(ctx); err != nil {
			return "", err
		}
		stopped = true
	}
	deploymentLock, err := acquireDeploymentLock(ctx, deploymentValue.Root, false, m.Progress)
	if err != nil {
		return "", resumeAfterError(err)
	}
	store = state.Store{Root: deploymentValue.Root}
	deploymentState, err := store.ReadDeployment()
	if err != nil {
		returnErr = resumeAfterError(lock.JoinRelease(err, deploymentLock, "deployment resize lock"))
		return "", returnErr
	}
	node, err = store.ReadNode(desired.Name)
	if err != nil {
		returnErr = resumeAfterError(lock.JoinRelease(err, deploymentLock, "deployment resize lock"))
		return "", returnErr
	}
	if _, _, err := commitResize(store, deploymentState, node, desired, next, false, m.BarnVersion, time.Now().UTC()); err != nil {
		returnErr = resumeAfterError(lock.JoinRelease(err, deploymentLock, "deployment resize lock"))
		return "", returnErr
	}
	returnErr = resumeAfterError(lock.JoinRelease(nil, deploymentLock, "deployment resize lock"))
	if returnErr != nil {
		return "", returnErr
	}
	if !wasRunning {
		return "offline", nil
	}
	starter := m
	starter.Nodes = []string{desired.Name}
	if _, err := starter.Start(ctx); err != nil {
		return "", failure.WithNext(fmt.Errorf("resized node %s but could not restart it: %w", desired.Name, err), "barn start "+desired.Name)
	}
	return "offline restart", nil
}

// Resize applies CPU and memory changes from the desired inventory. A running
// VM is hotplugged when its invocation has capacity and the guest confirms
// the new resources; otherwise the same VM is stopped and started with a
// patched invocation. Disks, UUIDs, and SSH keys are never recreated.
func (m Manager) Resize(ctx context.Context, requested spec.Resolved) (Status, error) {
	if err := validateResolved(requested); err != nil {
		return Status{}, err
	}
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Status{}, err
	}
	current, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil || current.Resolved.Network != "private" {
		return Status{}, errors.New("the deployment has no valid state")
	}
	selected := append([]string(nil), m.Nodes...)
	merged, resizeNames, err := resizeTargets(current.Resolved, requested, selected, state.Store{Root: deploymentValue.Root})
	if err != nil {
		return Status{}, err
	}
	resizeSet := nodeNameSet(resizeNames)
	selectedNames, err := selectedNodeNames(merged, selected)
	if err != nil {
		return Status{}, err
	}
	if len(selected) != 0 {
		selectedNames = selected
	}
	if len(resizeNames) == 0 {
		return m.statusFor(ctx, deploymentValue, "CPU and memory already match the inventory")
	}
	if len(selected) != 0 {
		selectedResize := 0
		for _, name := range selectedNames {
			if _, include := resizeSet[name]; include {
				selectedResize++
			}
		}
		if selectedResize == 0 {
			return m.statusFor(ctx, deploymentValue, "selected nodes already match the inventory CPU and memory")
		}
	}
	var online, offline, stopped []string
	var failures []NodeFailure
	attempted := 0
	for _, name := range selectedNames {
		if _, include := resizeSet[name]; !include {
			continue
		}
		attempted++
		target, ok := resizeTargetNode(merged, name)
		if !ok {
			failures = append(failures, NodeFailure{Node: name, Stage: "resize", Error: "desired node disappeared while resizing"})
			continue
		}
		applied, guestChanged, err := m.resizeOnline(ctx, target)
		if err != nil {
			if guestChanged && ctx.Err() == nil {
				// QMP may have applied part or all of an online change before
				// guest readiness or state persistence failed. Reconcile through
				// the offline path while the desired resource change is still
				// explicit, so a failed command does not leave a half-applied VM.
				mode, recoveryErr := m.resizeOffline(ctx, target)
				if recoveryErr == nil {
					if mode == "offline restart" {
						offline = append(offline, name)
					} else {
						stopped = append(stopped, name)
					}
					continue
				}
				err = errors.Join(fmt.Errorf("online resize did not finish: %w", err), fmt.Errorf("offline recovery failed: %w", recoveryErr))
			}
			failures = append(failures, resizeNodeFailure(name, err))
			continue
		}
		if applied {
			online = append(online, name)
			continue
		}
		mode, err := m.resizeOffline(ctx, target)
		if err != nil {
			failures = append(failures, resizeNodeFailure(name, err))
			continue
		}
		if mode == "offline restart" {
			offline = append(offline, name)
		} else {
			stopped = append(stopped, name)
		}
	}
	message := "resized the deployment"
	parts := make([]string, 0, 3)
	if len(online) != 0 {
		parts = append(parts, strings.Join(online, ", ")+" online")
	}
	if len(offline) != 0 {
		parts = append(parts, strings.Join(offline, ", ")+" with offline restart")
	}
	if len(stopped) != 0 {
		parts = append(parts, strings.Join(stopped, ", ")+" while stopped")
	}
	if len(parts) != 0 {
		message += ": " + strings.Join(parts, "; ")
	}
	status, statusErr := m.statusFor(ctx, deploymentValue, message)
	if len(failures) != 0 {
		return status, errors.Join(newPartialError(failures, attempted), statusErr)
	}
	return status, statusErr
}
