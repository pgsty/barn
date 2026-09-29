package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
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
