package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/lock"
	"github.com/pgsty/farrow/internal/macvm"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestMacListIgnoresInventoryAndNeverCreatesState(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "absent-home")
	t.Setenv("FARROW_HOME", root)
	t.Setenv("FARROW_OUTPUT", "")
	t.Setenv("FARROW_VERBOSE", "")
	t.Setenv("FARROW_MAC_RUNNER", filepath.Join(parent, "absent-runner"))
	t.Chdir(parent)
	invalid := []byte("this: [is: invalid YAML and must never be read by farrow mac\n")
	for _, name := range []string{"farrow.yml", "farrow.yaml", "pigsty.yml"} {
		if err := os.WriteFile(name, invalid, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mac", "--json"}, {"mac", "ls", "--json"}, {"--json", "mac", "ls"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitOK {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
		}
		var status macvm.Status
		if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.Prepared || status.Network != nil || len(status.Slots) != 2 {
			t.Fatalf("unexpected unprepared status: %+v", status)
		}
		for i, name := range []string{"mac1", "mac2"} {
			slot := status.Slots[i]
			if slot.Name != name || slot.State != "empty" || slot.IP != "" || slot.InstanceID != "" {
				t.Fatalf("slot %+v", slot)
			}
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("read-only mac ls created data root: %v", err)
		}
	}
	if data, err := os.ReadFile("farrow.yml"); err != nil || !bytes.Equal(data, invalid) {
		t.Fatalf("inventory changed: %q %v", data, err)
	}
}

func TestMacReadOnlyCommandsIgnoreBrokenLinuxDeployment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FARROW_HOME", root)
	t.Setenv("FARROW_MAC_RUNNER", filepath.Join(root, "missing-runner"))
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte("{broken Linux state"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mac", "ls", "--json"}, {"mac", "image", "ls", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitOK {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
		}
		if !json.Valid(stdout.Bytes()) {
			t.Fatalf("invalid JSON: %s", &stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "mac")); !os.IsNotExist(err) {
		t.Fatalf("read-only commands created Mac namespace: %v", err)
	}
}

func TestMacRejectsThirdSlotBeforeHostSetup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	t.Setenv("FARROW_HOME", root)
	t.Setenv("FARROW_MAC_RUNNER", filepath.Join(root, "missing-runner"))
	for _, verb := range []string{"init", "up", "start", "stop", "open", "password", "reset", "destroy", "ssh", "exec"} {
		t.Run(verb, func(t *testing.T) {
			args := []string{"--json", "mac", verb, "mac3"}
			if verb == "ssh" || verb == "exec" {
				args = append(args, "--", "id")
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != exitUsage {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
			}
			var failure commandFailure
			if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
				t.Fatal(err)
			}
			if failure.Error != "usage" || !strings.Contains(failure.Message, "mac1 or mac2") {
				t.Fatalf("failure=%+v", failure)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("invalid slot triggered setup: %v", err)
			}
		})
	}
}

// The full presentation parser and Cobra tree must leave remote arguments
// untouched on every build target, even where native Mac execution is absent.
func TestMacRemoteArgumentBoundaryAcrossPresentationModes(t *testing.T) {
	t.Setenv("FARROW_OUTPUT", "")
	t.Setenv("FARROW_VERBOSE", "")
	remote := []string{"printf", "two words", "quote's", "", "--json", "--yaml", "--force", "--verbose", "-v"}
	for _, verb := range []string{"ssh", "exec"} {
		for _, structured := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-json=%t", verb, structured), func(t *testing.T) {
				invocation := []string{"mac", verb, "2", "--"}
				if structured {
					invocation = append([]string{"--json"}, invocation...)
				}
				invocation = append(invocation, remote...)
				var stdout, stderr bytes.Buffer
				arguments, out, diagnostics, err := prepareOutput(invocation, &stdout, &stderr)
				if err != nil {
					t.Fatal(err)
				}
				root := newRootCommand(out, diagnostics)
				command, _, err := root.Find([]string{"mac", verb})
				if err != nil {
					t.Fatal(err)
				}
				called := false
				command.RunE = func(cmd *cobra.Command, args []string) error {
					called = true
					if cmd.ArgsLenAtDash() != 1 || !reflect.DeepEqual(args, append([]string{"2"}, remote...)) {
						t.Fatalf("dash=%d args=%q", cmd.ArgsLenAtDash(), args)
					}
					if structuredOutput(out) != structured || verboseOutput(diagnostics) {
						t.Fatalf("remote flags changed presentation: structured=%t verbose=%t", structuredOutput(out), verboseOutput(diagnostics))
					}
					return nil
				}
				root.SetArgs(arguments)
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				if !called {
					t.Fatal("remote command was not dispatched")
				}
			})
		}
	}
}

