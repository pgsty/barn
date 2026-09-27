package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pgsty/farrow/internal/macvm"
	"github.com/pgsty/farrow/internal/state"
	"github.com/spf13/cobra"
)

func macManager(stderr io.Writer, require bool) (*macvm.Manager, error) {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return nil, err
	}
	store, err := macvm.NewStore(root)
	if err != nil {
		return nil, err
	}
	manager := &macvm.Manager{Store: store, Progress: macProgressOutput(stderr)}
	binary, err := macvm.FindRunner()
	if require && err != nil {
		return nil, err
	}
	manager.Runner = macvm.Runner{Binary: binary, Progress: manager.Progress}
	return manager, nil
}

func macTarget(args []string) (string, error) {
	if len(args) > 1 {
		return "", errors.New("select only mac1 or mac2")
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	return macvm.NormalizeSlot(name)
}
func macTargets(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return []string{"mac1", "mac2"}, cobra.ShellCompDirectiveNoFileComp
}

// Keep command errors (including remote exit payloads) intact at the shared
// boundary. Cobra validates arguments and flags before discovering the runner.
func macRun(stderr io.Writer, requireRunner bool, action func(*cobra.Command, *macvm.Manager) (commandOutcome, error)) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		m, err := macManager(stderr, requireRunner)
		if err != nil {
			return newRuntimeError(err)
		}
		outcome, err := action(cmd, m)
		if err != nil {
			var typed typedCommandError
			if errors.As(err, &typed) {
				return err
			}
			return newRuntimeError(err)
		}
		return collectCommandOutcome(cmd.Context(), outcome)
	}
}

func macSlotCommand(verb, short string, stderr io.Writer, requireRunner bool, action func(*cobra.Command, *macvm.Manager, string) (commandOutcome, error)) *cobra.Command {
	var name string
	return &cobra.Command{
		Use: verb + " [mac1|mac2]", Short: short, ValidArgsFunction: macTargets,
		Args: func(_ *cobra.Command, args []string) error {
			var err error
			name, err = macTarget(args)
			if err != nil {
				return newUsageError(err)
			}
			return nil
		},
		RunE: macRun(stderr, requireRunner, func(cmd *cobra.Command, m *macvm.Manager) (commandOutcome, error) {
			return action(cmd, m, name)
		}),
	}
}

