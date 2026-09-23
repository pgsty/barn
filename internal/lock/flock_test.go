package lock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestExclusiveLockHonorsContext(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deployment.lock")
	first, err := Acquire(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.Release(); err != nil {
			t.Errorf("release first lock: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended lock error = %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateExclusiveRejectsWrongReleasedAndSharedTokens(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "deployment.lock")
	exclusive, err := Acquire(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := exclusive.ValidateExclusive(path); err != nil {
		t.Fatalf("live exclusive token rejected: %v", err)
	}
	if err := exclusive.ValidateExclusive(filepath.Join(root, ".", "other.lock")); err == nil {
		t.Fatal("exclusive token accepted a different path")
	}
	if err := exclusive.Release(); err != nil {
		t.Fatal(err)
	}
	if err := exclusive.ValidateExclusive(path); err == nil {
		t.Fatal("released exclusive token was accepted")
	}

	shared, err := Acquire(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := shared.Release(); err != nil {
			t.Errorf("release shared lock: %v", err)
		}
	})
	if err := shared.ValidateExclusive(path); err == nil {
		t.Fatal("shared token was accepted as exclusive")
	}
}

func TestJoinReleasePreservesOperationAndReleaseErrors(t *testing.T) {
	held, err := Acquire(context.Background(), filepath.Join(t.TempDir(), "join.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := held.handle.Close(); err != nil {
		t.Fatal(err)
	}
	operationErr := errors.New("operation failed")
	joined := JoinRelease(operationErr, held, "fixture lock")
	if !errors.Is(joined, operationErr) || joined == operationErr {
		t.Fatalf("joined error = %v", joined)
	}
}

func TestTryAcquireReportsBusyAndOwnerUntilRelease(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lock")
	held, err := TryAcquire(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Record([]byte("farrow up")); err != nil {
		t.Fatal(err)
	}
	if _, err := TryAcquire(path, true); !errors.Is(err, ErrBusy) {
		t.Fatalf("second holder = %v, want ErrBusy", err)
	}
	if owner := string(Owner(path)); owner != "farrow up" {
		t.Fatalf("owner = %q", owner)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if owner := Owner(path); len(owner) != 0 {
		t.Fatalf("released lock kept owner %q", owner)
	}
	shared, err := TryAcquire(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := shared.Record([]byte("x")); err == nil {
		t.Fatal("a shared holder recorded an owner")
	}
	_ = shared.Release()
}
