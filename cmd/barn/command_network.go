package main

import (
	"fmt"
	"io"
	"runtime"

	"github.com/pgsty/barn/internal/network/subnet"
	"github.com/spf13/cobra"
)

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

	statusOptions := networkOptions{Action: "status"}
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
			outcome, err := runNetwork(command.Context(), statusOptions, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	status.Flags().StringVarP(&statusOptions.CIDR, "cidr", "c", "", "expected network: an RFC1918 IPv4 /24")
	noFileCompletions(status, "cidr")
	parent.AddCommand(status)

	installOptions := networkOptions{Action: "install", CIDR: subnet.DefaultCIDR, Mode: "host"}
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
			outcome, err := runNetwork(command.Context(), installOptions, stderr)
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

	uninstallOptions := networkOptions{Action: "uninstall"}
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
			outcome, err := runNetwork(command.Context(), uninstallOptions, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	uninstall.Flags().BoolVarP(&uninstallOptions.Apply, "yes", "y", false, "apply the displayed privileged plan without confirmation (sudo may still ask for a password)")
	noFileCompletions(uninstall, "yes")
	parent.AddCommand(uninstall)
	return parent
}
