package private

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pgsty/barn/internal/cloudinit"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/identity"
	"github.com/pgsty/barn/internal/state"
)

type GuestSetupError struct{ Cause error }

func (e *GuestSetupError) Error() string { return "guest setup incomplete: " + e.Cause.Error() }
func (e *GuestSetupError) Unwrap() error { return e.Cause }

// Host probes normally have a 15-second command budget. Guest setup has its
// own longer context deadline and must be allowed to wait for guest devices.
func guestSetupRunner(runner execx.Runner) execx.Runner {
	switch runner := runner.(type) {
	case execx.OSRunner:
		runner.Timeout = 0
		return runner
	case *execx.OSRunner:
		copy := *runner
		copy.Timeout = 0
		return copy
	default:
		return runner
	}
}

func guestSetupError(cause error) error {
	var command *execx.CommandError
	if errors.As(cause, &command) {
		// Generated scripts are large and add no diagnostic value to argv.
		// Keep the exit status and stderr in both text and structured results.
		copy := *command
		copy.Args = []string{"[guest setup]"}
		cause = &copy
	}
	return &GuestSetupError{Cause: cause}
}

func (l NativeLifecycle) guestSetupScript(node state.NodeState, stages []string) (string, error) {
	resolved := l.Resolved
	if resolved.Private == nil {
		return "", fmt.Errorf("guest setup retry requires an applied private network")
	}
	publicKey, err := os.ReadFile(l.PrivateKey + ".pub")
	if err != nil {
		return "", err
	}
	prefix, err := prefixLength(resolved.Private.CIDR)
	if err != nil {
		return "", err
	}
	for _, definition := range resolved.Nodes {
		if definition.Name != node.Node {
			continue
		}
		mgmtMAC, err := identity.MAC(definition.Address, identity.NICManagement)
		if err != nil {
			return "", err
		}
		privateMAC, err := identity.MAC(definition.Address, identity.NICPrivate)
		if err != nil {
			return "", err
		}
		disks := make([]cloudinit.Disk, 0, len(node.DataDisks))
		for _, disk := range node.DataDisks {
			filesystem := disk.RequestedFilesystem
			if filesystem == "" {
				for _, configured := range definition.Disks {
					if configured.Name == disk.Name {
						filesystem = configured.Filesystem
						break
					}
				}
			}
			if filesystem == "" {
				filesystem = "auto"
			}
			disks = append(disks, cloudinit.Disk{Serial: disk.Serial, Mount: disk.Mount, Filesystem: filesystem})
		}
		return cloudinit.RetryScript(cloudinit.Input{
			Node: node.Node, Hostname: node.Node, Generation: node.Generation, SpecHash: node.SpecHash,
			SSHUser: resolved.SSHUser, PublicKey: strings.TrimSpace(string(publicKey)), MgmtMAC: mgmtMAC,
			Private: &cloudinit.PrivateNetwork{MAC: privateMAC, Address: definition.Address, Prefix: prefix, HostAddress: resolved.Private.HostAddress},
			Control: definition.Control, Hosts: deploymentHosts(resolved), Disks: disks, Shares: privateCloudShares(definition.Shares),
		}, stages...)
	}
	return "", fmt.Errorf("guest %s is absent from the applied deployment", node.Node)
}
