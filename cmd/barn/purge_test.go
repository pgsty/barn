package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func TestPurgeConfirmationIsOptIn(t *testing.T) {
	t.Parallel()
	// Default purge must neither read stdin nor write a prompt, even on a TTY.
	if err := confirmPurge(false, "scope", nil, failingWriter{}); err != nil {
		t.Fatal(err)
	}
	for input, accepted := range map[string]bool{
		"y\n": true, "YES\n": true, " Yes \n": true,
		"n\n": false, "NO\n": false, "\n": false, "": false,
		"purge\n": false, "y": false,
	} {
		var output bytes.Buffer
		err := confirmPurge(true, "scope: meta", strings.NewReader(input), &output)
		if accepted && err != nil || !accepted && !errors.Is(err, ErrCancelled) {
			t.Errorf("answer %q: %v", input, err)
		}
		if !strings.HasPrefix(output.String(), "scope: meta\n") || !strings.Contains(output.String(), "[y/N]") {
			t.Errorf("missing scope or yes/no prompt: %s", &output)
		}
	}
	if err := confirmPurge(true, "scope", strings.NewReader("y\n"), failingWriter{}); err == nil {
		t.Fatal("purge accepted confirmation without displaying its scope")
	}
}

func TestPurgeRejectsConfirmationBypassFlags(t *testing.T) {
	for _, flag := range []string{"--yes", "-y", "--force"} {
		var out, errOut bytes.Buffer
		if code := run([]string{"purge", flag}, &out, &errOut); code != exitUsage || !strings.Contains(errOut.String(), "unknown") {
			t.Fatalf("purge %s: code=%d stderr=%s", flag, code, &errOut)
		}
	}
}

func TestPurgeAskHonorsAnswersBeforeDeletingAppliedOrResidualData(t *testing.T) {
	for _, applied := range []bool{false, true} {
		for _, answer := range []string{"y\n", "yes\n", "n\n", "no\n", "\n", ""} {
			t.Run(strings.TrimSpace(answer)+"/applied="+map[bool]string{false: "false", true: "true"}[applied], func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("BARN_HOME", root)
				t.Setenv("HOME", t.TempDir())
				keys := filepath.Join(root, "keys")
				if err := os.Mkdir(keys, 0o700); err != nil {
					t.Fatal(err)
				}
				key := filepath.Join(keys, "id_ed25519")
				if err := os.WriteFile(key, []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
				resolved := quickResolved(false, false)
				if applied {
					hash, err := spec.Hash(resolved)
					if err != nil {
						t.Fatal(err)
					}
					if err := (state.Store{Root: root}).WriteDeployment(state.DeploymentState{
						Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash, Resolved: resolved, UpdatedAt: time.Now().UTC(),
					}); err != nil {
						t.Fatal(err)
					}
				}
				input, err := os.CreateTemp(t.TempDir(), "answer")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = input.Close() })
				if _, err := io.WriteString(input, answer); err != nil {
					t.Fatal(err)
				}
				if _, err := input.Seek(0, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				previous := os.Stdin
				os.Stdin = input
				t.Cleanup(func() { os.Stdin = previous })
				var out, errOut bytes.Buffer
				code := run([]string{"purge", "--ask", "--json"}, &out, &errOut)
				accepted := answer == "y\n" || answer == "yes\n"
				if accepted {
					if code != exitOK {
						t.Fatalf("accepted purge failed: %d %s %s", code, &out, &errOut)
					}
					if _, err := os.Stat(key); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("accepted purge left key: %v", err)
					}
				} else {
					if code != exitCancelled {
						t.Fatalf("declined purge returned %d: %s %s", code, &out, &errOut)
					}
					if content, err := os.ReadFile(key); err != nil || string(content) != "fixture" {
						t.Fatalf("declined purge changed key: %q %v", content, err)
					}
					if applied {
						if _, err := (state.Store{Root: root}).ReadDeployment(); err != nil {
							t.Fatalf("declined purge changed state: %v", err)
						}
					}
				}
				if !strings.Contains(errOut.String(), "persistent data disks") || applied && !strings.Contains(errOut.String(), "meta  10.10.10.10") {
					t.Fatalf("missing disposal scope: %s", &errOut)
				}
			})
		}
	}
}
