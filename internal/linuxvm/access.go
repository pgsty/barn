package linuxvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/diagnostics"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/openssh"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/vm"
)

type Connection struct {
	Node         string `json:"node"`
	User         string `json:"user"`
	Host         string `json:"host"`
	Port         uint16 `json:"port"`
	PrivateKey   string `json:"private_key"`
	KnownHosts   string `json:"known_hosts"`
	HostKeyAlias string `json:"host_key_alias,omitempty"`
}

func defaultCommittedNode(store state.Store, resolved spec.Resolved) (string, error) {
	committed, err := committedNodeNames(store, resolved, nil)
	if err != nil {
		return "", err
	}
	committedSet := nodeNameSet(committed)
	for _, node := range resolved.Nodes {
		if _, exists := committedSet[node.Name]; exists && node.Control {
			return node.Name, nil
		}
	}
	return committed[0], nil
}

func (m Manager) Connection(ctx context.Context, requestedNode string) (Connection, error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Connection{}, err
	}
	deploymentState, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil {
		return Connection{}, errors.New("the deployment has no valid state")
	}
	if requestedNode == "" {
		requestedNode, err = defaultCommittedNode(state.Store{Root: deploymentValue.Root}, deploymentState.Resolved)
		if err != nil {
			return Connection{}, err
		}
	}
	knownNode := false
	for _, node := range deploymentState.Resolved.Nodes {
		knownNode = knownNode || node.Name == requestedNode
	}
	if !knownNode {
		return Connection{}, UnknownNodeError(requestedNode, "deployment", deploymentState.Resolved.Nodes)
	}
	m.Nodes = []string{requestedNode}
	status, err := m.statusReadOnly(ctx, deploymentValue)
	if err != nil {
		return Connection{}, err
	}
	port := uint16(0)
	hostKeyAlias := ""
	for _, node := range status.Nodes {
		if node.Name == requestedNode {
			if node.State != state.Running || node.Runtime != "running" {
				return Connection{}, notRunningError(requestedNode)
			}
			port = node.SSHPort
			hostKeyAlias = node.hostKeyAlias
		}
	}
	privateKey, knownHosts, err := validateSSHArtifacts(deploymentValue)
	if err != nil {
		return Connection{}, err
	}
	for path, mode := range map[string]os.FileMode{privateKey: 0o600, knownHosts: 0o600} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != mode {
			return Connection{}, fmt.Errorf("SSH artifact is unsafe: %s", path)
		}
	}
	return Connection{Node: requestedNode, User: deploymentState.Resolved.SSHUser, Host: "127.0.0.1", Port: port, PrivateKey: privateKey, KnownHosts: knownHosts, HostKeyAlias: hostKeyAlias}, nil
}

func (m Manager) LogPath(nodeName, source string) (string, error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return "", err
	}
	if source == "events" {
		path := filepath.Join(deploymentValue.Root, "events.jsonl")
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("event log is missing or unsafe: %s", path)
		}
		return path, nil
	}
	deploymentState, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil {
		return "", errors.New("the deployment has no valid state")
	}
	if source != "serial" && source != "qemu" {
		return "", fmt.Errorf("unsupported private log source %q", source)
	}
	if nodeName == "" {
		nodeName, err = defaultCommittedNode(state.Store{Root: deploymentValue.Root}, deploymentState.Resolved)
		if err != nil {
			return "", err
		}
	}
	nodeDir, err := deploymentValue.NodeDir(nodeName)
	if err != nil {
		return "", err
	}
	known := false
	for _, node := range deploymentState.Resolved.Nodes {
		known = known || node.Name == nodeName
	}
	if !known {
		return "", UnknownNodeError(nodeName, "deployment", deploymentState.Resolved.Nodes)
	}
	logName := "serial.log"
	if source == "qemu" {
		logName = "qemu.log"
	}
	path := filepath.Join(nodeDir, logName)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s log is missing or unsafe: %s", source, path)
	}
	return path, nil
}

func (m Manager) SSHConfig(ctx context.Context) (string, error) {
	deploymentValue, deploymentState, nodes, err := m.integrationSnapshot(ctx)
	if err != nil {
		return "", err
	}
	privateKey, knownHosts, err := validateSSHArtifacts(deploymentValue)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	quotedIdentity, err := openssh.QuoteConfigValue(privateKey)
	if err != nil {
		return "", err
	}
	quotedKnownHosts, err := openssh.QuoteConfigValue(knownHosts)
	if err != nil {
		return "", err
	}
	for index, definition := range deploymentState.Resolved.Nodes {
		node := nodes[index]
		hostPatterns := []string{definition.Name}
		if definition.Address != "" {
			hostPatterns = append(hostPatterns, definition.Address)
		}
		hostPatterns = append(hostPatterns, definition.Aliases...)
		fmt.Fprintf(&output, "Host %s\n  HostName 127.0.0.1\n  User %s\n  Port %d\n  IdentityFile %s\n  UserKnownHostsFile %s\n  HostKeyAlias %s\n  IdentitiesOnly yes\n  StrictHostKeyChecking accept-new\n\n", strings.Join(hostPatterns, " "), deploymentState.Resolved.SSHUser, node.SSHPort, quotedIdentity, quotedKnownHosts, vm.HostKeyAlias(node.VMUUID))
	}
	return output.String(), nil
}

func (m Manager) RecordEvent(ctx context.Context, action, level, message string) error {
	if m.OperationID == "" {
		return errors.New("event requires an operation ID")
	}
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return err
	}
	event := diagnostics.Event{Schema: 1, Time: time.Now().UTC(), Level: level, Node: "deployment", OperationID: m.OperationID, Action: action, Message: message}
	if err := diagnostics.AppendEvent(ctx, filepath.Join(deploymentValue.Root, "events.jsonl"), event); err != nil {
		return err
	}
	deploymentState, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil {
		return nil
	}
	store := state.Store{Root: deploymentValue.Root}
	for _, definition := range deploymentState.Resolved.Nodes {
		node, err := store.ReadNode(definition.Name)
		if err != nil {
			continue
		}
		record := diagnostics.QEMULogRecord{Schema: 1, Time: time.Now().UTC(), Level: level, Node: node.Node, OperationID: m.OperationID, Action: action, Message: message, Argv: append([]string{node.Invocation.Binary}, node.Invocation.Args...)}
		if err := diagnostics.AppendQEMULog(ctx, filepath.Join(filepath.Dir(node.RootDisk), "qemu.log"), record); err != nil {
			return err
		}
	}
	return nil
}

// notRunningError is a guest command aimed at a node that is not running.
func notRunningError(node string) error {
	return failure.New(failure.Conflict, fmt.Errorf("node %s is not running", node)).Because("node_not_running").Then("barn start " + node)
}

// UnknownNodeError names the nodes that do exist so the user can pick one.
// where is the definition that lacks the node: "inventory" before it is
// applied, "deployment" after.
func UnknownNodeError(name, where string, nodes []spec.Node) error {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	return failure.New(failure.Usage, fmt.Errorf("the %s has no node %q; nodes: %s", where, name, strings.Join(names, ", "))).Because("unknown_node")
}
