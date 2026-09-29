package private

import (
	"context"
	"testing"

	"github.com/pgsty/barn/internal/state"
)

func TestLimitedGuestIsUsableAndWarningsClearAfterRepair(t *testing.T) {
	config, original := preparedStartFixture(t)
	config.Lifecycle = &fakeNodeLifecycle{warnings: []state.GuestWarning{{Stage: "shares", Detail: "/shared: read-only"}}}
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || len(startFailures(outcomes)) != 0 || !outcomes[0].Ready || len(outcomes[0].Warnings) != 1 {
		t.Fatalf("outcomes=%+v err=%v", outcomes, err)
	}
	store := state.Store{Root: config.Deployment.Root}
	node, err := store.ReadNode("meta")
	if err != nil || len(node.GuestWarnings) != 1 {
		t.Fatalf("warnings not persisted: %+v %v", node, err)
	}
	pid := node.Process.PID
	config.Lifecycle = &fakeNodeLifecycle{}
	outcomes, err = StartPrepared(context.Background(), config)
	if err != nil || len(startFailures(outcomes)) != 0 || len(outcomes[0].Warnings) != 0 {
		t.Fatalf("repair=%+v %v", outcomes, err)
	}
	node, err = store.ReadNode("meta")
	if err != nil || len(node.GuestWarnings) != 0 || node.Process.PID != pid || node.VMUUID != original[0].VMUUID {
		t.Fatalf("repair restarted guest or left warnings: %+v %v", node, err)
	}
}

func TestResetNoticeDoesNotMarkAUsableDiskLimited(t *testing.T) {
	config, _ := preparedStartFixture(t)
	config.Lifecycle = &fakeNodeLifecycle{warnings: []state.GuestWarning{{Stage: "disk-reset", Detail: "/data: previous data discarded"}}}
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || !outcomes[0].Ready || len(outcomes[0].Warnings) != 0 || len(outcomes[0].Repairs) != 1 {
		t.Fatalf("%+v %v", outcomes, err)
	}
	node, err := (state.Store{Root: config.Deployment.Root}).ReadNode("meta")
	if err != nil || len(node.GuestWarnings) != 0 {
		t.Fatalf("%+v %v", node, err)
	}
}
