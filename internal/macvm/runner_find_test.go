package macvm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestFindRunnerProcessFixture(t *testing.T) {
	if os.Getenv("FARROW_TEST_FIND_RUNNER") != "1" {
		return
	}
	found, err := FindRunner()
	if err != nil {
		t.Fatal(err)
	}
	if want := os.Getenv("FARROW_TEST_FIND_RUNNER_WANT"); found != want {
		t.Fatalf("FindRunner() = %q, want %q", found, want)
	}
}

func TestFindRunnerCandidates(t *testing.T) {
	if err := HostSupported(); err != nil {
		t.Skip(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	const bundle = "bin/Farrow Mac.app/Contents/MacOS/farrow-mac-runner"
	const localBundle = "bin/libexec/Farrow Mac.app/Contents/MacOS/farrow-mac-runner"
	const brewBundle = "libexec/Farrow Mac.app/Contents/MacOS/farrow-mac-runner"
	const bare = "bin/farrow-mac-runner"
	const localLibexec = "bin/libexec/farrow-mac-runner"
	const parentLibexec = "libexec/farrow-mac-runner"
	const override = "explicit/farrow-mac-runner"
	for _, tc := range []struct {
		name       string
		candidates []string
		override   string
		want       string
	}{
		{"bundle_preferred", []string{bundle, bare, localLibexec, parentLibexec}, "", bundle},
		{"local_bundle_preferred", []string{localBundle, bare}, "", localBundle},
		{"homebrew_bundle_preferred", []string{brewBundle, bare, parentLibexec}, "", brewBundle},
		{"bare_fallback", []string{bare, localLibexec, parentLibexec}, "", bare},
		{"local_libexec_fallback", []string{localLibexec, parentLibexec}, "", localLibexec},
		{"parent_libexec_fallback", []string{parentLibexec}, "", parentLibexec},
		{"explicit_override_preferred", []string{override, bundle, bare, localLibexec, parentLibexec}, override, override},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(root, "bin", "farrow-test")
			if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, binary, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, candidate := range tc.candidates {
				path := filepath.Join(root, candidate)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			explicit := ""
			if tc.override != "" {
				explicit = filepath.Join(root, tc.override)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "-test.run=^TestFindRunnerProcessFixture$", "-test.count=1")
			cmd.Env = append(os.Environ(),
				"FARROW_TEST_FIND_RUNNER=1",
				"FARROW_TEST_FIND_RUNNER_WANT="+filepath.Join(root, tc.want),
				"FARROW_MAC_RUNNER="+explicit,
			)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("runner discovery subprocess: %v\n%s", err, output)
			}
		})
	}
}
