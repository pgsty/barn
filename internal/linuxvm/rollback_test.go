package linuxvm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRollbackPreparedDryRunAndApply(t *testing.T) {
	deploymentValue, config, outcomes := commitFixture(t, &fakePrivateDisks{})
	_ = config
	artifacts := *outcomes[0].Artifacts
	dryRun, err := RollbackPrepared(deploymentValue, artifacts.Name, false)
	if err != nil || len(dryRun.Actions) < 4 {
		t.Fatalf("rollback dry run = %#v, %v", dryRun, err)
	}
	if _, err := os.Lstat(artifacts.Root); err != nil {
		t.Fatalf("dry run removed root: %v", err)
	}
	applied, err := RollbackPrepared(deploymentValue, artifacts.Name, true)
	if err != nil || len(applied.Actions) != len(dryRun.Actions) {
		t.Fatalf("rollback apply = %#v, %v", applied, err)
	}
	if _, err := os.Lstat(artifacts.NodeDir); !os.IsNotExist(err) {
		t.Fatalf("rolled back node directory remains: %v", err)
	}
}

func TestRollbackPreparedPreflightPreservesAllOnUnexpectedEntry(t *testing.T) {
	deploymentValue, _, outcomes := commitFixture(t, &fakePrivateDisks{})
	artifacts := *outcomes[0].Artifacts
	unexpected := filepath.Join(artifacts.NodeDir, "manual.img")
	if err := os.WriteFile(unexpected, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackPrepared(deploymentValue, artifacts.Name, true); err == nil {
		t.Fatal("unexpected node entry did not block rollback")
	}
	for _, pathname := range []string{artifacts.Root, artifacts.Seed, artifacts.NVRAM, artifacts.Journal, unexpected} {
		if _, err := os.Lstat(pathname); err != nil {
			t.Fatalf("blocked rollback removed %s: %v", pathname, err)
		}
	}
}

func TestRollbackPreparedRefusesCommittedStateAndRuntimeArtifact(t *testing.T) {
	deploymentValue, config, outcomes := commitFixture(t, &fakePrivateDisks{})
	if _, err := CommitPrepared(deploymentValue, config, outcomes, "test-version"); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackPrepared(deploymentValue, outcomes[0].Node, true); err == nil {
		t.Fatal("state-committed node was rolled back")
	}
}
