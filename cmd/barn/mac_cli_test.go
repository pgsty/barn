package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/macvm"
)

// macTestHome isolates BARN_HOME and the runner, and puts an invalid Linux
// inventory in the working directory that no Mac command may read.
func macTestHome(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "home")
	t.Setenv("BARN_HOME", root)
	t.Setenv("BARN_OUTPUT", "")
	t.Setenv("BARN_VERBOSE", "")
	t.Setenv("BARN_MAC_RUNNER", filepath.Join(parent, "absent-runner"))
	t.Chdir(parent)
	if err := os.WriteFile("barn.yml", []byte("this: [is: invalid YAML never read by barn mac\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func runMacCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestMacListIsReadOnlyAndIgnoresTheInventory(t *testing.T) {
	root := macTestHome(t)
	for _, args := range [][]string{{"mac", "--json"}, {"mac", "ls", "--json"}, {"--json", "mac", "status"}} {
		code, stdout, stderr := runMacCLI(t, args...)
		if code != exitOK {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, stdout, stderr)
		}
		var status macvm.Status
		if err := json.Unmarshal([]byte(stdout), &status); err != nil {
			t.Fatal(err)
		}
		if status.SchemaVersion != 2 || status.Prepared || len(status.Machines) != 0 || status.Limit != 2 {
			t.Fatalf("status %+v", status)
		}
	}
	code, stdout, _ := runMacCLI(t, "mac", "ls")
	if code != exitOK || !strings.Contains(stdout, "No Mac machines yet") || !strings.Contains(stdout, "barn mac up") {
		t.Fatalf("text: %d %s", code, stdout)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a read-only command created %s", root)
	}
}

func TestMacUsageErrors(t *testing.T) {
	macTestHome(t)
	for _, args := range [][]string{
		{"mac", "up", "Bad_Name"},
		{"mac", "up", "--cpu", "1"},
		{"mac", "up", "--memory", "1G"},
		{"mac", "up", "--share", "=/tmp"},
		{"mac", "up", "--clipboard", "maybe"},
		{"mac", "up", "one", "two"},
		{"mac", "exec", "mac1"},
		{"mac", "configure"},
		{"mac", "migrate"},
		{"mac", "destroy"},
		{"mac", "start", "mac1", "--all"},
		{"mac", "stop", "--all", "mac1"},
		{"mac", "logs", "-n", "0"},
		{"--json", "mac", "ssh", "mac1"},
		{"mac", "ssh-config", "--install", "--remove"},
	} {
		if code, stdout, stderr := runMacCLI(t, args...); code != exitUsage {
			t.Errorf("%v: code=%d stdout=%s stderr=%s", args, code, stdout, stderr)
		}
	}
}

func TestMacDestructiveCommandsNeedForceWithoutATerminal(t *testing.T) {
	macTestHome(t)
	code, _, stderr := runMacCLI(t, "mac", "destroy", "dev")
	if code != exitUsage || !strings.Contains(stderr, "--force") || !strings.Contains(stderr, "shared macOS base is kept") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestMacAbsentMachineNamesHowToCreateIt(t *testing.T) {
	macTestHome(t)
	for _, args := range [][]string{{"--json", "mac", "password", "dev"}, {"--json", "mac", "logs", "dev"}} {
		code, stdout, _ := runMacCLI(t, args...)
		var failure commandFailure
		_ = json.Unmarshal([]byte(stdout), &failure)
		if code != exitConflict || failure.Reason != "mac_machine_absent" || failure.Next != "barn mac up dev" {
			t.Fatalf("%v: code=%d %+v", args, code, failure)
		}
	}
}

func TestMacEmptyHomeImagesAndSSHConfig(t *testing.T) {
	macTestHome(t)
	code, stdout, _ := runMacCLI(t, "--json", "mac", "image", "ls")
	if code != exitOK || strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("image ls: %d %s", code, stdout)
	}
	code, stdout, _ = runMacCLI(t, "--json", "mac", "image", "prune")
	var prune macvm.PruneReport
	if code != exitOK || json.Unmarshal([]byte(stdout), &prune) != nil || len(prune.Candidates) != 0 || prune.Applied {
		t.Fatalf("prune: %d %s", code, stdout)
	}
	code, stdout, _ = runMacCLI(t, "mac", "ssh-config")
	if code != exitOK || !strings.HasPrefix(stdout, "# barn-mac:begin\n") {
		t.Fatalf("ssh-config: %d %q", code, stdout)
	}
}

