package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/config"
	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/network/subnet"
	"github.com/pgsty/barn/internal/spec"
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
  barn setup full --image el9 --yes # prepare a generated EL9 lab
  barn setup full -I u24 -A amd64 --dry-run # preview a generated amd64 lab
  barn setup --mirror       # use the China official repository
  barn setup full --yes     # prepare a generated four-node lab non-interactively
  barn setup -f pigsty.yml  # prepare an existing Pigsty inventory`,
		Args:              templateArgs,
		ValidArgsFunction: templateCompletion,
		PreRunE: func(command *cobra.Command, args []string) error {
			if err := validateOverrideFlags(command, args); err != nil {
				return err
			}
			return validateChoice("--mode", options.Mode, "host", "shared")
		},
	}
	command.Flags().StringVarP(&options.FilePath, "file", "f", "", "inventory to prepare (cannot be combined with a template)")
	addVMFlags(command, &options.vmInputs, true)
	command.Flags().StringVarP(&options.CIDR, "cidr", "c", "", "generated template network as a canonical RFC1918 IPv4 /24")
	command.Flags().StringVarP(&options.Repo, "repo", "r", "", "image repository URL or local directory, also used for socket_vmnet; overrides --mirror and $BARN_REPO")
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

type initOptions struct {
	vmInputs
	Template string
	CIDR     string
	Output   string
	Force    bool
}

type initResult struct {
	Resources []config.ResourceResolution `json:"resources"`
	Template  string                      `json:"template"`
	Image     string                      `json:"image,omitempty"`
	Arch      string                      `json:"arch,omitempty"`
	Content   string                      `json:"content,omitempty"`
	Path      string                      `json:"path,omitempty"`
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
  barn init full --image el9       # write a four-node EL9 inventory
  barn init full -I u24 -A amd64   # save image and guest architecture
  barn init dual -o -              # print the two-node inventory to stdout
  barn init --force                # replace ./barn.yml explicitly
  barn init full -c 10.20.30.0/24  # generate a lab on another private /24`,
		Args:              templateArgs,
		ValidArgsFunction: templateCompletion,
		PreRunE:           validateOverrideFlags,
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
	addVMFlags(command, &options.vmInputs, true)
	command.Flags().StringVarP(&options.CIDR, "cidr", "c", "", "rebase the generated template to a canonical RFC1918 IPv4 /24")
	command.Flags().StringVarP(&options.Output, "output", "o", "", "write to this path instead of ./barn.yml; '-' writes to stdout")
	command.Flags().BoolVar(&options.Force, "force", false, "overwrite an existing inventory file")
	noFileCompletions(command, "cidr", "force")
	return command
}

func runInit(options initOptions) (commandOutcome, error) {
	name := options.Template
	if name == "" {
		name = "meta"
	}
	input, err := options.vmInputs.resolve(true, true)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	data, err := config.TemplateWithOptions(name, options.CIDR, input.templateOptions())
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	generated, err := config.ParseInventory(data)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	selectedImage, selectedArch := generated.Nodes[0].Image, generated.Arch
	warnings := make([]string, 0, 1)
	if options.CIDR != "" {
		if layout, parseErr := subnet.Parse(options.CIDR); parseErr == nil {
			if warning := layout.Warning(); warning != "" {
				warnings = append(warnings, warning)
			}
		}
	}
	if options.Output == "-" {
		result := initResult{Resources: generated.Resources, Template: name, Image: selectedImage, Arch: selectedArch, Content: string(data)}
		return commandOutcome{payload: result, text: func(stdout, stderr io.Writer) error {
			printWarnings(stderr, warnings)
			_, _ = stdout.Write(data)
			return nil
		}}, nil
	}
	target := options.Output
	if target == "" {
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			return commandOutcome{}, newRuntimeError(cwdErr)
		}
		target = filepath.Join(cwd, "barn.yml")
	}
	target, err = fsutil.EnsureParent(target)
	if err != nil {
		return commandOutcome{}, newUsageError(err)
	}
	if options.Force {
		err = fsutil.AtomicWrite(target, data, 0o600)
	} else {
		err = fsutil.AtomicCreate(target, data, 0o600)
	}
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return commandOutcome{}, newConflictError(fmt.Errorf("%s already exists; edit it, pass --force to replace it, or -o for another path", target))
		}
		return commandOutcome{}, newRuntimeError(err)
	}
	result := initResult{Resources: generated.Resources, Template: name, Image: selectedImage, Arch: selectedArch, Path: target}
	return commandOutcome{payload: result, text: func(stdout, stderr io.Writer) error {
		printWarnings(stderr, warnings)
		printItemResult(stdout, "inventory", "created", name+" template")
		textField(stdout, 10, "SPEC", compactSpec(generated.Nodes[0].CPUs, generated.Nodes[0].Memory))
		textField(stdout, 10, "root", planSize(generated.Nodes[0].RootDisk))
		if selectedImage != "" {
			textField(stdout, 10, "image", selectedImage)
		}
		if selectedArch != "" {
			textField(stdout, 10, "arch", selectedArch)
		}
		textField(stdout, 10, "file", target)
		next := "barn up"
		if options.Output != "" {
			next += " -f " + shellQuote(options.Output)
		}
		textField(stdout, 10, "next", next)
		return nil
	}}, nil
}

