package hostshare

import (
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/spec"
)

func TestDarwinDirectoryReopenCapability(t *testing.T) {
	source, root := testDirs(t)
	bundle, err := Open(root, []spec.Share{{Host: source, Guest: "/project"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bundle.Close() })
	if info, err := bundle.Files()[0].Stat(); err != nil || !info.IsDir() {
		t.Fatalf("source is not an anchored directory: %v", err)
	}
	err = bundle.ValidateQEMUAccess()
	if err == nil {
		t.Fatal("Darwin directory FD unexpectedly reopenable; recheck native QEMU sharing support")
	}
	for _, want := range []string{source, "/project", "macOS", "vm_shares", "preserve needed data"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing actionable detail %q: %v", want, err)
		}
	}
	if err := (&Bundle{}).ValidateQEMUAccess(); err != nil {
		t.Fatalf("no shares must not require this capability: %v", err)
	}
}
