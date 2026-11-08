package macvm

import (
	"context"
	"errors"
	"strings"

	"github.com/pgsty/barn/internal/activity"
)

func macActivityState(phase string) string {
	switch phase {
	case "stop":
		return "stopping"
	case "guest-ready":
		return "waiting"
	case "image-download":
		return "downloading"
	case "image-verify":
		return "verifying"
	case "image-restore":
		return "restoring"
	case "network":
		return "network"
	default:
		return "preparing"
	}
}

func (m *Manager) queueNodes(names []string) {
	for _, name := range names {
		address := ""
		if m.Store != nil {
			// Progress must not run recovery before the operation acquires its locks.
			if machine, err := m.Store.loadMachine(name); err == nil {
				address = machine.Network.Address
			}
		}
		m.Report.Report(activity.Event{Node: name, Address: address, State: "queued", Message: "waiting for operation"})
	}
}

// Every concurrent machine owns its reporter and native progress decoder.
// Copying the manager keeps another machine's output out of this row.
func (m *Manager) nodeProgress(name string) *Manager {
	child := *m
	child.Report = func(event activity.Event) {
		if event.Item != "" && event.State != "failed" && event.State != "cancelled" {
			event.State = macActivityState(event.Phase)
		}
		event.Node, event.Item = name, ""
		if event.State == "" {
			event.State = macActivityState(event.Phase)
		}
		event.Done = false // Native stages do not finish the whole operation.
		m.Report.Report(event)
	}
	if _, native := m.Runner.Progress.(*progressWriter); native || m.Runner.Progress == nil {
		child.Runner.Progress = NewProgressWriter(child.Report)
	}
	return &child
}

func (m *Manager) finishNode(name string, outcome Outcome, err error) {
	phase, message := outcome.State, strings.ReplaceAll(outcome.Action, "_", " ")
	if outcome.Ready {
		phase, message = "ready", "SSH ready: barn mac ssh "+name
	} else if phase == "running" {
		phase, message = "started", "VM running; SSH readiness not checked"
	}
	switch outcome.Action {
	case "destroyed":
		phase, message = "removed", "VM and disks removed; shared base retained"
	case "absent":
		phase, message = "removed", "already absent"
	case "stopped", "powered_off", "already_stopped":
		phase = "stopped"
		if outcome.Forced {
			message = "powered off after normal shutdown did not finish"
		}
	}
	if err != nil {
		phase, message = "failed", err.Error()
		if errors.Is(err, context.Canceled) {
			phase = "cancelled"
		}
	}
	m.Report.Report(activity.Event{Node: name, Address: outcome.Address, State: phase, Message: message, Done: true})
}
