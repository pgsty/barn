package macvm

import (
	"context"
	"testing"
)

func TestSetupPreservesPreviouslySelectedPin(t *testing.T) {
	m, previous := testManager(t)
	current := DefaultIPSW()
	if previous.Build == current.Build {
		t.Fatal("fixture must use an earlier IPSW")
	}
	for _, tc := range []struct {
		name     string
		baseID   string
		capacity int64
		build    string
		reuse    bool
	}{
		{"new installation", "", 0, current.Build, false},
		{"existing base", previous.ID, 0, previous.Build, true},
		{"other capacity of existing build", previous.ID, 200 << 30, previous.Build, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := m.preflightSetup(context.Background(), &Config{DefaultBaseID: tc.baseID}, SetupOptions{DiskBytes: tc.capacity})
			if err != nil {
				t.Fatal(err)
			}
			if plan.spec.Build != tc.build {
				t.Fatalf("selected build %q, want %q", plan.spec.Build, tc.build)
			}
			if tc.reuse {
				if plan.readyBase == nil || plan.readyBase.ID != previous.ID {
					t.Fatal("setup did not reuse the selected base")
				}
			} else if plan.readyBase != nil || !appleRestoreURL(plan.spec.URL) || !validSHA256(plan.spec.SHA256) || plan.spec.SizeBytes <= 0 {
				t.Fatalf("expected a pinned Apple download, got %+v", plan)
			}
		})
	}
}
