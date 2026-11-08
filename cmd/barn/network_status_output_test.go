package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/doctor"
	netpreflight "github.com/pgsty/barn/internal/network/preflight"
)

func TestNetworkFindingsKeepEverySubjectAndFullRecoveryDetails(t *testing.T) {
	report := netpreflight.Report{CIDR: "10.10.10.0/24", Findings: []netpreflight.Finding{
		{Code: "route.overlap", Severity: netpreflight.Error, Subject: "vpn0", Evidence: "first conflicting route", Fix: "remove the first route"},
		{Code: "route.overlap", Severity: netpreflight.Error, Subject: "vpn1", Evidence: "second conflicting route", Fix: "remove the second route"},
		{Code: "route.overlap", Severity: netpreflight.Error, Subject: "vpn1", Evidence: "another conflicting route", Fix: "remove the other route"},
	}}
	checks := []doctor.Check{{Name: "bridge-helper", Status: doctor.Error, Evidence: "non-root attachment failed", Fix: "barn setup"}}
	rows := networkStatusRows(report, checks)
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row[0]] {
			t.Fatalf("live rows overwrite each other: %v", rows)
		}
		seen[row[0]] = true
	}
	var output bytes.Buffer
	writer := &outputWriter{Writer: &output, context: &outputContext{format: outputText, stdoutTTY: true, finalProgress: true, columns: 25}}
	if err := networkStatusOutcome("linux", "arm64", report, checks).text(writer, writer); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, "ITEM") {
		t.Fatalf("repeated frozen result table: %q", got)
	}
	for _, want := range []string{"first conflicting route", "second conflicting route", "another conflicting route", "non-root attachment failed", "remove the other route", "barn setup"} {
		if !strings.Contains(got, want) {
			t.Fatalf("narrow result lost %q: %s", want, got)
		}
	}
}
