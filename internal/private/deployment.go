package private

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pgsty/farrow/internal/activity"
	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/lock"
	"github.com/pgsty/farrow/internal/state"
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

// acquireDeploymentLock serializes every mutating farrow invocation on the
// one global deployment. flock releases automatically on process exit.
func acquireDeploymentLock(ctx context.Context, root string, shared bool, progress activity.Reporter) (*lock.File, error) {
	return waitForLock(ctx, deploymentLockPath(root), shared, progress)
}

// waitForLock takes a Farrow lock. While another command holds it, progress
// names that command; after deploymentLockWait the wait ends in a conflict.
// An exclusive holder records itself for the next waiter.
func waitForLock(ctx context.Context, path string, shared bool, progress activity.Reporter) (*lock.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	held, err := lock.TryAcquire(path, shared)
	if errors.Is(err, lock.ErrBusy) {
		progress.Report(activity.Event{Phase: "lock", Message: "Waiting for another farrow command: " + lockHolder(path)})
		waitCtx, cancel := context.WithTimeout(ctx, deploymentLockWait)
		defer cancel()
		held, err = lock.Acquire(waitCtx, path, shared)
		if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, failure.New(failure.Conflict, fmt.Errorf("another farrow command still holds the deployment: %s", lockHolder(path))).Because("deployment_busy").Then("retry when it finishes")
		}
	}
	if err != nil {
		return nil, err
	}
	if !shared {
		if err := held.Record(currentHolder()); err != nil {
			return nil, errors.Join(err, held.Release())
		}
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
		if recordErr := held.Record(currentHolder()); recordErr != nil {
			return nil, "", errors.Join(recordErr, held.Release())
		}
	}
	return held, "", err
}

type holderRecord struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Started time.Time `json:"started"`
}

func currentHolder() []byte {
	command := append([]string{filepath.Base(os.Args[0])}, os.Args[1:]...)
	data, _ := json.Marshal(holderRecord{PID: os.Getpid(), Command: strings.Join(command, " "), Started: time.Now().UTC()})
	return data
}

// lockHolder describes the command holding path, e.g.
// "farrow up (pid 4821, running 2m10s)".
func lockHolder(path string) string {
	var holder holderRecord
	if err := json.Unmarshal(lock.Owner(path), &holder); err != nil || holder.PID <= 0 || holder.Command == "" {
		return "another farrow command"
	}
	return fmt.Sprintf("%s (pid %d, running %s)", holder.Command, holder.PID, time.Since(holder.Started).Round(time.Second))
}

// Open returns the deployment handle without creating or locking anything.
func Open() (Deployment, error) { return openDeployment(false) }

// AcquireLock takes the deployment lock; the caller must Release it.
func AcquireLock(ctx context.Context, deployment Deployment, shared bool) (*lock.File, error) {
	return acquireDeploymentLock(ctx, deployment.Root, shared, nil)
}
