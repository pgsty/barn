package private

import (
	"fmt"

	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

// An explicit selection applies only those definitions. Keeping applied peers
// also prevents inventory omissions from becoming implicit destruction.
func applyNodeSelection(current, desired spec.Resolved, selectors []string) (spec.Resolved, error) {
	if len(selectors) == 0 {
		return desired, nil
	}
	selected, err := selectedNodeNames(desired, selectors)
	if err != nil {
		return spec.Resolved{}, err
	}
	selectedSet := nodeNameSet(selected)
	merged := cloneResolved(desired)
	merged.Nodes = nil
	currentByName := make(map[string]spec.Node, len(current.Nodes))
	for _, node := range current.Nodes {
		currentByName[node.Name] = node
	}
	seenDesired := make(map[string]struct{}, len(desired.Nodes))
	for _, node := range desired.Nodes {
		seenDesired[node.Name] = struct{}{}
		if _, include := selectedSet[node.Name]; include {
			merged.Nodes = append(merged.Nodes, cloneNode(node))
			continue
		}
		if peer, exists := currentByName[node.Name]; exists {
			if !equalResolvedEnvelope(current, desired) {
				return spec.Resolved{}, fmt.Errorf("%w: deployment-level settings changed; select all existing nodes to apply them", ErrRecreateRequired)
			}
			merged.Nodes = append(merged.Nodes, cloneNode(peer))
		}
	}
	// An explicit selection never turns an inventory omission into an implicit
	// destroy. Keep omitted applied peers after the desired inventory order.
	for _, node := range current.Nodes {
		if _, seen := seenDesired[node.Name]; seen {
			continue
		}
		if !equalResolvedEnvelope(current, desired) {
			return spec.Resolved{}, fmt.Errorf("%w: deployment-level settings changed; select all existing nodes to apply them", ErrRecreateRequired)
		}
		merged.Nodes = append(merged.Nodes, cloneNode(node))
	}
	if err := validateResolved(merged); err != nil {
		return spec.Resolved{}, fmt.Errorf("selected changes conflict with applied peers: %w", err)
	}
	names := make(map[string]string)
	for _, node := range merged.Nodes {
		names[node.Name] = node.Name
	}
	for _, node := range merged.Nodes {
		for _, alias := range node.Aliases {
			if owner, exists := names[alias]; exists {
				return spec.Resolved{}, fmt.Errorf("selected changes conflict with applied node %s: alias %q is also used by %s", node.Name, alias, owner)
			}
			names[alias] = node.Name
		}
	}
	return merged, nil
}

// selectedOperationResolved is the runtime view of a selected operation. The
// persisted deployment may contain desired definitions for nodes that have not
// been prepared yet (for example after a partial create). Keep committed peers
// because their runtime must remain compatible with a new selected node, but
// do not resolve or validate unselected additions until they are selected.
func selectedOperationResolved(store state.Store, desired spec.Resolved, selected []string, scoped bool) (spec.Resolved, error) {
	if !scoped {
		return desired, nil
	}
	selectedSet := nodeNameSet(selected)
	result := cloneResolved(desired)
	result.Nodes = result.Nodes[:0]
	for _, node := range desired.Nodes {
		if _, include := selectedSet[node.Name]; include {
			result.Nodes = append(result.Nodes, cloneNode(node))
			continue
		}
		if _, err := store.ReadNode(node.Name); err == nil {
			result.Nodes = append(result.Nodes, cloneNode(node))
		} else if !missingPath(err) {
			return spec.Resolved{}, err
		}
	}
	return result, nil
}
