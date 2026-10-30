package private

import (
	"context"

	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

// resolveBootImages uses the committed image and boot mode of unchanged nodes.
// Only new/replaced nodes depend on the active catalog; a catalog rotation must
// not prevent an existing disk from being planned or restarted. creating also
// forces catalog resolution for an explicit recreate of an unchanged node.
func (m *Manager) resolveBootImages(ctx context.Context, profile platform.Profile, requested spec.Resolved, creating []string) (map[string]image.Entry, string, error) {
	var persisted state.DeploymentState
	var store state.Store
	deployment, err := m.openDeployment(false)
	if err != nil && !missingPath(err) {
		return nil, "", err
	}
	if err == nil {
		store = state.Store{Root: deployment.Root}
		persisted, err = store.ReadDeployment()
		if err != nil && !missingPath(err) {
			return nil, "", err
		}
	}
	applied := make(map[string]spec.Node)
	if persisted.Schema != 0 {
		requested = materializeExistingForwardPorts(requested, persisted.Resolved)
		for _, node := range persisted.Resolved.Nodes {
			applied[node.Name] = node
		}
	}
	createSet := nodeNameSet(creating)
	entries := make(map[string]image.Entry)
	lookedUp := make(map[string]image.Entry)
	boot := ""
	for _, definition := range requested.Nodes {
		alias := definition.Image
		if alias == "" {
			alias = requested.Image
		}
		var entry image.Entry
		previous, exists := applied[definition.Name]
		_, create := createSet[definition.Name]
		if exists && !create && sameResolvedNode(previous, definition) {
			node, readErr := store.ReadNode(definition.Name)
			if readErr != nil && !missingPath(readErr) {
				return nil, "", readErr
			}
			arch := invocationGuestArch(node.Invocation.Binary, node.Invocation.Args)
			if readErr == nil && node.Image.Alias != "" && node.Image.Release != "" && node.Image.Digest != "" {
				bootMode := "bios"
				if node.NVRAM != "" {
					bootMode = "uefi"
				}
				entry = image.Entry{Alias: node.Image.Alias, Release: node.Image.Release, Arch: arch, SHA256: node.Image.Digest, Boot: bootMode}
			}
		}
		if entry.Alias == "" {
			var found bool
			entry, found = lookedUp[alias]
			if !found {
				if err := m.ensureImageSession(); err != nil {
					return nil, "", err
				}
				entry, err = m.lookupOneImage(ctx, alias, profile.Arch)
				if err != nil {
					return nil, "", err
				}
				lookedUp[alias] = entry
			}
		}
		boot, err = mergeBootMode(profile, boot, alias, entry.Boot)
		if err != nil {
			return nil, "", err
		}
		entries[definition.Name] = entry
	}
	return entries, boot, nil
}
