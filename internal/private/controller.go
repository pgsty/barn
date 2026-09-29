package private

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/lock"
)

type NodeFailure struct {
	Node   string `json:"node"`
	Stage  string `json:"stage"`
	Error  string `json:"error"`
	Reason string `json:"reason,omitempty"`
}

// failureReason is the stable reason a node outcome carries past the point
// where its error is flattened to text.
func failureReason(err error) string {
	_, reason, _ := failure.Classify(err)
	return reason
}

// PartialError reports a multi-node operation in which some nodes failed.
// Total counts every node the operation attempted.
type PartialError struct {
	Total      int
	Failures   []NodeFailure
	RolledBack []string
}

// IsolatedPartial returns the PartialError only when it is the whole failure.
// errors.Join can wrap a single failure after a successful state audit; a
// second (global) error must never be reported as a node-local partial result.
func IsolatedPartial(err error) *PartialError {
	if partial, ok := err.(*PartialError); ok {
		return partial
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		if causes := wrapped.Unwrap(); len(causes) == 1 {
			return IsolatedPartial(causes[0])
		}
	case interface{ Unwrap() error }:
		return IsolatedPartial(wrapped.Unwrap())
	}
	return nil
}

func (e *PartialError) Error() string {
	details := make([]string, 0, len(e.Failures))
	unready := make([]string, 0)
	for _, failure := range e.Failures {
		details = append(details, fmt.Sprintf("%s (%s: %s)", failure.Node, failure.Stage, strings.Join(strings.Fields(failure.Error), " ")))
		if failure.Stage == "readiness" || failure.Stage == "bootstrap" {
			unready = append(unready, failure.Node)
		}
	}
	message := fmt.Sprintf("%d of %d node(s) failed: %s", len(e.Failures), e.Total, strings.Join(details, "; "))
	if len(unready) == 1 {
		message += fmt.Sprintf("; run `barn logs %s` for the guest console", unready[0])
	} else if len(unready) > 1 {
		message += "; run `barn logs <node>` for the guest console"
	}
	if len(e.RolledBack) != 0 {
		message += "; rolled back prepare artifacts for " + strings.Join(e.RolledBack, ", ")
	}
	return message
}

type Controller struct {
	Deployment   Deployment
	Prepare      PrepareConfig
	Lifecycle    NodeLifecycle
	Concurrency  int
	ReadyTimeout time.Duration
	NoWait       bool
	CreateNodes  []string
	StartNodes   []string
	SetupRuntime func(string) error
	Version      string
	Progress     activity.Reporter
}

type CreateResult struct {
	Prepare []PrepareOutcome `json:"prepare"`
	Commit  CommitResult     `json:"commit"`
	Start   []StartOutcome   `json:"start"`
}

type FailedRollback struct {
	Node   string         `json:"node"`
	Result RollbackResult `json:"result"`
}

func newPartialError(failures []NodeFailure, total int) *PartialError {
	ordered := append([]NodeFailure(nil), failures...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Node < ordered[j].Node })
	return &PartialError{Total: total, Failures: ordered}
}

func startFailures(outcomes []StartOutcome) []NodeFailure {
	failures := make([]NodeFailure, 0)
	for _, outcome := range outcomes {
		if outcome.Error == "" && (outcome.Ready || outcome.ReadinessSkipped) {
			continue
		}
		stage := "start"
		if outcome.Running {
			stage = "readiness"
		}
		if outcome.BootstrapFailed {
			stage = "bootstrap"
		}
		if outcome.SetupFailed {
			stage = "guest-setup"
		}
		message := outcome.Error
		if message == "" {
			message = "guest readiness was not confirmed"
		}
		failures = append(failures, NodeFailure{Node: outcome.Node, Stage: stage, Error: message, Reason: outcome.Reason})
	}
	return failures
}

func createFailures(result CreateResult) []NodeFailure {
	failed := make(map[string]NodeFailure)
	for _, outcome := range result.Prepare {
		if outcome.Error != "" {
			failed[outcome.Node] = NodeFailure{Node: outcome.Node, Stage: "prepare", Error: outcome.Error, Reason: outcome.Reason}
		}
	}
	for _, node := range result.Commit.Failed {
		if _, exists := failed[node]; !exists {
			failed[node] = NodeFailure{Node: node, Stage: "prepare", Error: "prepared node state was not committed"}
		}
	}
	for _, failure := range startFailures(result.Start) {
		failed[failure.Node] = failure
	}
	failures := make([]NodeFailure, 0, len(failed))
	for _, failure := range failed {
		failures = append(failures, failure)
	}
	return failures
}

