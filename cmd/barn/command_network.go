package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/doctor"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/linuxvm"
	darwinnet "github.com/pgsty/barn/internal/network/darwin"
	linuxnet "github.com/pgsty/barn/internal/network/linux"
	netpreflight "github.com/pgsty/barn/internal/network/preflight"
	"github.com/pgsty/barn/internal/network/subnet"
	setuphost "github.com/pgsty/barn/internal/setup"
	"github.com/spf13/cobra"
)

type darwinNetworkInstaller interface {
	InstallModeNetwork(context.Context, string, string, string, string, string, bool) (darwinnet.InstallReport, error)
}

func installDarwinNetwork(ctx context.Context, installer darwinNetworkInstaller, archive, interfaceID, arch, mode, cidr string, apply bool) (darwinnet.InstallReport, error) {
	return installer.InstallModeNetwork(ctx, archive, interfaceID, arch, mode, cidr, apply)
}

type networkInstallOptions struct {
	CIDR        string
	Mode        string
	Archive     string
	InterfaceID string
	Apply       bool
}

func newNetworkCommand(stdout, stderr io.Writer) *cobra.Command {
	parent := subcommandGroup(
		"network",
		"Inspect and manage the Barn network",
		`Inspect, install, or remove the one host-wide Barn network that gives
nodes their fixed IPs. On a terminal, install and uninstall show their
privileged plan and ask before applying it; --yes applies without asking, and
without a terminal they only print the plan.`,
		`  barn network status
  barn network install
  barn network install --yes
  barn network uninstall --yes`,
		stdout, stderr,
	)
	parent.Aliases = []string{"n", "net"}

	var statusCIDR string
	status := &cobra.Command{
		Use:     "status",
		Aliases: []string{"st"},
		Short:   "Inspect installed network state and readiness",
		Long: `Inspect the platform backend, installed CIDR, ownership, health, and the
read-only preflight findings used before lifecycle mutation.`,
		Example: `  barn network status
  barn network status --cidr 10.10.10.0/24
  barn --json network status`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			outcome, err := runNetworkStatus(command.Context(), statusCIDR, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	status.Flags().StringVarP(&statusCIDR, "cidr", "c", "", "expected network: an RFC1918 IPv4 /24")
	noFileCompletions(status, "cidr")
	parent.AddCommand(status)

	installOptions := networkInstallOptions{CIDR: subnet.DefaultCIDR, Mode: "host"}
	install := &cobra.Command{
		Use:     "install",
		Aliases: []string{"i"},
		Short:   "Install or check the Barn network",
		Long: `Plan or install the platform-native network backend. On macOS this is
socket_vmnet in host or shared mode; on Linux it is the barn0 bridge through
the active network manager. A fresh macOS host installs it with barn setup,
which also fetches socket_vmnet.`,
		Example: `  barn network install                      # review the plan, then confirm
  barn network install --yes                # apply the default 10.10.10.0/24 plan
  barn network install --mode shared --yes  # macOS shared vmnet mode`,
		Args: cobra.NoArgs,
		PreRunE: func(command *cobra.Command, _ []string) error {
			if err := validateChoice("--mode", installOptions.Mode, "host", "shared"); err != nil {
				return err
			}
			if runtime.GOOS == "linux" && installOptions.Mode != "host" {
				return fmt.Errorf("--mode is a macOS socket_vmnet option; Linux uses barn0")
			}
			if runtime.GOOS != "darwin" && (command.Flags().Changed("archive") || command.Flags().Changed("interface-id")) {
				return fmt.Errorf("--archive and --interface-id are macOS-only")
			}
			return nil
		},
		RunE: func(command *cobra.Command, _ []string) error {
			outcome, err := runNetworkInstall(command.Context(), installOptions, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	install.Flags().StringVarP(&installOptions.CIDR, "cidr", "c", installOptions.CIDR, "network: an RFC1918 IPv4 /24")
	install.Flags().StringVarP(&installOptions.Mode, "mode", "m", installOptions.Mode, "macOS vmnet mode: host or shared")
	install.Flags().StringVarP(&installOptions.Archive, "archive", "a", "", "macOS: pinned socket_vmnet archive")
	install.Flags().StringVarP(&installOptions.InterfaceID, "interface-id", "i", "", "macOS: persistent vmnet UUID")
	install.Flags().BoolVarP(&installOptions.Apply, "yes", "y", false, "apply the displayed privileged plan without confirmation (sudo may still ask for a password)")
	_ = install.RegisterFlagCompletionFunc("mode", enumFlagCompletion("host", "shared"))
	noFileCompletions(install, "cidr", "interface-id", "yes")
	parent.AddCommand(install)

	var uninstallApply bool
	uninstall := &cobra.Command{
		Use:     "uninstall",
		Aliases: []string{"u"},
		Short:   "Remove Barn-owned host networking",
		Long: `Plan or remove only Barn-owned network state and restore recorded host
settings. It stops while any node is running. On a terminal it asks before
removing; --yes removes without asking.`,
		Example: `  barn network uninstall        # review the removal plan, then confirm
  barn network uninstall --yes  # remove after the deployment is stopped`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			outcome, err := runNetworkUninstall(command.Context(), uninstallApply, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	uninstall.Flags().BoolVarP(&uninstallApply, "yes", "y", false, "apply the displayed privileged plan without confirmation (sudo may still ask for a password)")
	noFileCompletions(uninstall, "yes")
	parent.AddCommand(uninstall)
	return parent
}

func runNetworkInstall(parent context.Context, options networkInstallOptions, stderr io.Writer) (commandOutcome, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return commandOutcome{}, newCommandError(exitCapability, errors.New("network install/uninstall supports native Linux and macOS"))
	}
	vmnetMode := options.Mode
	if vmnetMode == "" {
		vmnetMode = "host"
	}
	if vmnetMode != "host" && vmnetMode != "shared" {
		return commandOutcome{}, newUsageError(fmt.Errorf("--mode must be host or shared, got %q", vmnetMode))
	}
	if runtime.GOOS == "linux" && vmnetMode != "host" {
		return commandOutcome{}, newUsageError(errors.New("--mode is a macOS socket_vmnet option; Linux uses barn0"))
	}
	if runtime.GOOS != "darwin" && (options.Archive != "" || options.InterfaceID != "") {
		return commandOutcome{}, newUsageError(errors.New("--archive and --interface-id are macOS-only"))
	}
	cidr := options.CIDR
	if cidr == "" {
		cidr = subnet.DefaultCIDR
	}
	layout, err := subnet.Parse(cidr)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	baseRunner := execx.OSRunner{Timeout: 2 * time.Minute, OutputLimit: 1 << 20}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	addresses := linuxvm.FilterRecordedAddresses(layout.StaticAddresses())
	preflightProgress := startProgress(ctx, stderr, "Checking the Barn network plan")
	preflightReport := netpreflight.Run(ctx, netpreflight.Request{OS: runtime.GOOS, Arch: runtime.GOARCH, Purpose: netpreflight.Install, Layout: layout, Addresses: addresses}, netpreflight.Probe{Runner: baseRunner})
	preflightProgress.Stop(nil)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	if !preflightReport.Ready {
		return commandOutcome{}, newCommandError(preflightReport.ExitCode, setupFindingError(preflightReport)).withPayload(preflightReport)
	}
	// A fresh macOS install needs a socket_vmnet source and a new interface
	// UUID, which only setup supplies. Say so before sudo.
	absent := preflightReport.Installation.Status == "" || preflightReport.Installation.Status == "absent"
	if runtime.GOOS == "darwin" && absent && (options.Archive == "" || options.InterfaceID == "") {
		return commandOutcome{}, newCommandError(exitCapability, errNetworkNotInstalled)
	}
	nextSuffix := ""
	if options.Archive != "" || options.InterfaceID != "" {
		nextSuffix = " with the same --archive and --interface-id"
	}
	return runNetworkMutation(ctx, parent, baseRunner, "install", options.Apply, nextSuffix, stderr, func(rootRunner execx.Runner, apply, warn bool, next string) (commandOutcome, bool, error) {
		if runtime.GOOS == "darwin" {
			executor := darwinnet.Executor{User: baseRunner, Root: rootRunner, InUse: linuxvm.DeploymentInUse}
			return networkStep(ctx, parent, stderr, "install", apply, func(reporter activity.Reporter) (commandOutcome, bool, error) {
				executor.Progress = reporter
				report, err := installDarwinNetwork(ctx, executor, options.Archive, options.InterfaceID, runtime.GOARCH, vmnetMode, layout.CIDR(), apply)
				if err == nil && warn {
					printWarnings(stderr, report.Warnings)
				}
				return darwinInstallOutcome(report, next), report.Action != "none" && !report.Applied, err
			})
		}
		executor := linuxnet.Executor{User: baseRunner, Root: rootRunner, InUse: linuxvm.DeploymentInUse}
		linuxConfig, configErr := linuxnet.ConfigForCIDR(layout.CIDR())
		if configErr != nil {
			return commandOutcome{}, false, newUsageError(configErr)
		}
		return networkStep(ctx, parent, stderr, "install", apply, func(reporter activity.Reporter) (commandOutcome, bool, error) {
			executor.Progress = reporter
			report, err := executor.InstallConfig(ctx, linuxConfig, apply)
			if err == nil && warn {
				printWarnings(stderr, report.Warnings)
			}
			return linuxInstallOutcome(report, layout.CIDR(), next), !report.Applied, err
		})
	})
}

func runNetworkUninstall(parent context.Context, apply bool, stderr io.Writer) (commandOutcome, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return commandOutcome{}, newCommandError(exitCapability, errors.New("network install/uninstall supports native Linux and macOS"))
	}
	baseRunner := execx.OSRunner{Timeout: 2 * time.Minute, OutputLimit: 1 << 20}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	return runNetworkMutation(ctx, parent, baseRunner, "uninstall", apply, "", stderr, func(rootRunner execx.Runner, doApply, warn bool, next string) (commandOutcome, bool, error) {
		if runtime.GOOS == "darwin" {
			executor := darwinnet.Executor{User: baseRunner, Root: rootRunner, InUse: linuxvm.DeploymentInUse}
			return networkStep(ctx, parent, stderr, "uninstall", doApply, func(reporter activity.Reporter) (commandOutcome, bool, error) {
				executor.Progress = reporter
				report, err := executor.Uninstall(ctx, doApply)
				if err == nil && warn && report.Recovered {
					warningf(stderr, "network.json was unavailable; ownership was confirmed from the installed interface, launchd plist, and binary digests")
				}
				return networkRemovalOutcome(report, report.RemoveFiles, report.RemoveDirs, report.Applied, next), !report.Applied, err
			})
		}
		executor := linuxnet.Executor{User: baseRunner, Root: rootRunner, InUse: linuxvm.DeploymentInUse}
		return networkStep(ctx, parent, stderr, "uninstall", doApply, func(reporter activity.Reporter) (commandOutcome, bool, error) {
			executor.Progress = reporter
			report, err := executor.Uninstall(ctx, doApply)
			return networkRemovalOutcome(report, report.Plan.RemoveFiles, report.Plan.RemoveDirectories, report.Applied, next), !report.Applied, err
		})
	})
}

func runNetworkMutation(
	ctx, parent context.Context,
	baseRunner execx.Runner,
	command string,
	apply bool,
	nextSuffix string,
	stderr io.Writer,
	execute func(rootRunner execx.Runner, apply, warn bool, next string) (commandOutcome, bool, error),
) (commandOutcome, error) {
	// A terminal without --yes reviews the plan and confirms once; a pipe
	// without --yes only prints the plan.
	confirm := !apply && interactiveTextSession(stderr) && !structuredOutput(stderr)
	next := ""
	if !confirm {
		next = "barn network " + command + " --yes" + nextSuffix
	}
	privilege := newSudoSession(baseRunner, stderr, "network command")
	defer privilege.Close()
	reason := "read the root-owned Barn network state for the plan"
	if apply || confirm {
		reason = map[string]string{"install": "install", "uninstall": "remove"}[command] + " the Barn network"
	}
	if err := privilege.Ensure(ctx, reason); err != nil {
		return commandOutcome{}, newCommandError(exitCapability, err)
	}
	rootRunner := setuphost.RootRunner(baseRunner)
	// In a confirmed run, warnings belong to the applied result only.
	outcome, pending, err := execute(rootRunner, apply, apply || !confirm, next)
	if err != nil || !confirm || !pending {
		return outcome, err
	}
	if err := outcome.text(stderr, stderr); err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	question, defaultYes := "Install the Barn network? [Y/n] ", true
	if command == "uninstall" {
		question, defaultYes = "Remove the Barn network? [y/N] ", false
	}
	if err := confirmPlan(question, defaultYes, os.Stdin, stderr); err != nil {
		if errors.Is(err, ErrCancelled) {
			return commandOutcome{}, err
		}
		return commandOutcome{}, newRuntimeError(err)
	}
	outcome, _, err = execute(rootRunner, true, true, next)
	return outcome, err
}

func runNetworkStatus(parent context.Context, requestedCIDR string, stderr io.Writer) (commandOutcome, error) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	cidr := requestedCIDR
	if cidr == "" {
		cidr = subnet.DefaultCIDR
	}
	layout, err := subnet.Parse(cidr)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	baseRunner := execx.OSRunner{Timeout: 5 * time.Second, OutputLimit: 1 << 20}
	progressItem := startOperationProgress(ctx, stderr, "Inspecting the host network", "network status")
	progressItem.Report(activity.Event{Phase: "network", Item: "network", State: "checking", Message: "checking installation, routes and interfaces"})
	preflightReport := netpreflight.Run(ctx, netpreflight.Request{OS: runtime.GOOS, Arch: runtime.GOARCH, Purpose: netpreflight.Inspect, Layout: layout}, netpreflight.Probe{Runner: baseRunner})
	installedReadable := preflightReport.Installation.Status == "exact" || preflightReport.Installation.Status == "protected"
	if requestedCIDR == "" && installedReadable && preflightReport.Installation.CIDR != "" && preflightReport.Installation.CIDR != layout.CIDR() {
		if installedLayout, installedErr := subnet.Parse(preflightReport.Installation.CIDR); installedErr == nil {
			preflightReport = netpreflight.Run(ctx, netpreflight.Request{OS: runtime.GOOS, Arch: runtime.GOARCH, Purpose: netpreflight.Inspect, Layout: installedLayout}, netpreflight.Probe{Runner: baseRunner})
		}
	}
	for _, row := range networkStatusRows(preflightReport, nil) {
		progressItem.Report(activity.Event{Phase: "network", Item: row[0], State: row[1], Message: row[2], Done: true})
	}
	doctorReport := (doctor.Probe{Progress: func(event activity.Event) {
		switch event.Item {
		case "kvm", "linux-family", "linux-network-owner", "bridge-helper":
			progressItem.Report(event)
		}
	}}).Run(ctx)
	checks := make([]doctor.Check, 0, 4)
	hasError := !preflightReport.Ready
	for _, check := range doctorReport.Checks {
		if check.Name == "kvm" || check.Name == "linux-family" || check.Name == "linux-network-owner" || check.Name == "bridge-helper" {
			checks = append(checks, check)
			if check.Status == doctor.Error {
				hasError = true
			}
		}
	}
	var statusErr error
	if hasError {
		statusErr = errors.New("network status is not ready")
	}
	if ctx.Err() != nil {
		statusErr = ctx.Err()
	}
	progressItem.Stop(statusErr)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	outcome := networkStatusOutcome(doctorReport.OS, doctorReport.Arch, preflightReport, checks)
	if hasError || statusErr != nil {
		code := exitCapability
		if preflightReport.ExitCode != 0 {
			code = preflightReport.ExitCode
		}
		boundary := newCommandError(code, statusErr).withPayload(outcome.payload).withText(outcome.text)
		if hasError {
			boundary = boundary.quiet()
		}
		return commandOutcome{}, boundary
	}
	return outcome, nil
}

// installationWords turns the preflight installation status into words.
func installationWords(status string) string {
	switch status {
	case "exact", "protected":
		return "installed"
	case "partial", "invalid":
		return "partially installed"
	default:
		return "not installed"
	}
}
