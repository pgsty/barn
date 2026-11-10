package main

import (
	"fmt"
	"io"

	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/spf13/cobra"
)

type lifecycleHelp struct {
	long    string
	example string
}

var lifecycleHelpByName = map[string]lifecycleHelp{
	"plan": {
		long: `Compare the desired inventory with the applied deployment and classify each
selected node as create, resize, recreate, missing, or unchanged. Plan is read-only.
It uses -f first, then local discovery, then the last applied inventory.`,
		example: `  barn plan                # inspect all pending changes
  barn plan -f pigsty.yml  # compare one explicit desired inventory
  barn --json plan meta    # inspect one node as structured data`,
	},
	"up": {
		long: `Create missing nodes, start stopped ones, and refresh the SSH client
configuration so plain ssh reaches every node. Running nodes keep their process; unfinished guest setup is retried.

On first use in a terminal, up prepares the host and writes a one-node
barn.yml if no inventory or applied deployment exists. Use init to customize
the inventory first. For unattended preparation, run setup --yes before up.

Changed node definitions and nodes removed from the inventory are reported,
never applied: use resize for CPU or memory changes, recreate for other node
changes, and destroy for removals. A missing host share fails only its node;
restore the original directory or mount before retrying it.`,
		example: `  barn up                           # start your first VM, or continue the current lab
  barn up meta                      # converge only the meta node
  barn up -I u24 -A amd64            # override the image and guest architecture
  barn up --mirror                  # use the China official repository for downloads
  barn up -f pigsty.yml --rollback  # remove safe artifacts from failed prepares`,
	},
	"start": {
		long: `Start stopped nodes from the applied deployment state and wait for each
guest to become ready and refresh SSH connection details. Start does not read
an inventory or create nodes. Run up to retry unfinished guest setup.
Independent nodes continue if one node's host share is unavailable.`,
		example: `  barn start                        # start every stopped node
  barn start meta --no-wait         # return once QEMU is running`,
	},
	"stop": {
		long: `Stop running nodes recorded in the applied deployment state.`,
		example: `  barn stop                         # stop the deployment
  barn stop meta                    # stop one node`,
	},
	"restart": {
		long: `Stop and start existing nodes from the applied state without re-reading an
inventory. Use reload when the inventory may contain new nodes.`,
		example: `  barn restart                      # restart the deployment
  barn restart meta --no-wait       # restart one node without waiting for the guest`,
	},
	"reload": {
		long: `Stop the selected nodes, re-read the inventory, and run up. New nodes are
created; CPU or memory changes need resize, other changed definitions need
recreate, and removed nodes still need destroy. These changes are refused
before anything stops.`,
		example: `  barn reload                       # re-read the discovered inventory
  barn reload -f pigsty.yml         # reload from an explicit desired inventory
  barn reload --mirror              # use the China official repository for downloads
  barn reload meta                  # reload one selected node`,
	},
	"resize": {
		long: `Apply CPU and memory changes from the desired inventory to existing nodes.
Edit vm_cpu or vm_mem first, then run resize. Barn hot-plugs increases when
the running QEMU instance has capacity reserved; decreases and older VMs use
an automatic stop and start while preserving the VM identity and disks.
Resize never changes images, disks, networking, or shared directories; use
recreate for those changes. The inventory is never modified by this command.`,
		example: `  barn resize meta                # apply the edited resources to meta
  barn resize -f pigsty.yml         # apply resource changes for all nodes
  barn --json resize meta            # inspect structured result`,
	},
	"recreate": {
		long: `Destroy and recreate selected nodes from the desired inventory.

This is the explicit path for node-definition drift. On a terminal Barn lists
the nodes it replaces and asks you to type recreate; automation must pass
--yes (or --force). A whole-deployment recreate refuses an inventory that no
longer lists an existing node; destroy that node explicitly first. Persistent
disks follow their declared lifecycle policy. A successful recreate also
refreshes the SSH client configuration.`,
		example: `  barn recreate meta           # review and confirm one changed node
  barn recreate --yes meta     # non-interactive recreation
  barn recreate --mirror meta  # use the China official repository for downloads
  barn recreate -f pigsty.yml meta`,
	},
	"status": {
		long: `Show recorded applied state and live runtime identity for selected nodes.
Status works outside the inventory directory. After an interrupted
operation it first records which nodes are actually running.`,
		example: `  barn status                  # show every node
  barn status meta             # show one node
  barn --json status           # stable machine-readable status`,
	},
	"destroy": {
		long: `Destroy selected nodes, or the whole deployment when no selector is given.

Destroy never infers deletion from a missing inventory entry. On a terminal it
asks you to type destroy; automation must pass --yes (or --force). Persistent
disks and keys are preserved unless their wider deletion flags are explicit.
Destroying the control node makes the first remaining node the control node;
update the inventory control role, then run up to install its control SSH
access. Removing nodes refreshes the SSH client configuration for the
remaining deployment; destroying the whole deployment also removes the SSH
client configuration Barn installed.`,
		example: `  barn destroy meta             # review and confirm removal of one node
  barn destroy --yes            # remove the deployment, preserving persistent data
  barn destroy --yes --purge    # terminal disposal; image cache remains`,
	},
}

