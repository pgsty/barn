package private

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/lock"
	"github.com/pgsty/barn/internal/persistent"
	"github.com/pgsty/barn/internal/process"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/vm"
)

func ownedRegularWithin(root, path string) error {
	inside, err := fsutil.IsWithin(root, path)
	if err != nil || !inside {
		return fmt.Errorf("destroy target escapes node root: %s", path)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("destroy target is unsafe: %s", path)
	}
	return nil
}

func removeOwnedRegularWithin(root, path string) error {
	if err := ownedRegularWithin(root, path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsutil.SyncDir(root)
}

func privateNodeTargets(deploymentValue Deployment, node state.NodeState) (string, []string, error) {
	nodeDir, err := deploymentValue.NodeDir(node.Node)
	if err != nil {
		return "", nil, err
	}
	expected := map[string]struct{}{
		filepath.Join(nodeDir, "state.json"): {}, node.RootDisk: {}, node.Seed: {},
		filepath.Join(nodeDir, "serial.log"): {}, filepath.Join(nodeDir, "qemu.log"): {}, filepath.Join(nodeDir, "events.jsonl"): {},
	}
	if node.NVRAM != "" {
		expected[node.NVRAM] = struct{}{}
	}
	for _, disk := range node.DataDisks {
		inside, err := fsutil.IsWithin(nodeDir, disk.Path)
		if err != nil {
			return "", nil, err
		}
		if inside {
			expected[disk.Path] = struct{}{}
		} else if !disk.Persistent {
			return "", nil, fmt.Errorf("non-persistent private disk escapes node root: %s", disk.Path)
		}
	}
	entries, err := os.ReadDir(nodeDir)
	if err != nil {
		return "", nil, err
	}
	for _, entry := range entries {
		path := filepath.Join(nodeDir, entry.Name())
		if _, ok := expected[path]; !ok {
			return "", nil, fmt.Errorf("will not destroy: unexpected file %s in the node directory", path)
		}
		if err := ownedRegularWithin(nodeDir, path); err != nil {
			return "", nil, err
		}
	}
	ordered := make([]string, 0, len(expected))
	for _, path := range []string{node.RootDisk} {
		if path != "" {
			ordered = append(ordered, path)
		}
	}
	for _, disk := range node.DataDisks {
		if !disk.Persistent {
			ordered = append(ordered, disk.Path)
		}
	}
	for _, path := range []string{node.Seed, node.NVRAM, filepath.Join(nodeDir, "serial.log"), filepath.Join(nodeDir, "qemu.log"), filepath.Join(nodeDir, "events.jsonl"), filepath.Join(nodeDir, "state.json")} {
		if path != "" {
			ordered = append(ordered, path)
		}
	}
	return nodeDir, ordered, nil
}

func (m Manager) removeKnownHostEntries(ctx context.Context, deploymentValue Deployment, nodes []state.NodeState) error {
	sshKeygen, err := m.lookPath("ssh-keygen")
	if err != nil {
		return err
	}
	keysDir := filepath.Join(deploymentValue.Root, "keys")
	knownHosts := filepath.Join(keysDir, "known_hosts")
	for _, node := range nodes {
		if _, err := m.runner().Run(ctx, sshKeygen, "-f", knownHosts, "-R", vm.HostKeyAlias(node.VMUUID)); err != nil {
			return err
		}
		backup := knownHosts + ".old"
		if info, err := os.Lstat(backup); err == nil {
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("ssh-keygen created an unsafe private known_hosts backup")
			}
			if err := os.Remove(backup); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return fsutil.SyncDir(keysDir)
}

func (m Manager) Destroy(ctx context.Context) (_ Status, returnErr error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Status{}, err
	}
	deploymentLock, err := acquireDeploymentLock(ctx, deploymentValue.Root, false, m.Progress)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment destroy lock")
	}()
	store := state.Store{Root: deploymentValue.Root}
	deploymentState, err := store.ReadDeployment()
	if err != nil || deploymentState.Resolved.Network != "private" {
		return Status{}, errors.New("the deployment has no valid state")
	}
	selected, err := selectedNodeNames(deploymentState.Resolved, m.Nodes)
	if err != nil {
		return Status{}, err
	}
	partial := len(selected) != len(deploymentState.Resolved.Nodes)
	if partial && !m.allowPartialDestroy {
		return Status{}, errors.New("destroy currently requires selecting the complete deployment")
	}
	selectedSet := nodeNameSet(selected)
	// An interrupted start, stop, or destroy leaves a transitional phase.
	// Settle it first (status proves the runtime dead, or live and ours), so
	// destroy can finish the job instead of sending the user elsewhere.
	interrupted := make([]string, 0)
	for _, name := range selected {
		if node, err := store.ReadNode(name); err == nil && (node.Phase == state.Starting || node.Phase == state.Stopping || node.Phase == state.Destroying) {
			interrupted = append(interrupted, name)
		}
	}
	if len(interrupted) != 0 {
		settling := m
		settling.Nodes = interrupted
		if _, err := settling.statusForLocked(ctx, deploymentValue, ""); err != nil {
			return Status{}, err
		}
	}
	needsStop := false
	stopNodes := make([]string, 0, len(selected))
	for _, definition := range deploymentState.Resolved.Nodes {
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			// A node in the resolved spec without committed state is a pending
			// creation; there is nothing to stop or destroy for it.
			continue
		}
		if err != nil {
			return Status{}, err
		}
		stopNodes = append(stopNodes, definition.Name)
		needsStop = needsStop || node.Phase == state.Running || node.Phase == state.Prepared || node.Phase == state.Starting || node.Phase == state.Stopping
	}
	// Known-host cleanup runs after every artifact is gone; a missing tool
	// found only then would strand a half-destroyed deployment.
	if _, err := m.lookPath("ssh-keygen"); err != nil {
		return Status{}, failure.New(failure.Capability, fmt.Errorf("destroy needs ssh-keygen: %w", err))
	}
	if needsStop {
		stopper := m
		stopper.Nodes = stopNodes
		if _, err := stopper.stopLocked(ctx, deploymentValue); err != nil {
			return Status{}, err
		}
	}
	nodes := make([]state.NodeState, 0, len(deploymentState.Resolved.Nodes))
	allNodes := make([]state.NodeState, 0, len(deploymentState.Resolved.Nodes))
	targets := make(map[string][]string)
	nodeDirs := make(map[string]string)
	for _, definition := range deploymentState.Resolved.Nodes {
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			continue
		}
		if err != nil {
			return Status{}, err
		}
		allNodes = append(allNodes, node)
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		if node.Phase != state.Stopped && node.Phase != state.Prepared {
			return Status{}, interruptedPhaseError(node)
		}
		identityValue := process.Identity{PID: node.Process.PID, Executable: node.Process.Executable, Started: node.Process.Started, ArgvHash: node.Process.ArgvHash}
		if process.MatchesLive(ctx, m.runner(), identityValue, node.Invocation) || process.Alive(node.Process.PID) {
			return Status{}, fmt.Errorf("will not destroy node %s while its QEMU process is running", node.Node)
		}
		if err := checkRuntimeUnused(node); err != nil {
			return Status{}, err
		}
		nodeDir, nodeTargets, err := privateNodeTargets(deploymentValue, node)
		if err != nil {
			return Status{}, err
		}
		nodes = append(nodes, node)
		targets[node.Node] = nodeTargets
		nodeDirs[node.Node] = nodeDir
	}
	persistentIdentities, err := validatePrivatePersistentState(deploymentValue, deploymentState, allNodes)
	if err != nil {
		return Status{}, err
	}
	deploymentStatePath := filepath.Join(deploymentValue.Root, "state.json")
	if !partial {
		if err := ownedRegularWithin(deploymentValue.Root, deploymentStatePath); err != nil {
			return Status{}, err
		}
	}
	for index := range nodes {
		nodes[index].Phase = state.Destroying
		nodes[index].UpdatedAt = time.Now().UTC()
		if err := store.WriteNode(nodes[index]); err != nil {
			return Status{}, err
		}
	}
	for _, node := range nodes {
		for _, identityValue := range persistentIdentities {
			if identityValue.Node != node.Node {
				continue
			}
			source := ""
			for _, dataDisk := range node.DataDisks {
				if dataDisk.Name == identityValue.Name {
					source = dataDisk.Path
					break
				}
			}
			if source == "" {
				return Status{}, fmt.Errorf("persistent private disk %s/%s has no state path", identityValue.Node, identityValue.Name)
			}
			if _, err := persistent.Preserve(deploymentValue.Root, identityValue, source); err != nil {
				return Status{}, err
			}
		}
		for _, path := range targets[node.Node] {
			if err := removeOwnedRegularWithin(nodeDirs[node.Node], path); err != nil {
				return Status{}, err
			}
		}
		if err := cleanupRuntime(node); err != nil {
			return Status{}, err
		}
		if err := os.Remove(nodeDirs[node.Node]); err != nil {
			return Status{}, fmt.Errorf("node directory contains unexpected artifacts: %w", err)
		}
		// A stale diagnostic cache must not turn successful destruction into a
		// failure. Its instance identity also prevents reuse by a later VM.
		_ = store.WriteGuestWarnings(node, nil)
	}
	if err := m.removeKnownHostEntries(ctx, deploymentValue, nodes); err != nil {
		return Status{}, err
	}
	if !partial {
		if err := removeOwnedRegularWithin(deploymentValue.Root, deploymentStatePath); err != nil {
			return Status{}, err
		}
	}
	if partial && m.dropFromSpec {
		remaining := cloneResolved(deploymentState.Resolved)
		remaining.Nodes = remaining.Nodes[:0]
		for _, definition := range deploymentState.Resolved.Nodes {
			if _, removed := selectedSet[definition.Name]; !removed {
				remaining.Nodes = append(remaining.Nodes, definition)
			}
		}
		remainingHash, hashErr := spec.Hash(remaining)
		if hashErr != nil {
			return Status{}, hashErr
		}
		updated := state.DeploymentState{Schema: state.DeploymentSchema, BarnVersion: m.BarnVersion, SpecHash: remainingHash, Resolved: remaining, UpdatedAt: time.Now().UTC()}
		if err := store.WriteDeployment(updated); err != nil {
			return Status{}, err
		}
	}
	message := "destroyed node artifacts; image cache, keys, and persistent data disks preserved"
	switch {
	case partial && m.dropFromSpec:
		message = "destroyed and removed the selected node(s); peers, keys, and persistent data disks preserved"
	case partial:
		message = "destroyed selected node artifacts for immediate recreate; deployment state, peer nodes, keys, and persistent data disks preserved"
	}
	result := Status{OperationID: m.OperationID, SpecHash: deploymentState.SpecHash, Message: message}
	for _, definition := range deploymentState.Resolved.Nodes {
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		result.Nodes = append(result.Nodes, NodeStatus{Name: definition.Name, Address: definition.Address, State: state.Absent, Runtime: "inactive"})
	}
	return result, nil
}

