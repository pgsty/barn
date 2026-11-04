package private

import (
	"time"

	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

// reconcileControlHashes runs only under the exclusive deployment lock. A
// control election changes a node definition without rebuilding its guest.
// The deployment is the commit record: recover only a hash that differs by
// that one boolean, never accept arbitrary definition drift or a bad record.
func reconcileControlHashes(root string) error {
	store := state.Store{Root: root}
	deployment, err := store.ReadDeployment()
	if err != nil {
		// Preserve the operation's normal absence/corruption diagnostics.
		// A lock acquisition itself is not state validation.
		return nil
	}
	for index, definition := range deployment.Resolved.Nodes {
		node, err := store.ReadNode(definition.Name)
		if err != nil {
			// Let the operation's existing per-node validation report missing
			// or corrupt records. Recovery never invents state for them.
			continue
		}
		expected, err := spec.NodeHash(deployment.Resolved, definition.Name)
		if err != nil {
			return err
		}
		if node.SpecHash == expected {
			continue
		}
		previous := cloneResolved(deployment.Resolved)
		previous.Nodes[index].Control = !definition.Control
		oldHash, err := spec.NodeHash(previous, definition.Name)
		if err != nil {
			return err
		}
		if node.SpecHash != oldHash {
			continue
		}
		if err := FinalizePrepared(Deployment{Root: root}, node.Node); err != nil {
			return err
		}
		if node.GuestSpecHash == "" {
			node.GuestSpecHash = node.SpecHash
		}
		node.SpecHash = expected
		node.UpdatedAt = time.Now().UTC()
		if err := store.WriteNode(node); err != nil {
			return err
		}
	}
	return nil
}
