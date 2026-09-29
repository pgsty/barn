package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/pgsty/barn/internal/diagnostics"
	"github.com/pgsty/barn/internal/identity"
	"github.com/pgsty/barn/internal/state"
)

type operationContextKey struct{}

func operationContext(parent context.Context) (context.Context, string, error) {
	if id, ok := parent.Value(operationContextKey{}).(string); ok {
		return parent, id, nil
	}
	id, err := identity.NewUUID()
	if err != nil {
		return parent, "", err
	}
	return context.WithValue(parent, operationContextKey{}, id), id, nil
}

// Reuse the bounded event log before there is a deployment. Record phases and
// classifications only: setup argv, proxy URLs and authentication data do not
// belong in an automatically persisted trace.
func recordOperationPhase(parent context.Context, id, action, phase string, failure error, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), lifecycleEventTimeout)
	defer cancel()
	root, err := state.ResolveDataRoot()
	if err == nil {
		err = (state.Store{Root: root}).EnsureRoot()
	}
	if err == nil {
		level, message := "info", ""
		if failure != nil {
			level, message = "error", "operation failed; command output contains the cause"
			var typed typedCommandError
			if errors.As(failure, &typed) {
				message = fmt.Sprintf("exit_code=%d category=%s", typed.exitCode(), typed.commandFailure().Error)
			}
			if errors.Is(failure, ErrCancelled) || errors.Is(failure, context.Canceled) {
				level, phase, message = "warn", "cancelled", "interrupted"
			}
		}
		err = diagnostics.AppendEvent(ctx, filepath.Join(root, "events.jsonl"), diagnostics.Event{
			Schema: 1, Time: time.Now().UTC(), Level: level, Node: "deployment",
			OperationID: id, Action: action, Phase: phase, Message: message,
		})
	}
	if err != nil {
		debugf(stderr, "operation_id=%s: could not save %s/%s event: %v", id, action, phase, err)
	}
}
