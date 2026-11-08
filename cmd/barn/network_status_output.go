package main

import (
	"fmt"
	"io"

	"github.com/pgsty/barn/internal/doctor"
	netpreflight "github.com/pgsty/barn/internal/network/preflight"
)

func networkStatusRows(report netpreflight.Report, checks []doctor.Check) [][]string {
	state := "ready"
	for _, finding := range report.Findings {
		if finding.Severity == netpreflight.Warning {
			state = "warning"
		}
	}
	if !report.Ready {
		state = "failed"
	}
	rows := [][]string{{"network", state, fmt.Sprintf("%s (%s)", report.CIDR, installationWords(report.Installation.Status))}}
	seen := map[string]int{}
	for _, finding := range report.Findings {
		name := finding.Code
		if finding.Subject != "" {
			name += " " + finding.Subject
		}
		seen[name]++
		if seen[name] > 1 {
			name = fmt.Sprintf("%s #%d", name, seen[name])
		}
		rows = append(rows, []string{name, string(finding.Severity), finding.Evidence})
	}
	for _, check := range checks {
		rows = append(rows, []string{check.Name, string(check.Status), check.Evidence})
	}
	return rows
}

func networkStatusOutcome(osName, arch string, report netpreflight.Report, checks []doctor.Check) commandOutcome {
	result := struct {
		OS        string              `json:"os"`
		Arch      string              `json:"arch"`
		Preflight netpreflight.Report `json:"preflight"`
		Checks    []doctor.Check      `json:"checks,omitempty"`
	}{OS: osName, Arch: arch, Preflight: report, Checks: checks}
	return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
		if !lifecycleProgressFrozen(stdout) {
			printTable(stdout, []string{"ITEM", "STATE", "MESSAGE"}, networkStatusRows(report, checks), 1)
		}
		// Evidence and fixes must remain readable even when MESSAGE is hidden.
		for _, finding := range report.Findings {
			if finding.Severity == netpreflight.Error || finding.Severity == netpreflight.Warning {
				textField(stdout, 7, "detail", finding.Code+": "+finding.Evidence)
				if finding.Fix != "" {
					textField(stdout, 7, "fix", finding.Fix)
				}
			}
		}
		for _, check := range checks {
			if check.Status == doctor.Error || check.Status == doctor.Warn {
				textField(stdout, 7, "detail", check.Name+": "+check.Evidence)
				if check.Fix != "" {
					textField(stdout, 7, "fix", check.Fix)
				}
			}
		}
		return nil
	}}
}
