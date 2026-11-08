package main

import (
	"fmt"
	"io"
	"sort"

	"github.com/pgsty/barn/internal/image"
)

func printRepoScan(out io.Writer, report image.RepoScanReport, verified bool) {
	states := make(map[string]string)
	for _, group := range []struct {
		names []string
		state string
	}{
		{report.Tracked, "tracked"}, {report.Untracked, "untracked"}, {report.Missing, "missing"}, {report.Unsafe, "unsafe"},
	} {
		for _, name := range group.names {
			states[name] = group.state
		}
	}
	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([][]string, 0, len(names))
	for _, name := range names {
		state := states[name]
		message := map[string]string{"tracked": "referenced by repo.yaml", "missing": "referenced file is missing", "untracked": "not referenced by repo.yaml", "unsafe": "not a safe regular qcow2 file"}[state]
		if verified && state == "tracked" {
			state, message = "verified", "SHA-256 and qcow2 verified"
		}
		rows = append(rows, []string{name, state, message})
	}
	printTable(out, []string{"IMAGE", "STATE", "MESSAGE"}, rows, 1)
	textField(out, 10, "root", report.Root)
	textField(out, 10, "scan", fmt.Sprintf("%d tracked · %d missing · %d untracked · %d unsafe", len(report.Tracked), len(report.Missing), len(report.Untracked), len(report.Unsafe)))
}