func newMacCommand(stdout, stderr io.Writer) *cobra.Command {
	list := macRun(stderr, false, func(cmd *cobra.Command, m *macvm.Manager) (commandOutcome, error) {
		result, err := m.List(cmd.Context())
		return macOutcome(result), err
	})
	mac := &cobra.Command{Use: "mac", Short: "Manage two independent macOS 27 virtual machines", Long: `Manage the fixed mac1 and mac2 slots on Apple Silicon with macOS 27.
This command domain never reads farrow.yml. State, images, SSH trust and
network configuration live under $FARROW_HOME/mac. The default slot is mac1.
up prepares missing components and waits for SSH; start only starts an
existing initialized instance. Normal up never upgrades or clears data.`, Args: cobra.NoArgs, RunE: list}
	mac.AddCommand(&cobra.Command{Use: "ls", Aliases: []string{"status", "st"}, Short: "Show both slots without creating or downloading anything", Args: cobra.NoArgs, RunE: list})
	setupOptions := macvm.SetupOptions{}
	setup := &cobra.Command{Use: "setup", Short: "Prepare the Mac network, official IPSW and unbooted base", Args: cobra.NoArgs, RunE: macRun(stderr, true, func(cmd *cobra.Command, m *macvm.Manager) (commandOutcome, error) {
		base, err := m.Setup(cmd.Context(), setupOptions)
		return macOutcome(base), err
	})}
	macSetupFlags(setup, &setupOptions)
	mac.AddCommand(setup)
	for _, verb := range []string{"init", "up", "start"} {
		options := macvm.InitOptions{}
		short := map[string]string{"init": "Create a slot without overwriting an existing instance", "up": "Prepare, boot and initialize a slot until SSH is ready", "start": "Start an existing initialized slot without downloads"}[verb]
		cmd := macSlotCommand(verb, short, stderr, true, func(cmd *cobra.Command, m *macvm.Manager, name string) (commandOutcome, error) {
			var slot *macvm.Slot
			var err error
			if verb == "init" {
				slot, err = m.Init(cmd.Context(), name, options)
			} else {
				slot, err = m.Up(cmd.Context(), name, options, verb == "start")
			}
			return macSlotOutcome(m, slot, verb), err
		})
		if verb != "start" {
			cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
				return validateMacResources(cmd, options.CPU, options.MemoryBytes)
			}
			macSetupFlags(cmd, &options.SetupOptions)
			cmd.Flags().StringVar(&options.User, "user", "", "initial guest administrator (default: calling host user)")
			cmd.Flags().IntVar(&options.CPU, "cpu", 0, "initial vCPUs (default: 4)")
			macSizeFlag(cmd, &options.MemoryBytes, "memory", "initial memory, e.g. 8G or 8GiB (default: 8GiB)")
			noFileCompletions(cmd, "user", "cpu", "memory")
		}
		if verb != "init" {
			cmd.Flags().BoolVarP(&options.NoWait, "no-wait", "n", false, "return once the VM is running; use up later to finish SSH readiness")
		}
		mac.AddCommand(cmd)
	}
	var hardStop bool
	stop := macSlotCommand("stop", "Shut down a slot, preserving its disk and identity", stderr, true, func(cmd *cobra.Command, m *macvm.Manager, name string) (commandOutcome, error) {
		slot, err := m.Stop(cmd.Context(), name, hardStop)
		return macSlotOutcome(m, slot, "stop"), err
	})
	stop.Flags().BoolVar(&hardStop, "force", false, "explicitly power off instead of requesting graceful shutdown")
	noFileCompletions(stop, "force")
	mac.AddCommand(stop)
	for _, verb := range []string{"reset", "destroy"} {
		force, update := false, false
		short := map[string]string{"reset": "Clear one guest and recreate it with a new identity (stopped)", "destroy": "Remove one guest while preserving its reservation and shared base"}[verb]
		cmd := macSlotCommand(verb, short, stderr, true, func(cmd *cobra.Command, m *macvm.Manager, name string) (commandOutcome, error) {
			if _, err := fmt.Fprintf(stderr, "%s %s: remove this slot's system changes, user files, credentials and SSH trust under %s/slots/%s. Shared bases and the other slot remain.\n", verb, name, m.Store.Root, name); err != nil {
				return commandOutcome{}, err
			}
			if err := confirmCLIAction(force, verb, stderr); err != nil {
				return commandOutcome{}, newConflictError(err)
			}
			slot, err := m.Destroy(cmd.Context(), name, verb == "reset", update)
			return macSlotOutcome(m, slot, verb), err
		})
		cmd.Flags().BoolVar(&force, "force", false, "confirm clearing the selected slot without an interactive prompt")
		noFileCompletions(cmd, "force")
		if verb == "reset" {
			cmd.Flags().BoolVar(&update, "update", false, "recreate from the prepared default base instead of this slot's original base")
			noFileCompletions(cmd, "update")
		}
		mac.AddCommand(cmd)
	}
	mac.AddCommand(macSlotCommand("open", "Show the native desktop; closing its window leaves the VM running", stderr, true, func(cmd *cobra.Command, m *macvm.Manager, name string) (commandOutcome, error) {
		err := m.Open(cmd.Context(), name)
		return macOutcome(map[string]any{"slot": name, "window": "open"}), err
	}))
	mac.AddCommand(macSlotCommand("password", "Explicitly reveal the selected guest's local GUI password", stderr, true, func(cmd *cobra.Command, m *macvm.Manager, name string) (commandOutcome, error) {
		slot, err := m.Store.LoadSlot(name)
		if err != nil {
			return commandOutcome{}, err
		}
		password, err := m.Runner.Password(cmd.Context(), m.Store.Root, slot.InstanceID)
		return commandOutcome{payload: map[string]string{"slot": name, "password": password}, text: func(out, _ io.Writer) error { _, err := fmt.Fprintln(out, password); return err }}, macvm.CredentialErrorForSlot(name, err)
	}))
	for _, verb := range []string{"ssh", "exec"} {
		var name string
		var remote []string
		cmd := &cobra.Command{Use: verb + " [mac1|mac2] -- [command args...]", Short: "Connect using the slot's private SSH key and pinned host key", ValidArgsFunction: macTargets}
		cmd.Args = func(cmd *cobra.Command, args []string) error {
			targets := args
			remote = nil
			if index := cmd.ArgsLenAtDash(); index >= 0 {
				targets, remote = args[:index], args[index:]
			}
			var err error
			name, err = macTarget(targets)
			if err != nil {
				return newUsageError(err)
			}
			if verb == "exec" && len(remote) == 0 {
				return newUsageError(errors.New("mac exec requires -- followed by a command"))
			}
			if structuredOutput(stdout) && len(remote) == 0 {
				return newUsageError(errors.New("interactive mac ssh does not support --json"))
			}
			return nil
		}
		cmd.RunE = macRun(stderr, true, func(cmd *cobra.Command, m *macvm.Manager) (commandOutcome, error) {
			if structuredOutput(stdout) {
				var out, diagnostics bytes.Buffer
				code, err := m.SSHCommand(cmd.Context(), name, remote, os.Stdin, &out, &diagnostics, false)
				if err != nil {
					return commandOutcome{}, err
				}
				payload := map[string]any{"slot": name, "exit_code": code, "stdout": out.String(), "stderr": diagnostics.String()}
				if code != 0 {
					return commandOutcome{}, newRemoteExitError(code, fmt.Errorf("remote command exited %d", code), payload)
				}
				return macOutcome(payload), nil
			}
			code, err := m.SSHCommand(cmd.Context(), name, remote, os.Stdin, stdout, stderr, len(remote) == 0)
			if err != nil {
				return commandOutcome{}, err
			}
			if code != 0 {
				return commandOutcome{}, newRemoteExitError(code, fmt.Errorf("remote command exited %d", code), nil)
			}
			return commandOutcome{streamed: true}, nil
		})
		mac.AddCommand(cmd)
	}
	image := &cobra.Command{Use: "image", Short: "Inspect, explicitly update, or prune isolated Mac image caches"}
	image.AddCommand(&cobra.Command{Use: "ls", Short: "List IPSW and bases with their slot references", Args: cobra.NoArgs, RunE: macRun(stderr, false, func(cmd *cobra.Command, m *macvm.Manager) (commandOutcome, error) {
		result, err := m.Store.ImageList()
		return macOutcome(result), err
	})})
	updateOptions := macvm.SetupOptions{Update: true}
	update := &cobra.Command{Use: "update", Short: "Prepare a new macOS 27 base without changing existing slots", Args: cobra.NoArgs, RunE: macRun(stderr, true, func(cmd *cobra.Command, m *macvm.Manager) (commandOutcome, error) {
		base, err := m.Setup(cmd.Context(), updateOptions)
		return macOutcome(base), err
	})}
	macSetupFlags(update, &updateOptions)
	image.AddCommand(update)
	var installers bool
	prune := &cobra.Command{Use: "prune", Short: "Remove unused non-default bases; optionally remove IPSW caches", Args: cobra.NoArgs, RunE: macRun(stderr, false, func(cmd *cobra.Command, m *macvm.Manager) (commandOutcome, error) {
		removed, err := m.PruneImages(cmd.Context(), installers)
		return macOutcome(map[string]any{"removed": removed}), err
	})}
	prune.Flags().BoolVar(&installers, "installers", false, "also remove idle IPSW downloads (including partial downloads)")
	noFileCompletions(prune, "installers")
	image.AddCommand(prune)
	mac.AddCommand(image)
	mac.AddCommand(&cobra.Command{Use: "doctor", Short: "Check Mac host, runner, helper, state and SSH readiness", Args: cobra.NoArgs, RunE: macRun(stderr, false, func(cmd *cobra.Command, m *macvm.Manager) (commandOutcome, error) {
		report := m.Doctor(cmd.Context())
		outcome := macOutcome(report)
		if !report.OK {
			return commandOutcome{}, newCapabilityError(errors.New("mac environment needs attention")).(*commandBoundaryError).withPayload(report).withText(outcome.text)
		}
		return outcome, nil
	})})
	addMacConvenienceCommands(mac, stderr)
	mac.Example = "  farrow mac up\n  farrow mac up mac2\n  farrow mac ls\n  farrow mac exec mac1 -- sw_vers"
	applyMacHelp(mac, "farrow mac")
	wrapMacProgress(mac, stderr)
	return mac
}

