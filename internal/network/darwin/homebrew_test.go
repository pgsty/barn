package darwin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/execx"
)

const testInterfaceID = "018f4b8e-1234-7abc-9def-0123456789ab"

func digestOf(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func TestHomebrewProvenanceStateContract(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlan("arm64", testInterfaceID)
	if err != nil {
		t.Fatal(err)
	}
	socketSHA := digestOf([]byte("socket"))
	clientSHA := digestOf([]byte("client"))
	brewPlan, err := plan.WithRecordedBinaries(SourceHomebrew, socketSHA, clientSHA)
	if err != nil {
		t.Fatal(err)
	}
	if brewPlan.State.ArchiveSHA != "" || brewPlan.State.Source != SourceHomebrew {
		t.Fatalf("homebrew plan state = %#v", brewPlan.State)
	}
	if brewPlan.SocketDigest() != socketSHA || brewPlan.ClientDigest() != clientSHA {
		t.Fatalf("recorded digests not effective: %s %s", brewPlan.SocketDigest(), brewPlan.ClientDigest())
	}
	if plan.SocketDigest() != plan.Release.SocketSHA256 {
		t.Fatalf("archive plan must fall back to pinned digests, got %s", plan.SocketDigest())
	}
	stateJSON, err := brewPlan.StateJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := StrictNetworkState(stateJSON)
	if err != nil || parsed != brewPlan.State {
		t.Fatalf("strict homebrew state=%#v err=%v", parsed, err)
	}
	// Archive-sourced states use pinned digests without Homebrew provenance.
	archiveJSON, err := plan.StateJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"source", "socket_sha256", "client_sha256"} {
		if strings.Contains(string(archiveJSON), forbidden) {
			t.Fatalf("archive state gained Homebrew key %q:\n%s", forbidden, archiveJSON)
		}
	}
	invalid := []NetworkState{}
	mixed := brewPlan.State
	mixed.ArchiveSHA = plan.Release.SHA256
	invalid = append(invalid, mixed)
	missingDigest := brewPlan.State
	missingDigest.ClientSHA256 = ""
	invalid = append(invalid, missingDigest)
	unknownSource := brewPlan.State
	unknownSource.Source = "macports"
	invalid = append(invalid, unknownSource)
	strayDigest := plan.State
	strayDigest.SocketSHA256 = socketSHA
	invalid = append(invalid, strayDigest)
	for index, state := range invalid {
		data, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := StrictNetworkState(append(data, '\n')); err == nil {
			t.Errorf("invalid provenance state %d accepted", index)
		}
	}
}

func TestHomebrewProvenanceInterfaceMarkerRoundTrip(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlan("arm64", testInterfaceID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithRecordedBinaries(SourceHomebrew, digestOf([]byte("socket")), digestOf([]byte("client")))
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithBSDInterface("bridge100")
	if err != nil {
		t.Fatal(err)
	}
	data, err := plan.InterfaceJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := StrictInterfaceMarker(data)
	if err != nil || parsed != plan.Interface || parsed.Source != SourceHomebrew {
		t.Fatalf("marker=%#v err=%v", parsed, err)
	}
	if !strings.Contains(string(data), `"source": "homebrew"`) {
		t.Fatalf("marker does not publish provenance:\n%s", data)
	}
	// A marker claiming homebrew without digests must be rejected.
	tampered := bytes.Replace(data, []byte(`"socket_sha256": "`+plan.State.SocketSHA256+`",`), nil, 1)
	if _, err := StrictInterfaceMarker(tampered); err == nil {
		t.Fatal("marker with missing recorded digest accepted")
	}
	// Archive-sourced markers omit Homebrew provenance and remain valid.
	archivePlan, err := NewInstallPlan("arm64", testInterfaceID)
	if err != nil {
		t.Fatal(err)
	}
	archivePlan, err = archivePlan.WithBSDInterface("bridge100")
	if err != nil {
		t.Fatal(err)
	}
	archiveData, err := archivePlan.InterfaceJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(archiveData), "source") || strings.Contains(string(archiveData), "sha256") {
		t.Fatalf("archive marker gained Homebrew provenance keys:\n%s", archiveData)
	}
	if _, err := StrictInterfaceMarker(archiveData); err != nil {
		t.Fatal(err)
	}
}

