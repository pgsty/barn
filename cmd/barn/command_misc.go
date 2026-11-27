package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/doctor"
	"github.com/pgsty/barn/internal/version"
	"github.com/spf13/cobra"
)

type versionResult struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Built   string `json:"built"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func runVersionCommand() commandOutcome {
	result := versionResult{Name: "barn", Version: version.Version, Commit: version.Commit, Built: version.Date, OS: runtime.GOOS, Arch: runtime.GOARCH}
	return commandOutcome{
		payload: result,
		text: func(writer, _ io.Writer) error {
			_, err := fmt.Fprintf(writer, "barn %s (commit %s, built %s, %s/%s)\n", result.Version, result.Commit, result.Built, result.OS, result.Arch)
			return err
		},
	}
}

// doctorCheckLabel is the word a person reads, chosen to match the consequence
// rather than the raw severity. A network finding is real, but it never changes
// the exit status, and printing "error" next to a successful exit is the kind of
// contradiction that teaches people to stop reading the output.
func doctorCheckLabel(check doctor.Check) string {
	if check.Class == doctor.ClassNetwork && check.Status == doctor.Error {
		return "blocked"
	}
	return string(check.Status)
}

func runDoctor(parent context.Context, stderr io.Writer) (commandOutcome, error) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	progressItem := startOperationProgress(ctx, stderr, "Checking host capabilities", "doctor")
	report := (doctor.Probe{Progress: progressItem.Report}).Run(ctx)
	var reportErr error
	if report.HasErrors() {
		reportErr = errors.New("host compute capability is not ready")
	}
	if ctx.Err() != nil {
		reportErr = ctx.Err()
	}
	progressItem.Stop(reportErr)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	renderText := func(stdout, _ io.Writer) error {
		if !lifecycleProgressFrozen(stdout) {
			rows := make([][]string, 0, len(report.Checks))
			for _, check := range report.Checks {
				rows = append(rows, []string{check.Name, doctorCheckLabel(check), check.Evidence})
			}
			printTable(stdout, []string{"ITEM", "STATE", "MESSAGE"}, rows, 1)
		}
		for _, check := range report.Checks {
			if check.Status != doctor.OK && check.Evidence != "" {
				textField(stdout, 5, "detail", check.Name+": "+check.Evidence)
			}
			if check.Fix != "" {
				textField(stdout, 5, "fix", check.Name+": "+check.Fix)
			}
		}
		if report.HasErrors() {
			printResultSummary(stdout, "doctor", "failed", "this host cannot run Barn guests until the errors above are resolved")
		} else if !lifecycleProgressFrozen(stdout) {
			printResultSummary(stdout, "doctor", "ready", "host compute capability is ready")
		}
		if !report.NetworkReady() {
			warningf(stdout, "the Barn network is not ready; run barn setup to prepare it")
		}
		return nil
	}
	if ctx.Err() != nil {
		return commandOutcome{}, newCommandError(exitRuntime, ctx.Err()).withPayload(report).withText(renderText)
	}
	if report.HasErrors() {
		return commandOutcome{}, newCommandError(exitCapability, errors.New("host compute capability is not ready")).withPayload(report).withText(renderText).quiet()
	}
	return commandOutcome{payload: report, text: renderText}, nil
}

func newCompletionCommand(root *cobra.Command, stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion",
		Long: `Generate a shell completion script containing commands, scoped flags,
templates, image aliases, and best-effort node-name completion.`,
		Example: `  source <(barn completion bash)
  barn completion zsh > "${fpath[1]}/_barn"
  barn completion fish > ~/.config/fish/completions/barn.fish`,
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(command *cobra.Command, arguments []string) error {
			var script bytes.Buffer
			var err error
			switch arguments[0] {
			case "bash":
				err = root.GenBashCompletionV2(&script, true)
			case "zsh":
				err = root.GenZshCompletion(&script)
			case "fish":
				err = root.GenFishCompletion(&script, true)
			case "powershell":
				err = root.GenPowerShellCompletion(&script)
			default:
				return newUsageError(fmt.Errorf("unsupported completion shell %q; expected bash, zsh, fish, or powershell", arguments[0]))
			}
			if err != nil {
				return newRuntimeError(fmt.Errorf("generate %s completion: %w", arguments[0], err))
			}
			result := struct {
				Shell  string `json:"shell"`
				Script string `json:"script"`
			}{Shell: arguments[0], Script: script.String()}
			return collectCommandOutcome(command.Context(), commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
				_, err := io.Copy(stdout, strings.NewReader(result.Script))
				return err
			}})
		},
	}
	return command
}