func TestMacDoctorReportsAMissingComponent(t *testing.T) {
	macTestHome(t)
	code, stdout, _ := runMacCLI(t, "--json", "mac", "doctor")
	var report macvm.DoctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if code != exitCapability || report.OK || len(report.Checks) == 0 || report.Checks[0].Name != "host" {
		t.Fatalf("code=%d report=%+v", code, report)
	}
}

func TestMacFlagValues(t *testing.T) {
	var size int64
	for raw, want := range map[string]int64{"16G": 16 << 30, "16GiB": 16 << 30, "16GB": 16 << 30, "16gb": 16 << 30, "512M": 512 << 20, "512MB": 512 << 20, "17179869184": 16 << 30} {
		if err := (macSizeValue{&size}).Set(raw); err != nil || size != want {
			t.Errorf("%s: %d %v", raw, size, err)
		}
	}
	for _, raw := range []string{"", "-1", "lots", "0"} {
		if err := (macSizeValue{&size}).Set(raw); err == nil {
			t.Errorf("accepted size %q", raw)
		}
	}
	var enabled *bool
	for raw, want := range map[string]bool{"on": true, "OFF": false, "yes": true, "false": false} {
		if err := (macSwitch{&enabled}).Set(raw); err != nil || *enabled != want {
			t.Errorf("%s: %v %v", raw, enabled, err)
		}
	}
	if err := (macSwitch{&enabled}).Set("sometimes"); err == nil {
		t.Error("accepted a vague switch")
	}
}

func TestMacDownloadNeedsConsentWithoutATerminal(t *testing.T) {
	var stderr bytes.Buffer
	plan := macvm.DownloadPlan{Version: "27.0", Build: "26A428", URL: "https://updates.cdn-apple.com/x.ipsw", SizeBytes: 26 << 30, FreeBytes: 200 << 30}
	err := macDownloadConsent(false, &stderr)(plan)
	if class, reason, next := failure.Classify(err); class != failure.Usage || reason != "mac_download_consent" || !strings.Contains(next, "--yes") {
		t.Fatalf("err=%v %s %s %s", err, class, reason, next)
	}
	if !strings.Contains(stderr.String(), "updates.cdn-apple.com") || !strings.Contains(stderr.String(), "26.0 GiB") {
		t.Fatalf("the plan did not name Apple and the size:\n%s", stderr.String())
	}
	if err := macDownloadConsent(true, &stderr)(plan); err != nil {
		t.Fatal(err)
	}
}

func TestMacHelpListsTheCommandGroups(t *testing.T) {
	macTestHome(t)
	code, stdout, _ := runMacCLI(t, "mac", "--help")
	if code != exitOK {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"Machines:", "Access:", "Host:", "up", "open", "ssh-config", "only from Apple"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	if strings.Contains(stdout, "migrate") {
		t.Error("help lists the removed development migration command")
	}
}

func TestMacPartialFailureKeepsTheReport(t *testing.T) {
	macTestHome(t)
	report := macvm.Outcomes{Machines: []macvm.Outcome{{Name: "mac1", Action: "stopped", State: "stopped"}}}
	run := macRun(&bytes.Buffer{}, "", func(macCommandContext) (commandOutcome, error) {
		return macOutcomesOutcome(report, &bytes.Buffer{}), failure.New(failure.Partial, errors.New("dev: the runner did not answer"))
	})
	err := run(newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}), nil)
	var boundary *commandBoundaryError
	if !errors.As(err, &boundary) || boundary.code != exitPartial {
		t.Fatalf("err %v", err)
	}
	if outcomes, ok := boundary.payload.(macvm.Outcomes); !ok || len(outcomes.Machines) != 1 || outcomes.Machines[0].Name != "mac1" {
		t.Fatalf("payload %#v", boundary.payload)
	}
}

