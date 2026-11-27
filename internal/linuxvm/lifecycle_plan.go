package linuxvm

import (
	"context"
	"fmt"

	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

type LifecyclePlan struct {
	WholeDeployment bool     `json:"whole_deployment,omitempty"`
	Source          string   `json:"source,omitempty"`
	Start           []string `json:"start,omitempty"`
	Changes         []string `json:"changes,omitempty"`
	Images          []string `json:"images"`
	CPUs            int      `json:"cpus"`
	Memory          int64    `json:"memory_bytes"`
	Storage         int64    `json:"disk_bytes"`
	Blocked         string   `json:"blocked,omitempty"`
	Schema          int      `json:"schema"`
	Action          string   `json:"action"`
	SpecHash        string   `json:"spec_hash"`
	Nodes           []string `json:"nodes"`
	Destructive     bool     `json:"destructive"`
	Create          []string `json:"create,omitempty"`
	Recreate        []string `json:"recreate,omitempty"`
	Resize          []string `json:"resize,omitempty"`
	Missing         []string `json:"missing,omitempty"`
}

func (m Manager) Plan(ctx context.Context, requested spec.Resolved) (LifecyclePlan, error) {
	if err := validateResolved(requested); err != nil {
		return LifecyclePlan{}, err
	}
	selected, err := selectedNodeNames(requested, m.Nodes)
	if err != nil {
		return LifecyclePlan{}, err
	}
	deploymentValue, err := m.openDeployment(false)
	if err != nil && !missingPath(err) {
		return LifecyclePlan{}, err
	}
	var deploymentState state.DeploymentState
	var blocked string
	if err == nil {
		deploymentState, err = (state.Store{Root: deploymentValue.Root}).ReadDeployment()
		if err != nil && !missingPath(err) {
			return LifecyclePlan{}, err
		}
		if err == nil {
			effective, selectionErr := applyNodeSelection(deploymentState.Resolved, requested, m.Nodes)
			if selectionErr != nil {
				blocked = selectionErr.Error()
			} else {
				requested = effective
			}
		}
	}
	selectedRequested := resolvedNodeSelection(requested, selected)
	runtimeRequested := selectedRequested
	if deploymentState.Schema != 0 && len(m.Nodes) != 0 {
		runtimeRequested, err = selectedOperationResolved(state.Store{Root: deploymentValue.Root}, requested, selected, true)
		if err != nil {
			return LifecyclePlan{}, err
		}
	}
	profile, err := m.nativeProfile()
	if err != nil {
		return LifecyclePlan{}, err
	}
	runtime, err := selectRuntime(profile, runtimeRequested)
	if err != nil {
		return LifecyclePlan{}, err
	}
	// Planning compares local state/specs and looks up only new/replaced
	// images. Host checks and downloads belong to up.
	images, _, err := m.resolveBootImages(ctx, runtime.Profile, runtimeRequested, nil)
	if err != nil {
		return LifecyclePlan{}, err
	}
	hash, err := spec.Hash(requested)
	if err != nil {
		return LifecyclePlan{}, err
	}
	result := buildBaseLifecyclePlan(selectedRequested, selected, images, hash, blocked)
	if deploymentState.Schema == 0 {
		return result, nil
	}
	store := state.Store{Root: deploymentValue.Root}
	hasState := func(name string) bool {
		_, readErr := store.ReadNode(name)
		return readErr == nil
	}
	selectedSet := nodeNameSet(selected)
	diff := diffResolved(deploymentState.Resolved, requested, hasState)
	result.Changes = describeChanges(deploymentState.Resolved, requested, selectedSet)
	recordPendingRootResizes(store, requested, selected, &diff, &result)

	if len(m.Nodes) == 0 {
		result.Create = append([]string(nil), diff.Create...)
		result.Recreate = append([]string(nil), diff.Changed...)
		result.Resize = append([]string(nil), diff.Resize...)
		result.Missing = append([]string(nil), diff.Removed...)
	} else {
		result.Create = selectedNodeList(diff.Create, selectedSet)
		result.Recreate = selectedNodeList(diff.Changed, selectedSet)
		result.Resize = selectedNodeList(diff.Resize, selectedSet)
		result.Missing = selectedNodeList(diff.Removed, selectedSet)
	}
	runtimeDrift, err := runtimeDriftNodes(store, resolvedNodeSelection(requested, selected), runtime.Profile)
	if err != nil {
		return LifecyclePlan{}, err
	}
	return finalizeLifecyclePlanAction(store, deploymentState.Resolved, selected, selectedSet, hasState, diff, runtimeDrift, result)
}

func buildBaseLifecyclePlan(selectedRequested spec.Resolved, selected []string, images map[string]image.Entry, hash, blocked string) LifecyclePlan {
	result := LifecyclePlan{
		Schema:   1,
		Action:   "create",
		SpecHash: hash,
		Nodes:    append([]string(nil), selected...),
		Create:   append([]string(nil), selected...),
		Blocked:  blocked,
	}
	seenImages := make(map[string]struct{})
	for _, node := range selectedRequested.Nodes {
		entry := images[node.Name]
		label := entry.Alias + "@" + entry.Release + "/" + entry.Arch
		if _, seen := seenImages[label]; !seen {
			result.Images = append(result.Images, label)
			seenImages[label] = struct{}{}
		}
		result.CPUs += node.CPUs
		result.Memory += node.Memory
		result.Storage += node.RootDisk
		for _, disk := range node.Disks {
			result.Storage += disk.Size
		}
	}
	return result
}

func recordPendingRootResizes(store state.Store, requested spec.Resolved, selected []string, diff *resolvedDiff, result *LifecyclePlan) {
	resizeSet := nodeNameSet(diff.Resize)
	for _, name := range selected {
		node, err := store.ReadNode(name)
		if err != nil || node.RootResize == nil {
			continue
		}
		target, exists := resizeTargetNode(requested, name)
		if exists && target.RootDisk != node.RootResize.Target {
			result.Blocked = fmt.Sprintf("node %s root growth is pending; retry resize -D %dB %s", name, node.RootResize.Target, name)
		}
		result.Changes = append(result.Changes, fmt.Sprintf("%s root growth pending (%s)", name, node.RootResize.Stage))
		if _, exists := resizeSet[name]; !exists {
			diff.Resize = append(diff.Resize, name)
			resizeSet[name] = struct{}{}
		}
	}
}

func finalizeLifecyclePlanAction(store state.Store, applied spec.Resolved, selected []string, selectedSet map[string]struct{}, hasState func(string) bool, diff resolvedDiff, runtimeDrift []string, result LifecyclePlan) (LifecyclePlan, error) {
	switch {
	case diff.EnvelopeChanged:
		result.Action = "recreate"
		result.Destructive = true
		result.WholeDeployment = true
		// Deployment-level changes replace every existing selected node, even
		// when its own definition is unchanged. Keep the plan's replacement
		// list complete so follow-up commands cannot target only a subset.
		result.Recreate = nil
		for _, name := range selected {
			if hasState(name) {
				result.Recreate = append(result.Recreate, name)
			}
		}
		result.Resize = nil
		return result, nil
	case len(diff.Removed) != 0:
		result.Action = "blocked-removal"
		return result, nil
	case len(diff.Changed) != 0:
		result.Action = "recreate"
		result.Destructive = true
		return result, nil
	case len(runtimeDrift) != 0:
		result.Action = "recreate"
		result.Destructive = true
		result.Recreate = append([]string(nil), runtimeDrift...)
		return result, nil
	case len(diff.Resize) != 0:
		result.Action = "resize"
		return result, nil
	}
	allRunning, allStartable := true, true
	for _, definition := range applied.Nodes {
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			continue
		}
		if err != nil {
			return LifecyclePlan{}, err
		}
		allRunning = allRunning && node.Phase == state.Running
		allStartable = allStartable && (node.Phase == state.Running || node.Phase == state.Prepared || node.Phase == state.Stopped)
		if node.Phase == state.Prepared || node.Phase == state.Stopped {
			result.Start = append(result.Start, node.Node)
		}
	}
	switch {
	case len(result.Create) != 0:
		result.Action = "converge"
	case allRunning:
		result.Action = "none"
	case allStartable:
		result.Action = "start"
	default:
		result.Action = "repair"
	}
	return result, nil
}
