package darwin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/network/subnet"
)

type sharingCommand func(context.Context, string, ...string) (execx.Result, error)

func (f sharingCommand) Run(ctx context.Context, binary string, args ...string) (execx.Result, error) {
	return f(ctx, binary, args...)
}

func TestSharingSubnetWithSeparatePreferenceReader(t *testing.T) {
	const host, mask = "Host_Net_Address", "Host_Net_Mask"
	for _, tc := range []struct {
		name     string
		values   map[string]string
		readErr  error
		wantErr  error
		invalid  bool
		inactive bool
	}{
		{name: "absent preferences"},
		{name: "inactive service", inactive: true, readErr: errors.New("unexpected read")},
		{name: "different subnet", values: map[string]string{host: "192.168.64.1", mask: "255.255.255.0"}},
		{name: "same subnet", values: map[string]string{host: "10.10.10.1", mask: "255.255.255.0"}, wantErr: ErrVMNetSharingBusy},
		{name: "protected preferences", readErr: os.ErrPermission, wantErr: ErrSharingPreferencesUnreadable},
		{name: "only host", values: map[string]string{host: "10.10.10.1"}, invalid: true},
		{name: "only mask", values: map[string]string{mask: "255.255.255.0"}, invalid: true},
		{name: "empty settings", values: map[string]string{host: "", mask: ""}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := sharingCommand(func(_ context.Context, binary string, args ...string) (execx.Result, error) {
				if binary != "/bin/launchctl" || strings.Join(args, " ") != "print system/com.apple.NetworkSharing" {
					t.Fatalf("unexpected service query: %s %q", binary, args)
				}
				if tc.inactive {
					return execx.Result{Stdout: []byte("active count = 0\n")}, nil
				}
				return execx.Result{Stdout: []byte("active count = 2\n")}, nil
			})
			reader := sharingCommand(func(_ context.Context, binary string, args ...string) (execx.Result, error) {
				if binary != "/usr/bin/plutil" || len(args) != 6 || args[0] != "-extract" || strings.Join(args[2:], " ") != "raw -o - /Library/Preferences/SystemConfiguration/com.apple.vmnet.plist" || (args[1] != host && args[1] != mask) {
					t.Fatalf("unexpected preference read: %s %q", binary, args)
				}
				if tc.readErr != nil {
					return execx.Result{}, tc.readErr
				}
				if value, ok := tc.values[args[1]]; ok {
					return execx.Result{Stdout: []byte(value + "\n")}, nil
				}
				return execx.Result{ExitCode: 1, Stderr: []byte("Could not extract value, error: No value at that key path or invalid key path: " + args[1])}, errors.New("plutil exited with status 1")
			})
			err := CheckSharingSubnet(context.Background(), user, reader, subnet.Default())
			if tc.invalid {
				if err == nil || errors.Is(err, ErrSharingPreferencesUnreadable) {
					t.Fatalf("invalid configuration: %v", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestSharingPreferenceWithMacOSPlutil(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires macOS plutil")
	}
	path := filepath.Join(t.TempDir(), "vmnet.plist")
	if err := os.WriteFile(path, []byte(`<plist version="1.0"><dict><key>MAC_ARRAY</key><array/><key>Host_Net_Mask</key><string>255.255.255.0</string></dict></plist>`), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := sharingCommand(func(ctx context.Context, binary string, args ...string) (execx.Result, error) {
		args[len(args)-1] = path
		return (execx.OSRunner{}).Run(ctx, binary, args...)
	})
	host, err := sharingPreference(context.Background(), reader, "Host_Net_Address")
	if err != nil || host != nil {
		t.Fatalf("absent address: %v, %v", host, err)
	}
	mask, err := sharingPreference(context.Background(), reader, "Host_Net_Mask")
	if err != nil || mask == nil || *mask != "255.255.255.0" {
		t.Fatalf("configured mask: %v, %v", mask, err)
	}
}
