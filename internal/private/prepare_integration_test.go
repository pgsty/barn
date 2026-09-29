package private

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/disk"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/spec"
)

func TestIntegrationRealPrivateOfflinePrepare(t *testing.T) {
	imagePath := os.Getenv("BARN_PRIVATE_PREPARE_IMAGE")
	output := os.Getenv("BARN_PRIVATE_PREPARE_OUTPUT")
	if imagePath == "" || output == "" {
		t.Skip("set BARN_PRIVATE_PREPARE_IMAGE and BARN_PRIVATE_PREPARE_OUTPUT")
	}
	entries, err := os.ReadDir(output)
	if err != nil || len(entries) != 0 {
		t.Fatalf("integration output must be an existing empty directory: %v entries=%d", err, len(entries))
	}
	if err := os.Chmod(output, 0o700); err != nil {
		t.Fatal(err)
	}
	profile, err := platform.Native()
	if err != nil || profile.OS != "darwin" || profile.Arch != "arm64" {
		t.Skip("current retained integration evidence targets Darwin arm64")
	}
	qemuPath, err := exec.LookPath(profile.QEMUBinary)
	if err != nil {
		t.Fatal(err)
	}
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Fatal(err)
	}
	firmware, err := platform.FindFirmwareForBoot(profile, "uefi")
	if err != nil {
		t.Fatal(err)
	}
	resolved := privateResolved()
	for index := range resolved.Nodes {
		resolved.Nodes[index].RootDisk = 8 * spec.GiB
	}
	resolved.Nodes[0].Disks = []spec.Disk{{Name: "data", Size: 4 * spec.GiB, Mount: "/data", Filesystem: "ext4"}}
	plan, err := Build(resolved, os.Getuid(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := spec.Hash(resolved)
	nodeHashes, _ := spec.NodeHashes(resolved)
	publicKey, privateKey := testSSHKeyPair(t)
	seeds, err := RenderSeeds(resolved, plan, SeedInput{
		PublicKey:  publicKey,
		PrivateKey: privateKey,
		SpecHashes: nodeHashes, Generation: map[string]uint64{"meta": 1, "node-1": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := execx.OSRunner{Timeout: 30 * time.Second, OutputLimit: 1 << 20}
	config := PrepareConfig{
		DeploymentRoot: output, Resolved: resolved, SpecHash: hash, NodeHashes: nodeHashes, Plan: plan, Seeds: seeds,
		Bases:    map[string]BaseImage{"u24": {Path: imagePath, Alias: "u24", Release: "20260801", Digest: strings.Repeat("a", 64), VirtualSize: 3758096384}},
		SSHPorts: map[string]uint16{"meta": 2222, "node-1": 2223}, Profile: profile,
		QEMUBinary: qemuPath, Firmware: firmware,
		Backend: Backend{DarwinSocket: "/private/var/run/barn-vmnet.sock", ReconnectMS: 1000},
		Disks:   disk.Manager{QEMUImg: qemuImg, Runner: runner},
	}
	outcomes := prepareAll(context.Background(), config, 2)
	if len(PreparedNames(outcomes)) != 2 {
		t.Fatalf("real prepare outcomes = %#v", outcomes)
	}
	for _, outcome := range outcomes {
		artifacts := outcome.Artifacts
		for _, pathname := range append([]string{artifacts.Root}, func() []string {
			paths := make([]string, 0, len(artifacts.Data))
			for _, data := range artifacts.Data {
				paths = append(paths, data.Path)
			}
			return paths
		}()...) {
			if _, err := runner.Run(context.Background(), qemuImg, "check", "-f", "qcow2", pathname); err != nil {
				t.Fatalf("qemu-img check %s: %v", pathname, err)
			}
		}
		if _, err := ReadPrepareJournal(artifacts.Journal); err != nil {
			t.Fatal(err)
		}
		if label, _, err := readSeedISO(artifacts.Seed); err != nil || label != "CIDATA" {
			t.Fatalf("seed %s = %q, %v", outcome.Node, label, err)
		}
		nodePlan, _ := plan.Node(outcome.Node)
		if _, err := os.Lstat(nodePlan.Runtime.Directory); !os.IsNotExist(err) {
			t.Fatalf("offline real prepare created runtime %s: %v", outcome.Node, err)
		}
	}
	t.Logf("retained private prepare output=%s", filepath.Clean(output))
}
