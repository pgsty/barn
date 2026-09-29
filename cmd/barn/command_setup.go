package main

import (
	"io"

	"github.com/spf13/cobra"
)

func newSetupCommand(stdout, stderr io.Writer) *cobra.Command {
	options := setupCLIOptions{Mode: "host"}
	command := &cobra.Command{
		Use:   "setup [meta|dual|trio|full]",
		Short: "Prepare the host and lab for first use",
		Long: `Prepare the local host for Barn and select the desired inventory.

With no argument, setup reuses the first discovered inventory (barn.yml,
barn.yaml, pigsty.yml, or pigsty.yaml); if none exists it writes the meta
template to ./barn.yml. A template argument creates or reuses that template.
Use -f to prepare one explicit inventory instead. Setup prints one transaction
before any mutation and requests privilege only at the first privileged step.`,
		Example: `  barn setup --dry-run      # inspect dependencies, downloads, and privilege steps
  barn setup                # reuse a local inventory, or create the meta template
  barn setup --mirror       # use the China official repository
  barn setup full --yes     # prepare a generated four-node lab non-interactively
  barn setup -f pigsty.yml  # prepare an existing Pigsty inventory`,
		Args:              templateArgs,
		ValidArgsFunction: templateCompletion,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			return validateChoice("--mode", options.Mode, "host", "shared")
		},
	}
	command.Flags().StringVarP(&options.FilePath, "file", "f", "", "inventory to prepare (cannot be combined with a template)")
	command.Flags().StringVarP(&options.CIDR, "cidr", "c", "", "generated template network as a canonical RFC1918 IPv4 /24")
	command.Flags().StringVarP(&options.Repo, "repo", "r", "", "image repository URL or absolute directory, also used for socket_vmnet; overrides --mirror and $BARN_REPO")
	command.Flags().BoolVar(&options.Mirror, "mirror", false, mirrorFlagHelp)
	command.Flags().StringVarP(&options.Mode, "mode", "m", options.Mode, "macOS fixed-IP network backend: host or shared")
	command.Flags().BoolVarP(&options.DryRun, "dry-run", "d", false, "show the resolved setup plan without changing anything")
	command.Flags().BoolVarP(&options.Yes, "yes", "y", false, "accept the one-time setup plan (required without a terminal)")
	command.MarkFlagsMutuallyExclusive("dry-run", "yes")
	_ = command.RegisterFlagCompletionFunc("mode", enumFlagCompletion("host", "shared"))
	noFileCompletions(command, "cidr", "mirror", "dry-run", "yes")
	command.RunE = func(command *cobra.Command, arguments []string) error {
		profileName := ""
		if len(arguments) == 1 {
			profileName = arguments[0]
		}
		options.ModeExplicit = command.Flags().Changed("mode")
		outcome, err := runSetupCommand(command.Context(), profileName, options, outputFormatFor(stdout), verboseOutput(stderr), stderr)
		if err != nil {
			return err
		}
		return collectCommandOutcome(command.Context(), outcome)
	}
	return command
}

func newInitCommand(stdout, stderr io.Writer) *cobra.Command {
	options := initOptions{Template: "meta"}
	command := &cobra.Command{
		Use:   "init [meta|dual|trio|full]",
		Short: "Write a lab configuration (Pigsty-compatible inventory)",
		Long: `Render an editable Pigsty-compatible inventory without touching host state.

The default template is meta and the default destination is ./barn.yml.
Existing files are preserved unless --force is explicit. Use -o - to send the
inventory to stdout for inspection or composition.`,
		Example: `  barn init                        # write ./barn.yml with one meta node
  barn init full -o lab.yml        # write a four-node inventory elsewhere
  barn init dual -o -              # print the two-node inventory to stdout
  barn init --force                # replace ./barn.yml explicitly
  barn init full -c 10.20.30.0/24  # generate a lab on another private /24`,
		Args:              templateArgs,
		ValidArgsFunction: templateCompletion,
		RunE: func(command *cobra.Command, arguments []string) error {
			if len(arguments) == 1 {
				options.Template = arguments[0]
			}
			outcome, err := runInit(options)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	command.Flags().StringVarP(&options.CIDR, "cidr", "c", "", "rebase the generated template to a canonical RFC1918 IPv4 /24")
	command.Flags().StringVarP(&options.Output, "output", "o", "", "write to this path instead of ./barn.yml; '-' writes to stdout")
	command.Flags().BoolVar(&options.Force, "force", false, "overwrite an existing inventory file")
	noFileCompletions(command, "cidr", "force")
	return command
}

func newValidateCommand(stdout, stderr io.Writer) *cobra.Command {
	filePath, repository := "", ""
	command := &cobra.Command{
		Use:   "validate",
		Short: "Validate and resolve a Barn configuration",
		Long: `Parse a Pigsty-compatible inventory, validate every consumed Barn field,
and print its source and node count. Unlike lifecycle commands, validate
never falls back to the already-applied deployment state.`,
		Example: `  barn validate                # discover ` + configDiscoverySummary + `
  barn validate -f pigsty.yml  # validate one explicit inventory
  barn --json validate         # emit the resolved specification as JSON`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			outcome, err := runValidate(filePath, repository)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	command.Flags().StringVarP(&filePath, "file", "f", "", "inventory to validate; defaults to the discovered "+configDiscoverySummary)
	command.Flags().StringVarP(&repository, "repo", "r", "", "image repository whose catalog checks the images; overrides $BARN_REPO")
	return command
}
