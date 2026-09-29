package doctor

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/execx"
	netpreflight "github.com/pgsty/barn/internal/network/preflight"
	"github.com/pgsty/barn/internal/platform"
)

func TestReadableNetworkInstallationIncludesProtectedState(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]bool{
		"exact": true, "protected": true, "absent": false, "partial": false, "invalid": false,
	} {
		if got := readableNetworkInstallation(status); got != want {
			t.Errorf("status=%q got=%t want=%t", status, got, want)
		}
	}
}

type emptyNetworkRunner struct{}

func (emptyNetworkRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	return execx.Result{}, nil
}

func TestDoctorDoesNotProbeOtherLabsGuestAddresses(t *testing.T) {
	t.Parallel()
	probe := netpreflight.Probe{
		Runner:   emptyNetworkRunner{},
		Lstat:    func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Dial: func(network, address string, _ time.Duration) (net.Conn, error) {
			if network == "tcp" {
				t.Errorf("doctor probed unrelated guest %s", address)
			}
			return nil, errors.New("fixture unavailable")
		},
	}
	for _, hostOS := range []string{"darwin", "linux"} {
		checks := networkPreflightChecks(context.Background(), platform.Profile{OS: hostOS, Arch: "amd64"}, probe)
		if len(checks) == 0 {
			t.Fatal("lost host network diagnostics")
		}
	}
}
