package darwin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/network/subnet"
)

// ErrSharingPreferencesUnreadable lets a fresh installation defer this check
// until the approved installer can read the same keys.
var ErrSharingPreferencesUnreadable = errors.New("macOS Internet Sharing is active, but the subnet it uses could not be read")

// CheckSharingSubnet uses the caller's reader for the two vmnet preferences.
// Only the approved install plan supplies a privileged reader.
func CheckSharingSubnet(ctx context.Context, user, reader execx.Runner, layout subnet.Layout) error {
	service, err := user.Run(ctx, "/bin/launchctl", "print", "system/com.apple.NetworkSharing")
	if err != nil {
		if service.ExitCode == 113 && strings.Contains(string(service.Stderr), `Could not find service "com.apple.NetworkSharing"`) {
			return nil
		}
		return fmt.Errorf("check macOS Internet Sharing service: %w", err)
	}
	if !activeNetworkSharing(string(service.Stdout)) {
		return nil
	}
	host, err := sharingPreference(ctx, reader, "Host_Net_Address")
	if err != nil {
		return err
	}
	mask, err := sharingPreference(ctx, reader, "Host_Net_Mask")
	if err != nil {
		return err
	}
	if host == nil && mask == nil {
		return nil
	}
	if host == nil || mask == nil {
		return errors.New("macOS Internet Sharing has an incomplete subnet address and mask")
	}
	if *mask != "255.255.255.0" {
		return errors.New("macOS Internet Sharing is active, but the subnet it uses is not a /24 Barn can check")
	}
	shared, err := subnet.FromHostAddress(*host)
	if err != nil {
		return errors.New("macOS Internet Sharing is active, but the subnet it uses could not be parsed")
	}
	if shared == layout {
		return ErrVMNetSharingBusy
	}
	return nil
}

func activeNetworkSharing(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 4 && fields[0] == "active" && fields[1] == "count" && fields[2] == "=" {
			count, err := strconv.Atoi(fields[3])
			return err == nil && count > 0
		}
	}
	return false
}

// Only a successful plist read that specifically reports a missing key
// counts as an unset preference. An empty value is a different configuration.
func sharingPreference(ctx context.Context, reader execx.Runner, key string) (*string, error) {
	const plist = "/Library/Preferences/SystemConfiguration/com.apple.vmnet.plist"
	result, err := reader.Run(ctx, "/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist)
	if err != nil {
		if result.ExitCode == 1 && strings.HasSuffix(strings.TrimSpace(string(result.Stderr)), "No value at that key path or invalid key path: "+key) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %w", ErrSharingPreferencesUnreadable, err)
	}
	value := strings.TrimSpace(string(result.Stdout))
	return &value, nil
}
