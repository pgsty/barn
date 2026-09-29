//go:build !darwin

package macvm

import "errors"

// Mac machines run only on macOS; these keep the package buildable elsewhere.
func peerUID(uintptr) (uint32, error) { return 0, errors.ErrUnsupported }

func cloneFile(string, string) error { return errors.ErrUnsupported }
