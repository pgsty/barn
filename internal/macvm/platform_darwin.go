//go:build darwin

package macvm

import "golang.org/x/sys/unix"

// peerUID returns the user ID of a connected Unix socket's peer.
func peerUID(fd uintptr) (uint32, error) {
	credentials, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return credentials.Uid, nil
}

// cloneFile makes an APFS clone that shares every block with its source.
func cloneFile(source, target string) error {
	return unix.Clonefile(source, target, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY)
}
