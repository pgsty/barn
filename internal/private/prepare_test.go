package private

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/disk"
	"github.com/pgsty/farrow/internal/platform"
	"github.com/pgsty/farrow/internal/spec"
)

type fakePrivateDisks struct {
	mu            sync.Mutex
	failSubstring string
	active        int
	maxActive     int
	barrier       chan struct{}
	barrierOnce   sync.Once
}

func (fake *fakePrivateDisks) create(target string, size int64) (disk.Info, error) {
	fake.mu.Lock()
	fake.active++
	if fake.active > fake.maxActive {
		fake.maxActive = fake.active
	}
	active := fake.active
	fake.mu.Unlock()
	defer func() {
		fake.mu.Lock()
		fake.active--
		fake.mu.Unlock()
	}()
	if fake.barrier != nil {
		if active >= 2 {
			fake.barrierOnce.Do(func() { close(fake.barrier) })
		}
		select {
		case <-fake.barrier:
		case <-time.After(time.Second):
			return disk.Info{}, errors.New("parallel prepare barrier timed out")
		}
	}
	time.Sleep(10 * time.Millisecond)
	if fake.failSubstring != "" && strings.Contains(target, fake.failSubstring) {
		return disk.Info{}, errors.New("injected disk prepare failure")
	}
	if err := os.WriteFile(target, []byte("fake-qcow2"), 0o600); err != nil {
		return disk.Info{}, err
	}
	return disk.Info{Filename: target, Format: "qcow2", VirtualSize: size}, nil
}

func (fake *fakePrivateDisks) CreateOverlay(_ context.Context, _, target string, size int64) (disk.Info, error) {
	return fake.create(target, size)
}

func (fake *fakePrivateDisks) CreateBlank(_ context.Context, target string, size int64) (disk.Info, error) {
	return fake.create(target, size)
}

