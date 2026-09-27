package macvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/lock"
)

var errReadinessStopped = errors.New("SSH readiness was interrupted by a stop request")

type operationOwner struct {
	Command    string `json:"command"`
	PID        int    `json:"pid"`
	ReadyToken string `json:"ready_token,omitempty"`
}

// The slot operation lock continues to exclude conflicting writes during first
// SSH provisioning. Stop can cancel just this bounded wait through a private,
// nonce-bound endpoint instead of signalling a PID or racing slot files.
func (m *Manager) interruptibleReadiness(ctx context.Context, held *lock.File, name string) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	path, err := m.Store.Path("runtime", name+".operation.lock")
	if err != nil {
		return nil, nil, err
	}
	if err = held.ValidateExclusive(path); err != nil {
		return nil, nil, err
	}
	directory, err := RuntimeDir(m.Store.Root, true)
	if err != nil {
		return nil, nil, err
	}
	socket := filepath.Join(directory, name+"-ready.sock")
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, nil, errors.New("readiness endpoint is not a socket; it was preserved")
		}
		if err = os.Remove(socket); err != nil {
			return nil, nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	if err = os.Chmod(socket, 0600); err != nil {
		_ = listener.Close()
		return nil, nil, err
	}
	token, err := NewInstanceID()
	if err != nil {
		_ = listener.Close()
		return nil, nil, err
	}
	wait, cancel := context.WithCancelCause(ctx)
	owner, _ := json.Marshal(operationOwner{Command: "mac up " + name, PID: os.Getpid(), ReadyToken: token})
	if err = held.Record(owner); err != nil {
		cancel(err)
		_ = listener.Close()
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			var request struct {
				Token string `json:"token"`
			}
			if err = json.NewDecoder(io.LimitReader(conn, 1024)).Decode(&request); err == nil && request.Token == token {
				cancel(errReadinessStopped)
				_ = json.NewEncoder(conn).Encode(map[string]bool{"cancelled": true})
			}
			_ = conn.Close()
		}
	}()
	finish := func() { cancel(nil); _ = listener.Close(); <-done }
	return wait, finish, nil
}

func (m *Manager) waitForSSH(ctx context.Context, held *lock.File, slot *Slot, pref SlotPreference, password string) (SSHObservation, error) {
	wait, finish, err := m.interruptibleReadiness(ctx, held, slot.Name)
	if err != nil {
		return SSHObservation{}, err
	}
	defer finish()
	result, err := m.readySSH(wait, slot, pref, password)
	if errors.Is(context.Cause(wait), errReadinessStopped) {
		return SSHObservation{}, failure.New(failure.Cancelled, fmt.Errorf("%s: %w", slot.Name, errReadinessStopped)).Because("mac_readiness_interrupted")
	}
	return result, err
}

func (m *Manager) acquireStopOperation(ctx context.Context, name string) (*lock.File, error) {
	name, err := NormalizeSlot(name)
	if err != nil {
		return nil, err
	}
	acknowledged := ""
	interrupt := func(path string) {
		var owner operationOwner
		if json.Unmarshal(lock.Owner(path), &owner) != nil || !validUUID(owner.ReadyToken) || owner.ReadyToken == acknowledged {
			return
		}
		directory, err := RuntimeDir(m.Store.Root, false)
		if err != nil {
			return
		}
		probe, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		conn, err := (&net.Dialer{}).DialContext(probe, "unix", filepath.Join(directory, name+"-ready.sock"))
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		deadline, _ := probe.Deadline()
		_ = conn.SetDeadline(deadline)
		if err = json.NewEncoder(conn).Encode(map[string]string{"token": owner.ReadyToken}); err != nil {
			return
		}
		var response struct {
			Cancelled bool `json:"cancelled"`
		}
		if json.NewDecoder(io.LimitReader(conn, 1024)).Decode(&response) == nil && response.Cancelled {
			acknowledged = owner.ReadyToken
			m.progress("Cancelled %s SSH readiness wait; continuing shutdown", name)
		}
	}
	return m.acquireMacLock(ctx, name+".operation.lock", "mac stop "+name, interrupt)
}
