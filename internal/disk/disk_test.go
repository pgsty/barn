package disk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pgsty/barn/internal/execx"
)

type fakeRunner struct {
	base       string
	targetSize int64
	calls      [][]string
	standalone bool
}

func (f *fakeRunner) Run(_ context.Context, binary string, args ...string) (execx.Result, error) {
	f.calls = append(f.calls, append([]string{binary}, args...))
	if len(args) == 0 {
		return execx.Result{}, fmt.Errorf("missing command")
	}
	switch args[0] {
	case "create":
		target := args[len(args)-1]
		if f.standalone {
			target = args[len(args)-2]
		}
		if err := os.WriteFile(target, []byte("fake-qcow2"), 0o644); err != nil {
			return execx.Result{}, err
		}
		return execx.Result{}, nil
	case "resize":
		return execx.Result{}, nil
	case "info":
		if contains(args, "--backing-chain") {
			chain := []Info{{Filename: args[len(args)-1], Format: "qcow2", VirtualSize: f.targetSize, FullBackingFilename: f.base, BackingFilename: f.base, BackingFilenameFormat: "qcow2"}, {Filename: f.base, Format: "qcow2", VirtualSize: 4 << 30}}
			data, _ := json.Marshal(chain)
			return execx.Result{Stdout: data}, nil
		}
		virtualSize := int64(4 << 30)
		if f.standalone {
			virtualSize = f.targetSize
		}
		data, _ := json.Marshal(Info{Filename: args[len(args)-1], Format: "qcow2", VirtualSize: virtualSize})
		return execx.Result{Stdout: data}, nil
	default:
		return execx.Result{}, fmt.Errorf("unexpected command %q", args[0])
	}
}

func TestCreateBlankPublishesVerifiedStandaloneDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "data.qcow2")
	runner := &fakeRunner{targetSize: 64 << 30, standalone: true}
	manager := Manager{QEMUImg: "qemu-img", Runner: runner}
	info, err := manager.CreateBlank(context.Background(), target, 64<<30)
	if err != nil {
		t.Fatal(err)
	}
	if info.VirtualSize != 64<<30 || info.BackingFilename != "" {
		t.Fatalf("blank info = %#v", info)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("published disk must use 0600: %v %v", info, err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestCreateOverlayUsesExplicitBackingFormatAndVerifies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if err := os.WriteFile(base, []byte("base"), 0o400); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "root.qcow2")
	runner := &fakeRunner{base: base, targetSize: 8 << 30}
	manager := Manager{QEMUImg: "/usr/bin/qemu-img", Runner: runner}
	info, err := manager.CreateOverlay(context.Background(), base, target, 8<<30)
	if err != nil {
		t.Fatalf("CreateOverlay: %v", err)
	}
	if info.VirtualSize != 8<<30 {
		t.Fatalf("verified size = %d", info.VirtualSize)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("published overlay must use 0600: %v %v", info, err)
	}
	wantCreatePrefix := []string{"/usr/bin/qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", base}
	if len(runner.calls) < 4 || !reflect.DeepEqual(runner.calls[1][:len(wantCreatePrefix)], wantCreatePrefix) {
		t.Fatalf("create call = %v, want prefix %v", runner.calls, wantCreatePrefix)
	}
	if !contains(runner.calls[3], "--backing-chain") {
		t.Fatalf("backing-chain verification call missing: %v", runner.calls)
	}
}

func TestValidateBaseRejectsExternalOrUnsafeFeatures(t *testing.T) {
	t.Parallel()
	valid := Info{Format: "qcow2", VirtualSize: 1}
	if err := ValidateBase(valid); err != nil {
		t.Fatalf("valid base rejected: %v", err)
	}
	tests := []Info{
		{Format: "raw", VirtualSize: 1},
		{Format: "qcow2", VirtualSize: 1, BackingFilename: "other"},
		{Format: "qcow2", VirtualSize: 1, DataFile: "data"},
		{Format: "qcow2", VirtualSize: 1, Encrypted: true},
		{Format: "qcow2", VirtualSize: 1, FormatSpecific: &FormatSpecific{Data: FormatSpecificData{ExtendedL2: true}}},
	}
	for i, info := range tests {
		if err := ValidateBase(info); err == nil {
			t.Errorf("unsafe base %d unexpectedly accepted: %#v", i, info)
		}
	}
}

type leakedClusterRunner struct{}

func (leakedClusterRunner) Run(_ context.Context, binary string, args ...string) (execx.Result, error) {
	result := execx.Result{Stdout: []byte(`{"corruptions":0,"check-errors":0,"leaks":1}`), ExitCode: 3}
	return result, &execx.CommandError{Binary: binary, Args: args, ExitCode: 3, Cause: fmt.Errorf("leaked clusters")}
}

func TestCheckBaseParsesExitThreeLeakReport(t *testing.T) {
	t.Parallel()
	manager := Manager{QEMUImg: "qemu-img", Runner: leakedClusterRunner{}}
	if err := manager.CheckBase(context.Background(), "/fixture/base.qcow2"); err != nil {
		t.Fatalf("leak-only qemu-img check rejected: %v", err)
	}
}

func TestCreateOverlayRejectsExistingTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	target := filepath.Join(dir, "root.qcow2")
	for _, path := range []string{base, target} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := Manager{QEMUImg: "qemu-img", Runner: &fakeRunner{base: base, targetSize: 4 << 30}}
	if _, err := manager.CreateOverlay(context.Background(), base, target, 4<<30); err == nil {
		t.Fatal("existing target unexpectedly accepted")
	}
}
