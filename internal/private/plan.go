// Package private builds the deterministic node intent shared by Darwin
// socket_vmnet and Linux bridge runtimes and drives the node lifecycle.
package private

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pgsty/farrow/internal/identity"
	"github.com/pgsty/farrow/internal/naming"
	"github.com/pgsty/farrow/internal/network/subnet"
	"github.com/pgsty/farrow/internal/runtimepath"
	"github.com/pgsty/farrow/internal/spec"
	"github.com/pgsty/farrow/internal/state"
)

type UUIDSource func() (string, error)

type NodePlan struct {
	Name          string             `json:"name"`
	Control       bool               `json:"control"`
	Address       string             `json:"address"`
	Aliases       []string           `json:"aliases,omitempty"`
	ManagementMAC string             `json:"management_mac"`
	PrivateMAC    string             `json:"private_mac"`
	VMUUID        string             `json:"vm_uuid"`
	Runtime       state.RuntimePaths `json:"runtime"`
}

type Plan struct {
	Control string     `json:"control"`
	Nodes   []NodePlan `json:"nodes"`
}

func validateResolved(value spec.Resolved) error {
	if value.Schema != 1 || value.Network != "private" || value.Private == nil || len(value.Nodes) < 1 || len(value.Nodes) > 20 || value.SSHUser == "" {
		return errors.New("plan requires schema 1, private network, and 1..20 nodes")
	}
	if value.Arch != "" && value.Arch != "amd64" && value.Arch != "arm64" {
		return fmt.Errorf("unsupported resolved guest architecture %q", value.Arch)
	}
	if _, err := value.SSHWaitTimeout(); err != nil {
		return err
	}
	layout, err := subnet.Parse(value.Private.CIDR)
	if err != nil || value.Private.HostAddress != layout.HostAddress() || value.Private.DHCPEnd != layout.DHCPEnd() {
		return errors.New("plan network settings are invalid")
	}
	controls := 0
	names := make(map[string]struct{})
	addresses := make(map[string]struct{})
	for _, node := range value.Nodes {
		if !naming.ValidNodeName(node.Name) || !layout.IsStatic(node.Address) || node.CPUs < 1 || node.Memory < 512<<20 || node.RootDisk <= 0 {
			return fmt.Errorf("node %q identity, address, or resources are invalid", node.Name)
		}
		if _, duplicate := names[node.Name]; duplicate {
			return fmt.Errorf("duplicate private node %q", node.Name)
		}
		if _, duplicate := addresses[node.Address]; duplicate {
			return fmt.Errorf("duplicate private address %q", node.Address)
		}
		names[node.Name] = struct{}{}
		addresses[node.Address] = struct{}{}
		if node.Control {
			controls++
		}
	}
	if controls != 1 {
		return fmt.Errorf("plan requires exactly one control node, got %d", controls)
	}
	return nil
}

func runtimePaths(node string, uid int) (state.RuntimePaths, error) {
	directory, err := runtimepath.Directory(node, uid)
	if err != nil {
		return state.RuntimePaths{}, err
	}
	return state.RuntimePaths{Directory: directory, QMP: filepath.Join(directory, "qmp.sock"), PIDFile: filepath.Join(directory, "qemu.pid")}, nil
}

// Build derives the deterministic per-node intent. knownUUIDs preserves the
// VM UUIDs of nodes that already have committed state.
func Build(resolved spec.Resolved, ownerUID int, knownUUIDs map[string]string, source UUIDSource) (Plan, error) {
	if err := validateResolved(resolved); err != nil {
		return Plan{}, err
	}
	if ownerUID < 0 {
		return Plan{}, errors.New("plan owner UID is invalid")
	}
	if source == nil {
		source = identity.NewUUID
	}
	plan := Plan{Nodes: make([]NodePlan, 0, len(resolved.Nodes))}
	for _, node := range resolved.Nodes {
		vmUUID := knownUUIDs[node.Name]
		if vmUUID == "" {
			generated, err := source()
			if err != nil {
				return Plan{}, err
			}
			vmUUID = generated
		}
		if !identity.ValidUUID(vmUUID) {
			return Plan{}, fmt.Errorf("node %s VM UUID is invalid", node.Name)
		}
		managementMAC, err := identity.MAC(node.Address, identity.NICManagement)
		if err != nil {
			return Plan{}, err
		}
		privateMAC, err := identity.MAC(node.Address, identity.NICPrivate)
		if err != nil {
			return Plan{}, err
		}
		runtimeValue, err := runtimePaths(node.Name, ownerUID)
		if err != nil {
			return Plan{}, fmt.Errorf("node %s runtime path: %w", node.Name, err)
		}
		nodePlan := NodePlan{
			Name: node.Name, Control: node.Control, Address: node.Address,
			Aliases: append([]string(nil), node.Aliases...), ManagementMAC: managementMAC,
			PrivateMAC: privateMAC, VMUUID: vmUUID, Runtime: runtimeValue,
		}
		plan.Nodes = append(plan.Nodes, nodePlan)
		if node.Control {
			plan.Control = node.Name
		}
	}
	return plan, nil
}

func (plan Plan) Node(name string) (NodePlan, bool) {
	for _, node := range plan.Nodes {
		if strings.EqualFold(node.Name, name) {
			return node, true
		}
	}
	return NodePlan{}, false
}
