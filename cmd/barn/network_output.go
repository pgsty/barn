package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/hostconfig"
	darwinnet "github.com/pgsty/barn/internal/network/darwin"
	linuxnet "github.com/pgsty/barn/internal/network/linux"
)

// errNetworkNotInstalled is a fresh macOS host asked to install the network
// directly: only setup knows where socket_vmnet comes from.
var errNetworkNotInstalled error = failure.New(failure.Capability, errors.New("the Barn network is not installed")).Because("network_absent").Then("barn setup")

// networkStep runs one network plan or apply under a progress line and turns a
// failure into a command error; classified causes keep their own class.
func networkStep(ctx, parent context.Context, stderr io.Writer, message string, step func() (commandOutcome, bool, error)) (commandOutcome, bool, error) {
	progressItem := startProgress(ctx, stderr, message)
	outcome, pending, err := step()
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
			bestEffortln(stdout, "  "+line)
		}
		if pending && next != "" {
			textField(stdout, 9, "next", next)
		}
		return nil
	}}
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
