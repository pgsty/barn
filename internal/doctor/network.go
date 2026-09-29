package doctor

import (
	"context"
	"fmt"

	netpreflight "github.com/pgsty/barn/internal/network/preflight"
	"github.com/pgsty/barn/internal/network/subnet"
	"github.com/pgsty/barn/internal/platform"
)

func readableNetworkInstallation(status string) bool {
	return status == "exact" || status == "protected"
}

// Host diagnostics inspect the shared network, not every address in its pool.
// Lifecycle preflight checks the addresses of nodes it will actually start.
func networkPreflightChecks(ctx context.Context, profile platform.Profile, probe netpreflight.Probe) []Check {
	layout := subnet.Default()
	report := netpreflight.Run(ctx, netpreflight.Request{OS: profile.OS, Arch: profile.Arch, Purpose: netpreflight.Inspect, Layout: layout}, probe)
	if readableNetworkInstallation(report.Installation.Status) && report.Installation.CIDR != "" && report.Installation.CIDR != layout.CIDR() {
		if installed, err := subnet.Parse(report.Installation.CIDR); err == nil {
			report = netpreflight.Run(ctx, netpreflight.Request{OS: profile.OS, Arch: profile.Arch, Purpose: netpreflight.Inspect, Layout: installed}, probe)
		}
	}
	checks := make([]Check, 0, len(report.Findings)+1)
	if len(report.Findings) == 0 {
		checks = append(checks, Check{Name: "network-preflight", Status: OK, Evidence: fmt.Sprintf("%s ready (%s mode on %s)", report.CIDR, report.Installation.Mode, report.Installation.Interface)})
		return checks
	}
	for _, finding := range report.Findings {
		status := Warn
		if finding.Severity == netpreflight.Error {
			status = Error
		} else if finding.Severity == netpreflight.OK {
			status = OK
		}
		checks = append(checks, Check{Name: "network-" + finding.Code, Status: status, Evidence: finding.Evidence, Fix: finding.Fix})
	}
	return checks
}
