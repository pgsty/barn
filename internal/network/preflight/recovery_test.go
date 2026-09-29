package preflight

import (
	"net/netip"
	"testing"

	"github.com/pgsty/barn/internal/network/subnet"
)

func TestRepairRequiresIntactOwnedNetworkAndNoConflicts(t *testing.T) {
	request := Request{OS: "darwin", Arch: "arm64", Purpose: Use, Layout: subnet.Default()}
	snapshot := Snapshot{Installation: Installation{Status: "protected", Mode: "host", CIDR: request.Layout.CIDR(), HostAddress: request.Layout.HostAddress(), Interface: "bridge100", Problem: "socket absent"}}
	report := Evaluate(request, snapshot)
	if report.Ready || !report.CanRepair() {
		t.Fatalf("intact inactive service: %+v", report)
	}
	snapshot.Routes = []Route{{Prefix: netip.MustParsePrefix("10.10.10.0/24"), Interface: "utun9"}}
	if Evaluate(request, snapshot).CanRepair() {
		t.Fatal("foreign route was treated as repairable")
	}
	snapshot.Routes = nil
	for _, status := range []string{"partial", "absent", "mismatch"} {
		snapshot.Installation.Status = status
		if Evaluate(request, snapshot).CanRepair() {
			t.Fatalf("%s ownership allowed repair", status)
		}
	}
	snapshot.Installation.Status = "protected"
	snapshot.Installation.CIDR = "172.31.251.0/24"
	if Evaluate(request, snapshot).CanRepair() {
		t.Fatal("repair silently changed subnet")
	}
}
