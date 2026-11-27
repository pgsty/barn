package linuxvm

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/spec"
)

var ErrRecreateRequired error = failure.New(failure.Conflict, errors.New("recreate required")).Because("recreate_required")

var ErrResizeRequired error = failure.New(failure.Conflict, errors.New("resize required")).Because("resize_required")

// ErrNodesRemoved reports nodes present in deployment state but absent from the
// desired configuration. The absence of a node from a configuration never
// implies destruction; removal is an explicit `barn destroy <node> --force`.
var ErrNodesRemoved error = failure.New(failure.Conflict, errors.New("inventory no longer lists existing node(s)")).Because("nodes_removed")

// resolvedDiff is the node-granular classification of a desired configuration
// against the applied deployment state.
type resolvedDiff struct {
	EnvelopeChanged bool
	Create          []string // desired nodes without committed state: new peers, or the per-node recreate window
	Changed         []string // desired nodes whose non-resource definition differs while their state still exists
	Resize          []string // CPU/memory changes or root disk growth
	Removed         []string // stateful nodes the desired configuration dropped
	Unchanged       []string
}

func diffResolved(persisted, desired spec.Resolved, hasState func(string) bool) resolvedDiff {
	diff := resolvedDiff{}
	if !equalResolvedEnvelope(persisted, desired) {
		diff.EnvelopeChanged = true
	}
	persistedNodes := make(map[string]spec.Node, len(persisted.Nodes))
	for _, node := range persisted.Nodes {
		persistedNodes[node.Name] = node
	}
	desiredNames := make(map[string]struct{}, len(desired.Nodes))
	for _, node := range desired.Nodes {
		desiredNames[node.Name] = struct{}{}
		current, known := persistedNodes[node.Name]
		stateful := known && hasState(node.Name)
		switch {
		case !stateful:
			diff.Create = append(diff.Create, node.Name)
		case sameEffectiveResolvedNode(persisted, current, desired, node):
			diff.Unchanged = append(diff.Unchanged, node.Name)
		case resourceOnlyNodeChange(persisted, current, desired, node):
			diff.Resize = append(diff.Resize, node.Name)
		default:
			diff.Changed = append(diff.Changed, node.Name)
		}
	}
	for _, node := range persisted.Nodes {
		if _, kept := desiredNames[node.Name]; !kept && hasState(node.Name) {
			diff.Removed = append(diff.Removed, node.Name)
		}
	}
	return diff
}

func resourceOnlyNodeChange(first spec.Resolved, firstNode spec.Node, second spec.Resolved, secondNode spec.Node) bool {
	firstNode.Image = effectiveNodeImage(first, firstNode)
	secondNode.Image = effectiveNodeImage(second, secondNode)
	if secondNode.RootDisk < firstNode.RootDisk {
		return false
	}
	firstNode.RootDisk, secondNode.RootDisk = 0, 0
	firstNode.CPUs, firstNode.Memory = 0, 0
	secondNode.CPUs, secondNode.Memory = 0, 0
	return sameResolvedNode(firstNode, secondNode)
}

func describeChanges(current, desired spec.Resolved, selected map[string]struct{}) []string {
	changes := make([]string, 0)
	add := func(label string, before, after any) {
		if !reflect.DeepEqual(before, after) {
			changes = append(changes, fmt.Sprintf("%s: %v -> %v", label, before, after))
		}
	}
	add("default image", current.Image, desired.Image)
	add("guest architecture", current.Arch, desired.Arch)
	add("SSH user", current.SSHUser, desired.SSHUser)
	if !reflect.DeepEqual(current.Private, desired.Private) {
		changes = append(changes, "private network changed")
	}
	before := make(map[string]spec.Node, len(current.Nodes))
	for _, node := range current.Nodes {
		before[node.Name] = node
	}
	for _, node := range desired.Nodes {
		old, exists := before[node.Name]
		if _, include := selected[node.Name]; !exists || !include || sameEffectiveResolvedNode(current, old, desired, node) {
			continue
		}
		old, node = alignAutoFilesystems(old, node)
		prefix := node.Name + " "
		add(prefix+"CPU", old.CPUs, node.CPUs)
		add(prefix+"memory (MiB)", old.Memory>>20, node.Memory>>20)
		add(prefix+"root disk (GiB)", old.RootDisk/spec.GiB, node.RootDisk/spec.GiB)
		add(prefix+"image", effectiveNodeImage(current, old), effectiveNodeImage(desired, node))
		add(prefix+"address", old.Address, node.Address)
		add(prefix+"control node", old.Control, node.Control)
		for _, item := range []struct {
			name          string
			before, after any
		}{
			{"data disks", old.Disks, node.Disks}, {"shared directories", old.Shares, node.Shares},
			{"port forwards", old.Forwards, node.Forwards}, {"aliases", old.Aliases, node.Aliases},
		} {
			if !reflect.DeepEqual(item.before, item.after) {
				changes = append(changes, prefix+item.name+" changed")
			}
		}
	}
	return changes
}

