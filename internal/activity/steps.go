package activity

import (
	"context"
	"errors"
)

// Steps reports sequential work. Each operation owns its tracker; completed
// stages remain visible when a later stage fails. It never runs the work.
type Steps struct {
	Reporter Reporter
	Phase    string
	active   Event
}

func (steps *Steps) Start(name, state, message string) {
	steps.active = Event{Phase: steps.Phase, Item: name, State: state, Message: message}
	steps.Reporter.Report(steps.active)
}

func (steps *Steps) Done(state, message string) {
	steps.active.State, steps.active.Message, steps.active.Done = state, message, true
	steps.Reporter.Report(steps.active)
}

// Fail is called after cleanup, so rollback or cleanup errors remain visible.
func (steps *Steps) Fail(err error) {
	if err == nil {
		return
	}
	state := "failed"
	if errors.Is(err, context.Canceled) {
		state = "cancelled"
	}
	steps.Done(state, err.Error())
}