func macSetupFlags(cmd *cobra.Command, options *macvm.SetupOptions) {
	cmd.Flags().StringVar(&options.IPSW, "ipsw", "", "import an existing Apple macOS 27 IPSW")
	cmd.Flags().StringVar(&options.Subnet, "subnet", "", "private /24 subnet for first setup (default: 10.10.20.0/24)")
	macSizeFlag(cmd, &options.DiskBytes, "disk", "base capacity, e.g. 100G or 100GiB (default: prepared base, then 100GiB)")
	_ = cmd.MarkFlagFilename("ipsw", "ipsw")
	noFileCompletions(cmd, "subnet", "disk")
}

func applyMacHelp(command *cobra.Command, path string) {
	if command.Long == "" {
		command.Long = command.Short + ".\nMac commands use only $FARROW_HOME/mac and never read the Linux inventory."
	}
	if command.Example == "" {
		command.Example = "  " + path
	}
	if command.Args == nil {
		command.Args = cobra.ArbitraryArgs
	}
	switch command.Name() {
	case "ssh":
		command.Example = "  farrow mac ssh mac1\n  farrow mac ssh mac2 -- /usr/bin/id"
		command.Long += "\nUse -- before a remote command. Arguments retain their boundaries; use sh -c for shell expressions."
	case "exec":
		command.Example = "  farrow mac exec mac1 -- sw_vers\n  farrow --json mac exec mac2 -- sh -c 'id; sudo -n id'"
		command.Long += "\nUse -- before the remote command. Arguments retain their boundaries and the remote exit code is preserved."
	case "reset":
		command.Example = "  farrow mac reset mac1\n  farrow mac reset mac2 --force --update\n  farrow mac up mac2"
		command.Long += "\nThe recreated slot is stopped and uninitialized. Run up for first boot and SSH readiness."
	case "destroy":
		command.Example = "  farrow mac destroy mac1\n  farrow mac destroy mac2 --force"
	case "setup":
		command.Example = "  farrow mac setup\n  farrow mac setup --ipsw /path/to/macos27.ipsw"
	case "up":
		command.Example = "  farrow mac up\n  farrow mac up mac2"
	case "image":
		command.Example = "  farrow mac image ls\n  farrow mac image update\n  farrow mac image prune --installers"
	case "prune":
		command.Example = "  farrow mac image prune\n  farrow mac image prune --installers"
	}
	for _, child := range command.Commands() {
		applyMacHelp(child, path+" "+child.Name())
	}
}
