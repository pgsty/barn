package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/activity"
	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/pgsty/barn/internal/sshconfig"
)

const (
	lifecycleSSHTimeout   = 2 * time.Second
	lifecycleGuestTimeout = 5 * time.Second
	lifecycleEventTimeout = time.Second
	lifecycleTimeTimeout  = 20 * time.Second
)

type lifecycleIntegrator interface {
	sshConfigReconciler
	RefreshGuestMetadata(context.Context) error
	SyncGuestTime(context.Context, []string) error
	RecordEvent(context.Context, string, string, string) error
}

func reportLifecycleNodes(report activity.Reporter, nodes []privatevm.NodeStatus, event activity.Event) {
	for _, node := range nodes {
		event.Node, event.Address = node.Name, node.Address
		event.CPUs, event.Memory = node.CPUs, node.Memory
		report.Report(event)
	}
}

// The VM result is already known. Each optional integration gets its own
// deadline so a busy lock or an unresponsive guest cannot use the lifecycle's
// entire timeout, or prevent the next integration from being attempted.
func finishLifecycleIntegrations(ctx context.Context, command string, deploymentHasNodes, noWait bool, retry string, status privatevm.Status, operationErr error, integrator lifecycleIntegrator, report activity.Reporter) (*sshconfig.Result, []lifecycleWarning) {
	var sshResult *sshconfig.Result
	var warnings []lifecycleWarning
	var partial *privatevm.PartialError
	reconcileSSH := operationErr == nil || startupCommand(command) && errors.As(operationErr, &partial)
	if action := lifecycleSSHConfigAction(command, deploymentHasNodes); reconcileSSH && action != "" && ctx.Err() == nil {
		report.Report(activity.Event{Phase: "ssh-config", Message: "Updating the SSH client configuration"})
		err := runOptionalIntegration(ctx, lifecycleSSHTimeout, func(step context.Context) error {
			var err error
			sshResult, err = reconcileLifecycleSSHConfig(step, command, deploymentHasNodes, integrator)
			return err
		})
		if err != nil {
			warnings = append(warnings, sshIntegrationWarning(action, err))
		} else {
			report.Report(activity.Event{Phase: "ssh-config", Message: "SSH client configuration is up to date", Done: true})
		}
	}
	refreshGuests := startupCommand(command) || command == "destroy" && deploymentHasNodes
	if reconcileSSH && startupCommand(command) && !noWait && ctx.Err() == nil {
		nodes := make([]string, 0)
		for _, node := range status.Nodes {
			if node.Ready && node.Runtime == "running" {
				nodes = append(nodes, node.Name)
			}
		}
		if len(nodes) > 0 {
			report.Report(activity.Event{Phase: "time-sync", Message: "Checking guest clocks against the host"})
			err := runOptionalIntegration(ctx, lifecycleTimeBudget(len(nodes)), func(step context.Context) error { return integrator.SyncGuestTime(step, nodes) })
			if err != nil {
				warnings = append(warnings, lifecycleWarning{Code: "time_sync", Message: "Guest clock synchronization incomplete", Detail: err.Error(), Next: clockWarningNext(nodes, err)})
			} else {
				report.Report(activity.Event{Phase: "time-sync", Message: "Guest clock check completed", Done: true})
			}
		}
	}
	if operationErr == nil && !noWait && refreshGuests && ctx.Err() == nil {
		report.Report(activity.Event{Phase: "guest-metadata", Message: "Updating guest hostnames and control-node SSH configuration"})
		err := runOptionalIntegration(ctx, lifecycleGuestTimeout, integrator.RefreshGuestMetadata)
		if err != nil {
			message := "Guest hostname/control SSH refresh incomplete; fixed IPs remain available"
			if errors.Is(err, context.DeadlineExceeded) {
				message = "Guest hostname/control SSH refresh timed out; fixed IPs remain available"
			}
			warnings = append(warnings, lifecycleWarning{Code: "guest_metadata", Message: message, Detail: err.Error(), Next: retry})
		} else {
			report.Report(activity.Event{Phase: "guest-metadata", Message: "Guest hostname and SSH configuration is up to date", Done: true})
		}
	}
	if command != "status" && ctx.Err() == nil {
		level, message := "info", status.Message
		if operationErr != nil {
			level, message = "error", operationErr.Error()
		} else if len(warnings) != 0 {
			level = "warn"
		}
		for _, warning := range warnings {
			message += "; " + warning.Message + ": " + warning.Detail
		}
		report.Report(activity.Event{Phase: "event-log", Message: "Saving the operation result"})
		err := runOptionalIntegration(ctx, lifecycleEventTimeout, func(step context.Context) error {
			return integrator.RecordEvent(step, command, level, message)
		})
		if err != nil {
			warnings = append(warnings, lifecycleWarning{Code: "event_log", Message: "Could not save the diagnostic event", Detail: err.Error()})
		}
	}
	return sshResult, warnings
}

func lifecycleTimeBudget(nodes int) time.Duration {
	waves := (max(1, nodes) + 3) / 4
	budget := max(lifecycleTimeTimeout, time.Duration(waves)*15*time.Second)
	return min(budget, 2*time.Minute)
}

func clockWarningNext(nodes []string, err error) string {
	var partial *privatevm.PartialError
	if errors.As(err, &partial) && len(partial.Failures) > 0 {
		failed := make([]string, 0, len(partial.Failures))
		for _, failure := range partial.Failures {
			if failure.Node != "" {
				failed = append(failed, failure.Node)
			}
		}
		if len(failed) > 0 {
			return "barn time sync " + strings.Join(failed, " ")
		}
	}
	return "barn time sync " + strings.Join(nodes, " ")
}

func runOptionalIntegration(parent context.Context, timeout time.Duration, run func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	err := run(ctx)
	// CommandContext may report a killed process instead of the deadline.
	// Retain both the useful timeout classification and any original detail.
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), err)
	}
	return err
}
