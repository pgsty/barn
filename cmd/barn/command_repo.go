package main

import (
	"io"

	"github.com/spf13/cobra"
)

func newRepoCommand(stdout, stderr io.Writer) *cobra.Command {
	parent := subcommandGroup(
		"repo",
		"Build and verify a static Barn artifact repository",
		`Use repo.yaml as the human-maintained source and materialize a strict
catalog.json from the referenced flat images/ qcow2 artifacts. Scan is
read-only; build never changes repo.yaml or image bytes.`,
		`  barn repo scan /srv/barn
  barn repo build /srv/barn
  barn repo verify /srv/barn`,
		stdout, stderr,
	)
	for _, action := range []string{"scan", "build", "verify"} {
		action := action
		options := imageOptions{Action: "repo-" + action}
		short := map[string]string{
			"scan":   "Report tracked, missing, untracked, and unsafe qcow2 files",
			"build":  "Generate catalog.json without changing repo.yaml or qcow2 files",
			"verify": "Verify catalog.json exactly matches repo.yaml and image bytes",
		}[action]
		command := &cobra.Command{
			Use:     action + " <root>",
			Short:   short,
			Long:    short + ".\nThe repository root holds repo.yaml, catalog.json, and a flat images/\ndirectory.",
			Example: "  barn repo " + action + " /srv/barn",
			Args:    cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, arguments []string) error {
				options.Path = arguments[0]
				return collectImageCommand(command, options, stdout, stderr)
			},
		}
		parent.AddCommand(command)
	}
	return parent
}
