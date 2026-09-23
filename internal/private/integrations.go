package private

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pgsty/farrow/internal/hostconfig"
	"github.com/pgsty/farrow/internal/lock"
	"github.com/pgsty/farrow/internal/process"
	"github.com/pgsty/farrow/internal/qmp"
	"github.com/pgsty/farrow/internal/spec"
	"github.com/pgsty/farrow/internal/sshconfig"
	"github.com/pgsty/farrow/internal/sshkeys"
	"github.com/pgsty/farrow/internal/state"
	"github.com/pgsty/farrow/internal/vm"
)

func integrationHome(home string) (string, error) {
	if home != "" {
		return filepath.Abs(home)
	}
	return os.UserHomeDir()
}

func (m Manager) integrationSnapshot(ctx context.Context) (_ Deployment, _ state.DeploymentState, _ []state.NodeState, returnErr error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Deployment{}, state.DeploymentState{}, nil, err
	}
	// Integrations only read state, which is published atomically: while
	// another command holds the deployment, read without queueing behind it.
	deploymentLock, _, err := tryDeploymentLock(deploymentValue.Root, true)
	if err != nil {
		return Deployment{}, state.DeploymentState{}, nil, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment integration snapshot lock")
	}()
	return m.integrationSnapshotLocked(deploymentValue)
}

func (m Manager) integrationSnapshotLocked(deploymentValue Deployment) (Deployment, state.DeploymentState, []state.NodeState, error) {
	store := state.Store{Root: deploymentValue.Root}
	deploymentState, err := store.ReadDeployment()
	if err != nil {
		return Deployment{}, state.DeploymentState{}, nil, err
	}
	if deploymentState.Resolved.Network != "private" {
		return Deployment{}, state.DeploymentState{}, nil, errors.New("the deployment state is not private")
	}
	selected, err := committedNodeNames(store, deploymentState.Resolved, m.Nodes)
	if err != nil {
		return Deployment{}, state.DeploymentState{}, nil, err
	}
	selectedSet := nodeNameSet(selected)
	filtered := cloneResolved(deploymentState.Resolved)
	filtered.Nodes = filtered.Nodes[:0]
	nodes := make([]state.NodeState, 0, len(selected))
	for _, definition := range deploymentState.Resolved.Nodes {
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		node, err := store.ReadNode(definition.Name)
		if err != nil {
			return Deployment{}, state.DeploymentState{}, nil, err
		}
		expectedHash, hashErr := spec.NodeHash(deploymentState.Resolved, definition.Name)
		if hashErr != nil || node.SpecHash != expectedHash {
			return Deployment{}, state.DeploymentState{}, nil, fmt.Errorf("node %s state does not match its resolved node hash", definition.Name)
		}
		if node.SSHPort == 0 || node.Phase == state.Absent {
			return Deployment{}, state.DeploymentState{}, nil, fmt.Errorf("node %s has no installable SSH endpoint", definition.Name)
		}
		filtered.Nodes = append(filtered.Nodes, cloneNode(definition))
		nodes = append(nodes, node)
	}
	deploymentState.Resolved = filtered
	return deploymentValue, deploymentState, nodes, nil
}

func validateSSHArtifacts(deploymentValue Deployment) (string, string, error) {
	return sshkeys.ValidateSSHArtifacts(deploymentValue.Root)
}

// Connections resolves and verifies a selected batch while holding the
// deployment exclusive lock across the complete snapshot and runtime checks.
func (m Manager) Connections(ctx context.Context) (_ []Connection, returnErr error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return nil, err
	}
	deploymentLock, err := acquireDeploymentLock(ctx, deploymentValue.Root, false, m.Progress)
	if err != nil {
		return nil, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment connections lock")
	}()
	deploymentValue, err = m.openDeployment(false)
	if err != nil {
		return nil, err
	}
	return m.ConnectionsLocked(ctx, deploymentValue, deploymentLock)
}

