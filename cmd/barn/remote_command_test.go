package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestExecPreservesArgumentsAcrossRemoteShell(t *testing.T) {
	t.Parallel()
	arguments := []string{"printf", "<%s>\\n", "a b", "", "a'b", "$(printf wrong)", "; exit 99"}
	command := exec.Command("/bin/sh", "-c", remoteCommandText("exec", arguments))
	output, err := command.CombinedOutput()
	want := "<a b>\n<>\n<a'b>\n<$(printf wrong)>\n<; exit 99>\n"
	if err != nil || string(output) != want {
		t.Fatalf("remote argv changed: %q, err=%v", output, err)
	}
}

func TestExecShellScriptReturnsItsActualExitStatus(t *testing.T) {
	t.Parallel()
	command := exec.Command("/bin/sh", "-c", remoteCommandText("exec", []string{"sh", "-c", "exit 17"}))
	err := command.Run()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 17 {
		t.Fatalf("remote shell script exit status: %v", err)
	}
}

func TestRemoteShellShorthandRemainsAvailable(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"ssh", "exec"} {
		arguments := []string{"printf first; printf second"}
		output, err := exec.Command("/bin/sh", "-c", remoteCommandText(name, arguments)).CombinedOutput()
		if err != nil || string(output) != "firstsecond" {
			t.Fatalf("%s shell shorthand: %s, %v", name, output, err)
		}
	}
	arguments := []string{"echo", "$HOME", "|", "cat"}
	if got := remoteCommandText("ssh", arguments); got != strings.Join(arguments, " ") {
		t.Fatalf("plain SSH semantics changed: %q", got)
	}
}

func TestRemoteCobraBoundaryPreservesFlagsAndSecondSeparator(t *testing.T) {
	t.Setenv("BARN_OUTPUT", "text")
	t.Setenv("BARN_VERBOSE", "false")
	for _, path := range [][]string{{"ssh"}, {"exec"}, {"mac", "ssh"}, {"mac", "exec"}} {
		for _, selected := range [][]string{nil, {"meta"}} {
			for _, remote := range [][]string{
				{"printf", "%s", "", "a b", "--", "--json", "-h", "-v"},
				{"--"}, {"-V"}, {"--help"}, {"printf first; printf second"},
			} {
				invocation := append([]string{"--json"}, path...)
				invocation = append(invocation, selected...)
				invocation = append(invocation, "-v", "--")
				invocation = append(invocation, remote...)
				var out, errOut bytes.Buffer
				clean, stdout, stderr, err := prepareOutput(invocation, &out, &errOut)
				if err != nil {
					t.Fatal(err)
				}
				root := newRootCommand(stdout, stderr)
				command, _, err := root.Find(path)
				if err != nil {
					t.Fatal(err)
				}
				called := false
				// Stop at the parser boundary: no connection or guest is needed.
				command.RunE = func(cmd *cobra.Command, args []string) error {
					called = true
					head, got, err := splitRemoteArguments(remoteArguments(cmd, args), cmd.CommandPath(), "name", cmd.Name() == "exec")
					if err != nil || !slices.Equal(head, selected) || !slices.Equal(got, remote) {
						t.Fatalf("%q: head=%q remote=%q err=%v", invocation, head, got, err)
					}
					return nil
				}
				root.SetArgs(clean)
				if _, err := root.ExecuteContextC(context.Background()); err != nil || !called {
					t.Fatalf("%q: called=%t err=%v", invocation, called, err)
				}
				if !structuredOutput(stdout) || !verboseOutput(stderr) {
					t.Fatalf("%q lost Barn presentation flags", invocation)
				}
			}
		}
	}
}

func TestRemoteHelpAndUnknownFlagsStayLocalBeforeSeparator(t *testing.T) {
	for _, path := range [][]string{{"ssh"}, {"exec"}, {"mac", "ssh"}, {"mac", "exec"}} {
		for _, flag := range []string{"--help", "-h", "--bogus"} {
			var out, errOut bytes.Buffer
			code := run(append(append([]string(nil), path...), flag), &out, &errOut)
			if flag == "--bogus" {
				if code != exitUsage || !strings.Contains(errOut.String(), "unknown flag") {
					t.Errorf("%v %s: exit=%d stderr=%s", path, flag, code, errOut.String())
				}
			} else if code != exitOK || !strings.Contains(out.String(), "command...") {
				t.Errorf("%v %s: exit=%d stdout=%s stderr=%s", path, flag, code, out.String(), errOut.String())
			}
		}
	}
}

// The generated shell script calls the real CLI through this test process.
func TestCompletionProcess(t *testing.T) {
	if os.Getenv("BARN_TEST_COMPLETION_PROCESS") != "1" {
		return
	}
	index := slices.Index(os.Args, "--")
	if index < 0 {
		t.Fatal("missing CLI arguments")
	}
	os.Exit(run(os.Args[index+1:], os.Stdout, os.Stderr))
}

func TestGeneratedBashCompletionRespectsRemoteBoundary(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash is unavailable")
	}
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	t.Setenv("BARN_OUTPUT", "text")
	t.Setenv("BARN_VERBOSE", "false")
	var script, errOut bytes.Buffer
	if code := run([]string{"completion", "bash"}, &script, &errOut); code != exitOK {
		t.Fatalf("generate completion: exit=%d stderr=%s", code, errOut.String())
	}
	path := filepath.Join(root, "barn.bash")
	if err := os.WriteFile(path, script.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const complete = `source "$1"
shift
barn() { "$BARN_TEST_BINARY" -test.run='^TestCompletionProcess$' -- "$@"; }
_init_completion() {
    COMPREPLY=()
    words=("${COMP_WORDS[@]}")
    cword=$COMP_CWORD
    cur=${words[cword]}
    prev=${words[cword-1]}
}
# compopt requires an interactive completion context; candidates do not.
compopt() { :; }
COMP_WORDS=(barn "$@")
COMP_CWORD=$((${#COMP_WORDS[@]} - 1))
__start_barn
printf '%s\n' "${COMPREPLY[@]}"
`
	for _, command := range [][]string{{"ssh"}, {"exec"}, {"mac", "ssh"}, {"mac", "exec"}} {
		for _, test := range []struct {
			tail []string
			want string
		}{
			{[]string{"--j"}, "--json"},
			{[]string{"--", "--j"}, ""},
			{[]string{"meta", "--", "--v"}, ""},
		} {
			args := append(append([]string{"--noprofile", "--norc", "-c", complete, "bash", path}, command...), test.tail...)
			process := exec.Command(bash, args...)
			process.Env = append(os.Environ(), "BARN_TEST_COMPLETION_PROCESS=1", "BARN_TEST_BINARY="+binary)
			out, err := process.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != test.want {
				t.Errorf("completion %v %v: output=%q err=%v, want %q", command, test.tail, out, err, test.want)
			}
		}
	}
}