func TestMacExecRequiresExplicitRemoteCommand(t *testing.T) {
	t.Setenv("FARROW_HOME", filepath.Join(t.TempDir(), "absent"))
	for _, args := range [][]string{{"--json", "mac", "exec"}, {"--json", "mac", "exec", "mac1"}, {"--json", "mac", "exec", "mac1", "--"}, {"--json", "mac", "exec", "mac1", "id"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitUsage {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
		}
	}
}

func TestMacStartMissingDoesNotPrepareOrWriteState(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "absent-home")
	t.Setenv("FARROW_HOME", root)
	installMacCLIProcessFixtures(t, parent)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", "mac", "start", "mac1"}, &stdout, &stderr)
	if code == exitOK {
		t.Fatalf("missing start succeeded: %s", &stdout)
	}
	var failure commandFailure
	if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if macvm.HostSupported() == nil && !strings.Contains(failure.Message, "farrow mac up") {
		t.Fatalf("missing start lacks recovery: %+v", failure)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("start of missing slot created state/download directories: %v", err)
	}
	if _, err := os.Stat(os.Getenv("FARROW_TEST_MAC_RUNNER_CAPTURE")); !os.IsNotExist(err) {
		t.Fatalf("missing start invoked runner: %v", err)
	}
}

func TestMacUnsupportedHostsReportCapabilityWithoutSetup(t *testing.T) {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" && macvm.HostSupported() == nil {
		t.Skip("requires a non-supported build target or older macOS host")
	}
	root := filepath.Join(t.TempDir(), "absent")
	t.Setenv("FARROW_HOME", root)
	for _, verb := range []string{"setup", "init", "up", "start", "stop", "open"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--json", "mac", verb}, &stdout, &stderr); code != exitCapability {
			t.Fatalf("%s: code=%d stdout=%s stderr=%s", verb, code, &stdout, &stderr)
		}
		var failure commandFailure
		if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
			t.Fatal(err)
		}
		if failure.Error != "capability" || failure.Reason != "mac_host_unsupported" {
			t.Fatalf("%s: %+v", verb, failure)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("unsupported host created state: %v", err)
	}
}

// The fixture implements the runner status and OpenSSH process boundaries.
// It does not contact Keychain, start a VM, or create a network interface.
func TestMacCLIProcessFixture(t *testing.T) {
	if os.Getenv("FARROW_TEST_MAC_CLI_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(91)
	}
	mode, args := args[1], args[2:]
	if mode == "runner" {
		data, _ := json.Marshal(args)
		if err := os.WriteFile(os.Getenv("FARROW_TEST_MAC_RUNNER_CAPTURE"), data, 0o600); err != nil {
			os.Exit(92)
		}
		instance := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--instance" {
				instance = args[i+1]
			}
		}
		if len(args) == 0 || args[0] != "rpc" || instance == "" {
			os.Exit(93)
		}
		_ = json.NewEncoder(os.Stdout).Encode(macvm.RuntimeStatus{OK: true, Instance: instance, State: "running", PID: os.Getpid()})
		os.Exit(0)
	}
	if mode == "ssh" {
		data, _ := json.Marshal(args)
		if err := os.WriteFile(os.Getenv("FARROW_TEST_MAC_SSH_CAPTURE"), data, 0o600); err != nil {
			os.Exit(94)
		}
		_, _ = fmt.Fprintln(os.Stdout, "guest stdout")
		fmt.Fprintln(os.Stderr, "guest stderr")
		if os.Getenv("FARROW_TEST_MAC_REMOTE_FAILURE") == "1" {
			os.Exit(23)
		}
		os.Exit(0)
	}
	os.Exit(95)
}

func installMacCLIProcessFixtures(t *testing.T, directory string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ name, mode string }{{"runner-fixture", "runner"}, {"ssh", "ssh"}} {
		path := filepath.Join(directory, item.name)
		script := "#!/bin/sh\nexec " + macvm.QuoteCommand([]string{executable, "-test.run=^TestMacCLIProcessFixture$", "--", item.mode}) + " \"$@\"\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FARROW_MAC_RUNNER", filepath.Join(directory, "runner-fixture"))
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FARROW_TEST_MAC_CLI_PROCESS", "1")
	t.Setenv("FARROW_TEST_MAC_RUNNER_CAPTURE", filepath.Join(directory, "runner-args.json"))
	t.Setenv("FARROW_TEST_MAC_SSH_CAPTURE", filepath.Join(directory, "ssh-args.json"))
}

