package private

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Disk fixtures alone do not isolate the short QMP/pidfile paths.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("/tmp", "barn-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	root, err = filepath.EvalSymlinks(root)
	if err == nil {
		err = os.Setenv("XDG_RUNTIME_DIR", root)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
