package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/farrow/internal/execx"
	netpreflight "github.com/pgsty/farrow/internal/network/preflight"
	setuphost "github.com/pgsty/farrow/internal/setup"
	"golang.org/x/term"
)

type setupSequenceRunner struct {
	calls  int
	failAt int
}

func (runner *setupSequenceRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	runner.calls++
	if runner.calls == runner.failAt {
		return execx.Result{ExitCode: 42}, errors.New("fixture failure")
	}
	return execx.Result{}, nil
}

func TestResolveSetupSelectionDefaultsToMeta(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	selection, err := resolveSetupSelection("", "", "", directory)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Profile != "meta" || !selection.Publish || !selection.Generated {
		t.Fatalf("default selection = %#v", selection)
	}
	if selection.ConfigPath != filepath.Join(directory, "farrow.yml") {
		t.Fatalf("config path = %s", selection.ConfigPath)
	}
	if len(selection.Resolved.Nodes) != 1 || selection.Resolved.Nodes[0].Name != "meta" || selection.Resolved.Nodes[0].Address != "10.10.10.10" {
		t.Fatalf("default meta lab = %#v", selection.Resolved.Nodes)
	}
}

func TestResolveSetupSelectionGeneratesPrivateProfileOnce(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	selection, err := resolveSetupSelection("meta", "", "", directory)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Profile != "meta" || !selection.Publish || !selection.Generated || len(selection.ConfigData) == 0 {
		t.Fatalf("generated selection = %#v", selection)
	}
	if selection.ConfigPath != filepath.Join(directory, "farrow.yml") {
		t.Fatalf("config path = %s", selection.ConfigPath)
	}
	if err := os.WriteFile(selection.ConfigPath, selection.ConfigData, 0o600); err != nil {
		t.Fatal(err)
	}
	repeated, err := resolveSetupSelection("meta", "", "", directory)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Publish {
		t.Fatalf("repeated selection = %#v", repeated)
	}
}

func TestResolveSetupSelectionPreservesDifferentExistingConfig(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	meta, err := resolveSetupSelection("meta", "", "", directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta.ConfigPath, meta.ConfigData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSetupSelection("full", "", "", directory); err == nil {
		t.Fatal("setup accepted a template over a different existing configuration")
	}
	data, err := os.ReadFile(meta.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, meta.ConfigData) {
		t.Fatal("existing configuration changed")
	}
}

func TestGeneratedSetupProfileCanRebaseBeforePublish(t *testing.T) {
	t.Parallel()
	selection, err := resolveSetupSelection("full", "", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := selection.rebaseGenerated("172.31.251.0/24"); err != nil {
		t.Fatal(err)
	}
	if selection.Resolved.Private == nil || selection.Resolved.Private.CIDR != "172.31.251.0/24" || !selection.Publish {
		t.Fatalf("rebased selection = %#v", selection)
	}
}

func TestSetupFileCannotBeSilentlyRebased(t *testing.T) {
	t.Parallel()
	if _, err := resolveSetupSelection("", "/tmp/farrow.yaml", "172.31.251.0/24", t.TempDir()); err == nil {
		t.Fatal("-f accepted --cidr")
	}
}

func TestConstrainSetupNetworkModeReusesDefaultButRejectsExplicitMismatch(t *testing.T) {
	t.Parallel()
	report := netpreflight.Report{
		OS: "darwin", Ready: true,
		Installation: netpreflight.Installation{Status: "exact", Mode: "shared", Healthy: true},
	}
	defaulted, effective := constrainSetupNetworkMode(report, "host", false)
	if !defaulted.Ready || effective != "shared" {
		t.Fatalf("default mode = ready:%v effective:%q", defaulted.Ready, effective)
	}
	explicit, effective := constrainSetupNetworkMode(report, "host", true)
	if explicit.Ready || explicit.ExitCode != exitConflict || effective != "host" || len(explicit.Findings) != 1 {
		t.Fatalf("explicit mode = %#v effective=%q", explicit, effective)
	}
}

func TestSetupApplyCommandRemovesDryRunAndPreservesTypedArguments(t *testing.T) {
	t.Parallel()
	next, argv := setupApplyCommand([]string{"full", "--mode=shared", "--dry-run=1", "--cidr", "172.31.251.0/24"}, outputYAML, true)
	want := []string{"farrow", "setup", "full", "--mode=shared", "--cidr", "172.31.251.0/24", "--yaml", "--verbose"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") || strings.Contains(next, "dry-run") {
		t.Fatalf("next=%q argv=%#v", next, argv)
	}
}

func TestSetupNextCommandUsesShellQuoteAndTypedArgv(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "lab $(touch nope).yaml")
	selection := setupSelection{ConfigPath: path}
	next, argv := setupNextCommand(selection, directory)
	if len(argv) != 4 || argv[3] != path || !strings.Contains(next, "'"+path+"'") {
		t.Fatalf("next=%q argv=%#v", next, argv)
	}
}

func TestEmitPrivatePendingNetworkDoesNotClaimUserNAT(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	result := setupResult{
		Schema: 1, OS: "darwin", Arch: "arm64", Profile: "full",
		NetworkCIDR: "10.10.10.0/24", NetworkMode: "host", Next: "farrow up",
	}
	outcome := setupOutcome(result)
	if code := renderCommandOutcome(&outcome, &stdout, &stderr); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "user NAT") || !strings.Contains(stdout.String(), "pending dependencies") {
		t.Fatalf("output=%q", stdout.String())
	}
}

