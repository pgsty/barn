// Package hostshare opens user-selected host directories without following
// symlinks and keeps the resulting descriptors anchored through QEMU startup.
package hostshare

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/spec"
	"golang.org/x/sys/unix"
)

type Bundle struct {
	shares []spec.Share
	files  []*os.File
}

func contains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func overlaps(first, second string) bool {
	return contains(first, second) || contains(second, first)
}

func validateProtectedPaths(root string, share spec.Share) error {
	if root != "" && overlaps(share.Host, root) {
		return fmt.Errorf("host share %q overlaps the protected Barn data root %q", share.Host, root)
	}
	return nil
}

// missingShare is a vanished source directory; it matches os.ErrNotExist
// without repeating "file does not exist" after a message that says so.
type missingShare string

func (e missingShare) Error() string      { return string(e) }
func (missingShare) Is(target error) bool { return target == os.ErrNotExist }

func openDirectory(share spec.Share) (*os.File, error) {
	if !filepath.IsAbs(share.Host) || filepath.Clean(share.Host) != share.Host || share.Host == "/" || strings.ContainsAny(share.Host, "\x00\r\n") {
		return nil, fmt.Errorf("host share path must be a clean non-root absolute path: %q", share.Host)
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_DIRECTORY
	descriptor, err := unix.Open(string(filepath.Separator), flags, 0)
	if err != nil {
		return nil, fmt.Errorf("open host filesystem root: %w", err)
	}
	closeDescriptor := true
	defer func() {
		if closeDescriptor {
			_ = unix.Close(descriptor)
		}
	}()
	components := strings.Split(strings.TrimPrefix(share.Host, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("host share has an unsafe path component: %q", share.Host)
		}
		next, openErr := unix.Openat(descriptor, component, flags, 0)
		switch {
		case openErr == nil:
		case errors.Is(openErr, unix.ENOENT):
			return nil, missingShare(fmt.Sprintf("host share %s does not exist", share.Host))
		case errors.Is(openErr, unix.ELOOP):
			// Shares bind the directory named in the inventory; a symlink could
			// be repointed underneath the lab, so ask for the real path.
			if real, err := filepath.EvalSymlinks(share.Host); err == nil {
				return nil, fmt.Errorf("host share %s goes through a symlink at %q; write the real path %s in the inventory", share.Host, component, real)
			}
			return nil, fmt.Errorf("host share %s goes through a symlink at %q; write the real path in the inventory", share.Host, component)
		default:
			return nil, fmt.Errorf("open host share %s at %q: %w", share.Host, component, openErr)
		}
		_ = unix.Close(descriptor)
		descriptor = next
	}
	var stat unix.Stat_t
	if err := unix.Fstat(descriptor, &stat); err != nil {
		return nil, fmt.Errorf("fstat host share %q: %w", share.Host, err)
	}
	mode := uint32(stat.Mode)
	if mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("host share is not a directory: %q", share.Host)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("host share %q is owned by uid %d, expected %d", share.Host, stat.Uid, os.Geteuid())
	}
	permissions := mode & 0o777
	// Project directories often allow group writes. The user chose this
	// directory explicitly; its mode is not a reason to reject the whole lab.
	// Guest initialization checks actual access and can fall back to read-only.
	required := uint32(0o500)
	if permissions&required != required {
		return nil, fmt.Errorf("host share %q owner permissions %#o do not provide required %#o", share.Host, permissions, required)
	}
	file := os.NewFile(uintptr(descriptor), "barn-host-share")
	if file == nil {
		return nil, errors.New("wrap host share directory descriptor")
	}
	closeDescriptor = false
	return file, nil
}

// Open validates protected-path boundaries, securely opens every source in
// canonical spec order, and closes the partial set on every error.
func Open(root string, shares []spec.Share) (*Bundle, error) {
	bundle := &Bundle{shares: append([]spec.Share(nil), shares...)}
	for _, share := range shares {
		if err := validateProtectedPaths(root, share); err != nil {
			_ = bundle.Close()
			return nil, err
		}
		file, err := openDirectory(share)
		if err != nil {
			_ = bundle.Close()
			if errors.Is(err, os.ErrNotExist) {
				return nil, missingShare(fmt.Sprintf("host share %s (for %s) does not exist; restore the directory or its mount, then retry (Barn never creates an empty one)", share.Host, share.Guest))
			}
			return nil, fmt.Errorf("host share %q -> %q: %w", share.Host, share.Guest, err)
		}
		bundle.files = append(bundle.files, file)
	}
	return bundle, nil
}

func Validate(root string, shares []spec.Share) error {
	bundle, err := Open(root, shares)
	if err != nil {
		return err
	}
	return bundle.Close()
}

// ReasonUnsupportedOnDarwin marks a share QEMU cannot open on macOS. Retrying
// cannot fix it; only a configuration or host change can.
const ReasonUnsupportedOnDarwin = "host_share_unsupported"

// ValidateQEMUAccess checks the directory reopen used by QEMU's local 9p
// backend. Darwin's /dev/fd supports stat but not open(O_DIRECTORY). Do not
// substitute a pathname: that would lose the descriptor's identity guarantee.
func (b *Bundle) ValidateQEMUAccess() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	for index, file := range b.files {
		path := fmt.Sprintf("/dev/fd/%d", file.Fd())
		reopened, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			share := b.shares[index]
			return failure.New(failure.Capability, fmt.Errorf("host share %q -> %q cannot be safely opened by QEMU on macOS: %w; omit vm_shares when creating a new node, or use a Linux host; changing shares on an existing node requires explicit recreate and replaces its root disk, so preserve needed data first; Barn will not bypass directory identity checks", share.Host, share.Guest, err)).Because(ReasonUnsupportedOnDarwin)
		}
		if err := reopened.Close(); err != nil {
			return fmt.Errorf("close host-share capability probe: %w", err)
		}
	}
	return nil
}

func (b *Bundle) Files() []*os.File {
	if b == nil {
		return nil
	}
	return append([]*os.File(nil), b.files...)
}

// ValidateInvocation proves the QEMU invocation's inherited-file layout
// matches this bundle's share order and tags before any descriptor is handed
// to the guest.
func (b *Bundle) ValidateInvocation(invocation qemu.Invocation, prefixFiles int) error {
	if b == nil {
		return errors.New("host share bundle is nil")
	}
	files := invocation.ShareFiles()
	if len(files) != len(b.shares) {
		return fmt.Errorf("QEMU invocation has %d share descriptors for %d sources", len(files), len(b.shares))
	}
	for index, file := range files {
		expectedFD := 3 + prefixFiles + index
		expectedTag := spec.ShareTag(b.shares[index])
		if file.FD != expectedFD || file.ID != expectedTag {
			return fmt.Errorf("QEMU share %d inherited-file layout is %s/fd%d, expected %s/fd%d", index, file.ID, file.FD, expectedTag, expectedFD)
		}
	}
	return nil
}

func (b *Bundle) Close() error {
	if b == nil {
		return nil
	}
	var joined error
	for _, file := range b.files {
		if file != nil {
			joined = errors.Join(joined, file.Close())
		}
	}
	b.files = nil
	return joined
}

func QEMU(shares []spec.Share) []qemu.Share {
	result := make([]qemu.Share, 0, len(shares))
	for _, share := range shares {
		result = append(result, qemu.Share{Tag: spec.ShareTag(share), Readonly: share.Readonly})
	}
	return result
}