func effectiveNodeImage(resolved spec.Resolved, node spec.Node) string {
	if node.Image != "" {
		return node.Image
	}
	return resolved.Image
}

func sameEffectiveResolvedNode(first spec.Resolved, firstNode spec.Node, second spec.Resolved, secondNode spec.Node) bool {
	firstNode.Image = effectiveNodeImage(first, firstNode)
	secondNode.Image = effectiveNodeImage(second, secondNode)
	return sameResolvedNode(firstNode, secondNode)
}

func equalResolvedEnvelope(first, second spec.Resolved) bool {
	firstHash, firstErr := spec.Hash(envelopeOf(first))
	secondHash, secondErr := spec.Hash(envelopeOf(second))
	return firstErr == nil && secondErr == nil && firstHash == secondHash
}

// sameResolvedNode compares two definitions of one node. A disk requested as
// "auto" keeps whatever filesystem the guest already formatted, so auto never
// differs from a concrete xfs or ext4 on the other side.
func sameResolvedNode(first, second spec.Node) bool {
	first, second = alignAutoFilesystems(first, second)
	firstData, firstErr := spec.CanonicalJSON(spec.Resolved{Nodes: []spec.Node{first}})
	secondData, secondErr := spec.CanonicalJSON(spec.Resolved{Nodes: []spec.Node{second}})
	return firstErr == nil && secondErr == nil && string(firstData) == string(secondData)
}

func alignAutoFilesystems(first, second spec.Node) (spec.Node, spec.Node) {
	first.Disks = append([]spec.Disk(nil), first.Disks...)
	second.Disks = append([]spec.Disk(nil), second.Disks...)
	index := make(map[string]int, len(first.Disks))
	for i, disk := range first.Disks {
		index[disk.Name] = i
	}
	for j, disk := range second.Disks {
		i, ok := index[disk.Name]
		if !ok {
			continue
		}
		if autoFilesystem(first.Disks[i].Filesystem) || autoFilesystem(disk.Filesystem) {
			first.Disks[i].Filesystem = ""
			second.Disks[j].Filesystem = ""
		}
	}
	return first, second
}

// autoFilesystem accepts both spellings of "let the guest choose": the inventory
// says auto, the resolved spec leaves the field empty.
func autoFilesystem(value string) bool { return value == "" || value == "auto" }

// refuseDrift is the one converge boundary: up and reload apply additions
// and starts, and both report definition drift or removed nodes for the
// explicit recreate/destroy paths instead of acting on them.
func refuseDrift(diff resolvedDiff) error {
	if diff.EnvelopeChanged {
		return failure.WithNext(fmt.Errorf("%w: deployment-level settings changed", ErrRecreateRequired), "barn plan, then barn recreate")
	}
	if len(diff.Removed) != 0 {
		return failure.WithNext(fmt.Errorf("%w: %s; Barn never destroys a node because it left the inventory", ErrNodesRemoved, strings.Join(diff.Removed, ", ")), "barn destroy "+strings.Join(diff.Removed, " ")+", or add them back to the inventory")
	}
	if len(diff.Changed) != 0 {
		return fmt.Errorf("%w: node(s) %s changed; run `barn plan` to review, then `barn recreate %s`", ErrRecreateRequired, strings.Join(diff.Changed, ", "), strings.Join(diff.Changed, " "))
	}
	if len(diff.Resize) != 0 {
		return fmt.Errorf("%w: node(s) %s changed CPU, memory or root capacity; run `barn resize %s`", ErrResizeRequired, strings.Join(diff.Resize, ", "), strings.Join(diff.Resize, " "))
	}
	return nil
}
