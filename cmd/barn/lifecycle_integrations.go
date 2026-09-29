package main

import (
	"context"
	"errors"
	"time"

	"github.com/pgsty/barn/internal/activity"
	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/pgsty/barn/internal/sshconfig"
)

const (
	lifecycleSSHTimeout   = 2 * time.Second
	lifecycleGuestTimeout = 5 * time.Second
	lifecycleEventTimeout = time.Second
)

type lifecycleIntegrator interface {
	sshConfigReconciler
	RefreshGuestMetadata(context.Context) error
	RecordEvent(context.Context, string, string, string) error
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
