package macvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pgsty/barn/internal/lock"
)

// Lock order is a machine operation, then shared state, then the native
// runner. Runtime-only updates can use the machine lock, so preparing an image
// never prevents an unrelated running guest from shutting down.
func (m *Manager) acquireOperation(ctx context.Context, name, action string) (*lock.File, error) {
	if !ValidName(name) {
		return nil, invalidName(name)
	}
	return m.acquireMacLock(ctx, name+".operation.lock", action)
}

func (m *Manager) acquireState(ctx context.Context, action string) (*lock.File, error) {
	return m.acquireMacLock(ctx, "state.lock", action)
}

func (m *Manager) acquireMacLock(ctx context.Context, filename, action string, onBusy ...func(string)) (*lock.File, error) {
	if _, err := m.Store.mkdir("runtime"); err != nil {
		return nil, err
	}
	path, err := m.Store.Path("runtime", filename)
	if err != nil {
		return nil, err
	}
	lastNotice := time.Time{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		held, err := lock.TryAcquire(path, false)
		if err == nil {
			owner, _ := json.Marshal(operationOwner{Command: action, PID: os.Getpid()})
			if err := held.Record(owner); err != nil {
				return nil, errors.Join(err, held.Release())
			}
			return held, nil
		}
		if !errors.Is(err, lock.ErrBusy) {
			return nil, err
		}
		for _, callback := range onBusy {
			callback(path)
		}
		if lastNotice.IsZero() || time.Since(lastNotice) >= 15*time.Second {
			var owner operationOwner
			_ = json.Unmarshal(lock.Owner(path), &owner)
			label := "another Mac operation"
			if owner.Command != "" {
				label = fmt.Sprintf("%s (pid %d)", owner.Command, owner.PID)
			}
			m.report("lock", "Waiting for %s before %s; Ctrl-C cancels this wait", label, action)
			lastNotice = time.Now()
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
