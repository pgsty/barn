package macvm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDoctorChecksOfflineSSHAndStoppedProof(t *testing.T) {
	if HostSupported() != nil || os.Geteuid() == 0 {
		t.Skip("ordinary supported Mac host required")
	}
	for _, missing := range []string{"id_ed25519", "runner.lock"} {
		t.Run(missing, func(t *testing.T) {
			m, _, slot, _ := uxRuntimeStore(t)
			m.Runner, _, _ = uxRuntimeRunner(t, "unavailable")
			if missing == "runner.lock" {
				if err := os.Remove(filepath.Join(m.Store.Root, "slots", slot.Name, missing)); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			report := m.Doctor(ctx)
			name, detail := "mac1_ssh_material", "no replacement key was generated"
			if missing == "runner.lock" {
				name, detail = "mac1_runtime", "runner lock is missing"
			}
			for _, check := range report.Checks {
				if check.Name == name {
					if check.OK || !strings.Contains(check.Detail, detail) {
						t.Fatalf("doctor accepted damaged offline state: %+v", check)
					}
					if _, err := os.Stat(filepath.Join(m.Store.Root, "slots", slot.Name, missing)); !os.IsNotExist(err) {
						t.Fatal("doctor recreated missing recovery material")
					}
					return
				}
			}
			t.Fatalf("doctor omitted %s", name)
		})
	}
}

func TestDoctorReportsLiveRuntimeAndSSHSeparately(t *testing.T) {
	for _, tc := range []struct {
		state, ssh, runtimeError                string
		initialized, runtimeOK, checkSSH, sshOK bool
	}{
		{"ready", "ready", "", true, true, true, true},
		{"running", "unavailable", "", true, true, true, false},
		{"running", "pending", "", false, true, false, false},
		{"stopped", "offline", "", true, true, false, false},
		{"created", "offline", "", false, true, false, false},
		{"unresponsive", "unchecked", "runner is still active", true, false, false, false},
		{"paused", "offline", "", true, false, false, false},
	} {
		t.Run(tc.state+"-"+tc.ssh, func(t *testing.T) {
			report := DoctorReport{OK: true}
			report.addRuntime(SlotView{Slot: Slot{Name: "mac2", State: tc.state, Initialized: tc.initialized}, SSH: tc.ssh, RuntimeError: tc.runtimeError, SSHError: "SSH connection failed"})
			if len(report.Checks) == 0 || report.Checks[0].Name != "mac2_runtime" || report.Checks[0].OK != tc.runtimeOK {
				t.Fatalf("incorrect runtime diagnosis: %+v", report)
			}
			if tc.checkSSH {
				if len(report.Checks) != 2 || report.Checks[1].Name != "mac2_ssh" || report.Checks[1].OK != tc.sshOK {
					t.Fatalf("incorrect SSH diagnosis: %+v", report)
				}
			} else if len(report.Checks) != 1 {
				t.Fatalf("reported an SSH check without a running initialized guest: %+v", report)
			}
			if report.OK != (tc.runtimeOK && (!tc.checkSSH || tc.sshOK)) {
				t.Fatalf("incorrect overall health: %+v", report)
			}
		})
	}
}
