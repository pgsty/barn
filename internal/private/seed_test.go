package private

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/spec"
)

func TestRenderSeedsPrivateContractAndKeyBoundary(t *testing.T) {
	t.Parallel()
	resolved := privateResolved()
	resolved.Nodes[0].Disks = []spec.Disk{{Name: "data", Size: 4 * spec.GiB, Mount: "/data", Filesystem: "ext4"}}
	plan, err := Build(resolved, os.Getuid(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey := testSSHKeyPair(t)
	files, err := RenderSeeds(resolved, plan, SeedInput{
		PublicKey: publicKey, PrivateKey: privateKey,
		SpecHashes: map[string]string{"meta": strings.Repeat("a", 64), "node-1": strings.Repeat("a", 64)}, Generation: map[string]uint64{"meta": 1, "node-1": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("seed count = %d", len(files))
	}
	control := files["meta"]
	worker := files["node-1"]
	keyMarker := []byte("-----BEGIN PRIVATE KEY-----")
	if !bytes.Contains(control.UserData, keyMarker) || bytes.Contains(worker.UserData, keyMarker) {
		t.Fatal("deployment private key crossed the control-node boundary")
	}
	for name, seed := range files {
		for _, want := range []string{"meta", "10.10.10.10", "node-1", "10.10.10.11", "expected_mac=", "dhcp4: false"} {
			combined := string(seed.UserData) + string(seed.NetworkConfig)
			if !strings.Contains(combined, want) {
				t.Errorf("seed %s missing %q", name, want)
			}
		}
		if strings.Contains(string(seed.NetworkConfig), "gateway") || strings.Contains(string(seed.NetworkConfig), "nameservers") {
			t.Errorf("seed %s private NIC gained gateway/DNS: %s", name, seed.NetworkConfig)
		}
		if strings.Contains(string(seed.NetworkConfig), "set-name:") {
			t.Errorf("seed %s renames a guest interface: %s", name, seed.NetworkConfig)
		}
	}
	if !bytes.Contains(control.UserData, []byte("/data")) || !bytes.Contains(control.UserData, []byte("ext4")) {
		t.Fatal("control data disk contract missing")
	}
}

func TestRenderSeedsRequiresControlPrivateKeyAndEveryGeneration(t *testing.T) {
	t.Parallel()
	resolved := privateResolved()
	plan, err := Build(resolved, os.Getuid(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey := testSSHKeyPair(t)
	input := SeedInput{PublicKey: publicKey, SpecHashes: map[string]string{"meta": strings.Repeat("a", 64), "node-1": strings.Repeat("a", 64)}, Generation: map[string]uint64{"meta": 1, "node-1": 1}}
	if _, err := RenderSeeds(resolved, plan, input); err == nil {
		t.Fatal("control seed without private key accepted")
	}
	input.PrivateKey = privateKey
	delete(input.Generation, "node-1")
	if _, err := RenderSeeds(resolved, plan, input); err == nil {
		t.Fatal("missing worker generation accepted")
	}
}

func TestSingleNodeControlReceivesLateralKeyForFutureScaleOut(t *testing.T) {
	t.Parallel()
	resolved := privateResolved()
	resolved.Nodes = resolved.Nodes[:1]
	plan, err := Build(resolved, os.Getuid(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey := testSSHKeyPair(t)
	files, err := RenderSeeds(resolved, plan, SeedInput{PublicKey: publicKey, PrivateKey: privateKey, SpecHashes: map[string]string{"meta": strings.Repeat("a", 64)}, Generation: map[string]uint64{"meta": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(files["meta"].UserData, []byte("PRIVATE KEY")) || !bytes.Contains(files["meta"].UserData, []byte("barn-install-control-ssh")) {
		t.Fatal("single-node control guest lacks the key required for additive scale-out")
	}
}