func newLifecycleCommand(name, short string, stdout, stderr io.Writer) *cobra.Command {
	help := lifecycleHelpByName[name]
	command := &cobra.Command{
		Use:               name + " [node...]",
		Short:             short,
		Long:              help.long,
		Example:           help.example,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: nodeCompletion(name == "plan" || name == "up" || name == "reload" || name == "resize" || name == "recreate", false),
	}
	switch name {
	case "plan":
		command.Aliases = []string{"pl"}
	case "recreate":
		command.Aliases = []string{"rc"}
	case "status":
		command.Aliases = []string{"st"}
	case "destroy":
		command.Aliases = []string{"de"}
	}
	options := lifecycleOptions{}
	if lifecycleSupportsParallelism(name) {
		addLifecycleParallelFlag(command)
	}
	switch name {
	case "plan":
		command.Flags().StringVarP(&options.ConfigPath, "file", "f", "", "desired inventory; defaults to the discovered inventory, then the applied state")
		command.Flags().StringVarP(&options.Repository, "repo", "r", "", repositoryOnlyFlagHelp)
	case "up", "reload":
		command.Flags().StringVarP(&options.ConfigPath, "file", "f", "", "desired inventory; defaults to the discovered inventory, then the applied state")
		command.Flags().StringVarP(&options.Repository, "repo", "r", "", repositoryFlagHelp)
		command.Flags().BoolVar(&options.Mirror, "mirror", false, mirrorFlagHelp)
		command.Flags().BoolVarP(&options.NoWait, "no-wait", "n", false, "return once QEMU is running, without waiting for the guest to boot")
		command.Flags().BoolVar(&options.Rollback, "rollback", false, "remove safe artifacts from nodes that fail to prepare")
		noFileCompletions(command, "mirror", "no-wait", "rollback")
	case "resize":
		command.Flags().StringVarP(&options.ConfigPath, "file", "f", "", "desired inventory; defaults to the discovered inventory, then the applied state")
	case "start", "restart":
		command.Flags().BoolVarP(&options.NoWait, "no-wait", "n", false, "return once QEMU is running, without waiting for the guest to boot")
		noFileCompletions(command, "no-wait")
	case "recreate":
		command.Flags().StringVarP(&options.ConfigPath, "file", "f", "", "desired inventory; defaults to the discovered inventory, then the applied state")
		command.Flags().StringVarP(&options.Repository, "repo", "r", "", repositoryFlagHelp)
		command.Flags().BoolVar(&options.Mirror, "mirror", false, mirrorFlagHelp)
		command.Flags().BoolVar(&options.Force, "force", false, "recreate without the interactive confirmation (required without a terminal)")
		command.Flags().BoolVar(&options.Force, "yes", false, "same as --force")
		command.Flags().BoolVarP(&options.NoWait, "no-wait", "n", false, "return once QEMU is running, without waiting for the guest to boot")
		noFileCompletions(command, "mirror", "force", "yes", "no-wait")
	case "destroy":
		command.Flags().BoolVar(&options.Force, "force", false, "destroy without the interactive confirmation (required without a terminal)")
		command.Flags().BoolVar(&options.Force, "yes", false, "same as --force")
		command.Flags().BoolVar(&options.DeletePersistent, "delete-persistent", false, "also delete owned persistent data disks")
		command.Flags().BoolVar(&options.Purge, "purge", false, "terminal disposal: also delete persistent disks, keys, and deployment state")
		noFileCompletions(command, "force", "yes", "delete-persistent", "purge")
	}
	if lifecycleReadsConfig(name) {
		command.Flags().StringVarP(&options.Image, "image", "I", "", "override vm_image/vm_version for selected nodes without editing the inventory")
		addArchitectureFlag(command, &options.Arch, "override deployment vm_arch")
		_ = command.RegisterFlagCompletionFunc("image", imageAliasCompletion)
		command.PreRunE = validateOverrideFlags
		command.Long += `

Use --image/-I and --arch/-A to override the inventory for this command.
Image overrides apply to selected nodes; architecture is deployment-wide.
To keep these choices, set vm_image and vm_arch in the inventory. Cross-architecture
guests use slower QEMU TCG emulation. Up never replaces existing VM disks;
use recreate to apply image or architecture changes to existing nodes.`
	}
	command.RunE = func(command *cobra.Command, nodes []string) error {
		if lifecycleSupportsParallelism(name) {
			parallelism, err := lifecycleParallelism(command)
			if err != nil {
				return err
			}
			options.Parallelism = parallelism
		}
		outcome, err := runLifecycleCommand(command.Context(), name, options, nodes, stderr)
		if err != nil {
			return err
		}
		return collectCommandOutcome(command.Context(), outcome)
	}
	return command
}

