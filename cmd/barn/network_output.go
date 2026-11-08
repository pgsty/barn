package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/hostconfig"
	darwinnet "github.com/pgsty/barn/internal/network/darwin"
	linuxnet "github.com/pgsty/barn/internal/network/linux"
)

// errNetworkNotInstalled is a fresh macOS host asked to install the network
// directly: only setup knows where socket_vmnet comes from.
var errNetworkNotInstalled error = failure.New(failure.Capability, errors.New("the Barn network is not installed")).Because("network_absent").Then("barn setup")

// Plans retain their reviewed paths and commands. Only applied operations
// freeze live rows; a reviewed plan must never look like an applied change.
func networkStep(ctx, parent context.Context, stderr io.Writer, command string, apply bool, step func(activity.Reporter) (commandOutcome, bool, error)) (commandOutcome, bool, error) {
	message := "Checking the Barn network plan"
	if apply {
		message = map[string]string{"install": "Installing the Barn network", "uninstall": "Removing the Barn network"}[command]
	}
	progressItem := startOperationProgress(ctx, stderr, message, "network "+command)
	var reporter activity.Reporter
	if apply {
		reporter = progressItem.Report
	}
	outcome, pending, err := step(reporter)
	if parent.Err() != nil && err == nil {
		err = parent.Err()
	}
	progressItem.Stop(err)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, false, ErrCancelled
	}
	if err != nil {
		return commandOutcome{}, false, newCommandError(exitRuntime, err)
	}
	return outcome, pending, nil
}

// planStatus is the one word every host-change plan ends with.
func planStatus(applied, pending bool) string {
	switch {
	case applied:
		return "applied"
	case pending:
		return "planned"
	default:
		return "ready"
	}
}

func darwinInstallOutcome(report darwinnet.InstallReport, next string) commandOutcome {
	return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
		if lifecycleProgressFrozen(stdout) {
			return nil
		}
		pending := report.Action != "none" && !report.Applied
		printItemResult(stdout, "network", planStatus(report.Applied, pending), fmt.Sprintf("%s (vmnet %s mode)", report.Plan.State.CIDR, report.Plan.State.Mode))
		if pending || report.Applied {
			for _, path := range sortedMapKeys(report.Targets) {
				textField(stdout, 9, "install", path+"  "+report.Targets[path])
			}
		}
		if pending && next != "" {
			textField(stdout, 9, "next", next)
		}
		return nil
	}}
}

func linuxInstallOutcome(report linuxnet.InstallReport, cidr, next string) commandOutcome {
	return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
		if lifecycleProgressFrozen(stdout) {
			return nil
		}
		printItemResult(stdout, "network", planStatus(report.Applied, !report.Applied), cidr+" (barn0 bridge)")
		for _, directory := range report.Plan.Directories {
			textField(stdout, 9, "install", fmt.Sprintf("%s/  %s %s", strings.TrimSuffix(directory.Path, "/"), directory.Owner, directory.Mode))
		}
		for _, file := range report.Plan.Files {
			textField(stdout, 9, "install", fmt.Sprintf("%s  %s %s", file.Path, file.Owner, file.Mode))
		}
		for _, phase := range report.Plan.Phases {
			for _, action := range phase.Commands {
				textField(stdout, 9, "run", execx.Display(action.Binary, action.Args...))
			}
		}
		if !report.Applied && next != "" {
			textField(stdout, 9, "next", next)
		}
		return nil
	}}
}

func networkRemovalOutcome(report any, files, directories []string, applied bool, next string) commandOutcome {
	return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
		if lifecycleProgressFrozen(stdout) {
			return nil
		}
		state, message := "planned", "remove the Barn network"
		if applied {
			state, message = "removed", "Barn network removed"
		}
		printItemResult(stdout, "network", state, message)
		for _, path := range files {
			textField(stdout, 9, "remove", path)
		}
		for _, path := range directories {
			textField(stdout, 9, "remove", strings.TrimSuffix(path, "/")+"/")
		}
		if !applied && next != "" {
			textField(stdout, 9, "next", next)
		}
		return nil
	}}
}

func hostsOutcome(report hostconfig.Report, next string) commandOutcome {
	return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
		pending := report.Plan.Changed && !report.Applied
		printItemResult(stdout, report.Plan.Target, planStatus(report.Applied, pending), report.Plan.Action+" managed host entries")
		for _, line := range report.Plan.Lines {
			bestEffortln(stdout, hostPlanLine(stdout, line))
		}
		if pending && next != "" {
			textField(stdout, 9, "next", next)
		}
		return nil
	}}
}

func hostPlanLine(writer io.Writer, line string) string {
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]
	if trimmed == "" {
		return line
	}
	style := ""
	switch trimmed[0] {
	case '+':
		style = ansiGreen
	case '-':
		style = ansiRed
	}
	if style == "" {
		return line
	}
	return indent + styled(writer, style, trimmed)
}

// confirmPlan asks, on a terminal, whether to apply the plan just shown.
// Enter takes the default only on an answered prompt; end of input (Ctrl-D, a
// closed pipe) cancels because nobody agreed to anything.
func confirmPlan(question string, defaultYes bool, stdin io.Reader, stderr io.Writer) error {
	resume := suspendProgress(stderr)
	defer resume()
	if _, err := fmt.Fprint(stderr, question); err != nil {
		return fmt.Errorf("write confirmation prompt: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(stdin, 64)).ReadString('\n')
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: no answer was entered; nothing was changed", ErrCancelled)
	}
	if err != nil {
		return err
	}
	switch answer := strings.ToLower(strings.TrimSpace(line)); {
	case answer == "y" || answer == "yes" || answer == "" && defaultYes:
		return nil
	default:
		return fmt.Errorf("%w: nothing was changed", ErrCancelled)
	}
}
