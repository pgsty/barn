package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/platform"
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
	for _, item := range []struct {
		action string
		short  string
		run    func(context.Context, string) (commandOutcome, error)
	}{
		{"scan", "Report tracked, missing, untracked, and unsafe qcow2 files", func(_ context.Context, path string) (commandOutcome, error) { return runRepoScan(path) }},
		{"build", "Generate catalog.json without changing repo.yaml or qcow2 files", func(ctx context.Context, path string) (commandOutcome, error) { return runRepoBuild(ctx, path, stderr) }},
		{"verify", "Verify catalog.json exactly matches repo.yaml and image bytes", func(ctx context.Context, path string) (commandOutcome, error) {
			return runRepoVerify(ctx, path, stderr)
		}},
	} {
		command := &cobra.Command{
			Use:     item.action + " <root>",
			Short:   item.short,
			Long:    item.short + ".\nThe repository root holds repo.yaml, catalog.json, and a flat images/\ndirectory.",
			Example: "  barn repo " + item.action + " /srv/barn",
			Args:    cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, arguments []string) error {
				outcome, err := item.run(command.Context(), arguments[0])
				if err != nil {
					return err
				}
				return collectCommandOutcome(command.Context(), outcome)
			},
		}
		parent.AddCommand(command)
	}
	return parent
}

func runRepoScan(path string) (commandOutcome, error) {
	root, err := fsutil.UserPath(path, "")
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	report, scanErr := image.ScanRepository(root)
	if scanErr != nil {
		return commandOutcome{}, newCommandError(exitRuntime, scanErr)
	}
	return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
		printRepoScan(stdout, report, false)
		return nil
	}}, nil
}

func runRepoBuild(parent context.Context, path string, stderr io.Writer) (commandOutcome, error) {
	return runRepoBuildOrVerify(parent, false, path, stderr)
}

func runRepoVerify(parent context.Context, path string, stderr io.Writer) (commandOutcome, error) {
	return runRepoBuildOrVerify(parent, true, path, stderr)
}

func runRepoBuildOrVerify(parent context.Context, verify bool, path string, stderr io.Writer) (commandOutcome, error) {
	root, err := fsutil.UserPath(path, "")
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		return commandOutcome{}, newCapabilityError(platform.QEMUMissing("qemu-img", err))
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	operation, phase, fallback := "repo build", "created", exitRuntime
	if verify {
		operation, phase, fallback = "repo verify", "verified", exitIntegrity
	}
	progressItem := startOperationProgress(ctx, stderr, "Checking repository artifacts", operation)
	builder := image.RepoBuilder{QEMUImg: qemuImg, Runner: execx.OSRunner{Timeout: 10 * time.Minute, OutputLimit: 1 << 20}, Progress: progressItem.Report}
	var result image.RepoBuildResult
	if verify {
		result, err = builder.Verify(ctx, root)
	} else {
		result, err = builder.Build(ctx, root)
	}
	finishProgressItem(progressItem, "catalog.json", phase, fmt.Sprintf("revision %d, %d images", result.Catalog.Version, len(result.Catalog.Images)), err)
	if err != nil {
		return commandOutcome{}, newCommandError(fallback, err)
	}
	return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
		if !lifecycleProgressFrozen(stdout) {
			printRepoScan(stdout, result.Scan, true)
			printItemResult(stdout, "catalog.json", phase, fmt.Sprintf("revision %d", result.Catalog.Version))
		}
		textField(stdout, 12, "catalog", result.Path)
		textField(stdout, 12, "revision", result.Catalog.Version)
		textField(stdout, 12, "images", len(result.Catalog.Images))
		textField(stdout, 12, "bytes", result.Bytes)
		return nil
	}}, nil
}