func privatePrepareConfig(t *testing.T, root string, disks DiskOps) PrepareConfig {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved := privateResolved()
	resolved.Nodes[0].RootDisk = 8 * spec.GiB
	resolved.Nodes[1].RootDisk = 8 * spec.GiB
	resolved.Nodes[0].Disks = []spec.Disk{{Name: "data", Size: 4 * spec.GiB, Mount: "/data", Filesystem: "ext4"}}
	plan, err := Build(resolved, os.Getuid(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	nodeHashes, err := spec.NodeHashes(resolved)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey := testSSHKeyPair(t)
	seeds, err := RenderSeeds(resolved, plan, SeedInput{
		PublicKey: publicKey, PrivateKey: privateKey,
		SpecHashes: nodeHashes, Generation: map[string]uint64{"meta": 1, "node-1": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "base.qcow2")
	code := filepath.Join(root, "firmware-code.fd")
	vars := filepath.Join(root, "firmware-vars.fd")
	for _, pathname := range []string{base, code, vars} {
		if err := os.WriteFile(pathname, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := platform.Resolve("darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	return PrepareConfig{
		DeploymentRoot: root, Resolved: resolved, SpecHash: hash, NodeHashes: nodeHashes, Plan: plan, Seeds: seeds,
		Bases:    map[string]BaseImage{"u24": {Path: base, Alias: "u24", Release: "test", Digest: strings.Repeat("a", 64), VirtualSize: 4 * spec.GiB}},
		SSHPorts: map[string]uint16{"meta": 2222, "node-1": 2223},
		Profile:  profile, QEMUBinary: "/opt/qemu/bin/qemu-system-aarch64",
		Firmware: platform.Firmware{Code: code, Vars: vars},
		Backend:  Backend{DarwinSocket: "/private/var/run/farrow-vmnet.sock", ReconnectMS: 1000},
		Disks:    disks,
	}
}

func TestPrepareAllBuildsJournaledOfflineArtifacts(t *testing.T) {
	root := t.TempDir()
	fake := &fakePrivateDisks{barrier: make(chan struct{})}
	config := privatePrepareConfig(t, root, fake)
	outcomes := prepareAll(context.Background(), config, 2)
	if names := PreparedNames(outcomes); len(names) != 2 || names[0] != "meta" || names[1] != "node-1" {
		t.Fatalf("prepared outcomes = %#v names=%v", outcomes, names)
	}
	if fake.maxActive < 2 {
		t.Fatalf("prepare did not run concurrently: max=%d", fake.maxActive)
	}
	for _, outcome := range outcomes {
		artifacts := outcome.Artifacts
		journal, err := ReadPrepareJournal(artifacts.Journal)
		if err != nil || !journal.Prepared || journal.Invocation.Binary == "" || journal.SpecHash != config.NodeHashes[outcome.Node] {
			t.Fatalf("journal %s = %#v, %v", outcome.Node, journal, err)
		}
		for _, pathname := range []string{artifacts.Root, artifacts.Seed, artifacts.NVRAM} {
			if _, err := os.Lstat(pathname); err != nil {
				t.Errorf("node %s missing %s: %v", outcome.Node, pathname, err)
			}
		}
		label, _, err := readSeedISO(artifacts.Seed)
		if err != nil || label != "CIDATA" {
			t.Errorf("node %s seed = %q, %v", outcome.Node, label, err)
		}
		joined := strings.Join(artifacts.Invocation.Args, "\n")
		if !strings.Contains(joined, "stream,id=private") || !strings.Contains(joined, "hostfwd=tcp:127.0.0.1:") {
			t.Errorf("node %s invocation lacks private/SSH network: %s", outcome.Node, joined)
		}
		nodePlan, _ := config.Plan.Node(outcome.Node)
		if _, err := os.Lstat(nodePlan.Runtime.Directory); !os.IsNotExist(err) {
			t.Errorf("offline prepare created runtime directory for %s: %v", outcome.Node, err)
		}
	}
}

func TestPrepareAllPreservesSuccessOnPartialFailure(t *testing.T) {
	root := t.TempDir()
	fake := &fakePrivateDisks{failSubstring: "node-1/root.qcow2"}
	config := privatePrepareConfig(t, root, fake)
	outcomes := prepareAll(context.Background(), config, 2)
	if names := PreparedNames(outcomes); len(names) != 1 || names[0] != "meta" {
		t.Fatalf("partial prepared names = %v outcomes=%#v", names, outcomes)
	}
	if outcomes[1].Error == "" || outcomes[1].Artifacts != nil {
		t.Fatalf("failed node outcome = %#v", outcomes[1])
	}
	failedJournal := filepath.Join(root, "nodes", "node-1", "private-prepare.json")
	journal, err := ReadPrepareJournal(failedJournal)
	if err != nil || journal.Prepared || len(journal.Completed) != 0 {
		t.Fatalf("partial journal = %#v, %v", journal, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "nodes", "meta", "root.qcow2")); err != nil {
		t.Fatalf("successful node was rolled back: %v", err)
	}
}

func TestPrepareJournalStrictness(t *testing.T) {
	root := t.TempDir()
	config := privatePrepareConfig(t, root, &fakePrivateDisks{})
	artifacts, err := PrepareNode(context.Background(), config, "meta")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(artifacts.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifacts.Journal, append(data, []byte("{}")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrepareJournal(artifacts.Journal); err == nil {
		t.Fatal("trailing private journal JSON accepted")
	}
}

func TestPrepareRetryRecoversOfflineFailureAndPreservesPeers(t *testing.T) {
	root := t.TempDir()
	fake := &fakePrivateDisks{failSubstring: "node-1/root.qcow2"}
	config := privatePrepareConfig(t, root, fake)
	outcomes := prepareAll(context.Background(), config, 2)
	if len(PreparedNames(outcomes)) != 1 {
		t.Fatalf("expected partial prepare: %+v", outcomes)
	}
	peer := filepath.Join(root, "nodes", "meta", "root.qcow2")
	before, err := os.Stat(peer)
	if err != nil {
		t.Fatal(err)
	}
	fake.failSubstring = ""
	if _, err := PrepareNode(context.Background(), config, "node-1"); err != nil {
		t.Fatalf("repeated up cannot recover: %v", err)
	}
	after, err := os.Stat(peer)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("retry replaced an unrelated node disk")
	}
}

// The usual reaction to a failed first up is editing the inventory. The node's
// definition (and hash) then differs from the failed attempt's journal, which
// must not strand the unfinished directory.
func TestPrepareRetryRecoversAfterTheNodeDefinitionChanged(t *testing.T) {
	root := t.TempDir()
	fake := &fakePrivateDisks{failSubstring: "meta/data.qcow2"}
	config := privatePrepareConfig(t, root, fake)
	if _, err := PrepareNode(context.Background(), config, "meta"); err == nil {
		t.Fatal("expected data disk failure")
	}
	fake.failSubstring = ""
	config.NodeHashes["meta"] = strings.Repeat("e", 64)
	if _, err := PrepareNode(context.Background(), config, "meta"); err != nil {
		t.Fatalf("edited inventory cannot recover: %v", err)
	}
}

func TestPrepareRetryRemovesAnEmptyUnfinishedDirectory(t *testing.T) {
	root := t.TempDir()
	config := privatePrepareConfig(t, root, &fakePrivateDisks{})
	if err := os.MkdirAll(filepath.Join(root, "nodes", "meta"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareNode(context.Background(), config, "meta"); err != nil {
		t.Fatalf("crash before the first journal write cannot recover: %v", err)
	}
}

func TestPrepareRetryPreservesUnrecognizedArtifacts(t *testing.T) {
	root := t.TempDir()
	fake := &fakePrivateDisks{failSubstring: "meta/data.qcow2"}
	config := privatePrepareConfig(t, root, fake)
	if _, err := PrepareNode(context.Background(), config, "meta"); err == nil {
		t.Fatal("expected data disk failure")
	}
	manual := filepath.Join(root, "nodes", "meta", "manual.img")
	if err := os.WriteFile(manual, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	fake.failSubstring = ""
	if _, err := PrepareNode(context.Background(), config, "meta"); err == nil {
		t.Fatal("unrecognized artifacts were discarded")
	}
	for _, file := range []string{manual, filepath.Join(root, "nodes", "meta", "root.qcow2")} {
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("failed recovery discarded %s: %v", file, err)
		}
	}
}

func TestPrepareRetryDoesNotReplaceCommittedDisk(t *testing.T) {
	deployment, config, outcomes := commitFixture(t, &fakePrivateDisks{})
	if _, err := CommitPrepared(deployment, config, outcomes, "test-version"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(outcomes[0].Artifacts.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareNode(context.Background(), config, outcomes[0].Node); err == nil {
		t.Fatal("committed node was prepared again")
	}
	after, err := os.Stat(outcomes[0].Artifacts.Root)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("committed disk changed")
	}
}
