package main

import (
	"context"
	"errors"
	"io"

	"github.com/pgsty/barn/internal/activity"
	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/pgsty/barn/internal/spec"
)

func startImageProgress(ctx context.Context, stderr io.Writer, action, name string) *progress {
	item := startOperationProgress(ctx, stderr, "Preparing image operation", "image "+action)
	item.Report(activity.Event{Phase: "image-resolve", Item: name, State: "checking", Message: "checking image metadata"})
	return item
}

func finishProgressItem(item *progress, name, state, message string, err error) {
	if item == nil {
		return
	}
	if err != nil {
		state, message = "failed", err.Error()
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrCancelled) {
			state = "cancelled"
		}
	}
	item.Report(activity.Event{Item: name, State: state, Message: message, Done: true})
	item.Stop(err)
}

func printItemResult(out io.Writer, name, state, message string) {
	if !lifecycleProgressFrozen(out) {
		printTable(out, []string{"ITEM", "STATE", "MESSAGE"}, [][]string{{name, state, message}}, 1)
	}
}

func printPlanTable(out io.Writer, plan privatevm.LifecyclePlan, resolved spec.Resolved) {
	actions := make(map[string]string)
	for _, group := range []struct {
		names  []string
		action string
	}{
		{plan.Start, "start"}, {plan.Create, "create"}, {plan.Resize, "resize"}, {plan.Recreate, "recreate"}, {plan.Missing, "missing"},
	} {
		for _, name := range group.names {
			actions[name] = group.action
		}
	}
	addresses := make(map[string]string)
	for _, node := range resolved.Nodes {
		addresses[node.Name] = node.Address
	}
	rows := make([][]string, 0, len(plan.Nodes)+len(plan.Missing))
	seen := make(map[string]bool)
	for _, name := range append(append([]string(nil), plan.Nodes...), plan.Missing...) {
		if seen[name] {
			continue
		}
		seen[name] = true
		state, message := "unchanged", "matches the applied inventory"
		action := actions[name]
		if action == "" && plan.Action == "repair" {
			action = "repair"
		}
		if action != "" {
			state, message = "planned", "would "+action
		}
		if action == "missing" {
			state, message = "blocked", "not in inventory; explicit destroy required"
		}
		if plan.Blocked != "" {
			state, message = "blocked", plan.Blocked
		}
		address := addresses[name]
		if address == "" {
			address = "—"
		}
		rows = append(rows, []string{name, address, state, message})
	}
	printTable(out, []string{"NAME", "ADDRESS", "STATE", "MESSAGE"}, rows, 2)
}
