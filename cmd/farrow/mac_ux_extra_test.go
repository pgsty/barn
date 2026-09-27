package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/farrow/internal/macvm"
)

func TestMacUXExtraUninitializedSuccessHintCompletesFirstBoot(t *testing.T) {
	for _, test := range []struct {
		verb, state string
		initialized bool
		want        string
	}{
		{"stop", "stopped", false, "farrow mac up mac2"},
		{"configure", "created", false, "farrow mac up mac2"},
		{"configure", "stopped", false, "farrow mac up mac2"},
		{"stop", "stopped", true, "farrow mac start mac2"},
	} {
		t.Run(test.verb+"-"+test.state+"-"+test.want, func(t *testing.T) {
			var out bytes.Buffer
			slot := &macvm.Slot{Name: "mac2", State: test.state, Initialized: test.initialized, CPU: 4, MemoryBytes: 8 << 30}
			if err := macSlotOutcome(&macvm.Manager{}, slot, test.verb).text(&out, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), test.want) {
				t.Fatalf("success suggests an unusable next step; want %q in %s", test.want, &out)
			}
		})
	}
}

func TestMacUXExtraDamagedInstallerShowsDiagnosisAndRecovery(t *testing.T) {
	var out bytes.Buffer
	entries := []macvm.ImageEntry{{Kind: "installer", Build: "26B100", State: "damaged", Detail: "cached IPSW SHA256 mismatch", References: []string{}}}
	if err := macOutcome(entries).text(&out, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cached IPSW SHA256 mismatch", "farrow mac image prune --installers"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("damaged cache has no actionable explanation %q: %s", want, &out)
		}
	}
}

func TestMacUXExtraRefusedConfirmationClosesProgressWithoutMutatingState(t *testing.T) {
	if macvm.HostSupported() != nil {
		t.Skip("native runner lookup requires the supported Mac host")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "absent-data-root")
	t.Setenv("FARROW_HOME", root)
	t.Setenv("FARROW_OUTPUT", "")
	t.Setenv("FARROW_VERBOSE", "")
	installMacCLIProcessFixtures(t, parent)
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = previous; _ = input.Close() })
	for _, args := range [][]string{{"--verbose", "mac", "destroy", "mac2"}, {"--verbose", "mac", "reset", "mac2"}, {"--verbose", "mac", "network", "uninstall"}} {
		var out, diagnostics bytes.Buffer
		arguments, output, stderr, err := prepareOutput(args, &out, &diagnostics)
		if err != nil {
			t.Fatal(err)
		}
		command := newRootCommand(output, stderr)
		command.SetArgs(arguments)
		if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "requires --force") {
			t.Fatalf("noninteractive destructive command was not refused: %v", err)
		}
		state := outputContextFrom(stderr)
		state.mu.Lock()
		active := state.active
		state.mu.Unlock()
		if active != nil {
			t.Fatal("confirmation refusal left the progress spinner active")
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("confirmation refusal created state: %v", err)
		}
		if _, err := os.Stat(os.Getenv("FARROW_TEST_MAC_RUNNER_CAPTURE")); !os.IsNotExist(err) {
			t.Fatalf("confirmation refusal invoked the native runner: %v", err)
		}
	}
}
