package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/pgsty/barn/internal/config"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/macvm"
	"github.com/pgsty/barn/internal/state"
)

// macSupported is the cheap platform check used to hide the command domain;
// the macOS 27 requirement is checked when a command runs.
func macSupported() bool { return runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" }

func macStore() (*macvm.Store, error) {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return nil, err
	}
	return macvm.NewStore(root)
}

// macCommandContext carries one Mac command: its manager, and the progress
// display that receives the manager's events.
type macCommandContext struct {
	cmd     *cobra.Command
	manager *macvm.Manager
	stderr  io.Writer
}

// macRun builds the manager for one command. Mutating commands show progress
// on stderr; read-only commands stay silent until their result.
func macRun(stderr io.Writer, progressMessage string, action func(macCommandContext) (commandOutcome, error)) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) (err error) {
		store, err := macStore()
		if err != nil {
			return newRuntimeError(err)
		}
		manager := &macvm.Manager{Store: store}
		if progressMessage != "" {
			item := startProgress(cmd.Context(), stderr, progressMessage)
			defer func() { item.Stop(err) }()
			manager.Report = item.Report
		}
		outcome, err := action(macCommandContext{cmd: cmd, manager: manager, stderr: stderr})
		if err != nil {
			if errors.Is(err, ErrCancelled) || errors.Is(err, context.Canceled) {
				return err
			}
			var typed typedCommandError
			if errors.As(err, &typed) {
				return err
			}
			boundary := newCommandError(exitRuntime, err)
			// A partial result still says which machines changed.
			if boundary.code == exitPartial && outcome.payload != nil {
				boundary = boundary.withPayload(outcome.payload).withText(outcome.text)
			}
			return boundary
		}
		return collectCommandOutcome(cmd.Context(), outcome)
	}
}

// macNames resolves machine arguments: explicit names, every machine with
// all, or the default machine when none is given.
func macNames(args []string, all bool) ([]string, error) {
	store, err := macStore()
	if err != nil {
		return nil, err
	}
	if all {
		if len(args) > 0 {
			return nil, newUsageError(errors.New("--all does not take machine names"))
		}
		machines, err := store.ListMachines()
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(machines))
		for _, machine := range machines {
			names = append(names, machine.Name)
		}
		if len(names) == 0 {
			return nil, failure.New(failure.Conflict, errors.New("there are no Mac machines yet")).Because("mac_machine_absent").Then("barn mac up")
		}
		return names, nil
	}
	if len(args) == 0 {
		name, err := store.DefaultMachine()
		if err != nil {
			return nil, err
		}
		return []string{name}, nil
	}
	seen := map[string]bool{}
	var names []string
	for _, name := range args {
		if !macvm.ValidName(name) {
			return nil, newUsageError(fmt.Errorf("invalid machine name %q: use lowercase letters, digits and inner hyphens, starting with a letter", name))
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

func macName(args []string) (string, error) {
	if len(args) > 1 {
		return "", newUsageError(errors.New("select one machine"))
	}
	names, err := macNames(args, false)
	if err != nil {
		return "", err
	}
	return names[0], nil
}

// macCompletion offers existing machine names.
func macCompletion(multiple bool) cobra.CompletionFunc {
	return func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 && !multiple {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		store, err := macStore()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		machines, err := store.ListMachines()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		names := []string{}
		for _, machine := range machines {
			names = append(names, machine.Name)
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}

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
	// Machine sizes are binary: 16GB means 16 GiB, as memory is sold, and VZ
	// needs memory in whole MiB.
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB", "KB", "MB", "GB", "TB"} {
		if strings.HasSuffix(upper, strings.ToUpper(unit)) {
			n, err := config.ParseSize(text[:len(text)-len(unit)] + unit[:1] + "iB")
			if err != nil {
				return err
			}
			*v.value = n
			return nil
		}
	}
	if len(text) > 1 && strings.Contains("KMGT", strings.ToUpper(text[len(text)-1:])) {
		n, err := config.ParseSize(text[:len(text)-1] + strings.ToUpper(text[len(text)-1:]) + "iB")
		if err != nil {
			return err
		}
		*v.value = n
		return nil
	}
	return fmt.Errorf("invalid size %q; use 16G, 16GiB, 16GB, or a byte count", raw)
}

func macSizeFlag(cmd *cobra.Command, value *int64, name, help string) {
	cmd.Flags().Var(macSizeValue{value}, name, help)
}

// macSwitch parses on/off flags such as --clipboard.
type macSwitch struct{ value **bool }

func (v macSwitch) String() string {
	if v.value == nil || *v.value == nil {
		return ""
	}
	return map[bool]string{true: "on", false: "off"}[**v.value]
}
func (v macSwitch) Type() string { return "on|off" }
func (v macSwitch) Set(raw string) error {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "true", "yes":
		enabled := true
		*v.value = &enabled
	case "off", "false", "no":
		enabled := false
		*v.value = &enabled
	default:
		return fmt.Errorf("use on or off, not %q", raw)
	}
	return nil
}

type macShares struct{ shares *[]macvm.Share }

func (v macShares) String() string { return "" }
func (v macShares) Type() string   { return "[name=]path[:ro]" }
func (v macShares) Set(raw string) error {
	share, err := macvm.ParseShare(raw)
	if err != nil {
		return err
	}
	*v.shares = append(*v.shares, share)
	return nil
}

func validateMacResources(cmd *cobra.Command, cpu int, memory int64) error {
	if cmd.Flags().Changed("cpu") && cpu < 2 {
		return newUsageError(errors.New("--cpu must be at least 2"))
	}
	if memory != 0 && memory < 4<<30 {
		return newUsageError(errors.New("--memory must be at least 4GiB"))
	}
	return nil
}

// macDownloadConsent shows the Apple download before it starts. Without a
// terminal, --yes is required, as for host setup.
func macDownloadConsent(yes bool, stderr io.Writer) func(macvm.DownloadPlan) error {
	return func(plan macvm.DownloadPlan) error {
		if yes {
			return nil
		}
		resume := suspendProgress(stderr)
		defer resume()
		remaining := plan.SizeBytes - plan.ResumeBytes
		bestEffortf(stderr, "%s macOS %s (%s) is not prepared on this Mac yet.\n", styled(stderr, ansiCyan, "→"), plan.Version, plan.Build)
		textField(stderr, 10, "download", fmt.Sprintf("%s from Apple (%s)", progressBytes(remaining), macHost(plan.URL)))
		if plan.ResumeBytes > 0 {
			textField(stderr, 10, "resume", progressBytes(plan.ResumeBytes)+" already downloaded")
		}
		textField(stderr, 10, "then", "install macOS once into a reusable base (about 20 minutes)")
		textField(stderr, 10, "free", progressBytes(plan.FreeBytes))
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return failure.New(failure.Usage, errors.New("downloading macOS needs --yes when stdin is not a terminal")).
				Because("mac_download_consent").Then("rerun with --yes, or pass --ipsw with a restore image you already have")
		}
		return confirmPlan("Download macOS from Apple now? [Y/n] ", true, os.Stdin, stderr)
	}
}

