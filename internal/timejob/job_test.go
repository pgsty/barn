package timejob

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/execx"
)

type jobRunner struct {
	active   bool
	calls    []string
	failLoad bool
}

func (r *jobRunner) Run(_ context.Context, binary string, args ...string) (execx.Result, error) {
	command := binary + " " + strings.Join(args, " ")
	r.calls = append(r.calls, command)
	if strings.Contains(command, " print ") || strings.Contains(command, " is-active ") {
		if r.active {
			return execx.Result{}, nil
		}
		code := 3
		if binary == "launchctl" {
			code = 113
		}
		return execx.Result{ExitCode: code}, errors.New("not loaded")
	}
	if strings.Contains(command, "bootstrap") || strings.Contains(command, "enable --now") {
		if r.failLoad {
			return execx.Result{ExitCode: 5}, errors.New("scheduler rejected job")
		}
		r.active = true
	}
	if strings.Contains(command, "bootout") || strings.Contains(command, "disable --now") {
		r.active = false
	}
	return execx.Result{}, nil
}

func TestScheduledJobsRoundTripAndDisable(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			home := t.TempDir()
			executable := filepath.Join(home, "barn with $dollar %percent &quote\"")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			runner := &jobRunner{}
			manager := Manager{OS: platform, Home: home, UID: 501, Executable: executable, DataRoot: filepath.Join(home, "lab $data %x &\""), Path: "/a path:/usr/bin", Runner: runner}
			status, err := manager.Change(context.Background(), true)
			if err != nil || !status.Installed || !status.Active || status.DataRoot != manager.DataRoot {
				t.Fatalf("enable: %+v %v", status, err)
			}
			for _, path := range status.Files {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if platform == "darwin" {
					decoder := xml.NewDecoder(strings.NewReader(string(data)))
					for {
						_, err := decoder.Token()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatalf("invalid plist: %v", err)
						}
					}
					if !strings.Contains(string(data), "StartCalendarInterval") || strings.Contains(string(data), "KeepAlive") {
						t.Fatalf("wrong wake policy: %s", data)
					}
				} else if strings.HasSuffix(path, ".service") {
					if !strings.Contains(string(data), "$$dollar %%percent") || !strings.Contains(string(data), "Type=oneshot") {
						t.Fatalf("unsafe service: %s", data)
					}
				} else if !strings.Contains(string(data), "Persistent=true") || !strings.Contains(string(data), "AccuracySec=1s") {
					t.Fatalf("wrong timer: %s", data)
				}
			}
			if _, err := manager.Change(context.Background(), true); err != nil {
				t.Fatalf("enable twice: %v", err)
			}
			status, err = manager.Change(context.Background(), false)
			if err != nil || status.Installed || status.Active {
				t.Fatalf("disable: %+v %v", status, err)
			}
			if _, err := manager.Change(context.Background(), false); err != nil {
				t.Fatalf("disable twice: %v", err)
			}
		})
	}
}

func TestJobPreservesUnmanagedFilesAndReportsRegistrationFailure(t *testing.T) {
	home := t.TempDir()
	executable := filepath.Join(home, "barn")
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &jobRunner{}
	manager := Manager{OS: "darwin", Home: home, Executable: executable, DataRoot: filepath.Join(home, "state"), Runner: runner}
	paths, _ := manager.paths()
	if err := os.MkdirAll(filepath.Dir(paths[0]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[0], []byte("other user's job"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, enable := range []bool{true, false} {
		if _, err := manager.Change(context.Background(), enable); err == nil {
			t.Fatal("overwrote unmanaged job")
		}
	}
	if len(runner.calls) != 0 {
		t.Fatal("touched scheduler before checking ownership")
	}
	if err := os.Remove(paths[0]); err != nil {
		t.Fatal(err)
	}
	runner.failLoad = true
	if _, err := manager.Change(context.Background(), true); err == nil {
		t.Fatal("reported failed scheduler registration as success")
	}
	status, err := manager.Status(context.Background())
	if err != nil || !status.Installed || status.Active {
		t.Fatalf("lost recoverable registration state: %+v %v", status, err)
	}
}

func TestSameExecutablePreservesSymlinkAndRejectsDifferentBinary(t *testing.T) {
	dir := t.TempDir()
	realBinary := filepath.Join(dir, "cellar-barn")
	symlinkBinary := filepath.Join(dir, "homebrew-barn")
	otherBinary := filepath.Join(dir, "other-barn")
	for _, path := range []string{realBinary, otherBinary} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realBinary, symlinkBinary); err != nil {
		t.Fatal(err)
	}
	if !sameExecutable(symlinkBinary, realBinary) {
		t.Fatal("symlink to running binary was rejected")
	}
	if sameExecutable(otherBinary, realBinary) {
		t.Fatal("unrelated binary was accepted as the running executable")
	}
}
