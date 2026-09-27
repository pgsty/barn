package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/pgsty/farrow/internal/config"
	"github.com/pgsty/farrow/internal/macvm"
	"github.com/spf13/cobra"
)

type macSizeValue struct{ value *int64 }

func (v macSizeValue) String() string {
	if v.value == nil || *v.value == 0 {
		return ""
	}
	return strconv.FormatInt(*v.value, 10)
}
func (v macSizeValue) Type() string { return "size" }
func (v macSizeValue) Set(raw string) error {
	text := strings.TrimSpace(raw)
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		if n <= 0 {
			return errors.New("size must be positive")
		}
		*v.value = n
		return nil
	}
	upper := strings.ToUpper(text)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB", "KB", "MB", "GB", "TB", "B"} {
		if strings.HasSuffix(upper, strings.ToUpper(unit)) {
			n, err := config.ParseSize(text[:len(text)-len(unit)] + unit)
			if err != nil {
				return err
			}
			*v.value = n
			return nil
		}
	}
	if len(text) > 1 {
		suffix := strings.ToUpper(text[len(text)-1:])
		if strings.Contains("KMGT", suffix) {
			n, err := config.ParseSize(text[:len(text)-1] + suffix + "iB")
			if err != nil {
				return err
			}
			*v.value = n
			return nil
		}
	}
	return fmt.Errorf("invalid size %q; use 16G, 16GiB, 16GB, or an integer byte count", raw)
}
func macSizeFlag(cmd *cobra.Command, value *int64, name, help string) {
	cmd.Flags().Var(macSizeValue{value}, name, help)
}

func validateMacResources(cmd *cobra.Command, cpu int, memory int64) error {
	if memory != 0 && memory < 4<<30 {
		return newUsageError(errors.New("--memory must be at least 4GiB"))
	}
	if cmd.Flags().Changed("cpu") && cpu < 2 {
		return newUsageError(errors.New("--cpu must be at least 2"))
	}
	return nil
}

func addMacConvenienceCommands(mac *cobra.Command, stderr io.Writer) {
	var cpu int
	var memory int64
	configure := macSlotCommand("configure", "Change CPU or memory of a stopped instance, preserving its data", stderr, true, func(cmd *cobra.Command, m *macvm.Manager, name string) (commandOutcome, error) {
		slot, err := m.Configure(cmd.Context(), name, cpu, memory)
		return macSlotOutcome(m, slot, "configure"), err
	})
	configure.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if cpu == 0 && memory == 0 && !cmd.Flags().Changed("cpu") {
			return newUsageError(errors.New("specify --cpu or --memory"))
		}
		return validateMacResources(cmd, cpu, memory)
	}
	configure.Flags().IntVar(&cpu, "cpu", 0, "virtual CPUs (at least 2)")
	macSizeFlag(configure, &memory, "memory", "memory, e.g. 16G or 16GiB (at least 4GiB)")
	configure.Example = "  farrow mac stop mac1\n  farrow mac configure mac1 --cpu 8 --memory 16G\n  farrow mac start mac1"
	mac.AddCommand(configure)
	var noWait bool
	restart := macSlotCommand("restart", "Normally stop and start an initialized instance, preserving data", stderr, true, func(cmd *cobra.Command, m *macvm.Manager, name string) (commandOutcome, error) {
		slot, err := m.Store.LoadSlot(name)
		if err != nil {
			return commandOutcome{}, err
		}
		if !slot.Initialized {
			return commandOutcome{}, newConflictError(fmt.Errorf("%s needs first boot; run farrow mac up %s", name, name))
		}
		if _, err = m.Stop(cmd.Context(), name, false); err != nil {
			return commandOutcome{}, err
		}
		slot, err = m.Up(cmd.Context(), name, macvm.InitOptions{NoWait: noWait}, true)
		return macSlotOutcome(m, slot, "restart"), err
	})
	restart.Flags().BoolVarP(&noWait, "no-wait", "n", false, "return once the VM is running, without waiting for SSH")
	mac.AddCommand(restart)
	var lines int
	logs := macSlotCommand("logs", "Show recent native runtime diagnostics", stderr, false, func(cmd *cobra.Command, m *macvm.Manager, name string) (commandOutcome, error) {
		text, err := m.Logs(name, lines)
		return commandOutcome{payload: map[string]any{"slot": name, "logs": text}, text: func(out, _ io.Writer) error { _, err := io.WriteString(out, text); return err }}, err
	})
	logs.PreRunE = func(_ *cobra.Command, _ []string) error {
		if lines < 1 || lines > 10000 {
			return newUsageError(errors.New("--lines must be between 1 and 10000"))
		}
		return nil
	}
	logs.Flags().IntVarP(&lines, "lines", "n", 100, "number of recent lines (maximum 10000)")
	mac.AddCommand(logs)
	credentials := &cobra.Command{Use: "credentials", Short: "Recover credentials after changing the runner's signing identity"}
	var oldRunner string
	migrate := macSlotCommand("migrate", "Move a stopped instance's GUI credential using its original runner", stderr, true, func(cmd *cobra.Command, m *macvm.Manager, name string) (commandOutcome, error) {
		result, err := m.MigrateCredentials(cmd.Context(), name, oldRunner)
		return macMessageOutcome(result, name+": GUI credential migrated; guest data and SSH identity preserved"), err
	})
	migrate.Flags().StringVar(&oldRunner, "from", "", "original signed runner or app (default: retained owner or previous build)")
	_ = migrate.MarkFlagFilename("from")
	migrate.Example = "  farrow mac credentials migrate mac1\n  farrow mac credentials migrate mac1 --from '/path/to/previous/Farrow Mac.app'"
	credentials.AddCommand(migrate)
	mac.AddCommand(credentials)

	network := &cobra.Command{Use: "network", Short: "Inspect or remove the dedicated Mac network helper"}
	network.AddCommand(&cobra.Command{Use: "status", Short: "Show this environment's helper, subnet and update status", Args: cobra.NoArgs, RunE: macRun(stderr, false, func(cmd *cobra.Command, m *macvm.Manager) (commandOutcome, error) {
		info, err := m.NetworkInfo(cmd.Context())
		return macNetworkOutcome(info), err
	})})
	var force bool
	uninstall := &cobra.Command{Use: "uninstall", Short: "Remove this environment's idle network helper; preserve all guest data", Args: cobra.NoArgs, RunE: macRun(stderr, true, func(cmd *cobra.Command, m *macvm.Manager) (commandOutcome, error) {
		if _, err := fmt.Fprintf(stderr, "Remove the dedicated Mac network for %s. Both slots must be stopped; guest disks, credentials, images and other environments are preserved.\n", m.Store.Root); err != nil {
			return commandOutcome{}, err
		}
		if err := confirmCLIAction(force, "uninstall", stderr); err != nil {
			return commandOutcome{}, newConflictError(err)
		}
		result, err := m.UninstallNetwork(cmd.Context())
		return macMessageOutcome(result, "Mac network uninstalled; guest data preserved", "farrow mac setup"), err
	})}
	uninstall.Flags().BoolVar(&force, "force", false, "confirm uninstall without an interactive prompt; never stop a running VM")
	network.AddCommand(uninstall)
	mac.AddCommand(network)
}
