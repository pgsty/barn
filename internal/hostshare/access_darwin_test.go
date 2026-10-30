package hostshare

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/qemu"
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
	if err := bundle.ValidateQEMUAccess(); err != nil {
		t.Fatal(err)
	}
	if err := (&Bundle{}).ValidateQEMUAccess(); err != nil {
		t.Fatalf("no shares must not require this capability: %v", err)
	}
}

func TestDarwinQEMUOpensLinkedDirectoryWithSpacesAndComma(t *testing.T) {
	work, root := testDirs(t)
	source := filepath.Join(work, "项目, build tree")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(work, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	share := spec.Share{Host: link, Guest: "/src"}
	bundle, err := Open(root, []spec.Share{share})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bundle.Close() })
	invocation := qemu.Invocation{
		Args:           []string{"-machine", "virt", "-accel", "tcg", "-cpu", "max", "-m", "128", "-S", "-display", "none", "-nodefaults", "-nic", "none", "-qmp", "stdio", "-fsdev", "local,id=share0,path=/dev/fd/3,security_model=mapped-xattr", "-device", "virtio-9p-pci,fsdev=share0,mount_tag=" + spec.ShareTag(share)},
		InheritedFiles: []qemu.InheritedFile{{FD: 3, Kind: "share", ID: spec.ShareTag(share)}},
	}
	bound, err := bundle.BindInvocation(invocation)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.ValidateInvocation(bound, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(bound.Args, " "), "path="+strings.ReplaceAll(source, ",", ",,")) || strings.Contains(strings.Join(bound.Args, " "), "/dev/fd/") {
		t.Fatalf("bound argv = %q", bound.Args)
	}
	binary, err := exec.LookPath("qemu-system-aarch64")
	if err != nil {
		t.Skip("qemu-system-aarch64 is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, bound.Args...)
	command.ExtraFiles = bundle.Files()
	command.Stdin = strings.NewReader("{\"execute\":\"qmp_capabilities\"}\n{\"execute\":\"quit\"}\n")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "SHUTDOWN") {
		t.Fatalf("QEMU shared directory startup: %v\n%s", err, output)
	}
}
