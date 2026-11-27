package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/linuxvm"
	"github.com/pgsty/barn/internal/sshconfig"
	"github.com/pgsty/barn/internal/state"
)

type integrationFixture struct {
	t     *testing.T
	calls []string
	guest func(context.Context) error
	level string
	event string
	clock func(context.Context, []string) error
}

func (f *integrationFixture) checkDeadline(ctx context.Context, action string, budget time.Duration) {
	f.t.Helper()
	f.calls = append(f.calls, action)
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > budget {
		f.t.Fatalf("%s missing its own live deadline: %v", action, deadline)
	}
}

func (f *integrationFixture) InstallSSHConfig(ctx context.Context, _, _ string) (sshconfig.Result, error) {
	f.checkDeadline(ctx, "ssh", lifecycleSSHTimeout)
	return sshconfig.Result{Action: "install"}, nil
}

func (f *integrationFixture) RemoveSSHConfig(_, _ string) (sshconfig.Result, error) {
	f.calls = append(f.calls, "remove")
	return sshconfig.Result{Action: "remove"}, nil
}

func (f *integrationFixture) RefreshGuestMetadata(ctx context.Context) error {
	f.checkDeadline(ctx, "guests", lifecycleGuestTimeout)
	if f.guest != nil {
		return f.guest(ctx)
	}
	return nil
}

func (f *integrationFixture) RecordEvent(ctx context.Context, _, level, message string) error {
	f.checkDeadline(ctx, "event", lifecycleEventTimeout)
	f.level, f.event = level, message
	return nil
}

func (f *integrationFixture) SyncGuestTime(ctx context.Context, nodes []string) error {
	f.checkDeadline(ctx, "time", lifecycleTimeBudget(len(nodes)))
	if f.clock != nil {
		return f.clock(ctx, nodes)
	}
	return nil
}

func TestClockIntegrationUsesOnlyReadySelectedNodesAndPreservesReadiness(t *testing.T) {
	fixture := &integrationFixture{t: t, clock: func(_ context.Context, nodes []string) error {
		if strings.Join(nodes, ",") != "meta" {
			t.Fatalf("time check escaped ready-node selection: %v", nodes)
		}
		return errors.New("clock response unavailable")
	}}
	status := linuxvm.Status{Nodes: []linuxvm.NodeStatus{
		{Name: "meta", State: state.Running, Runtime: "running", Ready: true},
		{Name: "pending", State: state.Running, Runtime: "running"},
	}}
	partial := &linuxvm.PartialError{Failures: []linuxvm.NodeFailure{{Node: "pending", Stage: "ready", Error: "SSH unavailable"}}}
	_, warnings := finishLifecycleIntegrations(context.Background(), "start", true, false, "barn start", status, partial, fixture, nil)
	if len(warnings) != 1 || warnings[0].Code != "time_sync" || warnings[0].Next != "barn time sync meta" || !status.Nodes[0].Ready {
		t.Fatalf("clock failure changed readiness or retry selection: %+v %+v", status, warnings)
	}
	if strings.Join(fixture.calls, ",") != "ssh,time,event" {
		t.Fatalf("integrations did not continue: %v", fixture.calls)
	}

	// Large deployments get a scaled clock budget and narrow Next to failed nodes.
	many := linuxvm.Status{}
	for _, name := range []string{"meta", "n1", "n2", "n3", "n4", "n5"} {
		many.Nodes = append(many.Nodes, linuxvm.NodeStatus{Name: name, State: state.Running, Runtime: "running", Ready: true})
	}
	if got := lifecycleTimeBudget(len(many.Nodes)); got != 30*time.Second {
		t.Fatalf("scaled time budget = %s, want 30s", got)
	}
	scaledFixture := &integrationFixture{t: t, clock: func(_ context.Context, _ []string) error {
		return &linuxvm.PartialError{Total: 6, Failures: []linuxvm.NodeFailure{{Node: "n4", Stage: "time-sync", Error: "timeout"}}}
	}}
	_, warnings = finishLifecycleIntegrations(context.Background(), "up", true, false, "barn up", many, nil, scaledFixture, nil)
	if len(warnings) != 1 || warnings[0].Next != "barn time sync n4" {
		t.Fatalf("partial clock failure next = %+v", warnings)
	}
}

