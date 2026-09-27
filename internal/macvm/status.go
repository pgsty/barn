package macvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pgsty/farrow/internal/lock"
	"golang.org/x/crypto/ssh"
)

func (m *Manager) inspectRuntime(ctx context.Context, view *SlotView, pref SlotPreference) {
	view.SSH = "unchecked"
	if m.Runner.Binary == "" {
		view.State = "unknown"
		view.RuntimeError = "Mac runner unavailable; run farrow mac doctor"
		return
	}
	status, err := m.status(ctx, &view.Slot)
	if err != nil {
		if ctx.Err() != nil {
			view.State, view.RuntimeError = "unknown", ctx.Err().Error()
			return
		}
		// An RPC error is not proof of shutdown. An in-flight operation can also
		// be between publishing starting state and acquiring the runner lock.
		path, pathErr := m.Store.Path("runtime", view.Name+".operation.lock")
		if pathErr != nil {
			view.State, view.RuntimeError = "unknown", pathErr.Error()
			return
		}
		if _, statErr := os.Stat(path); statErr == nil {
			op, lockErr := lock.TryAcquire(path, false)
			if lockErr != nil {
				view.State = "unknown"
				view.RuntimeError = "another operation is changing this slot"
				return
			}
			// Keep the operation excluded until the stopped proof is complete.
			defer func() {
				if err := op.Release(); err != nil {
					view.State, view.RuntimeError = "unknown", err.Error()
				}
			}()
		} else if !errors.Is(statErr, os.ErrNotExist) {
			view.State, view.RuntimeError = "unknown", statErr.Error()
			return
		}
		proof, proofErr := m.stoppedRunnerLock(&view.Slot)
		if proofErr != nil {
			view.State = "unknown"
			if errors.Is(proofErr, lock.ErrBusy) {
				view.State = "unresponsive"
			}
			view.RuntimeError = proofErr.Error()
			return
		}
		if proof != nil {
			if err := proof.Release(); err != nil {
				view.State = "unknown"
				view.RuntimeError = err.Error()
				return
			}
		}
		view.SSH = "offline"
		if view.Initialized {
			view.State = "stopped"
		} else if view.State != "failed" {
			view.State = "created"
		}
		return
	}
	view.Runtime = &status
	switch status.State {
	case "stopped", "running", "paused", "error", "starting", "pausing", "resuming", "stopping", "saving", "restoring", "unknown":
	default:
		view.State, view.RuntimeError = "unknown", fmt.Sprintf("runner returned an invalid runtime state %q", status.State)
		return
	}
	view.State = status.State
	if status.State != "running" {
		view.SSH = "offline"
		return
	}
	if !view.Initialized {
		view.SSH = "pending"
		return
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := m.probeGuestSSH(check, &view.Slot, pref); err != nil {
		view.SSH = "unavailable"
		view.SSHError = err.Error()
		return
	}
	view.SSH = "ready"
	view.State = "ready"
	view.LastError = ""
}

func (m *Manager) probeGuestSSH(ctx context.Context, slot *Slot, pref SlotPreference) error {
	signer, verify, err := m.sshIdentity(slot)
	if err != nil {
		return err
	}
	client, err := dialSSH(ctx, pref.IP, slot.User, []ssh.AuthMethod{ssh.PublicKeys(signer)}, verify)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	_, err = observeSSH(ctx, client, slot.User)
	return err
}

// A failed RPC permits a new runner only after independently proving the old
// runner no longer owns the slot. A responding paused/stopping/stopped runner
// cannot be reported running by up --no-wait.
func (m *Manager) shouldLaunch(ctx context.Context, slot *Slot) (bool, error) {
	status, err := m.status(ctx, slot)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err == nil {
		if status.State != "running" {
			return false, fmt.Errorf("%s runtime reports %s; inspect farrow mac ls and finish its current transition before retrying up", slot.Name, status.State)
		}
		return false, nil
	}
	proof, err := m.stoppedRunnerLock(slot)
	if err != nil {
		return false, err
	}
	if err = proof.Release(); err != nil {
		return false, err
	}
	return true, nil
}
