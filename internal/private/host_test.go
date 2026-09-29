package private

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/execx"
	linuxnet "github.com/pgsty/barn/internal/network/linux"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/spec"
)

type qemuVersionRunner struct {
	result execx.Result
	err    error
	calls  int
}

func (runner *qemuVersionRunner) Run(_ context.Context, _ string, args ...string) (execx.Result, error) {
	runner.calls++
	if len(args) != 1 || args[0] != "--version" {
		return execx.Result{}, errors.New("unexpected command")
	}
	return runner.result, runner.err
}

func TestSelectDarwinBackendUsesFDForOldOrMissingStream(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		version platform.Version
		help    string
		wantFD  bool
	}{
		{platform.Version{Major: 8, Minor: 2, Patch: 1}, "user stream socket", true},
		{platform.Version{Major: 10, Minor: 1}, "user stream socket", true},
		{platform.Version{Major: 10, Minor: 2}, "user socket", true},
		{platform.Version{Major: 10, Minor: 2}, "user stream socket", false},
		{platform.Version{Major: 11, Minor: 1}, "user stream socket", false},
	} {
		backend := selectDarwinBackend(test.version, test.help, "/private/var/run/barn-vmnet.sock")
		if backend.DarwinUseFD != test.wantFD || (test.wantFD && backend.ReconnectMS != 0) || (!test.wantFD && backend.ReconnectMS != 1000) {
			t.Errorf("version=%s help=%q backend=%#v", test.version, test.help, backend)
		}
	}
}

func TestLinuxHelperPolicySupportsDebianAndRPMContracts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		family linuxnet.Family
		mode   uint32
		group  string
		ok     bool
	}{
		{linuxnet.Debian, 0o4750, "kvm", true},
		{linuxnet.Debian, 0o4750, "vonng", true},
		{linuxnet.Debian, 0o4755, "root", false},
		{linuxnet.RPM, 0o4755, "root", true},
		{linuxnet.RPM, 0o4755, "kvm", false},
		{linuxnet.RPM, 0o4750, "kvm", false},
	} {
		err := validateLinuxHelperPolicy(test.family, test.mode, test.group)
		if (err == nil) != test.ok {
			t.Errorf("family=%s mode=%04o group=%s err=%v", test.family, test.mode, test.group, err)
		}
	}
	for input, want := range map[string]linuxnet.Family{
		"ID=ubuntu\nID_LIKE=debian\n":         linuxnet.Debian,
		"ID=rocky\nID_LIKE=\"rhel fedora\"\n": linuxnet.RPM,
	} {
		family, err := linuxHostFamily([]byte(input))
		if err != nil || family != want {
			t.Errorf("family(%q)=%q, %v", input, family, err)
		}
	}
}

func TestPrivatePreflightRejectsQEMUBelowProfileMinimum(t *testing.T) {
	t.Parallel()
	qemuPath := filepath.Join(t.TempDir(), "qemu-system-x86_64")
	if err := os.WriteFile(qemuPath, []byte("test fixture\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	profile := platform.Profile{
		OS:          "linux",
		QEMUBinary:  qemuPath,
		MinimumQEMU: platform.Version{Major: 6, Minor: 2},
	}
	runner := &qemuVersionRunner{result: execx.Result{Stdout: []byte("QEMU emulator version 6.1.0\n")}}
	expected := &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"}
	_, err := PreflightHost(context.Background(), profile, expected, runner)
	var capability *CapabilityError
	if !errors.As(err, &capability) || !strings.Contains(err.Error(), "minimum is 6.2.0") {
		t.Fatalf("version preflight error = %T %v", err, err)
	}
	if runner.calls != 1 {
		t.Fatalf("version probe calls = %d, want 1", runner.calls)
	}
}
