package main

import (
	"bytes"
	"strings"
	"testing"

	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/pgsty/barn/internal/state"
)

func TestLimitedLifecycleGroupsWarningsAndReportsRepair(t *testing.T) {
	var out bytes.Buffer
	status := privatevm.Status{Message: "already running"}
	for _, name := range []string{"meta", "node-1"} {
		status.Nodes = append(status.Nodes, privatevm.NodeStatus{Name: name, State: state.Running, Ready: true, Warnings: []state.GuestWarning{{Stage: "shares", Detail: "/shared: read-only"}}})
	}
	status.Nodes[0].Repairs = []string{"SSH port 2222 busy; using 12222"}
	printLifecycleResult(&out, "up", lifecycleResult{Status: status}, false)
	for _, want := range []string{"2 nodes ready · 2 with limitations", "meta, node-1: shared directories: /shared: read-only", "SSH port 2222 busy; using 12222", "barn up meta"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "unchanged") || strings.Count(out.String(), "/shared: read-only") != 1 {
		t.Fatalf("noisy or misleading output: %s", out.String())
	}
}

func TestRepairIsNotAUserCommand(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand(&out, &out)
	for _, command := range root.Commands() {
		if command.Name() == "repair" {
			t.Fatal("repair must be handled by up")
		}
	}
}

func TestDiskResetIsDisclosedWithoutClaimingUnchangedOrLimited(t *testing.T) {
	var out bytes.Buffer
	status := privatevm.Status{Message: "already running", Nodes: []privatevm.NodeStatus{{Name: "meta", State: state.Running, Ready: true, Repairs: []string{"/data: reset to an empty ext4 filesystem; previous data discarded"}}}}
	printLifecycleResult(&out, "up", lifecycleResult{Status: status}, false)
	if !strings.Contains(out.String(), "previous data discarded") || strings.Contains(out.String(), "unchanged") || strings.Contains(out.String(), "limitations") {
		t.Fatal(out.String())
	}
}
