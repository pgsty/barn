package main

import (
	"io"

	"github.com/spf13/cobra"
)

func newUpdateCommand(stdout, stderr io.Writer) *cobra.Command {
	options := imageOptions{Action: "update"}
	command := &cobra.Command{
		Use:   "update",
		Short: "Refresh the image catalog",
		Long: `Fetch the configured image repository's catalog, verify its signature, and
activate it. Barn never refreshes the catalog implicitly: ordinary image and
lifecycle commands use the active local catalog. Use image sync only to
activate an exact URL or file. This does not update the Barn executable.`,
		Example: `  barn update
  barn update --mirror
  barn update --repo https://mirror.example/barn
  barn --json update`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return collectImageCommand(command, options, stdout, stderr)
		},
	}
	command.Flags().StringVarP(&options.Repository, "repo", "r", "", repositoryFlagHelp)
	command.Flags().BoolVar(&options.Mirror, "mirror", false, mirrorFlagHelp)
	noFileCompletions(command, "mirror")
	return command
}

func collectImageCommand(command *cobra.Command, options imageOptions, _ io.Writer, stderr io.Writer) error {
	outcome, err := runImage(command.Context(), options, stderr)
	if err != nil {
		return err
	}
	return collectCommandOutcome(command.Context(), outcome)
}