func macHost(raw string) string {
	if host := progressSource(raw); host != "" {
		if parts := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/", 2); len(parts) > 0 {
			return parts[0]
		}
	}
	return "Apple"
}

func newMacCommand(stdout, stderr io.Writer) *cobra.Command {
	list := macRun(stderr, "", func(c macCommandContext) (commandOutcome, error) {
		status, err := c.manager.List(c.cmd.Context(), true)
		return macStatusOutcome(status), err
	})
	mac := &cobra.Command{
		Use:   "mac",
		Short: "Run macOS virtual machines on this Mac",
		Long: `Create and run macOS 27 virtual machines on Apple Silicon.

Each machine has a name (mac1 by default), its own disk cloned from a shared
macOS base, its own private network with a fixed address, a login account with
passwordless sudo, and pinned SSH keys. Machines never join the Linux lab or
its inventory; their data lives under $BARN_HOME/mac.

The macOS restore image comes only from Apple (about 27 GB, downloaded once and
kept resumable) or from a local file you pass with --ipsw. Apple allows two
macOS virtual machines to run at a time on one Mac.`,
		Example: `  barn mac up    # create and start mac1, wait for SSH
  barn mac open  # show its desktop
  barn mac ssh   # open a shell in it
  barn mac up dev --cpu 8 --memory 16G --share ~/src
  barn mac ls    # every machine and its state`,
		Args: cobra.NoArgs,
		RunE: list,
	}
	if !macSupported() {
		mac.Hidden = true
	}
	mac.AddGroup(&cobra.Group{ID: "mac-life", Title: "Machines:"}, &cobra.Group{ID: "mac-access", Title: "Access:"}, &cobra.Group{ID: "mac-host", Title: "Host:"})

	ls := &cobra.Command{Use: "ls", Aliases: []string{"list", "status", "st"}, Short: "Show every machine, its state and address", Args: cobra.NoArgs, RunE: list, GroupID: "mac-life",
		Long: `Show every machine: its state, address, SSH readiness, macOS version and
resources. It reads local records and asks each running machine, and never
creates, downloads or starts anything.`,
		Example: "  barn mac ls\n  barn --json mac ls"}
	mac.AddCommand(ls)

	var upOptions macvm.UpOptions
	var upYes bool
	up := &cobra.Command{
		Use:   "up [name]",
		Short: "Create a machine if needed, start it and wait for SSH",
		Long: `Create the named machine (mac1 by default) if it does not exist, start it and
wait until SSH and passwordless sudo work. On a Mac without a prepared macOS
base, up first downloads the restore image from Apple after you confirm, and
installs it once; later machines reuse it in seconds.

Creation options apply only to a new machine. For an existing machine up only
starts it, and refuses options that differ from its configuration.`,
		Example: `  barn mac up
  barn mac up dev --cpu 8 --memory 16G --share ~/src --share docs=~/Documents:ro
  barn mac up --ipsw ~/Downloads/UniversalMac_27.0_26A428_Restore.ipsw
  barn mac up build --yes --no-wait`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: macCompletion(false),
		GroupID:           "mac-life",
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return validateMacResources(cmd, upOptions.CPU, upOptions.MemoryBytes)
		},
	}
	up.RunE = func(cmd *cobra.Command, args []string) error {
		name, err := macName(args)
		if err != nil {
			return err
		}
		upOptions.Setup.Confirm = macDownloadConsent(upYes, stderr)
		return macRun(stderr, "Starting "+name, func(c macCommandContext) (commandOutcome, error) {
			outcome, err := c.manager.Up(cmd.Context(), name, upOptions)
			return macLifecycleOutcome(outcome, stderr), err
		})(cmd, args)
	}
	up.Flags().IntVar(&upOptions.CPU, "cpu", 0, "virtual CPUs for a new machine (default 4)")
	macSizeFlag(up, &upOptions.MemoryBytes, "memory", "memory for a new machine, e.g. 16G (default 8G)")
	macSizeFlag(up, &upOptions.DiskBytes, "disk", "disk capacity for a new machine, e.g. 200G (default: the prepared base, 100G)")
	up.Flags().StringVar(&upOptions.User, "user", "", "administrator account for a new machine (default: your macOS user name)")
	up.Flags().Var(macShares{&upOptions.Shares}, "share", "share a host folder, shown in the guest under /Volumes/My Shared Files (repeatable)")
	up.Flags().Var(macSwitch{&upOptions.Clipboard}, "clipboard", "share the clipboard while the desktop window is focused (default on)")
	up.Flags().StringVar(&upOptions.Subnet, "subnet", "", "private /24 network for a new machine (default: the first free one)")
	up.Flags().StringVar(&upOptions.Setup.IPSW, "ipsw", "", "install macOS from this restore image instead of downloading from Apple")
	up.Flags().BoolVarP(&upYes, "yes", "y", false, "download macOS from Apple without asking (required without a terminal)")
	up.Flags().BoolVarP(&upOptions.NoWait, "no-wait", "n", false, "return once the VM runs, without waiting for SSH")
	up.Flags().BoolVar(&upOptions.Window, "open", false, "show the desktop as soon as the VM runs")
	_ = up.MarkFlagFilename("ipsw", "ipsw")
	_ = up.MarkFlagDirname("share")
	noFileCompletions(up, "cpu", "memory", "disk", "user", "clipboard", "subnet", "yes", "no-wait", "open")
	mac.AddCommand(up)

	var startOptions macvm.StartOptions
	var startAll bool
	start := &cobra.Command{
		Use: "start [name...]", Short: "Start existing machines", GroupID: "mac-life", Args: cobra.ArbitraryArgs, ValidArgsFunction: macCompletion(true),
		Long: `Start existing machines and wait for SSH. start never creates a machine or
downloads anything. --recovery boots macOS Recovery with its desktop shown.`,
		Example: "  barn mac start\n  barn mac start mac1 mac2\n  barn mac start --all\n  barn mac start mac1 --recovery",
	}
	start.RunE = func(cmd *cobra.Command, args []string) error {
		names, err := macNames(args, startAll)
		if err != nil {
			return err
		}
		return macRun(stderr, "Starting "+strings.Join(names, ", "), func(c macCommandContext) (commandOutcome, error) {
			report, err := c.manager.Start(cmd.Context(), names, startOptions)
			return macOutcomesOutcome(report, stderr), err
		})(cmd, args)
	}
	start.Flags().BoolVar(&startAll, "all", false, "start every machine")
	start.Flags().BoolVarP(&startOptions.NoWait, "no-wait", "n", false, "return once the VM runs, without waiting for SSH")
	start.Flags().BoolVar(&startOptions.Window, "open", false, "show the desktop as soon as the VM runs")
	start.Flags().BoolVar(&startOptions.Recovery, "recovery", false, "boot macOS Recovery and show its desktop")
	noFileCompletions(start, "all", "no-wait", "open", "recovery")
	mac.AddCommand(start)

	var stopForce, stopAll bool
	stop := &cobra.Command{
		Use: "stop [name...]", Short: "Shut machines down, powering off after two minutes", GroupID: "mac-life", Args: cobra.ArbitraryArgs, ValidArgsFunction: macCompletion(true),
		Long: `Shut machines down normally through macOS. A machine that has not stopped
after two minutes is powered off, and the result says so. --force powers off
at once, like holding a power button; unsaved work in the guest is lost.`,
		Example: "  barn mac stop\n  barn mac stop --all\n  barn mac stop mac2 --force",
	}
	stop.RunE = func(cmd *cobra.Command, args []string) error {
		names, err := macNames(args, stopAll)
		if err != nil {
			return err
		}
		return macRun(stderr, "Stopping "+strings.Join(names, ", "), func(c macCommandContext) (commandOutcome, error) {
			report, err := c.manager.Stop(cmd.Context(), names, stopForce)
			return macOutcomesOutcome(report, stderr), err
		})(cmd, args)
	}
	stop.Flags().BoolVar(&stopAll, "all", false, "stop every machine")
	stop.Flags().BoolVar(&stopForce, "force", false, "power off at once instead of shutting down")
	noFileCompletions(stop, "all", "force")
	mac.AddCommand(stop)

	var restartOptions macvm.StartOptions
	restart := &cobra.Command{
		Use: "restart [name]", Short: "Shut a machine down and start it again", GroupID: "mac-life", Args: cobra.MaximumNArgs(1), ValidArgsFunction: macCompletion(false),
		Long:    "Shut a machine down normally and start it again, applying configuration changes.",
		Example: "  barn mac restart\n  barn mac restart dev --no-wait",
	}
	restart.RunE = func(cmd *cobra.Command, args []string) error {
		name, err := macName(args)
		if err != nil {
			return err
		}
		return macRun(stderr, "Restarting "+name, func(c macCommandContext) (commandOutcome, error) {
			outcome, err := c.manager.Restart(cmd.Context(), name, restartOptions)
			return macLifecycleOutcome(outcome, stderr), err
		})(cmd, args)
	}
	restart.Flags().BoolVarP(&restartOptions.NoWait, "no-wait", "n", false, "return once the VM runs, without waiting for SSH")
	restart.Flags().BoolVar(&restartOptions.Window, "open", false, "show the desktop as soon as the VM runs")
	noFileCompletions(restart, "no-wait", "open")
	mac.AddCommand(restart)

	open := &cobra.Command{
		Use: "open [name]", Short: "Show a machine's desktop, starting it if needed", GroupID: "mac-access", Args: cobra.MaximumNArgs(1), ValidArgsFunction: macCompletion(false),
		Long: `Show the machine's desktop window, starting the machine first when it is
stopped. Closing the window keeps the machine running in the background; the
clipboard is shared while the window is focused.`,
		Example: "  barn mac open\n  barn mac open dev",
	}
	open.RunE = func(cmd *cobra.Command, args []string) error {
		name, err := macName(args)
		if err != nil {
			return err
		}
		return macRun(stderr, "Opening "+name, func(c macCommandContext) (commandOutcome, error) {
			outcome, err := c.manager.Open(cmd.Context(), name)
			return macLifecycleOutcome(outcome, stderr), err
		})(cmd, args)
	}
	mac.AddCommand(open)

	for _, verb := range []string{"ssh", "exec"} {
		verb := verb
		use, short := "ssh [name] [-- command [args...]]", "Open a shell in a machine, or run a command"
		example := "  barn mac ssh\n  barn mac ssh dev\n  barn mac ssh dev -- 'sw_vers; id'"
		if verb == "exec" {
			use, short = "exec [name] -- <command> [args...]", "Run a command in a machine and pass its exit status through"
			example = "  barn mac exec -- sw_vers\n  barn mac exec dev -- xcodebuild -version\n  barn --json mac exec -- sh -c 'exit 3'"
		}
		command := &cobra.Command{
			Use: use, Short: short, Example: example, GroupID: "mac-access", Args: cobra.ArbitraryArgs, ValidArgsFunction: macCompletion(false),
			Long: `Connect with the machine's own key and pinned host key. Everything before --
selects at most one machine; everything after it is the remote command.
exec keeps argument boundaries; ssh passes the command line to the remote
shell like plain ssh.`,
		}
		command.RunE = func(cmd *cobra.Command, args []string) error {
			head, remote := args, []string(nil)
			if index := cmd.ArgsLenAtDash(); index >= 0 {
				head, remote = args[:index], args[index:]
			} else if len(args) > 1 {
				head, remote = args[:1], args[1:]
			}
			name, err := macName(head)
			if err != nil {
				return err
			}
			if verb == "exec" && len(remote) == 0 {
				return newUsageError(errors.New("exec needs a command after --: barn mac exec [name] -- command [args...]"))
			}
			return macRun(stderr, "", func(c macCommandContext) (commandOutcome, error) {
				return runMacSSH(cmd.Context(), c.manager, verb, name, remote, stdout, stderr)
			})(cmd, args)
		}
		mac.AddCommand(command)
	}

	var configureOptions macvm.ConfigureOptions
	configure := &cobra.Command{
		Use: "configure <name>", Short: "Change a machine's CPUs, memory, shared folders, clipboard or network", GroupID: "mac-life",
		Args: cobra.ExactArgs(1), ValidArgsFunction: macCompletion(false),
		Long: `Change settings of an existing machine without touching its disk or account.
CPUs, memory, shared folders and the network change while the machine is
stopped; the clipboard setting can change anytime. Changes apply at the next
start.`,
		Example: "  barn mac stop dev\n  barn mac configure dev --cpu 8 --memory 16G --share ~/src\n  barn mac configure dev --unshare src --clipboard off\n  barn mac start dev",
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return validateMacResources(cmd, configureOptions.CPU, configureOptions.MemoryBytes)
		},
	}
	configure.RunE = func(cmd *cobra.Command, args []string) error {
		name, err := macName(args)
		if err != nil {
			return err
		}
		return macRun(stderr, "", func(c macCommandContext) (commandOutcome, error) {
			outcome, err := c.manager.Configure(cmd.Context(), name, configureOptions)
			return macLifecycleOutcome(outcome, stderr), err
		})(cmd, args)
	}
	configure.Flags().IntVar(&configureOptions.CPU, "cpu", 0, "virtual CPUs (at least 2)")
	macSizeFlag(configure, &configureOptions.MemoryBytes, "memory", "memory, e.g. 16G (at least 4G)")
	configure.Flags().Var(macShares{&configureOptions.Share}, "share", "share a host folder, or replace the share of that name (repeatable)")
	configure.Flags().StringArrayVar(&configureOptions.Unshare, "unshare", nil, "stop sharing the named folder (repeatable)")
	configure.Flags().Var(macSwitch{&configureOptions.Clipboard}, "clipboard", "share the clipboard: on or off")
	configure.Flags().StringVar(&configureOptions.Subnet, "subnet", "", "move to this private /24 network, or auto for the first free one")
	_ = configure.MarkFlagDirname("share")
	noFileCompletions(configure, "cpu", "memory", "unshare", "clipboard", "subnet")
	mac.AddCommand(configure)

	var recreateOptions macvm.RecreateOptions
	var recreateForce bool
	recreate := &cobra.Command{
		Use: "recreate <name>", Short: "Replace a machine with a fresh macOS of the same settings", GroupID: "mac-life",
		Args: cobra.ExactArgs(1), ValidArgsFunction: macCompletion(false),
		Long: `Erase the machine's disk and start a fresh macOS with the same name, account,
resources, shared folders and address. --update uses the newest prepared base
(see barn mac image update). Its files, apps and settings are lost.`,
		Example: "  barn mac recreate dev\n  barn mac recreate dev --update --force",
	}
	recreate.RunE = func(cmd *cobra.Command, args []string) error {
		name, err := macName(args)
		if err != nil {
			return err
		}
		return macRun(stderr, "Recreating "+name, func(c macCommandContext) (commandOutcome, error) {
			if _, err := c.manager.Machine(name); err != nil {
				return commandOutcome{}, err
			}
			bestEffortf(stderr, "recreate %s erases its disk: files, apps and settings in the guest are lost. Shared folders on this Mac are untouched.\n", name)
			if err := confirmCLIAction(recreateForce, "recreate", stderr); err != nil {
				return commandOutcome{}, macConfirmError(err)
			}
			outcome, err := c.manager.Recreate(cmd.Context(), name, recreateOptions)
			return macLifecycleOutcome(outcome, stderr), err
		})(cmd, args)
	}
	recreate.Flags().BoolVar(&recreateOptions.Update, "update", false, "use the newest prepared macOS base")
	recreate.Flags().BoolVar(&recreateForce, "force", false, "skip the confirmation (required without a terminal)")
	recreate.Flags().BoolVarP(&recreateOptions.NoWait, "no-wait", "n", false, "return once the VM runs, without waiting for SSH")
	noFileCompletions(recreate, "update", "force", "no-wait")
	mac.AddCommand(recreate)

	var destroyForce bool
	destroy := &cobra.Command{
		Use: "destroy <name...>", Short: "Delete machines and their disks", GroupID: "mac-life",
		Args: cobra.MinimumNArgs(1), ValidArgsFunction: macCompletion(true),
		Long: `Power machines off and delete them: disk, keys, password and settings. The
shared macOS base and other machines are kept; barn mac image prune removes
unused bases.`,
		Example: "  barn mac destroy dev\n  barn mac destroy mac1 mac2 --force",
	}
	destroy.RunE = func(cmd *cobra.Command, args []string) error {
		names, err := macNames(args, false)
		if err != nil {
			return err
		}
		return macRun(stderr, "Destroying "+strings.Join(names, ", "), func(c macCommandContext) (commandOutcome, error) {
			bestEffortf(stderr, "destroy %s deletes %s: disk, keys, password and settings. The shared macOS base is kept.\n", strings.Join(names, ", "), map[bool]string{true: "these machines", false: "this machine"}[len(names) > 1])
			if err := confirmCLIAction(destroyForce, "destroy", stderr); err != nil {
				return commandOutcome{}, macConfirmError(err)
			}
			report, err := c.manager.Destroy(cmd.Context(), names)
			return macOutcomesOutcome(report, stderr), err
		})(cmd, args)
	}
	destroy.Flags().BoolVar(&destroyForce, "force", false, "skip the confirmation (required without a terminal)")
	noFileCompletions(destroy, "force")
	mac.AddCommand(destroy)

	var copyPassword bool
	password := &cobra.Command{
		Use: "password [name]", Short: "Show or copy the guest login password", GroupID: "mac-access", Args: cobra.MaximumNArgs(1), ValidArgsFunction: macCompletion(false),
		Long: `Show the password of the machine's login account, needed for the lock
screen and administrator prompts in its desktop. It is stored next to the
machine's SSH key with the same protection. --copy puts it on the clipboard
instead of printing it.`,
		Example: "  barn mac password --copy\n  barn mac password dev",
	}
	password.RunE = func(cmd *cobra.Command, args []string) error {
		name, err := macName(args)
		if err != nil {
			return err
		}
		return macRun(stderr, "", func(c macCommandContext) (commandOutcome, error) {
			secret, err := c.manager.Password(name)
			if err != nil {
				return commandOutcome{}, err
			}
			if copyPassword {
				copy := exec.CommandContext(cmd.Context(), "/usr/bin/pbcopy")
				copy.Stdin = strings.NewReader(secret)
				if err := copy.Run(); err != nil {
					return commandOutcome{}, fmt.Errorf("copy the password: %w", err)
				}
				return macMessageOutcome(map[string]any{"name": name, "copied": true}, name+" password copied to the clipboard"), nil
			}
			return commandOutcome{payload: map[string]string{"name": name, "password": secret}, text: func(out, _ io.Writer) error {
				_, err := fmt.Fprintln(out, secret)
				return err
			}}, nil
		})(cmd, args)
	}
	password.Flags().BoolVarP(&copyPassword, "copy", "c", false, "copy to the clipboard instead of printing")
	noFileCompletions(password, "copy")
	mac.AddCommand(password)

	var install, remove bool
	sshConfig := &cobra.Command{
		Use: "ssh-config", Short: "Print, install or remove the OpenSSH entries of every machine", GroupID: "mac-access", Args: cobra.NoArgs,
		Long: `Print the OpenSSH fragment that lets ssh, scp, rsync and editors reach each
machine by name with its own key and pinned host key. The first machine to
become ready installs it as one Include in ~/.ssh/config, and lifecycle
commands keep it current. --remove deletes only what Barn installed and keeps
it out until --install. A name that ~/.ssh/config already uses for another
host is published by address only.`,
		Example: "  barn mac ssh-config\n  barn mac ssh-config --install && ssh mac1\n  barn mac ssh-config --remove",
	}
	sshConfig.RunE = macRun(stderr, "", func(c macCommandContext) (commandOutcome, error) {
		switch {
		case install:
			result, err := c.manager.InstallSSHConfig(c.cmd.Context())
			if result.Action == "pending" {
				return macMessageOutcome(result, "No machine is ready yet; the first one to become ready installs its SSH entries"), err
			}
			outcome := macMessageOutcome(result, "SSH entries installed: ssh <machine> now works", result.Fragment)
			if len(result.Shadowed) > 0 {
				text := outcome.text
				outcome.text = func(out, errOut io.Writer) error {
					warningf(stderr, "%s", macvm.ShadowedWarning(result.Shadowed))
					return text(out, errOut)
				}
			}
			return outcome, err
		case remove:
			result, err := c.manager.RemoveSSHConfig(c.cmd.Context())
			return macMessageOutcome(result, "SSH entries removed; lifecycle commands leave ~/.ssh alone until --install"), err
		}
		fragment, err := c.manager.SSHConfig()
		return commandOutcome{payload: map[string]string{"fragment": fragment}, text: func(out, _ io.Writer) error {
			_, err := io.WriteString(out, fragment)
			return err
		}}, err
	})
	sshConfig.Flags().BoolVarP(&install, "install", "i", false, "install the entries as one Include in ~/.ssh/config")
	sshConfig.Flags().BoolVar(&remove, "remove", false, "remove the entries Barn installed")
	sshConfig.MarkFlagsMutuallyExclusive("install", "remove")
	noFileCompletions(sshConfig, "install", "remove")
	mac.AddCommand(sshConfig)

	var lines int
	logs := &cobra.Command{
		Use: "logs [name]", Short: "Show a machine's runtime log", GroupID: "mac-access", Args: cobra.MaximumNArgs(1), ValidArgsFunction: macCompletion(false),
		Long:    "Show the last lines of the machine's runtime log: startup, network, shutdown and Apple Virtualization errors.",
		Example: "  barn mac logs\n  barn mac logs dev -n 200",
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if lines < 1 || lines > 10000 {
				return newUsageError(errors.New("--lines must be between 1 and 10000"))
			}
			return nil
		},
	}
	logs.RunE = func(cmd *cobra.Command, args []string) error {
		name, err := macName(args)
		if err != nil {
			return err
		}
		return macRun(stderr, "", func(c macCommandContext) (commandOutcome, error) {
			text, err := c.manager.Logs(name, lines)
			return commandOutcome{payload: map[string]any{"name": name, "logs": text}, text: func(out, _ io.Writer) error {
				_, err := io.WriteString(out, text)
				return err
			}}, err
		})(cmd, args)
	}
	logs.Flags().IntVarP(&lines, "lines", "n", 100, "number of recent lines (at most 10000)")
	noFileCompletions(logs, "lines")
	mac.AddCommand(logs)

	var setupOptions macvm.SetupOptions
	var setupYes bool
	setup := &cobra.Command{
		Use: "setup", Short: "Prepare the macOS base without creating a machine", GroupID: "mac-host", Args: cobra.NoArgs,
		Long: `Download macOS from Apple after you confirm, or read --ipsw, and install it
once into the unbooted base that every machine is cloned from. up does this
automatically; setup lets you do it ahead of time.`,
		Example: "  barn mac setup\n  barn mac setup --ipsw ~/Downloads/UniversalMac_27.0_26A428_Restore.ipsw\n  barn mac setup --yes",
	}
	setup.RunE = func(cmd *cobra.Command, args []string) error {
		setupOptions.Confirm = macDownloadConsent(setupYes, stderr)
		return macRun(stderr, "Preparing macOS", func(c macCommandContext) (commandOutcome, error) {
			base, err := c.manager.Setup(cmd.Context(), setupOptions)
			return macBaseOutcome(base), err
		})(cmd, args)
	}
	setup.Flags().StringVar(&setupOptions.IPSW, "ipsw", "", "install macOS from this restore image instead of downloading from Apple")
	macSizeFlag(setup, &setupOptions.DiskBytes, "disk", "disk capacity of the base, e.g. 200G (default 100G)")
	setup.Flags().BoolVarP(&setupYes, "yes", "y", false, "download macOS from Apple without asking (required without a terminal)")
	_ = setup.MarkFlagFilename("ipsw", "ipsw")
	noFileCompletions(setup, "disk", "yes")
	mac.AddCommand(setup)

	image := &cobra.Command{Use: "image", Short: "Inspect, update or prune macOS restore images and bases", GroupID: "mac-host", Args: cobra.NoArgs,
		Long: `A restore image is Apple's macOS installer (IPSW), downloaded once from Apple
or taken from --ipsw. A base is macOS installed from it, never booted; every
machine is a copy-on-write clone of one base.`,
		Example: "  barn mac image ls\n  barn mac image update\n  barn mac image prune --installers --yes"}
	configureHelpOnly(image, "barn mac image requires a subcommand", stdout, stderr)
	image.AddCommand(&cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List restore images and bases, with the machines using them", Args: cobra.NoArgs,
		Long:    "List restore images and bases with their state, disk usage and the machines that use them.",
		Example: "  barn mac image ls\n  barn --json mac image ls",
		RunE: macRun(stderr, "", func(c macCommandContext) (commandOutcome, error) {
			images, err := c.manager.Store.ImageList()
			return macImagesOutcome(images), err
		})})
	var updateOptions = macvm.SetupOptions{Update: true}
	var updateYes bool
	update := &cobra.Command{Use: "update", Short: "Prepare the newest macOS 27 from Apple as the default base", Args: cobra.NoArgs,
		Long: `Ask Apple for the newest macOS 27 restore image, download it after you confirm,
and install it as the default base for new machines. Existing machines keep
their base until barn mac recreate --update.`,
		Example: "  barn mac image update\n  barn mac recreate dev --update"}
	update.RunE = func(cmd *cobra.Command, args []string) error {
		updateOptions.Confirm = macDownloadConsent(updateYes, stderr)
		return macRun(stderr, "Updating macOS", func(c macCommandContext) (commandOutcome, error) {
			base, err := c.manager.Setup(cmd.Context(), updateOptions)
			return macBaseOutcome(base), err
		})(cmd, args)
	}
	update.Flags().StringVar(&updateOptions.IPSW, "ipsw", "", "use this restore image instead of downloading from Apple")
	update.Flags().BoolVarP(&updateYes, "yes", "y", false, "download without asking (required without a terminal)")
	_ = update.MarkFlagFilename("ipsw", "ipsw")
	noFileCompletions(update, "yes")
	image.AddCommand(update)
	var pruneInstallers, pruneYes bool
	prune := &cobra.Command{Use: "prune", Short: "Remove bases no machine uses; optionally restore images", Args: cobra.NoArgs,
		Long:    "List bases that no machine uses and that are not the default. --installers adds downloaded restore images. Nothing is deleted without --yes.",
		Example: "  barn mac image prune\n  barn mac image prune --installers --yes"}
	prune.RunE = macRun(stderr, "", func(c macCommandContext) (commandOutcome, error) {
		report, err := c.manager.PruneImages(c.cmd.Context(), pruneInstallers, pruneYes)
		return macPruneOutcome(report), err
	})
	prune.Flags().BoolVar(&pruneInstallers, "installers", false, "include downloaded restore images and partial downloads")
	prune.Flags().BoolVarP(&pruneYes, "yes", "y", false, "delete the listed images")
	noFileCompletions(prune, "installers", "yes")
	image.AddCommand(prune)
	mac.AddCommand(image)

	mac.AddCommand(&cobra.Command{Use: "doctor", Short: "Check this Mac, the Mac component and every machine", GroupID: "mac-host", Args: cobra.NoArgs,
		Long: `Check macOS and Apple Silicon support, the Mac component's signature, free
disk space, the prepared base, and every machine: its files, keys, shared
folders, network and SSH. It changes nothing and starts nothing.`,
		Example: "  barn mac doctor\n  barn --json mac doctor",
		RunE: macRun(stderr, "", func(c macCommandContext) (commandOutcome, error) {
			report := c.manager.Doctor(c.cmd.Context())
			outcome := macDoctorOutcome(report)
			if !report.OK {
				return commandOutcome{}, newCapabilityError(errors.New("the Mac environment needs attention")).(*commandBoundaryError).withPayload(report).withText(outcome.text)
			}
			return outcome, nil
		})})

	return mac
}