type validateResult struct {
	Resources []config.ResourceResolution `json:"resources"`
	Valid     bool                        `json:"valid"`
	Source    string                      `json:"source"`
	SpecHash  string                      `json:"spec_hash"`
	Resolved  spec.Resolved               `json:"resolved"`
	Warnings  []string                    `json:"warnings,omitempty"`
}

func newValidateCommand(stdout, stderr io.Writer) *cobra.Command {
	options := lifecycleOptions{}
	command := &cobra.Command{
		Use:   "validate",
		Short: "Validate and resolve a Barn configuration",
		Long: `Parse a Pigsty-compatible inventory, validate every consumed Barn field,
and print its source and node count. Unlike lifecycle commands, validate
never falls back to the already-applied deployment state. Specification flags
-I/-A/-C/-M/-S/-D override environment and inventory values for this validation.`,
		Example: `  barn validate                # discover ` + configDiscoverySummary + `
  barn validate -f pigsty.yml  # validate one explicit inventory
  barn validate -I u24 -A arm64 # validate temporary image and architecture choices
  barn --json validate         # emit the resolved specification as JSON`,
		Args:    cobra.NoArgs,
		PreRunE: validateOverrideFlags,
		RunE: func(command *cobra.Command, _ []string) error {
			outcome, err := runValidate(options)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	addVMFlags(command, &options.vmInputs, true)
	command.Flags().StringVarP(&options.ConfigPath, "file", "f", "", "inventory to validate; defaults to the discovered "+configDiscoverySummary)
	command.Flags().StringVarP(&options.Repository, "repo", "r", "", "image repository whose catalog checks the images; overrides $BARN_REPO")
	return command
}

func runValidate(options lifecycleOptions) (commandOutcome, error) {
	started := time.Now()
	rows := make([][]string, 0, 3)
	failed := func(item string, code int, err error) (commandOutcome, error) {
		rows = append(rows, []string{item, "failed", err.Error()})
		elapsed := time.Since(started)
		return commandOutcome{}, newCommandError(code, err).withText(func(stdout, _ io.Writer) error {
			printValidationResult(stdout, rows, elapsed, false)
			return nil
		})
	}
	input, err := options.vmInputs.resolve(true, true)
	options.vmInputs = input
	if err != nil {
		return failed("options", exitUsage, err)
	}
	if _, _, err := image.ResolveRepository(options.Repository, false); err != nil {
		return failed("repository", exitUsage, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return failed("configuration", exitRuntime, err)
	}
	file, source, err := config.Discover(cwd, options.ConfigPath, config.InventoryOptions{Resources: options.Resources})
	if err != nil {
		return failed("configuration", exitUsage, err)
	}
	rows = append(rows, []string{"configuration", "valid", source})
	resolved, err := file.Resolve()
	if err != nil {
		return failed("nodes", exitUsage, err)
	}
	if err := file.CheckArchOverride(nil, options.Image, options.Arch, options.Sources["arch"]); err != nil {
		return failed("nodes", exitUsage, err)
	}
	resolved = applyLifecycleOverrides(resolved, nil, options)
	names := make([]string, 0, len(resolved.Nodes))
	for _, node := range resolved.Nodes {
		names = append(names, node.Name)
	}
	rows = append(rows, []string{"nodes", "valid", fmt.Sprintf("%d nodes: %s", len(names), strings.Join(names, ", "))})
	imageWarnings, err := checkInventoryImages(context.Background(), resolved, options.Repository)
	if err != nil {
		return failed("images", exitUsage, err)
	}
	rows = append(rows, validationImageRow(resolved, imageWarnings))
	hash, err := spec.Hash(resolved)
	if err != nil {
		return failed("configuration hash", exitRuntime, err)
	}
	warnings := configurationWarnings(resolved)
	if len(warnings) > 0 {
		rows[1][1] = "warning"
	}
	warnings = append(warnings, imageWarnings...)
	result := validateResult{Resources: resourceInputSources(file.Resources, options.vmInputs, nil), Valid: true, Source: source, SpecHash: hash, Resolved: resolved, Warnings: warnings}
	elapsed := time.Since(started)
	return commandOutcome{payload: result, text: func(stdout, stderr io.Writer) error {
		printValidationResult(stdout, rows, elapsed, true)
		printSpecificationTable(stdout, resolved, nil)
		printResourceCalculations(stdout, result.Resources)
		printWarnings(stderr, warnings)
		printLifecycleOverrides(stderr, options)
		return nil
	}}, nil
}
