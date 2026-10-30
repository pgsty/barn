package private

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/state"
)

func TestDarwinDirectoryAccessPrecedesRestartStop(t *testing.T) {
	fixture, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	store := state.Store{Root: fixture.Deployment.Root}
	share := privateShareFixture(t)
	persistPrivateShare(t, store, share)
	if err := os.Remove(share.Host); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	_, err = privateShareCapabilityManager(t, fixture, unavailableSharePeerRunner{}).Restart(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected actionable directory access error: %v", err)
	}
	after, err := store.ReadNode("meta")
	if err != nil || after.Phase != before.Phase || after.SpecHash != before.SpecHash {
		t.Fatalf("node mutated before capability check: %#v, %v", after, err)
	}
}