func newPurgeCommand(stdout, stderr io.Writer) *cobra.Command {
	var ask bool
	command := &cobra.Command{
		Use:   "purge",
		Short: "Discard the entire deployment, including persistent data",
		Long: `Destroy every virtual machine in the applied deployment and delete its
root disks, persistent data disks, SSH keys, state, and default SSH client
configuration. The verified image cache and host-global network remain
installed.

Purge deletes immediately by default. Use --ask to review the nodes and
deletion scope first, then confirm with y/yes or cancel with n/no.

Purge accepts no node selectors and never reads an inventory. It is idempotent
when no deployment exists, but still refuses ambiguous or unsafe retained
artifacts rather than deleting them by path alone.`,
		Example: `  barn purge          # discard the local lab immediately
  barn purge --ask    # review the lab and confirm with y/yes
  barn --json purge   # stable output for automation`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			parallelism, err := lifecycleParallelism(command)
			if err != nil {
				return err
			}
			outcome, err := runPurgeCommand(command.Context(), ask, parallelism, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	command.Flags().BoolVar(&ask, "ask", false, "show the deletion scope and ask for y/yes before purging")
	addLifecycleParallelFlag(command)
	noFileCompletions(command, "ask")
	return command
}

func lifecycleSupportsParallelism(name string) bool {
	return startupCommand(name) || name == "stop" || name == "destroy" || name == "purge"
}

func addLifecycleParallelFlag(command *cobra.Command) {
	command.Flags().IntP("parallel", "p", privatevm.DefaultParallelism(), "node concurrency; 0 runs all selected nodes concurrently; defaults to logical CPUs minus two (minimum 1)")
	noFileCompletions(command, "parallel")
}

func lifecycleParallelism(command *cobra.Command) (*int, error) {
	parallelism, err := command.Flags().GetInt("parallel")
	if err != nil {
		return nil, newUsageError(err)
	}
	if parallelism < 0 {
		return nil, newUsageError(fmt.Errorf("--parallel must be zero or positive, got %d", parallelism))
	}
	// Keep automatic scheduling out of retry commands so only an explicit
	// override is carried to the next invocation.
	if !command.Flags().Changed("parallel") {
		return nil, nil
	}
	return &parallelism, nil
}
