package private

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pgsty/farrow/internal/disk"
	"github.com/pgsty/farrow/internal/identity"
	"github.com/pgsty/farrow/internal/lock"
	"github.com/pgsty/farrow/internal/persistent"
	"github.com/pgsty/farrow/internal/spec"
	"github.com/pgsty/farrow/internal/state"
)

func privateFilesystem(value string) string {
	if value == "" {
		return "auto"
	}
	return value
}

func privatePersistentIdentities(deploymentValue Deployment, resolved spec.Resolved) ([]persistent.Identity, error) {
	result := make([]persistent.Identity, 0)
	for _, node := range resolved.Nodes {
		for _, definition := range node.Disks {
			if !definition.Persistent {
				continue
			}
			serial, err := identity.DiskSerial(node.Name, definition.Name)
			if err != nil {
				return nil, err
			}
			result = append(result, persistent.Identity{
				Node: node.Name, Name: definition.Name,
				Serial: serial, Size: definition.Size, Mount: definition.Mount, Filesystem: privateFilesystem(definition.Filesystem),
			})
		}
	}
	return result, nil
}

func validatePrivatePersistentDesired(deploymentValue Deployment, resolved spec.Resolved) error {
	desired, err := privatePersistentIdentities(deploymentValue, resolved)
	if err != nil {
		return err
	}
	_, err = persistent.ValidateDesired(deploymentValue.Root, desired)
	return err
}

func privatePrepareDeployment(config PrepareConfig) Deployment {
	return Deployment{Root: config.DeploymentRoot}
}

func validatePrivatePersistentState(deploymentValue Deployment, deploymentState state.DeploymentState, nodes []state.NodeState) ([]persistent.Identity, error) {
	desired, err := privatePersistentIdentities(deploymentValue, deploymentState.Resolved)
	if err != nil {
		return nil, err
	}
	// Explicit node removal leaves owned persistent disks behind after dropping
	// that node from the spec. Destroy must preserve those disks, not demand
	// that the removed node be recreated before the rest can be cleaned up.
	// Inventory still rejects unknown ownership, symlinks and extra artifacts;
	// disks belonging to a node still in the spec retain exact identity checks.
	retained, err := persistent.Inventory(deploymentValue.Root)
	if err != nil {
		return nil, err
	}
	declared := make(map[string]bool, len(deploymentState.Resolved.Nodes))
	for _, node := range deploymentState.Resolved.Nodes {
		declared[node.Name] = true
	}
	validated := append([]persistent.Identity(nil), desired...)
	for _, record := range retained {
		if !declared[record.Node] {
			validated = append(validated, persistent.Identity{Node: record.Node, Name: record.Name, Serial: record.Serial, Size: record.Size, Mount: record.Mount, Filesystem: record.Filesystem})
		}
	}
	if _, err := persistent.ValidateDesired(deploymentValue.Root, validated); err != nil {
		return nil, err
	}
	desiredDisks := make(map[string]persistent.Identity, len(desired))
	for _, identityValue := range desired {
		desiredDisks[identityValue.Node+"\x00"+identityValue.Name] = identityValue
	}
	preserved := make(map[string]persistent.Record, len(retained))
	for _, record := range retained {
		preserved[record.Node+"\x00"+record.Name] = record
	}
	stateDisks := make(map[string]state.DataDisk)
	for _, node := range nodes {
		for _, dataDisk := range node.DataDisks {
			if dataDisk.Persistent {
				if err := persistent.ValidateSource(deploymentValue.Root, dataDisk.Path); err != nil && !alreadyPreserved(dataDisk, preserved[node.Node+"\x00"+dataDisk.Name]) {
					return nil, err
				}
				stateDisks[node.Node+"\x00"+dataDisk.Name] = dataDisk
			}
		}
	}
	for key, dataDisk := range stateDisks {
		identityValue, ok := desiredDisks[key]
		if !ok {
			return nil, errors.New("persistent disk state is not present in the resolved configuration")
		}
		if dataDisk.Serial != identityValue.Serial || dataDisk.Size != identityValue.Size || dataDisk.Mount != identityValue.Mount {
			return nil, fmt.Errorf("persistent disk %s/%s has incompatible size, mount, or serial", identityValue.Node, identityValue.Name)
		}
	}
	return desired, nil
}

// alreadyPreserved is a destroy interrupted between preserving a persistent
// disk and removing its node: the node-local source is gone and the retained
// record carries the same identity, so a retry may continue.
func alreadyPreserved(dataDisk state.DataDisk, record persistent.Record) bool {
	if _, err := os.Lstat(dataDisk.Path); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return record.Path != "" && record.Serial == dataDisk.Serial && record.Size == dataDisk.Size && record.Mount == dataDisk.Mount
}

type privateDiskInspector interface {
	Inspect(context.Context, string) (disk.Info, error)
}

func resolvePrivateDataDisk(ctx context.Context, config PrepareConfig, nodeDir string, definition spec.Disk, serial string) (string, bool, error) {
	defaultPath := filepath.Join(nodeDir, definition.Name+".qcow2")
	if !definition.Persistent {
		if _, err := config.Disks.CreateBlank(ctx, defaultPath, definition.Size); err != nil {
			return "", false, err
		}
		return defaultPath, true, nil
	}
	deploymentValue := privatePrepareDeployment(config)
	desired, err := privatePersistentIdentities(deploymentValue, config.Resolved)
	if err != nil {
		return "", false, err
	}
	identityValue := persistent.Identity{Node: filepath.Base(nodeDir), Name: definition.Name, Serial: serial, Size: definition.Size, Mount: definition.Mount, Filesystem: privateFilesystem(definition.Filesystem)}
	record, found, err := persistent.Find(deploymentValue.Root, desired, identityValue)
	if err != nil {
		return "", false, err
	}
	if !found {
		if _, err := config.Disks.CreateBlank(ctx, defaultPath, definition.Size); err != nil {
			return "", false, err
		}
		return defaultPath, true, nil
	}
	if inspector, ok := config.Disks.(privateDiskInspector); ok {
		info, err := inspector.Inspect(ctx, record.Path)
		if err != nil {
			return "", false, fmt.Errorf("inspect retained private disk: %w", err)
		}
		if err := disk.ValidateRuntime(info, false); err != nil || info.VirtualSize != definition.Size {
			return "", false, fmt.Errorf("retained private disk %s/%s virtual size is incompatible", identityValue.Node, identityValue.Name)
		}
	}
	return record.Path, false, nil
}

// PersistentDisks returns the strict deployment inventory without mutation.
func (m Manager) PersistentDisks() ([]persistent.Record, error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return nil, err
	}
	return persistent.Inventory(deploymentValue.Root)
}

// DeletePersistent is the only private API which deletes retained data disks.
// Every node must already be destroyed and the global lease inactive. CLI
// confirmation is intentionally kept outside this filesystem boundary.
func (m Manager) DeletePersistent(ctx context.Context) (_ []persistent.Record, returnErr error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return nil, err
	}
	deploymentLock, err := acquireDeploymentLock(ctx, deploymentValue.Root, false, m.Progress)
	if err != nil {
		return nil, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "persistent disk deletion lock")
	}()
	nodesDir := filepath.Join(deploymentValue.Root, "nodes")
	entries, err := os.ReadDir(nodesDir)
	if err == nil && len(entries) != 0 {
		return nil, errors.New("will not delete persistent disks while node files remain")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return persistent.DeleteAll(deploymentValue.Root)
}