func TestSetupDryRunPrintsThePlanOnce(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	outcome := setupOutcome(setupResult{Schema: 1, OS: "darwin", Arch: "arm64", DryRun: true, NetworkCIDR: "10.10.10.0/24", Next: "farrow up"})
	if code := renderCommandOutcome(&outcome, &stdout, &stderr); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); strings.Contains(got, "10.10.10.0/24") || !strings.Contains(got, "farrow up") {
		t.Fatalf("dry-run stdout repeats the plan or lacks next: %q", got)
	}
}

func TestSetupWithoutTerminalNeedsYesAsAUsageError(t *testing.T) {
	t.Parallel()
	err := confirmSetup(false, true, strings.NewReader("y\n"), io.Discard)
	if !errors.Is(err, errSetupNeedsYes) || errors.Is(err, ErrCancelled) {
		t.Fatalf("non-terminal confirmation err = %v; want the --yes usage error, not a cancellation", err)
	}
	if err := confirmSetup(true, true, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("--yes rejected: %v", err)
	}
	if err := confirmSetup(false, false, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("non-mutating setup asked for confirmation: %v", err)
	}
}

func TestConfirmationTreatsEndOfInputAsCancellation(t *testing.T) {
	t.Parallel()
	for input, accepted := range map[string]bool{"\n": true, "y\n": true, "YES\n": true, "n\n": false, "no\n": false, "": false, "y": false} {
		err := confirmPlan("? ", true, strings.NewReader(input), io.Discard)
		if accepted && err != nil {
			t.Errorf("input %q rejected: %v", input, err)
		}
		if !accepted && err == nil {
			t.Errorf("input %q accepted; only an answered [Y/n] prompt may default to yes", input)
		}
		if !accepted && !errors.Is(err, ErrCancelled) {
			t.Errorf("input %q error = %v, want ErrCancelled", input, err)
		}
	}
	if err := confirmPlan("? ", true, strings.NewReader(""), io.Discard); !strings.Contains(err.Error(), "no answer was entered") {
		t.Fatalf("EOF err = %v, want a specific cancellation reason", err)
	}
	if err := confirmPlan("? ", false, strings.NewReader("\n"), io.Discard); !errors.Is(err, ErrCancelled) {
		t.Fatalf("[y/N] default accepted: %v", err)
	}
}

