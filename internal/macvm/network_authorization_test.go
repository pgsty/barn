package macvm

import (
	"context"
	"errors"
	"testing"

	"github.com/pgsty/farrow/internal/failure"
)

func TestNetworkAdministratorCancellationRemainsRetryable(t *testing.T) {
	err := networkAdministratorError(context.Background(), errors.New("exit status 1"), "0:531: execution error: User canceled. (-128)\n")
	class, reason, next := failure.Classify(err)
	if class != failure.Cancelled || reason != "mac_authorization_cancelled" || next == "" {
		t.Fatalf("authorization cancel is not classified: %v", err)
	}
	original := errors.New("installer exited")
	err = networkAdministratorError(context.Background(), original, "installer rejected active VM")
	if !errors.Is(err, original) {
		t.Fatal("installer failure cause lost")
	}
	class, _, _ = failure.Classify(err)
	if class == failure.Cancelled {
		t.Fatal("installer failure classified as cancellation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = networkAdministratorError(ctx, original, "killed"); !errors.Is(err, context.Canceled) {
		t.Fatal("Ctrl-C cause lost")
	}
}