func (controller Controller) CreateAndStart(ctx context.Context) (_ CreateResult, returnErr error) {
	result := CreateResult{}
	if controller.Deployment.Root == "" || controller.Prepare.DeploymentRoot != controller.Deployment.Root || controller.Lifecycle == nil || controller.Version == "" {
		return result, fmt.Errorf("controller deployment, prepare, lifecycle, or version is incomplete")
	}
	deploymentLock, err := acquireDeploymentLock(ctx, controller.Deployment.Root, false, controller.Progress)
	if err != nil {
		return result, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment controller lock")
	}()
	createNames, err := selectedNodeNames(controller.Prepare.Resolved, controller.CreateNodes)
	if err != nil {
		return result, err
	}
	controller.Progress.Report(activity.Event{Phase: "prepare", Message: fmt.Sprintf("Preparing disks and cloud-init for %d node(s)", len(createNames))})
	result.Prepare = PrepareSelected(ctx, controller.Prepare, createNames, controller.Concurrency)
	controller.Progress.Report(activity.Event{Phase: "prepare", Message: fmt.Sprintf("Prepared %d of %d node(s)", len(PreparedNames(result.Prepare)), len(createNames)), Done: true})
	controller.Progress.Report(activity.Event{Phase: "commit", Message: "Committing prepared node state"})
	result.Commit, err = CommitPrepared(controller.Deployment, controller.Prepare, result.Prepare, controller.Version)
	if err != nil {
		return result, err
	}
	if len(result.Commit.Nodes) == 0 {
		return result, newPartialError(createFailures(result), len(createNames))
	}
	for _, node := range result.Commit.Nodes {
		if err := FinalizePrepared(controller.Deployment, node.Node); err != nil {
			return result, err
		}
	}
	startNames := make([]string, 0, len(result.Commit.Nodes))
	startAll := len(controller.StartNodes) == 0
	selected := nodeNameSet(controller.StartNodes)
	// Manager supplies the complete requested create set as StartNodes. A node
	// that failed offline prepare is therefore still present in that selection,
	// but it has no committed state and must not prevent successfully prepared
	// peers from starting. Keep rejecting genuinely out-of-scope selections by
	// removing only names that CommitPrepared classified as failed.
	for _, node := range result.Commit.Failed {
		delete(selected, node)
	}
	for _, node := range result.Commit.Nodes {
		if startAll {
			startNames = append(startNames, node.Node)
		} else if _, include := selected[node.Node]; include {
			startNames = append(startNames, node.Node)
			delete(selected, node.Node)
		}
	}
	if len(selected) != 0 {
		return result, fmt.Errorf("controller start selection contains nodes outside the committed deployment")
	}
	if len(startNames) != 0 {
		startMessage := fmt.Sprintf("Starting %d node(s) and waiting up to %s for guest readiness", len(startNames), controller.ReadyTimeout)
		if controller.NoWait {
			startMessage = fmt.Sprintf("Starting %d node(s) without waiting for guest readiness", len(startNames))
		}
		controller.Progress.Report(activity.Event{Phase: "guest-ready", Message: startMessage})
		result.Start, err = StartPrepared(ctx, StartConfig{
			Deployment: controller.Deployment, Lifecycle: controller.Lifecycle,
			Nodes: startNames, Concurrency: controller.Concurrency, ReadyTimeout: controller.ReadyTimeout, NoWait: controller.NoWait,
			SetupRuntime: controller.SetupRuntime, Progress: controller.Progress,
		})
		if err != nil {
			return result, err
		}
	}
	failures := createFailures(result)
	if len(failures) > 0 {
		return result, newPartialError(failures, len(createNames))
	}
	if len(startNames) != 0 {
		readyMessage := fmt.Sprintf("All %d node(s) are ready", len(startNames))
		if controller.NoWait {
			readyMessage = fmt.Sprintf("QEMU is running for %d node(s); guest readiness was skipped", len(startNames))
		}
		controller.Progress.Report(activity.Event{Phase: "guest-ready", Message: readyMessage, Done: true})
	}
	return result, nil
}

func RollbackFailedPrepares(deploymentValue Deployment, result CreateResult, apply bool) ([]FailedRollback, error) {
	rollbacks := make([]FailedRollback, 0)
	for _, outcome := range result.Prepare {
		if outcome.Error == "" {
			continue
		}
		rollback, err := RollbackPrepared(deploymentValue, outcome.Node, apply)
		if err != nil {
			return rollbacks, err
		}
		rollbacks = append(rollbacks, FailedRollback{Node: outcome.Node, Result: rollback})
	}
	return rollbacks, nil
}

func rollbackCreateFailure(deploymentValue Deployment, result CreateResult, operationErr error) error {
	rollbacks, err := RollbackFailedPrepares(deploymentValue, result, true)
	if err != nil {
		return fmt.Errorf("%w; requested failed-prepare rollback also failed: %v", operationErr, err)
	}
	rolledBack := make([]string, 0, len(rollbacks))
	for _, rollback := range rollbacks {
		rolledBack = append(rolledBack, rollback.Node)
	}
	var partial *PartialError
	if errors.As(operationErr, &partial) {
		partial.RolledBack = rolledBack
	}
	return operationErr
}
