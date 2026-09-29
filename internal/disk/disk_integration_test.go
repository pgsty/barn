package disk

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/execx"
)

func TestIntegrationQEMUImgOverlayResizeAndChain(t *testing.T) {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runner := execx.OSRunner{Timeout: 15 * time.Second}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if _, err := runner.Run(ctx, qemuImg, "create", "-f", "qcow2", base, "1G"); err != nil {
		t.Fatalf("create base: %v", err)
	}
	if err := os.Chmod(base, 0o400); err != nil {
		t.Fatal(err)
	}
	manager := Manager{QEMUImg: qemuImg, Runner: runner}
	baseInfo, err := manager.Inspect(ctx, base)
	if err != nil {
		t.Fatalf("inspect base: %v", err)
	}
	if err := ValidateBase(baseInfo); err != nil {
		t.Fatalf("validate base: %v", err)
	}
	overlayPath := filepath.Join(dir, "root.qcow2")
	overlayInfo, err := manager.CreateOverlay(ctx, base, overlayPath, 2<<30)
	if err != nil {
		t.Fatalf("create overlay: %v", err)
	}
	if overlayInfo.VirtualSize != 2<<30 || overlayInfo.FullBackingFilename != base || overlayInfo.BackingFilenameFormat != "qcow2" {
		t.Fatalf("overlay info = %#v", overlayInfo)
	}
	grown, changed, err := manager.Grow(ctx, overlayPath, 3<<30, true)
	if err != nil || !changed || grown.VirtualSize != 3<<30 {
		t.Fatalf("grow overlay = %#v changed=%t err=%v", grown, changed, err)
	}
	_, changed, err = manager.Grow(ctx, overlayPath, 3<<30, true)
	if err != nil || changed {
		t.Fatalf("idempotent grow changed=%t err=%v", changed, err)
	}
	if _, _, err := manager.Grow(ctx, overlayPath, 2<<30, true); err == nil {
		t.Fatal("runtime disk shrink unexpectedly succeeded")
	}
}
