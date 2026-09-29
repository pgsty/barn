package private

import (
	"fmt"
	"testing"

	"github.com/pgsty/barn/internal/config"
	"github.com/pgsty/barn/internal/spec"
)

func inventoryDefaultDiskResolved(t *testing.T, persistent bool) (spec.Resolved, spec.Resolved) {
	t.Helper()
	file, err := config.ParseInventory([]byte(fmt.Sprintf(`
all:
  vars:
    vm_image: u24
  children:
    nodes:
      hosts:
        10.10.10.10:
          nodename: meta
          vm_disks:
            - path: /data
              size: 128
              persistent: %t
`, persistent)))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Nodes) != 1 || len(resolved.Nodes[0].Disks) != 1 || resolved.Nodes[0].Disks[0].Filesystem != "" {
		t.Fatalf("implicit inventory filesystem = %#v, want auto (empty at the spec level)", resolved.Nodes)
	}
	persisted := cloneResolved(resolved)
	persisted.Nodes[0].Disks = []spec.Disk{{
		Name: "data", Size: 128 * spec.GiB, Mount: "/data",
		Filesystem: "xfs", Persistent: persistent,
	}}
	return resolved, persisted
}

func TestInventoryAutoFilesystemPreservesFormattedState(t *testing.T) {
	desired, persisted := inventoryDefaultDiskResolved(t, false)
	diff := diffResolved(persisted, desired, func(string) bool { return true })
	if err := refuseDrift(diff); err != nil || len(diff.Changed) != 0 || len(diff.Unchanged) != 1 || diff.Unchanged[0] != "meta" {
		t.Fatalf("auto filesystem default caused drift: diff=%#v err=%v", diff, err)
	}
}

func TestInventoryAutoFilesystemAllowsPersistentRecreate(t *testing.T) {
	desired, persisted := inventoryDefaultDiskResolved(t, true)
	if err := validatePrivateRecreatePersistent(Deployment{Root: t.TempDir()}, persisted, desired); err != nil {
		t.Fatalf("auto filesystem default prevented persistent recreation: %v", err)
	}
}
