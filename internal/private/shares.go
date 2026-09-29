package private

import (
	"context"
	"errors"
	"fmt"

	"github.com/pgsty/barn/internal/cloudinit"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/hostshare"
	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func openPrivateNodeShares(value Deployment, sharesByNode map[string][]spec.Share, node state.NodeState) (*hostshare.Bundle, error) {
	shares := append([]spec.Share(nil), sharesByNode[node.Node]...)
	bundle, err := hostshare.Open(value.Root, shares)
	if err != nil {
		return nil, fmt.Errorf("open host shares for private node %s: %w", node.Node, err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = bundle.Close()
		}
	}()

	prefixFiles := 0
	if node.Invocation.UsesPrivateFD3() {
		prefixFiles = 1
	}
	if err := validatePrivateInheritedLayout(node, prefixFiles, len(shares)); err != nil {
		return nil, err
	}
	if err := bundle.ValidateInvocation(node.Invocation, prefixFiles); err != nil {
		return nil, fmt.Errorf("validate host-share invocation for private node %s: %w", node.Node, err)
	}
	if err := bundle.ValidateQEMUAccess(); err != nil {
		return nil, fmt.Errorf("validate host-share access for private node %s: %w", node.Node, err)
	}
	closeOnError = false
	return bundle, nil
}

func validatePrivateInheritedLayout(node state.NodeState, prefixFiles, shareFiles int) error {
	files := node.Invocation.InheritedFiles
	if len(files) == 0 {
		// Compatibility expiry: inherited-files-v0 in CONTRIBUTING.md#compatibility-expiry.
		// State written before inherited files became typed may still describe
		// the Darwin network FD in argv. Shares have never used that legacy
		// representation and are rejected by Bundle.ValidateInvocation.
		return nil
	}
	expected := prefixFiles + shareFiles
	if len(files) != expected {
		return fmt.Errorf("node %s invocation has %d inherited files, expected %d", node.Node, len(files), expected)
	}
	for index, file := range files {
		expectedFD := 3 + index
		if file.FD != expectedFD {
			return fmt.Errorf("node %s inherited file %d uses fd%d, expected fd%d", node.Node, index, file.FD, expectedFD)
		}
		if index < prefixFiles {
			if index != 0 || file.Kind != "private-network" || file.ID != "private" {
				return fmt.Errorf("node %s inherited file %d is not the private-network fd", node.Node, index)
			}
			continue
		}
		if file.Kind != "share" {
			return fmt.Errorf("node %s inherited file %d has unexpected kind %q", node.Node, index, file.Kind)
		}
	}
	return nil
}

func selectedShareSources(value Deployment, resolved spec.Resolved, names []string) error {
	selected := nodeNameSet(names)
	for _, node := range resolved.Nodes {
		if len(selected) != 0 {
			if _, include := selected[node.Name]; !include {
				continue
			}
		}
		if err := hostshare.Validate(value.Root, node.Shares); err != nil {
			return fmt.Errorf("validate host shares for private node %s: %w", node.Name, err)
		}
	}
	return nil
}

// Destructive lifecycle actions must prove that the replacement can reopen
// its sources before stopping or deleting the current VM.
func selectedShareAccess(value Deployment, resolved spec.Resolved, names []string) error {
	for _, node := range resolvedNodeSelection(resolved, names).Nodes {
		bundle, err := hostshare.Open(value.Root, node.Shares)
		if err != nil {
			return err
		}
		if err := errors.Join(bundle.ValidateQEMUAccess(), bundle.Close()); err != nil {
			return fmt.Errorf("validate host-share access for private node %s: %w", node.Name, err)
		}
	}
	return nil
}

func selectedHasShares(resolved spec.Resolved, names []string) bool {
	selected := nodeNameSet(names)
	for _, node := range resolved.Nodes {
		if len(selected) != 0 {
			if _, include := selected[node.Name]; !include {
				continue
			}
		}
		if len(node.Shares) != 0 {
			return true
		}
	}
	return false
}

func selectedShareInvocationBinaries(store state.Store, resolved spec.Resolved, names []string) ([]string, error) {
	selected := nodeNameSet(names)
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, definition := range resolved.Nodes {
		if len(selected) != 0 {
			if _, include := selected[definition.Name]; !include {
				continue
			}
		}
		if len(definition.Shares) == 0 {
			continue
		}
		node, err := store.ReadNode(definition.Name)
		if err != nil {
			return nil, fmt.Errorf("read private node %s for host-share capability: %w", definition.Name, err)
		}
		binary := node.Invocation.Binary
		if binary == "" {
			return nil, fmt.Errorf("node %s host-share invocation has no QEMU binary", definition.Name)
		}
		if _, duplicate := seen[binary]; duplicate {
			continue
		}
		seen[binary] = struct{}{}
		result = append(result, binary)
	}
	return result, nil
}

func validatePrivateShareDeviceHelp(ctx context.Context, runner execx.Runner, binaries []string) error {
	if len(binaries) == 0 {
		return nil
	}
	if runner == nil {
		return &CapabilityError{Reason: "host-share capability requires a command runner"}
	}
	seen := make(map[string]struct{}, len(binaries))
	for _, binary := range binaries {
		if binary == "" {
			return &CapabilityError{Reason: "host-share capability requires an exact QEMU binary"}
		}
		if _, duplicate := seen[binary]; duplicate {
			continue
		}
		seen[binary] = struct{}{}
		result, err := runner.Run(ctx, binary, "-device", "help")
		if err != nil {
			return &CapabilityError{Reason: fmt.Sprintf("host-share capability probe for QEMU %q failed: %v", binary, err)}
		}
		if err := qemu.ValidateShareDeviceHelp(string(result.Stdout) + "\n" + string(result.Stderr)); err != nil {
			return &CapabilityError{Reason: fmt.Sprintf("host-share capability unavailable for QEMU %q: %v", binary, err)}
		}
	}
	return nil
}

func shareSourcesByNode(resolved spec.Resolved) map[string][]spec.Share {
	result := make(map[string][]spec.Share, len(resolved.Nodes))
	for _, node := range resolved.Nodes {
		result[node.Name] = append([]spec.Share(nil), node.Shares...)
	}
	return result
}

func privateCloudShares(shares []spec.Share) []cloudinit.Share {
	result := make([]cloudinit.Share, 0, len(shares))
	for _, share := range shares {
		result = append(result, cloudinit.Share{Tag: spec.ShareTag(share), Guest: share.Guest, Readonly: share.Readonly})
	}
	return result
}