func TestRunSetupEmitsStructuredEarlyFailure(t *testing.T) {
	t.Setenv("FARROW_HOME", filepath.Join(t.TempDir(), "state"))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	outputState := &outputContext{format: outputJSON}
	out := &outputWriter{Writer: &stdout, context: outputState}
	errOut := &outputWriter{Writer: &stderr, context: outputState, stderr: true}
	outcome, runErr := runSetupCommand(context.TODO(), "profile-that-does-not-exist", setupCLIOptions{Mode: "host"}, outputJSON, false, errOut)
	code := exitOK
	if runErr != nil {
		typed, ok := runErr.(typedCommandError)
		if !ok {
			t.Fatalf("untyped setup error: %v", runErr)
		}
		code = renderTypedCommandError(typed, out, errOut)
	} else {
		code = renderCommandOutcome(&outcome, out, errOut)
	}
	if code != exitUsage {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var result setupResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode structured failure: %v; stdout=%q", err, stdout.String())
	}
	if result.Schema != 1 || result.Ready || result.ExitCode != exitUsage || result.Error == "" {
		t.Fatalf("structured failure = %#v", result)
	}
}

func TestRunSetupHelpIsSuccessful(t *testing.T) {
	for _, test := range []struct {
		name   string
		format string
	}{
		{name: "text"},
		{name: "json", format: "--json"},
		{name: "yaml", format: "--yaml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := []string{"setup", "--help"}
			if test.format != "" {
				arguments = append(arguments, test.format)
			}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := run(arguments, &stdout, &stderr); code != exitOK {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("help wrote diagnostics to stderr: %q", stderr.String())
			}
			help := stdout.String()
			if !strings.Contains(help, "Usage:") || !strings.Contains(help, "farrow setup") || strings.Contains(help, "flag: help requested") {
				t.Fatalf("unexpected setup help:\n%s", help)
			}
		})
	}
}

func TestRunSetupTreatsSingleDashModeAsExplicit(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"setup", "-mode=host", "--dry-run", "--json"}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var failure commandFailure
	if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil || failure.Error != "usage" || !strings.Contains(failure.Message, "--mode must be one of") || !strings.Contains(stderr.String(), "--mode must be one of") {
		t.Fatalf("non-standard single-dash long flag was accepted: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

type recordingSetupRunner struct{ calls []string }

func (r *recordingSetupRunner) Run(_ context.Context, binary string, args ...string) (execx.Result, error) {
	r.calls = append(r.calls, strings.Join(append([]string{binary}, args...), " "))
	return execx.Result{}, nil
}

func TestSetupSudoSessionAsksAtMostOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; sudo session is a no-op")
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("interactive stdin would open a real sudo prompt")
	}
	runner := &recordingSetupRunner{}
	var stderr bytes.Buffer
	session := &sudoSession{base: runner, stderr: &stderr, scope: "setup run"}
	defer session.close()
	ctx := context.TODO()
	if err := session.ensure(ctx, "install the network"); err != nil {
		t.Fatal(err)
	}
	if err := session.ensure(ctx, "install the hosts helper"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "/usr/bin/sudo -n -v" {
		t.Fatalf("sudo credential calls = %v", runner.calls)
	}
	output := stderr.String()
	if !strings.Contains(output, "install the network") || !strings.Contains(output, "one password prompt") {
		t.Fatalf("first privileged step was not announced: %q", output)
	}
	if strings.Contains(output, "install the hosts helper") {
		t.Fatalf("later privileged steps must not re-prompt or re-announce: %q", output)
	}
}

type passwordlessSetupRunner struct{ calls []string }

func (runner *passwordlessSetupRunner) Run(_ context.Context, binary string, args ...string) (execx.Result, error) {
	call := strings.Join(append([]string{binary}, args...), " ")
	runner.calls = append(runner.calls, call)
	if call == "/usr/bin/sudo -n -v" {
		return execx.Result{ExitCode: 1}, errors.New("interactive authentication is required")
	}
	return execx.Result{}, nil
}

func TestSetupSudoSessionAcceptsPasswordlessCommandPolicy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; sudo session is a no-op")
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("interactive stdin would open a real sudo prompt")
	}
	runner := &passwordlessSetupRunner{}
	session := &sudoSession{base: runner, stderr: &bytes.Buffer{}}
	defer session.close()
	if err := session.ensure(context.TODO(), "install the network"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/sudo -n -v", "/usr/bin/sudo -n -- /usr/bin/true"}
	if strings.Join(runner.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sudo probes = %v, want %v", runner.calls, want)
	}
}

