package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestLifecycleParallelFlag(t *testing.T) {
	for _, name := range []string{"up", "start", "stop", "restart", "reload", "recreate", "destroy", "purge"} {
		t.Run(name, func(t *testing.T) {
			root := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{})
			command, _, err := root.Find([]string{name})
			if err != nil {
				t.Fatal(err)
			}
			flag := command.Flags().Lookup("parallel")
			if flag == nil || flag.Shorthand != "p" || flag.DefValue != strconv.Itoa(max(1, runtime.NumCPU()-2)) {
				t.Fatalf("unexpected parallel flag: %+v", flag)
			}
			if value, err := lifecycleParallelism(command); err != nil || value != nil {
				t.Fatalf("automatic parallelism=%v, err=%v", value, err)
			}
			for _, args := range [][]string{{"--parallel", "0"}, {"-p", "0"}, {"--parallel", "1"}, {"-p", "32"}, {"meta", "--parallel=64"}} {
				if err := command.ParseFlags(args); err != nil {
					t.Fatal(err)
				}
				value, err := lifecycleParallelism(command)
				parsed, _ := command.Flags().GetInt("parallel")
				if err != nil || value == nil || *value != parsed {
					t.Fatalf("override=%v parsed=%d err=%v", value, parsed, err)
				}
			}
		})
	}
	for _, name := range []string{"plan", "status", "resize"} {
		command, _, err := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}).Find([]string{name})
		if err != nil || command.Flags().Lookup("parallel") != nil {
			t.Fatalf("%s unexpectedly accepts --parallel: %v", name, err)
		}
	}
}

func TestLifecycleRejectsInvalidParallelBeforeChangingState(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "absent")
	t.Setenv("BARN_HOME", dataRoot)
	for _, name := range []string{"up", "start", "stop", "restart", "reload", "recreate", "destroy", "purge"} {
		for _, value := range []string{"-1", "-32"} {
			var stdout, stderr bytes.Buffer
			code := run([]string{name, "--parallel=" + value, "--json"}, &stdout, &stderr)
			if code != exitUsage || !strings.Contains(stdout.String(), `"error": "usage"`) || !strings.Contains(stdout.String(), "--parallel must be zero or positive") {
				t.Fatalf("%s --parallel=%s: code=%d stdout=%s stderr=%s", name, value, code, &stdout, &stderr)
			}
		}
	}
	if _, err := os.Stat(dataRoot); !os.IsNotExist(err) {
		t.Fatalf("invalid parallelism created deployment state: %v", err)
	}
}

func TestLifecycleAcceptsZeroParallelForEmptyPurge(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "absent")
	t.Setenv("BARN_HOME", dataRoot)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"purge", "--parallel=0", "--json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("empty purge --parallel=0: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	if _, err := os.Stat(dataRoot); !os.IsNotExist(err) {
		t.Fatalf("empty purge created deployment state: %v", err)
	}
}
