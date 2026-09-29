package macvm

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/lock"
)

func readinessManager(t *testing.T) *Manager {
	t.Helper()
	m := &Manager{Store: testStore(t)}
	directory, err := RuntimeDir(m.Store.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return m
}

func TestStopInterruptsOnlyRequestedSlotsReadiness(t *testing.T) {
	m := readinessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, err := m.acquireOperation(ctx, "mac1", "mac up mac1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Release() }()
	second, err := m.acquireOperation(ctx, "mac2", "mac up mac2")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Release() }()
	waiting, finish, err := m.interruptibleReadiness(ctx, first, "mac1")
	if err != nil {
		t.Fatal(err)
	}
	other, finishOther, err := m.interruptibleReadiness(ctx, second, "mac2")
	if err != nil {
		t.Fatal(err)
	}
	defer finishOther()
	released := make(chan error, 1)
	go func() { <-waiting.Done(); finish(); released <- first.Release() }()
	started := time.Now()
	stopped, err := m.acquireStopOperation(ctx, "mac1", "mac stop mac1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stopped.Release() }()
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(context.Cause(waiting), errReadinessStopped) || other.Err() != nil || time.Since(started) > 2*time.Second {
		t.Fatal("stop did not promptly cancel just its own slot")
	}
	path, _ := RuntimeDir(m.Store.Root, false)
	if _, err := os.Lstat(readinessSocket(path, "mac1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readiness endpoint leaked: %v", err)
	}
}

func TestReadinessRejectsStaleNonce(t *testing.T) {
	m := readinessManager(t)
	held, err := m.acquireOperation(context.Background(), "mac1", "mac up mac1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	ctx, finish, err := m.interruptibleReadiness(context.Background(), held, "mac1")
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	directory, _ := RuntimeDir(m.Store.Root, false)
	conn, err := net.Dial("unix", readinessSocket(directory, "mac1"))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	token, _ := NewInstanceID()
	_ = json.NewEncoder(conn).Encode(map[string]string{"token": token})
	var reply any
	if err := json.NewDecoder(conn).Decode(&reply); err == nil {
		t.Fatal("stale nonce was acknowledged")
	}
	_ = conn.Close()
	if ctx.Err() != nil {
		t.Fatal("stale nonce cancelled readiness")
	}
}

func TestStopDoesNotInterruptOtherOperations(t *testing.T) {
	m := readinessManager(t)
	held, err := m.acquireOperation(context.Background(), "mac1", "mac reset mac1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := m.acquireStopOperation(ctx, "mac1", "mac stop mac1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("conflicting operation bypassed: %v", err)
	}
	path, _ := m.Store.Path("runtime", "mac1.operation.lock")
	if err := held.ValidateExclusive(path); err != nil {
		t.Fatal(err)
	}
}

func TestReadinessPreservesUnexpectedEndpointAndRequiresSlotLock(t *testing.T) {
	m := readinessManager(t)
	held, err := m.acquireOperation(context.Background(), "mac1", "mac up mac1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	if _, _, err := m.interruptibleReadiness(context.Background(), held, "mac2"); err == nil {
		t.Fatal("accepted a different slot's lock")
	}
	directory, _ := RuntimeDir(m.Store.Root, false)
	endpoint := readinessSocket(directory, "mac1")
	if err := os.WriteFile(endpoint, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.interruptibleReadiness(context.Background(), held, "mac1"); err == nil {
		t.Fatal("replaced a nonsocket endpoint")
	}
	if b, err := os.ReadFile(endpoint); err != nil || string(b) != "preserve" {
		t.Fatal("unexpected endpoint modified")
	}
	path, _ := m.Store.Path("runtime", "mac1.operation.lock")
	var owner operationOwner
	if err := json.Unmarshal(lock.Owner(path), &owner); err != nil || owner.ReadyToken != "" {
		t.Fatal("failed readiness left an interrupt token")
	}
}

func TestReadinessEndpointNeverTakesAnotherMachinesSocket(t *testing.T) {
	m, _ := testManager(t)
	other := testMachine(t, m, "a-ready", "10.10.21.0/24", true)
	startFakeRuntime(t, m, other)
	testMachine(t, m, "a", "10.10.20.0/24", true)
	ctx := context.Background()
	op, err := m.acquireOperation(ctx, "a", "mac up a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = op.Release() }()
	_, finish, err := m.interruptibleReadiness(ctx, op, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.status(ctx, other); err != nil {
		t.Fatalf("a's readiness wait broke a-ready's runner socket: %v", err)
	}
	finish()
	if _, err := m.status(ctx, other); err != nil {
		t.Fatalf("finishing a's readiness wait removed a-ready's runner socket: %v", err)
	}
}