func TestOptionalIntegrationTimeoutRetainsCauseAndParent(t *testing.T) {
	parent := context.Background()
	processErr := errors.New("SSH process killed")
	err := runOptionalIntegration(parent, 20*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return processErr
	})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, processErr) || parent.Err() != nil {
		t.Fatalf("timeout lost cause or cancelled the parent: %v", err)
	}
	called := false
	if err := runOptionalIntegration(parent, time.Second, func(context.Context) error { called = true; return nil }); err != nil || !called {
		t.Fatalf("later integration could not proceed: %v", err)
	}
}

func TestLifecycleIntegrationTimeoutKeepsReadyResultAndRecordsWarning(t *testing.T) {
	fixture := &integrationFixture{t: t, guest: func(context.Context) error { return context.DeadlineExceeded }}
	status := linuxvm.Status{Message: "already running", Nodes: []linuxvm.NodeStatus{{Name: "meta", State: state.Running, Runtime: "running", Ready: true}}}
	sshResult, warnings := finishLifecycleIntegrations(context.Background(), "up", true, false, "barn up -f 'my lab.yml'", status, nil, fixture, nil)
	result := lifecycleResult{Status: status, SSHConfig: sshResult, Warnings: warnings}
	if !allNodesReady(result.Status) || sshResult == nil || len(warnings) != 1 || warnings[0].Code != "guest_metadata" || !strings.Contains(warnings[0].Message, "timed out") || warnings[0].Next != "barn up -f 'my lab.yml'" {
		t.Fatalf("lost ready result or actionable warning: %#v", result)
	}
	if strings.Join(fixture.calls, ",") != "ssh,time,guests,event" || fixture.level != "warn" || !strings.Contains(fixture.event, "timed out") {
		t.Fatalf("later event did not record degraded success: %#v", fixture)
	}
}

func TestLifecycleIntegrationPolicyKeepsPartialAndNoWaitSemantics(t *testing.T) {
	partial := &linuxvm.PartialError{Failures: []linuxvm.NodeFailure{{Node: "node-1", Stage: "ready", Error: "SSH pending"}}}
	for _, test := range []struct {
		name, command, calls, level string
		hasNodes, noWait            bool
		err                         error
	}{
		{name: "partial", command: "up", hasNodes: true, err: partial, calls: "ssh,event", level: "error"},
		{name: "partial start", command: "start", hasNodes: true, err: partial, calls: "ssh,event", level: "error"},
		{name: "partial restart", command: "restart", hasNodes: true, err: partial, calls: "ssh,event", level: "error"},
		{name: "no wait", command: "up", hasNodes: true, noWait: true, calls: "ssh,event", level: "info"},
		{name: "start", command: "start", hasNodes: true, calls: "ssh,guests,event", level: "info"},
		{name: "remove peer", command: "destroy", hasNodes: true, calls: "ssh,guests,event", level: "info"},
		{name: "remove all", command: "destroy", calls: "remove,event", level: "info"},
		{name: "status", command: "status", hasNodes: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := &integrationFixture{t: t}
			_, warnings := finishLifecycleIntegrations(context.Background(), test.command, test.hasNodes, test.noWait, "barn up", linuxvm.Status{}, test.err, fixture, nil)
			if strings.Join(fixture.calls, ",") != test.calls || fixture.level != test.level || len(warnings) != 0 {
				t.Fatalf("integration policy changed: %#v, warnings=%v", fixture, warnings)
			}
		})
	}
}

func TestLifecycleIntegrationsStopOnUserCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := &integrationFixture{t: t, guest: func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	}}
	_, _ = finishLifecycleIntegrations(ctx, "up", true, false, "barn up", linuxvm.Status{}, nil, fixture, nil)
	if ctx.Err() != context.Canceled || strings.Join(fixture.calls, ",") != "ssh,guests" {
		t.Fatalf("ignored user cancellation: %#v", fixture)
	}
}