func TestMacRemoteCommandsRejectInvalidInvocations(t *testing.T) {
	root := macTestHome(t)
	for _, verb := range []string{"ssh", "exec"} {
		cases := [][]string{{"dev", "uptime"}, {"missing", "--", "id"}, {"--", ""}, {"dev", "--", "", "id"}, {"dev", "other", "--", "id"}}
		if verb == "exec" {
			cases = append(cases, nil, []string{"dev"}, []string{"--"})
		}
		for _, args := range cases {
			invocation := append([]string{"mac", verb}, args...)
			if code, out, errOut := runMacCLI(t, invocation...); code != exitUsage {
				t.Errorf("%q: exit=%d stdout=%s stderr=%s", invocation, code, out, errOut)
			}
		}
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid invocation created state: %v", err)
	}
}

func TestMacRemoteSelectorsAndCompletionUseExistingMachines(t *testing.T) {
	root := macTestHome(t)
	machine := macvm.Machine{
		SchemaVersion: macvm.SchemaVersion, Name: "dev",
		InstanceID: "e4409c09-1804-4cf9-b28a-1f0bec7a2984", BaseID: "fixture",
		State: "stopped", User: "barn", CPU: 4, MemoryBytes: 8 << 30,
		DiskBytes: 100 << 30, MAC: "02:00:00:00:00:01",
		Network: macvm.MachineNetwork{Subnet: "10.10.30.0/24", Gateway: "10.10.30.1", Address: "10.10.30.10"},
	}
	data, err := json.Marshal(machine)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "mac", "slots", machine.Name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"ssh", "exec"} {
		// An uninitialized fixture stops before runtime or SSH access.
		for _, args := range [][]string{{"--", "sw_vers"}, {"dev", "--", "sw_vers"}, {"dev", "--", "--"}} {
			invocation := append([]string{"--json", "mac", verb}, args...)
			code, out, errOut := runMacCLI(t, invocation...)
			if code != exitConflict || !strings.Contains(out, `"reason": "mac_not_initialized"`) {
				t.Errorf("%q: exit=%d stdout=%s stderr=%s", invocation, code, out, errOut)
			}
		}
		for _, test := range []struct {
			tail []string
			want string
		}{
			{[]string{""}, "dev\n:4"},
			{[]string{"dev", ""}, ":4"},
			{[]string{"--", ""}, ":4"},
			{[]string{"dev", "--", "--j"}, ":4"},
		} {
			invocation := append([]string{"__complete", "mac", verb}, test.tail...)
			code, out, errOut := runMacCLI(t, invocation...)
			if code != exitOK || strings.TrimSpace(out) != test.want {
				t.Errorf("%q: exit=%d stdout=%q stderr=%q", invocation, code, out, errOut)
			}
		}
	}
	for _, request := range []string{"__complete", "__completeNoDesc"} {
		for _, verb := range []string{"up", "restart", "open", "configure", "recreate", "password", "logs"} {
			invocation := []string{request, "mac", verb, "--", ""}
			code, out, errOut := runMacCLI(t, invocation...)
			if code != exitOK || strings.TrimSpace(out) != "dev\n:4" {
				t.Errorf("ordinary completion %q: exit=%d stdout=%q stderr=%q", invocation, code, out, errOut)
			}
		}
	}
	for _, args := range [][]string{{"ssh"}, {"ssh", "dev"}, {"ssh", "--"}} {
		code, out, errOut := runMacCLI(t, append([]string{"mac"}, args...)...)
		if code != exitConflict || !strings.Contains(errOut, "has not finished its first boot") {
			t.Errorf("%q: exit=%d stdout=%s stderr=%s", args, code, out, errOut)
		}
	}
}