func TestMacStructuredSSHCommandPreservesArgumentsAndRemoteStatus(t *testing.T) {
	if err := macvm.HostSupported(); err != nil {
		t.Skip("native CLI companion fixture requires supported host; parsing contract runs on every target")
	}
	parent := t.TempDir()
	t.Setenv("FARROW_HOME", filepath.Join(parent, "home"))
	t.Setenv("FARROW_OUTPUT", "")
	t.Setenv("FARROW_VERBOSE", "")
	installMacCLIProcessFixtures(t, parent)
	store, err := macvm.NewStore(os.Getenv("FARROW_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store.Root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	held, err := lock.Acquire(context.Background(), filepath.Join(store.Root, "runtime", "state.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	config, err := macvm.NewConfig(macvm.DefaultSubnet)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConfig(held, config); err != nil {
		t.Fatal(err)
	}
	base := &macvm.BaseImage{SchemaVersion: 1, ID: "fixture-base", Version: "27.0", Build: "26A428", HardwareModelHash: strings.Repeat("a", 64), InstallerSHA256: strings.Repeat("b", 64), State: "ready", DiskBytes: macvm.DefaultDiskBytes, RecipeVersion: 1, CreatedAt: time.Now().UTC()}
	if err := store.SaveBase(held, base); err != nil {
		t.Fatal(err)
	}
	instance, err := macvm.NewInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	slot := &macvm.Slot{SchemaVersion: 1, Name: "mac2", InstanceID: instance, BaseID: base.ID, State: "stopped", Initialized: true, User: "farrow", CPU: 4, MemoryBytes: macvm.DefaultMemoryBytes, DiskBytes: macvm.DefaultDiskBytes, Version: base.Version, Build: base.Build, CreatedAt: time.Now().UTC()}
	if err := store.SaveSlot(held, slot); err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := store.SlotPath("mac2")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "id_ed25519"), pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "known_hosts"), []byte(knownhosts.Line([]string{"10.10.20.11"}, signer.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	remote := []string{"printf", "%s|%s|%s", "two words", "quote's", "", "--json", "--yaml", "--force", "-v", "literal;$(touch SHOULD_NOT_RUN)"}
	wantCommand := `'printf' '%s|%s|%s' 'two words' 'quote'"'"'s' '' '--json' '--yaml' '--force' '-v' 'literal;$(touch SHOULD_NOT_RUN)'`
	for _, test := range []struct {
		verb    string
		failure bool
	}{{"exec", false}, {"ssh", false}, {"exec", true}} {
		t.Run(fmt.Sprintf("%s-failure=%t", test.verb, test.failure), func(t *testing.T) {
			flag := "0"
			wantCode := exitOK
			if test.failure {
				flag = "1"
				wantCode = 23
			}
			t.Setenv("FARROW_TEST_MAC_REMOTE_FAILURE", flag)
			args := append([]string{"--json", "mac", test.verb, "2", "--"}, remote...)
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != wantCode {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
			}
			var result struct {
				Slot     string `json:"slot"`
				ExitCode int    `json:"exit_code"`
				Stdout   string `json:"stdout"`
				Stderr   string `json:"stderr"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Slot != "mac2" || result.ExitCode != wantCode || result.Stdout != "guest stdout\n" || result.Stderr != "guest stderr\n" {
				t.Fatalf("result=%+v", result)
			}
			capture, err := os.ReadFile(os.Getenv("FARROW_TEST_MAC_SSH_CAPTURE"))
			if err != nil {
				t.Fatal(err)
			}
			var sshArgs []string
			if err := json.Unmarshal(capture, &sshArgs); err != nil {
				t.Fatal(err)
			}
			if len(sshArgs) < 2 || sshArgs[len(sshArgs)-2] != "farrow@10.10.20.11" || sshArgs[len(sshArgs)-1] != wantCommand {
				t.Fatalf("SSH command boundaries=%q", sshArgs)
			}
			joined := strings.Join(sshArgs, "\n")
			for _, required := range []string{"StrictHostKeyChecking=yes", "ForwardAgent=no", "BatchMode=yes", "GlobalKnownHostsFile=/dev/null"} {
				if !strings.Contains(joined, required) {
					t.Errorf("SSH trust option missing: %s", required)
				}
			}
			if strings.Contains(joined, "\n-t\n") {
				t.Fatal("structured remote execution requested interactive TTY")
			}
		})
	}
	// A structured interactive login is a usage error, not an SSH invocation.
	if err := os.Remove(os.Getenv("FARROW_TEST_MAC_SSH_CAPTURE")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--json", "mac", "ssh", "2"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("interactive JSON SSH code=%d output=%s %s", code, &stdout, &stderr)
	}
	if _, err := os.Stat(os.Getenv("FARROW_TEST_MAC_SSH_CAPTURE")); !os.IsNotExist(err) {
		t.Fatal("interactive JSON SSH invoked OpenSSH")
	}
}