// macConfirmError keeps a declined confirmation a cancellation and a missing
// --force a usage error, as for Linux destroy.
func macConfirmError(err error) error {
	if errors.Is(err, ErrCancelled) {
		return err
	}
	return newUsageError(err)
}

// runMacSSH runs OpenSSH with the machine's key and pinned host key, sharing
// the Linux executor so exit codes and JSON results behave the same.
func runMacSSH(ctx context.Context, manager *macvm.Manager, verb, name string, remote []string, stdout, stderr io.Writer) (commandOutcome, error) {
	if structuredOutput(stdout) && len(remote) == 0 {
		return commandOutcome{}, newUsageError(errors.New("an interactive shell has no JSON form; pass a command after --"))
	}
	connection, err := manager.Connection(ctx, name)
	if err != nil {
		return commandOutcome{}, err
	}
	// Apple's ssh, never one found first in PATH: see macvm.SSHPath.
	args := connection.OpenSSHArgs(len(remote) == 0)
	if len(remote) != 0 {
		args = append(args, remoteCommandText(verb, remote))
	}
	result, runErr := executeSSHProcess(ctx, verb, connection.Name, connection.User, connection.Host, connection.Port, macvm.SSHPath, args, remote, stdout, stderr)
	if ctx.Err() != nil {
		return commandOutcome{}, ErrCancelled
	}
	if runErr != nil {
		var exitError *exec.ExitError
		if errors.As(runErr, &exitError) {
			code := exitError.ExitCode()
			return commandOutcome{}, newRemoteExitError(code, fmt.Errorf("remote command exited with status %d", code), result)
		}
		return commandOutcome{}, newRuntimeError(runErr)
	}
	if structuredOutput(stdout) {
		return commandOutcome{payload: result}, nil
	}
	return commandOutcome{streamed: true}, nil
}