func TestSetupCommandFailureSeparatesChangedFromUncertain(t *testing.T) {
	commands := []setuphost.Command{
		{Name: "first", Binary: "/usr/bin/true"},
		{Name: "second", Binary: "/usr/bin/false"},
	}
	for _, test := range []struct {
		name        string
		failAt      int
		wantChanged bool
	}{
		{name: "first command", failAt: 1, wantChanged: false},
		{name: "after one success", failAt: 2, wantChanged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &setupSequenceRunner{failAt: test.failAt}
			sudo := &sudoSession{base: runner, stderr: &bytes.Buffer{}}
			changed, uncertain, err := runSetupCommands(context.TODO(), commands, runner, sudo, &bytes.Buffer{})
			if err == nil || changed != test.wantChanged || !uncertain {
				t.Fatalf("changed=%t uncertain=%t err=%v", changed, uncertain, err)
			}
		})
	}
}

func TestSudoRunnerPreservesOnlyNamedProxyEnvironment(t *testing.T) {
	runner := &recordingSetupRunner{}
	wrapped := sudoRunner{
		base:                runner,
		preserveEnvironment: []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"},
	}
	if _, err := wrapped.Run(context.TODO(), "/usr/bin/apt-get", "update"); err != nil {
		t.Fatal(err)
	}
	want := "/usr/bin/sudo -n --preserve-env=HTTP_PROXY,HTTPS_PROXY,NO_PROXY -- /usr/bin/apt-get update"
	if len(runner.calls) != 1 || runner.calls[0] != want {
		t.Fatalf("privileged package-manager call = %v, want %q", runner.calls, want)
	}
}

func TestSetupPlanShowsProxyNamesWithoutValues(t *testing.T) {
	for _, name := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("HTTPS_PROXY", "http://user:secret@127.0.0.1:9443")
	selection, err := resolveSetupSelection("meta", "", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan := setuphost.DependencyPlan{
		Manager: "homebrew", Missing: []string{"qemu-system-aarch64"},
		Commands: []setuphost.Command{{Name: "Install QEMU", Binary: "/opt/homebrew/bin/brew", Args: []string{"install", "qemu"}}},
	}
	report := netpreflight.Report{
		OS: "darwin", CIDR: "10.10.10.0/24", Ready: true,
		Installation: netpreflight.Installation{Status: "exact", Healthy: true, Mode: "host"},
	}
	var output bytes.Buffer
	printSetupPlan(&output, plan, selection, &report, true)
	got := output.String()
	if !strings.Contains(got, "proxy environment: HTTPS_PROXY (values hidden)") {
		t.Fatalf("setup plan omitted proxy evidence: %q", got)
	}
	for _, secret := range []string{"user", "secret", "127.0.0.1", "9443"} {
		if strings.Contains(got, secret) {
			t.Fatalf("setup plan exposed proxy value fragment %q: %q", secret, got)
		}
	}
	if strings.Contains(got, "package installation (homebrew)") {
		t.Fatalf("user-level Homebrew install was described as sudo work: %q", got)
	}
}

func TestSetupPlanNamesLinuxBridgeWithoutVmnet(t *testing.T) {
	selection, err := resolveSetupSelection("meta", "", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report := netpreflight.Report{OS: "linux", CIDR: "10.10.10.0/24", Ready: true, Installation: netpreflight.Installation{Status: "protected", Mode: "bridge", Healthy: true}}
	printSetupPlan(&out, setuphost.DependencyPlan{}, selection, &report, true)
	if !strings.Contains(out.String(), "(bridge mode)") || strings.Contains(out.String(), "vmnet") {
		t.Fatalf("misleading Linux network plan: %s", out.String())
	}
}
