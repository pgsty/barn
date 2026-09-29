package main

import (
	"fmt"
	"io"
	"time"

	"github.com/pgsty/barn/internal/sshconfig"
	"github.com/spf13/cobra"
)

func newProvisionCommand(stdout, stderr io.Writer) *cobra.Command {
	options := provisionOptions{Parallelism: 1, Timeout: time.Hour}
	command := &cobra.Command{
		Use:   "provision [node...]",
		Short: "Run a local Bash script in selected guests",
		Long: `Run one local Bash script on the selected running guests over the verified
deployment SSH connection. Execution is serial by default, audited by script
digest, and never leaves the script behind as a guest file.`,
		Example: `  barn provision --script ./bootstrap.sh
  barn provision --script ./check.sh meta node-1
  barn provision --script ./admin.sh --sudo --parallel 2 --timeout 30m`,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: nodeCompletion(false, false),
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if options.Parallelism < 1 || options.Parallelism > 4 {
				return fmt.Errorf("--parallel must be between 1 and 4, got %d", options.Parallelism)
			}
			if options.Timeout <= 0 || options.Timeout > 24*time.Hour {
				return fmt.Errorf("--timeout must be greater than zero and no more than 24h, got %s", options.Timeout)
			}
			return nil
		},
		RunE: func(command *cobra.Command, nodes []string) error {
			outcome, err := runProvision(command.Context(), options, nodes, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	command.Flags().StringVarP(&options.ScriptPath, "script", "s", "", "local Bash script to stream to each selected guest")
	command.Flags().BoolVar(&options.Sudo, "sudo", false, "run the guest script through sudo -n")
	command.Flags().IntVarP(&options.Parallelism, "parallel", "p", options.Parallelism, "bounded node concurrency, 1..4")
	command.Flags().DurationVarP(&options.Timeout, "timeout", "t", options.Timeout, "hard deadline for the operation, maximum 24h")
	noFileCompletions(command, "sudo", "parallel", "timeout")
	_ = command.MarkFlagRequired("script")
	return command
}

func newSSHConfigCommand(stdout, stderr io.Writer) *cobra.Command {
	options := sshConfigOptions{Name: "barn"}
	command := &cobra.Command{
		Use:     "ssh-config [node...]",
		Aliases: []string{"sc"},
		Short:   "Print, install, or remove OpenSSH configuration",
		Long: `Print the deployment OpenSSH fragment, install it as one Include in
~/.ssh/config, or remove only what Barn installed. Printing and installation
can be limited to selected nodes; removal needs no deployment state and
accepts no nodes.`,
		Example: `  barn ssh-config                       # print the fragment
  barn ssh-config --install meta        # install aliases for one node
  barn ssh-config --install --name lab  # use lab-* as the prefixed aliases
  barn ssh-config --remove --name lab   # remove only the lab fragment Barn installed`,
		Args: func(_ *cobra.Command, arguments []string) error {
			if options.Remove && len(arguments) != 0 {
				return fmt.Errorf("--remove does not accept node selectors")
			}
			return nil
		},
		ValidArgsFunction: nodeCompletion(false, false),
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if !sshconfig.ValidName(options.Name) {
				return fmt.Errorf("invalid SSH config name %q", options.Name)
			}
			return nil
		},
		RunE: func(command *cobra.Command, nodes []string) error {
			outcome, err := runSSHConfig(command.Context(), options, nodes)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	command.Flags().BoolVarP(&options.Install, "install", "i", false, "install the deployment fragment and one Include in ~/.ssh/config")
	command.Flags().BoolVar(&options.Remove, "remove", false, "remove the Include and fragment Barn installed")
	command.Flags().StringVar(&options.Name, "name", options.Name, "SSH Host and fragment prefix")
	noFileCompletions(command, "install", "remove", "name")
	command.MarkFlagsMutuallyExclusive("install", "remove")
	return command
}

func newLogsCommand(stdout, stderr io.Writer) *cobra.Command {
	options := logOptions{Source: "serial"}
	command := &cobra.Command{
		Use:     "logs [node]",
		Aliases: []string{"l"},
		Short:   "Read or follow deployment logs",
		Long: `Read the selected node's serial or QEMU log, or the deployment-wide Barn
event log (--source events takes no node). With --follow, text mode streams
bytes and structured modes emit a record stream (NDJSON for JSON) so stdout
remains machine-readable. Events include setup and its lifecycle retry under
one operation ID, and can be read before deployment state exists.`,
		Example: `  barn logs meta                  # read the serial log
  barn logs meta --source qemu    # read QEMU diagnostics
  barn logs --source events -f    # follow the deployment event log
  barn --json logs meta --follow  # stream structured records`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: nodeCompletion(false, true),
		PreRunE: func(_ *cobra.Command, _ []string) error {
			return validateChoice("--source", options.Source, "serial", "qemu", "events")
		},
		RunE: func(command *cobra.Command, arguments []string) error {
			node := ""
			if len(arguments) == 1 {
				node = arguments[0]
			}
			outcome, err := runLogs(command.Context(), options, node, stdout, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	command.Flags().StringVarP(&options.Source, "source", "s", options.Source, "log source: serial, qemu, or events")
	command.Flags().BoolVarP(&options.Follow, "follow", "f", false, "continue streaming appended log data")
	_ = command.RegisterFlagCompletionFunc("source", enumFlagCompletion("serial", "qemu", "events"))
	noFileCompletions(command, "follow")
	return command
}

func newHostsCommand(stdout, stderr io.Writer) *cobra.Command {
	parent := subcommandGroup(
		"hosts",
		"Manage deployment entries in the host hosts file",
		`Publish or remove the deployment's block in /etc/hosts through the
digest-matched Barn helper. Both operations show their exact privileged plan
before applying it.`,
		`  barn hosts install          # show the proposed /etc/hosts block
  barn hosts install --yes    # apply it
  barn hosts uninstall --yes  # remove only the Barn-owned block`,
		stdout, stderr,
	)
	for _, action := range []string{"install", "uninstall"} {
		action := action
		apply := false
		short := "Install deployment host entries"
		if action == "uninstall" {
			short = "Remove deployment host entries"
		}
		example := "  barn hosts " + action + "\n  barn hosts " + action + " --yes"
		command := &cobra.Command{
			Use:     action,
			Short:   short,
			Long:    short + " through the digest-matched helper.\nOn a terminal, show the exact change and ask before applying it; --yes applies\nwithout asking, and without a terminal the command only prints the plan.",
			Example: example,
			Args:    cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				outcome, err := runHosts(command.Context(), action, apply, stderr)
				if err != nil {
					return err
				}
				return collectCommandOutcome(command.Context(), outcome)
			},
		}
		if action == "install" {
			command.Aliases = []string{"i"}
		} else {
			command.Aliases = []string{"u"}
		}
		command.Flags().BoolVarP(&apply, "yes", "y", false, "apply the displayed privileged plan without confirmation (sudo may still ask for a password)")
		noFileCompletions(command, "yes")
		parent.AddCommand(command)
	}
	return parent
}
