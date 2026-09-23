package private

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/activity"
	"github.com/pgsty/farrow/internal/failure"
)

func TestStatusReadsRecordedStateWhileAnotherCommandHoldsTheLock(t *testing.T) {
	config, _ := statusFixture(t)
	held, err := acquireDeploymentLock(context.Background(), config.Deployment.Root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, err := (Manager{FarrowVersion: "test"}).Status(ctx)
	if err != nil {
		t.Fatalf("status queued or failed behind the holder: %v", err)
	}
	if !strings.Contains(status.Note, "(pid ") || !strings.HasSuffix(status.Note, "is running; showing recorded state") {
		t.Fatalf("note = %q", status.Note)
	}
}

func TestMutatingCommandNamesTheHolderAndGivesUpAsConflict(t *testing.T) {
	config, _ := statusFixture(t)
	held, err := acquireDeploymentLock(context.Background(), config.Deployment.Root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	previous := deploymentLockWait
	deploymentLockWait = 50 * time.Millisecond
	t.Cleanup(func() { deploymentLockWait = previous })

	var waiting string
	progress := activity.Reporter(func(event activity.Event) { waiting = event.Message })
	_, err = acquireDeploymentLock(context.Background(), config.Deployment.Root, false, progress)
	class, reason, _ := failure.Classify(err)
	if class != failure.Conflict || reason != "deployment_busy" || !strings.Contains(err.Error(), "(pid ") {
		t.Fatalf("busy lock = %q %q %v", class, reason, err)
	}
	if !strings.HasPrefix(waiting, "Waiting for another farrow command: ") || !strings.Contains(waiting, ", since ") {
		t.Fatalf("wait was not reported: %q", waiting)
	}
}

func TestLongCommandLineStillTakesTheLock(t *testing.T) {
	config, _ := statusFixture(t)
	previous := os.Args
	os.Args = []string{"farrow", "exec", "meta", "--", "bash", "-c", strings.Repeat("x", 4096)}
	t.Cleanup(func() { os.Args = previous })
	held, holder, err := tryDeploymentLock(config.Deployment.Root, false)
	if err != nil || held == nil || holder != "" {
		t.Fatalf("long exec could not take the free lock: held=%v holder=%q err=%v", held != nil, holder, err)
	}
	defer func() { _ = held.Release() }()
	if described := lockHolder(deploymentLockPath(config.Deployment.Root)); !strings.Contains(described, "farrow exec meta") || len(described) > 400 {
		t.Fatalf("holder = %q", described)
	}
}
