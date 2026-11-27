package linuxvm

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/lock"
	"github.com/pgsty/barn/internal/state"
)

// Deployment locates the single global deployment. Its root IS the data root:
// state, nodes, keys, and retained disks all live directly under it.
type Deployment struct {
	Root string
}

func (d Deployment) NodeDir(name string) (string, error) {
	return state.Store{Root: d.Root}.NodeDir(name)
}

func (d Deployment) EnsureNodeDir(name string) (string, error) {
	return state.Store{Root: d.Root}.EnsureNodeDir(name)
}

// openDeployment resolves the deployment root; with create it also ensures
// the root and lock directories exist.
func openDeployment(create bool) (Deployment, error) {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return Deployment{}, err
	}
	value := Deployment{Root: root}
	if create {
		if err := (state.Store{Root: root}).EnsureRoot(); err != nil {
			return Deployment{}, err
		}
	}
	return value, nil
}

func deploymentLockPath(root string) string { return filepath.Join(root, "locks", "lock") }

// deploymentLockWait bounds how long a command queues behind another one.
var deploymentLockWait = 10 * time.Minute

// acquireDeploymentLock serializes every mutating barn invocation on the
// one global deployment. flock releases automatically on process exit.
func acquireDeploymentLock(ctx context.Context, root string, shared bool, progress activity.Reporter) (*lock.File, error) {
	held, err := waitForLock(ctx, deploymentLockPath(root), shared, progress)
	if err == nil && !shared {
		if err := reconcileControlHashes(root); err != nil {
			return nil, lock.JoinRelease(err, held, "deployment recovery lock")
		}
	}
	return held, err
}

// waitForLock takes a Barn lock. While another command holds it, progress
// names that command; after deploymentLockWait the wait ends in a conflict.
// An exclusive holder records itself for the next waiter.
func waitForLock(ctx context.Context, path string, shared bool, progress activity.Reporter) (*lock.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	held, err := lock.TryAcquire(path, shared)
	if errors.Is(err, lock.ErrBusy) {
		progress.Report(activity.Event{Phase: "lock", Message: "Waiting for another barn command: " + lockHolder(path)})
		waitCtx, cancel := context.WithTimeout(ctx, deploymentLockWait)
		defer cancel()
		held, err = lock.Acquire(waitCtx, path, shared)
		if err != nil && errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.Canceled) {
			return nil, failure.New(failure.Conflict, fmt.Errorf("another barn command still holds the deployment: %s", lockHolder(path))).Because("deployment_busy").Then("retry when it finishes")
		}
	}
	if err != nil {
		return nil, err
	}
	if !shared {
		// The record only makes a waiter's message readable; never fail on it.
		_ = held.Record(currentHolder())
	}
	return held, nil
}

// tryDeploymentLock takes the deployment lock only if it is free. When it is
// busy it returns no lock and the holder: read-only commands then show the
// recorded state instead of queueing behind a long operation.
func tryDeploymentLock(root string, shared bool) (*lock.File, string, error) {
	path := deploymentLockPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", err
	}
	held, err := lock.TryAcquire(path, shared)
	if errors.Is(err, lock.ErrBusy) {
		return nil, lockHolder(path), nil
	}
	if err == nil && !shared {
		if err := reconcileControlHashes(root); err != nil {
			return nil, "", lock.JoinRelease(err, held, "deployment recovery lock")
		}
		_ = held.Record(currentHolder())
	}
	return held, "", err
}

type holderRecord struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Started time.Time `json:"started"`
}

// maxHolderCommand keeps a long exec script from outgrowing the lock record.
const maxHolderCommand = 200

func currentHolder() []byte {
	command := strings.Join(append([]string{filepath.Base(os.Args[0])}, os.Args[1:]...), " ")
	if len(command) > maxHolderCommand {
		command = command[:maxHolderCommand] + "…"
	}
	data, _ := json.Marshal(holderRecord{PID: os.Getpid(), Command: command, Started: time.Now().UTC()})
	return data
}

// lockHolder describes the command holding path, e.g.
// "barn up (pid 4821, since 14:02:31)". A start time stays true however
// long the message is displayed; an elapsed time would not.
func lockHolder(path string) string {
	var holder holderRecord
	if err := json.Unmarshal(lock.Owner(path), &holder); err != nil || holder.PID <= 0 || holder.Command == "" {
		return "another barn command"
	}
	return fmt.Sprintf("%s (pid %d, since %s)", holder.Command, holder.PID, holder.Started.Local().Format("15:04:05"))
}

// Open returns the deployment handle without creating or locking anything.
func Open() (Deployment, error) { return openDeployment(false) }

