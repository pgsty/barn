package private

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pgsty/farrow/internal/lock"
	"github.com/pgsty/farrow/internal/persistent"
	"github.com/pgsty/farrow/internal/process"
	"github.com/pgsty/farrow/internal/sshkeys"
	"github.com/pgsty/farrow/internal/state"
)

type KeyPurgeAction = sshkeys.PurgeAction
type KeyPurgeReport = sshkeys.PurgeReport
type KeyPurgeStateError = sshkeys.PurgeStateError
type KeyPurgeIntegrityError = sshkeys.PurgeIntegrityError

// ensureNoRetainedNodes refuses a key purge while any node artifact remains.
// A node directory counts as retained even when its state cannot be decoded.
func ensureNoRetainedNodes(ctx context.Context, deploymentValue Deployment) error {
	if err := ctx.Err(); err != nil {
		return &sshkeys.PurgeStateError{Reason: fmt.Sprintf("preflight was canceled: %v", err)}
	}
	nodesDir := filepath.Join(deploymentValue.Root, "nodes")
	entries, err := os.ReadDir(nodesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return &sshkeys.PurgeIntegrityError{Reason: fmt.Sprintf("read node artifact directory: %v", err)}
	}
	if len(entries) == 0 {
		return nil
	}
	store := state.Store{Root: deploymentValue.Root}
	for _, entry := range entries {
		node, readErr := store.ReadNode(entry.Name())
		if readErr != nil {
			continue
		}
		if process.Alive(node.Process.PID) {
			return &sshkeys.PurgeStateError{Reason: fmt.Sprintf("node %s still has a live recorded process pid=%d", node.Node, node.Process.PID)}
		}
		if len(node.DataDisks) > 0 {
			return &sshkeys.PurgeStateError{Reason: fmt.Sprintf("node %s still retains %d data disk(s)", node.Node, len(node.DataDisks))}
		}
	}
	return &sshkeys.PurgeStateError{Reason: fmt.Sprintf("%d retained node artifact(s) remain under %s; destroy them first", len(entries), nodesDir)}
}

// PurgeKeys remains available after a full destroy removes state.json: it refuses
// while node artifacts or retained persistent disks exist, then removes
// exactly the allowlisted key files.
func (m Manager) PurgeKeys(ctx context.Context, apply bool) (_ KeyPurgeReport, returnErr error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return KeyPurgeReport{}, err
	}
	deploymentLock, err := acquireDeploymentLock(ctx, deploymentValue.Root, false, m.Progress)
	if err != nil {
		return KeyPurgeReport{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment key purge lock")
	}()
	if err := ensureNoRetainedNodes(ctx, deploymentValue); err != nil {
		return KeyPurgeReport{}, err
	}
	retainedDisks, err := persistent.Inventory(deploymentValue.Root)
	if err != nil {
		return KeyPurgeReport{}, &sshkeys.PurgeIntegrityError{Reason: fmt.Sprintf("inspect retained persistent disks: %v", err)}
	}
	if len(retainedDisks) != 0 {
		return KeyPurgeReport{}, &sshkeys.PurgeStateError{Reason: fmt.Sprintf("%d persistent data disk(s) remain; explicitly delete them before purging keys", len(retainedDisks))}
	}
	return sshkeys.PurgeKeys(deploymentValue.Root, apply)
}