func newImageCommand(stdout, stderr io.Writer) *cobra.Command {
	parent := subcommandGroup(
		"image",
		"Manage image catalogs and the local image cache",
		`Inspect signed image metadata, populate or prune the verified local cache,
import local qcow2 files, and manage the active signed catalog.`,
		`  barn image list
  barn image info u24
  barn image pull u24
  barn image prune --dry-run`,
		stdout, stderr,
	)
	parent.Aliases = []string{"images", "im"}
	listOptions := imageOptions{Action: "list"}
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List available images",
		Long: `List every catalog entry for every architecture plus the registered local
images. Each row names its architecture; info and pull select the native one.
Run 'barn update' first to see a newer catalog.`,
		Example: `  barn image list
  barn image list --repo https://mirror.example/barn
  barn --json image list`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return collectImageCommand(command, listOptions, stdout, stderr)
		},
	}
	list.Flags().StringVarP(&listOptions.Repository, "repo", "r", "", repositoryOnlyFlagHelp)
	parent.AddCommand(list)

	for _, action := range []string{"info", "pull"} {
		action := action
		options := imageOptions{Action: action}
		short := "Show image metadata and cache state"
		if action == "pull" {
			short = "Download and verify an image"
		}
		long := `Show signed metadata, native-architecture selection, and verified local
cache state for one image alias.`
		example := "  barn image info u24\n  barn image info u24:stable\n  barn --json image info u24"
		if action == "pull" {
			long = `Download one native-architecture image from the selected repository, then
verify its catalog digest, byte size, qcow2 structure, and virtual size before
activation. The catalog upstream URL records build provenance; it is not a
repository-failure fallback.`
			example = "  barn image pull\n  barn image pull u24 --mirror\n  barn image pull u24 --repo /srv/barn"
		}
		command := &cobra.Command{
			Use:               action + " [image[:channel]|image@version-prefix]",
			Short:             short,
			Long:              long,
			Example:           example,
			Args:              cobra.MaximumNArgs(1),
			ValidArgsFunction: imageAliasCompletion,
			PreRunE: func(_ *cobra.Command, _ []string) error {
				if options.Arch == "" {
					return nil
				}
				return validateChoice("--arch", options.Arch, "amd64", "arm64")
			},
			RunE: func(command *cobra.Command, arguments []string) error {
				if len(arguments) != 0 {
					options.Alias = arguments[0]
				}
				return collectImageCommand(command, options, stdout, stderr)
			},
		}
		if action == "info" {
			command.Aliases = []string{"in"}
		} else {
			command.Aliases = []string{"p"}
		}
		command.Flags().StringVarP(&options.Arch, "arch", "a", "", "artifact architecture, amd64 or arm64; defaults to the host architecture")
		_ = command.RegisterFlagCompletionFunc("arch", enumFlagCompletion("amd64", "arm64"))
		repoHelp := repositoryOnlyFlagHelp
		if action == "pull" {
			repoHelp = repositoryFlagHelp
		}
		command.Flags().StringVarP(&options.Repository, "repo", "r", "", repoHelp)
		if action == "pull" {
			command.Flags().BoolVar(&options.Mirror, "mirror", false, mirrorFlagHelp)
			noFileCompletions(command, "mirror")
		}
		parent.AddCommand(command)
	}

	pruneOptions := imageOptions{Action: "prune"}
	prune := &cobra.Command{
		Use:     "prune",
		Aliases: []string{"pr"},
		Short:   "Remove unreferenced images and stale staging files",
		Long: `Find cache entries not referenced by the applied deployment plus stale
staging files. The default and --dry-run are read-only; deletion requires
--yes.`,
		Example: `  barn image prune            # inspect candidates
  barn image prune --dry-run  # explicit read-only scan
  barn image prune --yes      # delete the displayed candidates`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return collectImageCommand(command, pruneOptions, stdout, stderr)
		},
	}
	prune.Flags().BoolVarP(&pruneOptions.DryRun, "dry-run", "d", false, "show unreferenced images and stale staging files without deleting")
	prune.Flags().BoolVarP(&pruneOptions.Apply, "yes", "y", false, "delete the displayed unreferenced images and stale staging files")
	prune.Flags().StringVarP(&pruneOptions.Repository, "repo", "r", "", repositoryOnlyFlagHelp)
	prune.MarkFlagsMutuallyExclusive("dry-run", "yes")
	noFileCompletions(prune, "dry-run", "yes")
	parent.AddCommand(prune)

	syncOptions := imageOptions{Action: "sync"}
	syncCommand := &cobra.Command{
		Use:     "sync <url|path>",
		Aliases: []string{"sy"},
		Short:   "Activate a signed image catalog",
		Long: `Fetch or read a catalog and its detached signature, verify the signature,
refuse older revisions unless --allow-downgrade, and activate it atomically.`,
		Example: `  barn image sync https://mirror.example/barn/catalog.json
  barn image sync /srv/barn/catalog.json
  barn image sync --allow-downgrade /srv/recovery/catalog.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			syncOptions.Source = arguments[0]
			return collectImageCommand(command, syncOptions, stdout, stderr)
		},
	}
	syncCommand.Flags().BoolVar(&syncOptions.AllowDowngrade, "allow-downgrade", false, "activate a catalog older than the newest revision seen so far")
	syncCommand.Flags().StringVarP(&syncOptions.Repository, "repo", "r", "", "repository whose active catalog is replaced")
	noFileCompletions(syncCommand, "allow-downgrade")
	parent.AddCommand(syncCommand)

	resetOptions := imageOptions{Action: "reset"}
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Restore the built-in catalog",
		Long: `Reactivate the catalog embedded in this Barn binary. Downgrade protection
still remembers the newest revision ever activated.`,
		Example: `  barn image reset
  barn --json image reset`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return collectImageCommand(command, resetOptions, stdout, stderr)
		},
	}
	reset.Flags().StringVarP(&resetOptions.Repository, "repo", "r", "", "repository whose active catalog is reset to the built-in one")
	parent.AddCommand(reset)

	importOptions := imageOptions{Action: "import"}
	importCommand := &cobra.Command{
		Use:     "import <path>",
		Aliases: []string{"i"},
		Short:   "Import and verify a local qcow2 image",
		Long: `Copy a regular local image into the private cache, verify an optional SHA-256,
and inspect its qcow2 metadata. Supplying --name registers an immutable local-
prefixed alias and therefore also requires --boot and --source-user.`,
		Example: `  barn image import ./base.qcow2
  barn image import ./base.qcow2 --sha256 <digest>
  barn image import ./base.qcow2 --name local-dev --boot uefi --source-user dba`,
		Args: cobra.ExactArgs(1),
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if importOptions.Boot == "" {
				return nil
			}
			return validateChoice("--boot", importOptions.Boot, "bios", "uefi")
		},
		RunE: func(command *cobra.Command, arguments []string) error {
			importOptions.Path = arguments[0]
			return collectImageCommand(command, importOptions, stdout, stderr)
		},
	}
	importCommand.Flags().StringVarP(&importOptions.ExpectedSHA256, "sha256", "s", "", "optional expected SHA-256")
	importCommand.Flags().StringVar(&importOptions.Name, "name", "", "optional immutable local- prefixed alias")
	importCommand.Flags().StringVarP(&importOptions.Boot, "boot", "b", "", "required with --name: bios or uefi")
	importCommand.Flags().StringVarP(&importOptions.SourceUser, "source-user", "u", "", "required with --name: source image login user")
	importCommand.MarkFlagsRequiredTogether("name", "boot", "source-user")
	_ = importCommand.RegisterFlagCompletionFunc("boot", enumFlagCompletion("bios", "uefi"))
	noFileCompletions(importCommand, "sha256", "name", "source-user")
	parent.AddCommand(importCommand)
	return parent
}