func TestWithRecordedBinariesRejectsInvalidProvenance(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlan("arm64", testInterfaceID)
	if err != nil {
		t.Fatal(err)
	}
	digest := digestOf([]byte("x"))
	for _, test := range []struct{ source, socket, client string }{
		{"macports", digest, digest},
		{SourceHomebrew, "", digest},
		{SourceHomebrew, digest, "not-a-digest"},
		{"", digest, digest},
	} {
		if _, err := plan.WithRecordedBinaries(test.source, test.socket, test.client); err == nil {
			t.Errorf("invalid provenance accepted: %#v", test)
		}
	}
	if same, err := plan.WithRecordedBinaries("", "", ""); err != nil || same.State != plan.State {
		t.Fatalf("empty overlay must restate the archive default: %#v err=%v", same.State, err)
	}
}

func writeExecutable(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyAndStageLocalBinaries(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	socketPath := filepath.Join(directory, "socket_vmnet")
	clientPath := filepath.Join(directory, "socket_vmnet_client")
	writeExecutable(t, socketPath, []byte("socket bytes"))
	writeExecutable(t, clientPath, []byte("client bytes"))
	binaries := LocalBinaries{Socket: socketPath, Client: clientPath}
	socketSHA, clientSHA, err := VerifyLocalBinaries(binaries)
	if err != nil || socketSHA != digestOf([]byte("socket bytes")) || clientSHA != digestOf([]byte("client bytes")) {
		t.Fatalf("digests=%s %s err=%v", socketSHA, clientSHA, err)
	}
	if _, _, err := VerifyLocalBinaries(LocalBinaries{Socket: "relative", Client: clientPath}); err == nil {
		t.Fatal("relative binary path accepted")
	}
	empty := filepath.Join(directory, "empty")
	writeExecutable(t, empty, nil)
	if _, _, err := VerifyLocalBinaries(LocalBinaries{Socket: empty, Client: clientPath}); err == nil {
		t.Fatal("empty binary accepted")
	}

	staging := filepath.Join(directory, "staging")
	if err := StageVerifiedLocalBinaries(binaries, staging, socketSHA, clientSHA); err != nil {
		t.Fatal(err)
	}
	staged, err := os.ReadFile(filepath.Join(staging, "socket_vmnet"))
	if err != nil || string(staged) != "socket bytes" {
		t.Fatalf("staged socket=%q err=%v", staged, err)
	}
	if err := StageVerifiedLocalBinaries(binaries, staging, socketSHA, clientSHA); err == nil {
		t.Fatal("non-empty staging directory accepted")
	}
	// A source rewritten between planning and staging must be refused.
	writeExecutable(t, socketPath, []byte("tampered"))
	if err := StageVerifiedLocalBinaries(binaries, filepath.Join(directory, "staging2"), socketSHA, clientSHA); err == nil {
		t.Fatal("tampered source staged")
	}
	if err := StageVerifiedLocalBinaries(binaries, filepath.Join(directory, "staging3"), "", ""); err == nil {
		t.Fatal("staging without recorded digests accepted")
	}
}

type homebrewRunner struct {
	prefix string
	err    error
}

func (r homebrewRunner) Run(_ context.Context, binary string, args ...string) (execx.Result, error) {
	if filepath.Base(binary) != "brew" || len(args) != 2 || args[0] != "--prefix" || args[1] != SocketVMNetFormula {
		return execx.Result{}, errors.New("unexpected command")
	}
	if r.err != nil {
		return execx.Result{}, r.err
	}
	return execx.Result{Stdout: []byte(r.prefix + "\n")}, nil
}

func TestHomebrewDiscover(t *testing.T) {
	t.Parallel()
	keg := t.TempDir()
	versioned := filepath.Join(keg, "Cellar", "socket_vmnet", ReleaseVersion+"_1")
	if err := os.MkdirAll(filepath.Join(versioned, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(versioned, "bin", "socket_vmnet"), []byte("daemon"))
	writeExecutable(t, filepath.Join(versioned, "bin", "socket_vmnet_client"), []byte("client"))
	optPrefix := filepath.Join(keg, "opt", "socket_vmnet")

	brewPath := func(string) (string, error) { return "/opt/homebrew/bin/brew", nil }
	resolveToVersioned := func(path string) (string, error) {
		if path != optPrefix {
			return "", errors.New("unexpected prefix")
		}
		return versioned, nil
	}
	probe := HomebrewProbe{Runner: homebrewRunner{prefix: optPrefix}, LookPath: brewPath, EvalSymlinks: resolveToVersioned}
	discovery, err := probe.Discover(context.Background())
	if err != nil || discovery.Status != HomebrewFound || discovery.Binaries.Socket != filepath.Join(versioned, "bin", "socket_vmnet") {
		t.Fatalf("discovery=%#v err=%v", discovery, err)
	}

	noBrew := HomebrewProbe{Runner: homebrewRunner{}, LookPath: func(string) (string, error) { return "", errors.New("absent") }}
	if discovery, err := noBrew.Discover(context.Background()); err != nil || discovery.Status != HomebrewMissing {
		t.Fatalf("no-brew discovery=%#v err=%v", discovery, err)
	}

	noFormula := HomebrewProbe{Runner: homebrewRunner{err: errors.New("not installed")}, LookPath: brewPath}
	if discovery, err := noFormula.Discover(context.Background()); err != nil || discovery.Status != HomebrewFormulaMissing {
		t.Fatalf("no-formula discovery=%#v err=%v", discovery, err)
	}

	// brew --prefix succeeds even for never-installed formulas; only the opt
	// symlink of an installed keg resolves.
	neverInstalled := HomebrewProbe{Runner: homebrewRunner{prefix: optPrefix}, LookPath: brewPath, EvalSymlinks: func(string) (string, error) {
		return "", &os.PathError{Op: "lstat", Path: optPrefix, Err: os.ErrNotExist}
	}}
	if discovery, err := neverInstalled.Discover(context.Background()); err != nil || discovery.Status != HomebrewFormulaMissing {
		t.Fatalf("never-installed discovery=%#v err=%v", discovery, err)
	}

	drifted := HomebrewProbe{Runner: homebrewRunner{prefix: optPrefix}, LookPath: brewPath, EvalSymlinks: func(string) (string, error) {
		return filepath.Join(keg, "Cellar", "socket_vmnet", "9.9.9"), nil
	}}
	if discovery, err := drifted.Discover(context.Background()); err != nil || discovery.Status != HomebrewUnusable || !strings.Contains(discovery.Reason, "9.9.9") {
		t.Fatalf("drifted discovery=%#v err=%v", discovery, err)
	}
}

func TestPlanInstallFromHomebrewRecordsProvenance(t *testing.T) {
	t.Parallel()
	// planInstall inspects the real host installation paths.
	if _, err := os.Lstat(InstallRoot); err == nil {
		t.Skip("host has an existing /opt/barn installation")
	}
	directory := t.TempDir()
	socketPath := filepath.Join(directory, "socket_vmnet")
	clientPath := filepath.Join(directory, "socket_vmnet_client")
	writeExecutable(t, socketPath, []byte("daemon bytes"))
	writeExecutable(t, clientPath, []byte("client bytes"))
	runner := &recordingRunner{}
	executor := Executor{User: runner, Root: runner}
	report, err := executor.PlanInstallFromHomebrew(context.Background(), LocalBinaries{Socket: socketPath, Client: clientPath}, testInterfaceID, "arm64", "host", "10.10.10.0/24")
	if err != nil {
		t.Fatal(err)
	}
	state := report.Plan.State
	if report.Action != "install" || state.Source != SourceHomebrew || state.ArchiveSHA != "" ||
		state.SocketSHA256 != digestOf([]byte("daemon bytes")) || state.ClientSHA256 != digestOf([]byte("client bytes")) {
		t.Fatalf("report action=%s state=%#v", report.Action, state)
	}
	if _, err := executor.PlanInstallFromHomebrew(context.Background(), LocalBinaries{Socket: socketPath, Client: filepath.Join(directory, "missing")}, testInterfaceID, "arm64", "host", "10.10.10.0/24"); err == nil {
		t.Fatal("missing client binary accepted")
	}
}
