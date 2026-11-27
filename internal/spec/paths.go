package spec

import (
	"path"
	"strings"
)

// ReservedGuestMounts are Linux system trees that data disks and host shares
// must never mount over or inside.
var ReservedGuestMounts = []string{
	"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64",
	"/proc", "/root", "/run", "/sbin", "/sys", "/usr", "/var/lib/barn",
}

// NormalizeGuestPath cleans an absolute non-root POSIX guest path and rejects
// control characters.
func NormalizeGuestPath(value string) (string, bool) {
	if !path.IsAbs(value) {
		return "", false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return "", false
		}
	}
	cleaned := path.Clean(value)
	if cleaned == "/" {
		return "", false
	}
	return cleaned, true
}

// MountPathsOverlap reports whether two cleaned POSIX paths are equal or nested.
func MountPathsOverlap(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

// SafeDiskMount reports whether value is a valid non-reserved guest mount path.
func SafeDiskMount(value string) bool {
	cleaned, ok := NormalizeGuestPath(value)
	if !ok {
		return false
	}
	for _, root := range ReservedGuestMounts {
		if MountPathsOverlap(cleaned, root) {
			return false
		}
	}
	return true
}

// SafeShareMount reports whether value is a valid guest share mount path that
// neither overlaps system trees nor the guest SSH user's ~/.ssh directory.
func SafeShareMount(value, user string) bool {
	cleaned, ok := NormalizeGuestPath(value)
	if !ok {
		return false
	}
	if !SafeDiskMount(cleaned) {
		return false
	}
	return !MountPathsOverlap(cleaned, path.Join("/home", user, ".ssh"))
}

// UnsafeGuestPathReason explains why SafeDiskMount rejected value.
func UnsafeGuestPathReason(value string) string {
	if cleaned, ok := NormalizeGuestPath(value); ok {
		value = cleaned
	}
	for _, root := range ReservedGuestMounts {
		if MountPathsOverlap(value, root) {
			return "overlaps the reserved system path " + root
		}
	}
	return "must be an absolute path other than / and must not contain control characters"
}