// ConnectionsLocked is the non-locking provisioning entrypoint. The caller
// must pass the live exclusive deployment lock token and keep it held for the
// complete remote operation, preventing lifecycle changes after validation.
func (m Manager) ConnectionsLocked(ctx context.Context, deploymentValue Deployment, deploymentLock *lock.File) ([]Connection, error) {
	if err := deploymentLock.ValidateExclusive(deploymentLockPath(deploymentValue.Root)); err != nil {
		return nil, fmt.Errorf("connections require the matching exclusive deployment lock: %w", err)
	}
	_, deploymentState, nodes, err := m.integrationSnapshotLocked(deploymentValue)
	if err != nil {
		return nil, err
	}
	privateKey, knownHosts, err := validateSSHArtifacts(deploymentValue)
	if err != nil {
		return nil, err
	}
	lifecycle := vm.Lifecycle{Runner: m.runner(), QMP: &qmp.Client{Timeout: 5 * time.Second}, SSHUser: deploymentState.Resolved.SSHUser}
	connections := make([]Connection, 0, len(nodes))
	for index, node := range nodes {
		definition := deploymentState.Resolved.Nodes[index]
		if node.Phase != state.Running {
			return nil, fmt.Errorf("node %s is not running", node.Node)
		}
		identity := process.Identity{PID: node.Process.PID, Executable: node.Process.Executable, Started: node.Process.Started, ArgvHash: node.Process.ArgvHash}
		if err := lifecycle.ValidateIdentity(ctx, node.Runtime.QMP, node.Node, node.VMUUID); err != nil {
			return nil, fmt.Errorf("node %s QMP identity does not match: %w", node.Node, err)
		}
		if !process.MatchesLive(ctx, m.runner(), identity, node.Invocation) {
			return nil, fmt.Errorf("node %s recorded process identity does not match", node.Node)
		}
		connections = append(connections, Connection{
			Node: definition.Name, User: deploymentState.Resolved.SSHUser,
			Host: "127.0.0.1", Port: node.SSHPort,
			PrivateKey: privateKey, KnownHosts: knownHosts, HostKeyAlias: vm.HostKeyAlias(node.VMUUID),
		})
	}
	return connections, nil
}

// InstallSSHConfig writes all persisted private nodes into one marker-owned
// fragment. Each node is addressable by the explicit fragment prefix, its
// node name, and its fixed private address.
func (m Manager) InstallSSHConfig(ctx context.Context, name, home string) (sshconfig.Result, error) {
	home, err := integrationHome(home)
	if err != nil {
		return sshconfig.Result{}, err
	}
	deploymentValue, deploymentState, nodes, err := m.integrationSnapshot(ctx)
	if err != nil {
		return sshconfig.Result{}, err
	}
	identityFile, knownHosts, err := validateSSHArtifacts(deploymentValue)
	if err != nil {
		return sshconfig.Result{}, err
	}
	entries := make([]sshconfig.Entry, 0, len(nodes))
	for index, definition := range deploymentState.Resolved.Nodes {
		aliases := []string{definition.Name}
		if definition.Address != "" {
			aliases = append(aliases, definition.Address)
		}
		aliases = append(aliases, definition.Aliases...)
		entries = append(entries, sshconfig.Entry{
			Name:         name,
			Node:         definition.Name,
			Aliases:      aliases,
			User:         deploymentState.Resolved.SSHUser,
			Host:         "127.0.0.1",
			Port:         nodes[index].SSHPort,
			Identity:     identityFile,
			KnownHosts:   knownHosts,
			HostKeyAlias: vm.HostKeyAlias(nodes[index].VMUUID),
		})
	}
	return sshconfig.InstallMany(home, entries)
}

func (m Manager) RemoveSSHConfig(name, home string) (sshconfig.Result, error) {
	home, err := integrationHome(home)
	if err != nil {
		return sshconfig.Result{}, err
	}
	return sshconfig.Remove(home, name)
}

// HostEntries returns the fixed private address, node name, and declared
// profile aliases for the marker-owned /etc/hosts block.
func (m Manager) HostEntries(ctx context.Context) ([]hostconfig.Entry, error) {
	_, deploymentState, _, err := m.integrationSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]hostconfig.Entry, 0, len(deploymentState.Resolved.Nodes))
	for _, definition := range deploymentState.Resolved.Nodes {
		names := make([]string, 0, len(definition.Aliases)+1)
		seen := make(map[string]struct{}, len(definition.Aliases)+1)
		for _, name := range append([]string{definition.Name}, definition.Aliases...) {
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
		entries = append(entries, hostconfig.Entry{Address: definition.Address, Names: names})
	}
	return entries, nil
}
