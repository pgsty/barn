package main

import (
	"os"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/hostconfig"
)

// validArguments is the exact flag set the helper defines. It must stay in
// step with run: a stale flag would fail parsing and silently turn the root
// and positional-argument tests into false positives.
func validArguments() []string {
	target, _ := hostconfig.NativePath()
	return []string{
		"--target", target,
		"--staging", "/tmp/barn-hosts-helper-test",
		"--action", hostconfig.ActionInstall,
		"--before-sha256", strings.Repeat("a", 64),
		"--after-sha256", strings.Repeat("b", 64),
	}
}

func TestHelperRejectsNonNativeAndRelativeInputs(t *testing.T) {
	t.Parallel()
	nativeTarget, _ := hostconfig.NativePath()
	for _, args := range [][]string{
		{"--target", "/tmp/not-hosts", "--staging", "/tmp/stage"},
		{"--target", nativeTarget, "--staging", "relative"},
	} {
		err := run(args)
		if err == nil || !strings.Contains(err.Error(), "non-native target or a relative staging path") {
			t.Errorf("run(%v) = %v, want the target/staging refusal", args, err)
		}
	}
}

func TestHelperRejectsPositionalArguments(t *testing.T) {
	t.Parallel()
	err := run(append(validArguments(), "extra"))
	if err == nil || !strings.Contains(err.Error(), "does not accept positional arguments") {
		t.Fatalf("run(valid + extra) = %v, want the positional-argument refusal", err)
	}
}

func TestHelperRejectsUnknownFlags(t *testing.T) {
	t.Parallel()
	err := run(append(validArguments(), "--project-id", "x"))
	if err == nil || !strings.Contains(err.Error(), "invalid helper arguments") {
		t.Fatalf("run(valid + --project-id) = %v, want a flag-parsing refusal", err)
	}
}

func TestHelperHelpIsNotAFailure(t *testing.T) {
	t.Parallel()
	if err := run([]string{"--help"}); err != nil {
		t.Fatalf("run(--help) = %v", err)
	}
}

func TestHelperCannotApplyWithoutRoot(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("non-root rejection test must not touch the native hosts lock as root")
	}
	err := run(validArguments())
	if err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("run(valid) as non-root = %v, want the EUID refusal", err)
	}
}
