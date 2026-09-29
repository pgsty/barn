package main

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"strings"

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
				err = root.GenBashCompletion(&script)
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