// DestroyNodes destroys only the selected nodes and removes them from the
// deployment's resolved state and lock, so a later `up` does not resurrect
// them. This is the explicit removal path required by scale-in edits.
func (m Manager) DestroyNodes(ctx context.Context) (Status, error) {
	if len(m.Nodes) == 0 {
		return Status{}, errors.New("node destroy requires explicit node selectors")
	}
	m.allowPartialDestroy = true
	m.dropFromSpec = true
	return m.Destroy(ctx)
}

func (m Manager) Recreate(ctx context.Context) (Status, error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Status{}, err
	}
	deploymentState, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil || deploymentState.Resolved.Network != "private" {
		return Status{}, errors.New("the deployment has no valid state")
	}
	return m.RecreateResolved(ctx, deploymentState.Resolved)
}

func validatePrivateRecreatePersistent(deploymentValue Deployment, current, requested spec.Resolved) error {
	currentIdentities, err := privatePersistentIdentities(deploymentValue, current)
	if err != nil {
		return err
	}
	requestedIdentities, err := privatePersistentIdentities(deploymentValue, requested)
	if err != nil {
		return err
	}
	if len(currentIdentities) != len(requestedIdentities) {
		return errors.New("recreate desired configuration changes the persistent disk set")
	}
	wanted := make(map[string]persistent.Identity, len(requestedIdentities))
	for _, identityValue := range requestedIdentities {
		wanted[identityValue.Node+"\x00"+identityValue.Name] = identityValue
	}
	for _, identityValue := range currentIdentities {
		candidate, ok := wanted[identityValue.Node+"\x00"+identityValue.Name]
		// "auto" keeps whatever the retained disk was formatted with, so it is
		// compatible with any concrete filesystem on the other side.
		sameFilesystem := candidate.Filesystem == identityValue.Filesystem || candidate.Filesystem == "auto" || identityValue.Filesystem == "auto"
		if !ok || candidate.Serial != identityValue.Serial || candidate.Size != identityValue.Size || candidate.Mount != identityValue.Mount || !sameFilesystem {
			return fmt.Errorf("recreate desired configuration is incompatible with persistent disk %s/%s", identityValue.Node, identityValue.Name)
		}
	}
	_, err = persistent.ValidateDesired(deploymentValue.Root, requestedIdentities)
	return err
}

