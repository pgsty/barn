// Package hostshare resolves user-selected directories and holds them open
// through QEMU startup. Access follows the invoking user's filesystem rights.
package hostshare

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/spec"
	"golang.org/x/sys/unix"
)

type Bundle struct {
	shares []spec.Share
	paths  []string
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
			return nil, fmt.Errorf("host share %s changed while opening; retry", share.Host)
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
	if canonical, err := filepath.EvalSymlinks(root); err == nil {
		root = canonical
	}
	bundle := &Bundle{shares: append([]spec.Share(nil), shares...)}
	for _, share := range shares {
		canonical, err := filepath.EvalSymlinks(share.Host)
		if err != nil {
			_ = bundle.Close()
			if errors.Is(err, os.ErrNotExist) {
				return nil, missingShare(fmt.Sprintf("host share %s (for %s) does not exist; restore the directory or its mount, then retry (Barn never creates an empty one)", share.Host, share.Guest))
			}
			return nil, fmt.Errorf("resolve host share %s: %w", share.Host, err)
		}
		share.Host = canonical
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
		bundle.paths = append(bundle.paths, canonical)
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

// Darwin opens the resolved path because /dev/fd cannot reopen directories.
// Check it against the held directory immediately before launching QEMU.
func (b *Bundle) ValidateQEMUAccess() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	for index, file := range b.files {
		path := b.paths[index]
		reopened, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open host share %s: %w", path, err)
		}
		actual, statErr := reopened.Stat()
		expected, expectedErr := file.Stat()
		if err := errors.Join(statErr, expectedErr, reopened.Close()); err != nil {
			return err
		}
		if !os.SameFile(actual, expected) {
			return fmt.Errorf("host share %s changed while opening; retry", path)
		}
	}
	return nil
}

// BindInvocation records the exact path used on macOS. Persist this invocation
// before startup so process identity checks see the argv QEMU actually uses.
func (b *Bundle) BindInvocation(invocation qemu.Invocation) (qemu.Invocation, error) {
	if runtime.GOOS != "darwin" || len(b.shares) == 0 {
		return invocation, nil
	}
	invocation.Args = append([]string(nil), invocation.Args...)
	matched := 0
	for index := 0; index+1 < len(invocation.Args); index++ {
		if invocation.Args[index] != "-fsdev" {
			continue
		}
		if matched >= len(b.shares) {
			return qemu.Invocation{}, errors.New("host-share invocation has unexpected exports")
		}
		share := b.shares[matched]
		path := strings.ReplaceAll(b.paths[matched], ",", ",,")
		options := fmt.Sprintf("local,id=share%d,path=%s,security_model=mapped-xattr,multidevs=remap,fmode=0600,dmode=0700", matched, path)
		if share.Readonly {
			options += ",readonly=on"
		}
		invocation.Args[index+1] = options
		matched++
	}
	if matched != len(b.shares) {
		return qemu.Invocation{}, errors.New("host-share invocation is missing exports")
	}
	return invocation, nil
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