// AcquireLock takes the deployment lock; the caller must Release it.
func AcquireLock(ctx context.Context, deployment Deployment, shared bool) (*lock.File, error) {
	return acquireDeploymentLock(ctx, deployment.Root, shared, nil)
}

// PurgeDeployment removes what destroy deliberately preserves: keys and the
// deployment state documents. Images stay cached; persistent disks must have
// been deleted already.
func (m Manager) PurgeDeployment(ctx context.Context) error {
	if _, err := m.PurgeKeys(ctx, true); err != nil {
		return err
	}
	root, err := state.ResolveDataRoot()
	if err != nil {
		return err
	}
	for _, name := range []string{"state.json", "events.jsonl", "time-sync.json", "time-last.json"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, directory := range []string{"nodes", "disks", "guest-warnings"} {
		if err := os.Remove(filepath.Join(root, directory)); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			return err
		}
	}
	return nil
}

// VerifyResidualPurgeRoot checks whether root exists and is a current-user
// real directory protected from group/world writes before a residual purge.
func VerifyResidualPurgeRoot(root string) (exists bool, err error) {
	info, statErr := os.Lstat(root)
	if errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	if statErr != nil {
		return false, fmt.Errorf("inspect deployment root for purge: %w", statErr)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || int(stat.Uid) != os.Geteuid() {
		return true, fmt.Errorf("deployment root is not a current-user directory protected from group/world writes: %s", root)
	}
	return true, nil
}

// DeploymentInUse refuses privileged network teardown while any recorded node
// of the single deployment is provably (or ambiguously) live.
func DeploymentInUse(ctx context.Context) error {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return nil
	}
	store := state.Store{Root: root}
	entries, err := os.ReadDir(filepath.Join(root, "nodes"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	audit := RuntimeIdentityAuditor(execx.OSRunner{Timeout: 15 * time.Second, OutputLimit: 1 << 20}, time.Second)
	for _, entry := range entries {
		node, readErr := store.ReadNode(entry.Name())
		if readErr != nil {
			continue
		}
		observation, auditErr := audit(ctx, node)
		if auditErr != nil {
			return fmt.Errorf("node %s runtime audit is inconclusive (%v); stop the deployment first", entry.Name(), auditErr)
		}
		if observation.Live {
			return fmt.Errorf("node %s is running; stop the deployment first", node.Node)
		}
	}
	return nil
}

// FilterRecordedAddresses drops addresses belonging to this deployment's own
// recorded nodes so preflight does not report a running lab as a conflict.
func FilterRecordedAddresses(addresses []string) []string {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return addresses
	}
	deploymentState, err := (state.Store{Root: root}).ReadDeployment()
	if err != nil {
		return addresses
	}
	recorded := make(map[string]struct{}, len(deploymentState.Resolved.Nodes))
	for _, node := range deploymentState.Resolved.Nodes {
		recorded[node.Address] = struct{}{}
	}
	filtered := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if _, mine := recorded[address]; !mine {
			filtered = append(filtered, address)
		}
	}
	return filtered
}

func validImageDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == fmt.Sprintf("%x", decoded)
}

// NodeImageReferences protects each node's backing filename as well as its
// recorded digest, including bases retired from the current catalog.
func NodeImageReferences(dataRoot string) ([]image.Entry, error) {
	var references []image.Entry
	store := state.Store{Root: dataRoot}
	entries, readErr := os.ReadDir(filepath.Join(dataRoot, "nodes"))
	if errors.Is(readErr, os.ErrNotExist) {
		return references, nil
	}
	if readErr != nil {
		return nil, readErr
	}
	for _, entry := range entries {
		if transaction, transactionErr := store.ReadTransaction(entry.Name()); transactionErr == nil {
			return nil, fmt.Errorf("node %s has pending transaction %s", entry.Name(), transaction.OperationID)
		} else if !errors.Is(transactionErr, os.ErrNotExist) {
			return nil, transactionErr
		}
		node, nodeErr := store.ReadNode(entry.Name())
		if nodeErr != nil {
			return nil, fmt.Errorf("read node %s before image prune: %w", entry.Name(), nodeErr)
		}
		if !validImageDigest(node.Image.Digest) {
			return nil, fmt.Errorf("node %s has an invalid image digest", entry.Name())
		}
		arches := []string{invocationGuestArch(node.Invocation.Binary, node.Invocation.Args)}
		if arches[0] == "" {
			// Older invocations may not identify the guest architecture.
			arches = []string{"amd64", "arm64"}
		}
		for _, arch := range arches {
			file := image.CacheFile(node.Image.Alias, node.Image.Release, arch, node.Image.Digest)
			references = append(references, image.Entry{SHA256: node.Image.Digest, CacheFile: file})
		}
	}
	return references, nil
}
