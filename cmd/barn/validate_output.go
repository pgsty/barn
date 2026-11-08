package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/spec"
)

func printValidationResult(out io.Writer, rows [][]string, elapsed time.Duration, valid bool) {
	printTable(out, []string{"ITEM", "STATE", "MESSAGE"}, rows, 1)
	completed, marker := len(rows), "[DONE]"
	if !valid {
		completed--
		marker = "[FAILED]"
	}
	warnings := 0
	for _, row := range rows {
		if len(row) > 1 && (row[1] == "warn" || row[1] == "warning") {
			warnings++
		}
	}
	detail := ""
	if warnings > 0 {
		detail = fmt.Sprintf(", %d warning(s)", warnings)
	}
	bestEffortf(out, "validate %d/%d checks: %s%s %s\n", completed, len(rows), elapsedLabel(elapsed), detail, styledCompletionMarker(out, marker))
}

// Validation resolves catalog references; it does not verify cached image
// bytes, and imported aliases deliberately defer their checks to barn up.
func validationImageRow(resolved spec.Resolved, warnings []string) []string {
	if len(warnings) > 0 {
		return []string{"images", "warning", strings.Join(warnings, "; ")}
	}
	var catalog, local []string
	seen := map[string]bool{}
	for _, node := range resolved.Nodes {
		reference := node.Image
		if reference == "" {
			reference = resolved.Image
		}
		if seen[reference] {
			continue
		}
		seen[reference] = true
		if image.IsLocalAlias(reference) {
			local = append(local, reference)
		} else {
			catalog = append(catalog, reference)
		}
	}
	state, message := "valid", fmt.Sprintf("%s catalog references: %s", lifecycleImageArch(resolved), strings.Join(catalog, ", "))
	if len(local) > 0 {
		deferred := "local images checked by barn up: " + strings.Join(local, ", ")
		if len(catalog) == 0 {
			state, message = "deferred", deferred
		} else {
			state, message = "partial", message+"; "+deferred
		}
	}
	return []string{"images", state, message}
}