// RecreateResolved validates the complete desired private spec and persistent
// attachment contract before the first stop/destroy mutation.
func (m Manager) RecreateResolved(ctx context.Context, requested spec.Resolved) (Status, error) {
	var err error
	if err := validateResolved(requested); err != nil {
		return Status{}, err
	}
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Status{}, err
	}
	deploymentState, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil || deploymentState.Resolved.Network != "private" {
		return Status{}, errors.New("the deployment has no valid state")
	}
	if err := validatePrivateRecreatePersistent(deploymentValue, deploymentState.Resolved, requested); err != nil {
		return Status{}, err
	}
	selected, err := selectedNodeNames(deploymentState.Resolved, m.Nodes)
	if err != nil {
		return Status{}, err
	}
	requestedSelection, err := selectedNodeNames(requested, m.Nodes)
	if err != nil {
		return Status{}, err
	}
	if len(selected) != len(deploymentState.Resolved.Nodes) {
		// Remaining peer drift would prevent Up after the selected disks are gone.
		store := state.Store{Root: deploymentValue.Root}
		hasState := func(name string) bool {
			_, readErr := store.ReadNode(name)
			return readErr == nil
		}
		diff := diffResolved(deploymentState.Resolved, materializeExistingForwardPorts(requested, deploymentState.Resolved), hasState)
		if err := refuseUnselectedDrift(diff, selected); err != nil {
			return Status{}, err
		}
	}
	runtime, qemuPath, err := m.preflightBoot(ctx, requested, requestedSelection)
	if err != nil {
		return Status{}, err
	}
	destroyManager := m
	if len(m.Nodes) != 0 {
		drifted, driftErr := runtimeDriftNodes(state.Store{Root: deploymentValue.Root}, requested, runtime.Profile)
		if driftErr != nil {
			return Status{}, driftErr
		}
		if len(drifted) != 0 {
			return Status{}, fmt.Errorf("%w: runtime changes require whole-deployment recreate without node selectors", ErrRecreateRequired)
		}
	}
	if err := selectedShareSources(deploymentValue, requested, requestedSelection); err != nil {
		return Status{}, err
	}
	shareBinaries, err := selectedShareInvocationBinaries(state.Store{Root: deploymentValue.Root}, deploymentState.Resolved, selected)
	if err != nil {
		return Status{}, err
	}
	if selectedHasShares(requested, requestedSelection) {
		shareBinaries = append(shareBinaries, qemuPath)
	}
	if err := validatePrivateShareDeviceHelp(ctx, m.runner(), shareBinaries); err != nil {
		return Status{}, err
	}
	if err := selectedShareAccess(deploymentValue, requested, requestedSelection); err != nil {
		return Status{}, err
	}
	if len(selected) != len(deploymentState.Resolved.Nodes) {
		destroyManager.allowPartialDestroy = true
	}
	if _, err := destroyManager.Destroy(ctx); err != nil {
		return Status{}, err
	}
	return m.Up(ctx, requested)
}
