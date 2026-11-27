package main

import (
	"io"

	"github.com/spf13/cobra"
)

func newUpdateCommand(stdout, stderr io.Writer) *cobra.Command {
	var repository string
	var mirror bool
	command := &cobra.Command{
		Use:   "update",
		Short: "Refresh the image catalog",
		Long: `Fetch the configured image repository's catalog, verify its signature, and
activate it. Barn never refreshes the catalog implicitly: ordinary image and
lifecycle commands use the active local catalog. Use 'barn image update' to
refresh the catalog and fetch selected or cached images, or 'barn image
sync' to activate an exact URL or file.
This does not update the Barn executable.`,
		Example: `  barn update
  barn update --mirror
  barn update --repo https://mirror.example/barn
  barn --json update`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			outcome, err := runUpdateCatalog(command.Context(), repository, mirror, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	command.Flags().StringVarP(&repository, "repo", "r", "", repositoryFlagHelp)
	command.Flags().BoolVar(&mirror, "mirror", false, mirrorFlagHelp)
	noFileCompletions(command, "mirror")
	return command
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
  barn image update
  barn image prune --dry-run`,
		stdout, stderr,
	)
	parent.Aliases = []string{"images", "im"}

	var listRepo string
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
			outcome, err := runImageList(command.Context(), listRepo)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	list.Flags().StringVarP(&listRepo, "repo", "r", "", repositoryOnlyFlagHelp)
	parent.AddCommand(list)

	parent.AddCommand(newImageInfoCommand(stderr))
	parent.AddCommand(newImagePullCommand(stderr))
	parent.AddCommand(newImageUpdateCommand(stderr))
	parent.AddCommand(newImagePruneCommand(stderr))
	parent.AddCommand(newImageSyncCommand(stderr))
	parent.AddCommand(newImageResetCommand())
	parent.AddCommand(newImageImportCommand(stderr))
	return parent
}

func addImageArchAliasFlag(command *cobra.Command, arch *string) {
	addArchitectureFlag(command, arch, "artifact architecture (defaults to native)")
	// Keep the original -a spelling available to existing scripts.
	command.Flags().StringVarP(arch, "architecture", "a", "", "compatibility alias for --arch/-A")
	_ = command.Flags().MarkHidden("architecture")
	_ = command.RegisterFlagCompletionFunc("architecture", enumFlagCompletion("native", "amd64", "arm64"))
}

func newImageInfoCommand(stderr io.Writer) *cobra.Command {
	var arch, repository string
	command := &cobra.Command{
		Use:     "info [image[:channel]|image@version-prefix]",
		Aliases: []string{"in"},
		Short:   "Show image metadata and cache state",
		Long: `Show signed metadata, architecture selection, and verified local
cache state for one image alias. Architecture-specific aliases such as el10a
(arm64) or u24x/u24i (amd64) select their target architecture directly.`,
		Example:           "  barn image info u24\n  barn image info el10a\n  barn image info u24:stable\n  barn --json image info u24",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: imageAliasCompletion,
		PreRunE:           validateOverrideFlags,
		RunE: func(command *cobra.Command, arguments []string) error {
			alias := ""
			if len(arguments) != 0 {
				alias = arguments[0]
			}
			outcome, err := runImageInfo(command.Context(), alias, arch, repository, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	addImageArchAliasFlag(command, &arch)
	command.Flags().StringVarP(&repository, "repo", "r", "", repositoryOnlyFlagHelp)
	return command
}

func newImagePullCommand(stderr io.Writer) *cobra.Command {
	var arch, repository string
	var mirror bool
	command := &cobra.Command{
		Use:     "pull [image[:channel]|image@version-prefix]",
		Aliases: []string{"p"},
		Short:   "Download and verify an image",
		Long: `Download one image from the selected repository, then verify its catalog
digest, byte size, qcow2 structure, and virtual size before activation. Bare
aliases default to the native architecture; architecture-specific aliases such
as el10a or u24x/u24i select their pinned architecture. The catalog upstream
URL records build provenance; it is not a repository-failure fallback.`,
		Example:           "  barn image pull\n  barn image pull el10a\n  barn image pull u24 --mirror\n  barn image pull u24 --repo /srv/barn",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: imageAliasCompletion,
		PreRunE:           validateOverrideFlags,
		RunE: func(command *cobra.Command, arguments []string) error {
			alias := ""
			if len(arguments) != 0 {
				alias = arguments[0]
			}
			outcome, err := runImagePull(command.Context(), alias, arch, repository, mirror, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	addImageArchAliasFlag(command, &arch)
	command.Flags().StringVarP(&repository, "repo", "r", "", repositoryFlagHelp)
	command.Flags().BoolVar(&mirror, "mirror", false, mirrorFlagHelp)
	noFileCompletions(command, "mirror")
	return command
}

func newImageUpdateCommand(stderr io.Writer) *cobra.Command {
	var options imageUpdateCLIOptions
	command := &cobra.Command{
		Use:     "update [image[:channel]...]",
		Aliases: []string{"u"},
		Short:   "Refresh the catalog and fetch selected or cached images",
		Long: `Fetch and verify the configured repository's catalog, then download the
latest release for each cached family and architecture. Name an image to fetch
it even on the first run. A bare name updates its cached architectures, or
the host architecture for its first download. Existing VMs keep their bases.
Use --prune to remove unreferenced files after updating or --dry-run to
preview the operation without writing to the cache or catalog.`,
		Example: `  barn image update
  barn image update --dry-run
  barn image update d13i d13a
  barn image update --mirror --prune`,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: imageAliasCompletion,
		PreRunE:           validateOverrideFlags,
		RunE: func(command *cobra.Command, arguments []string) error {
			options.Selectors = arguments
			outcome, err := runImageUpdate(command.Context(), options, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	addArchitectureFlag(command, &options.Arch, "limit updates to one architecture")
	command.Flags().StringVarP(&options.Repository, "repo", "r", "", repositoryFlagHelp)
	command.Flags().BoolVar(&options.Mirror, "mirror", false, mirrorFlagHelp)
	command.Flags().BoolVarP(&options.DryRun, "dry-run", "d", false, "preview catalog and image updates without updating local catalog or downloading")
	command.Flags().BoolVar(&options.Prune, "prune", false, "remove unreferenced cached images and stale staging files after updating")
	noFileCompletions(command, "mirror", "dry-run", "prune")
	return command
}

func newImagePruneCommand(stderr io.Writer) *cobra.Command {
	var dryRun, apply bool
	var repository string
	command := &cobra.Command{
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
			outcome, err := runImagePrune(command.Context(), dryRun, apply, repository, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	command.Flags().BoolVarP(&dryRun, "dry-run", "d", false, "show unreferenced images and stale staging files without deleting")
	command.Flags().BoolVarP(&apply, "yes", "y", false, "delete the displayed unreferenced images and stale staging files")
	command.Flags().StringVarP(&repository, "repo", "r", "", repositoryOnlyFlagHelp)
	command.MarkFlagsMutuallyExclusive("dry-run", "yes")
	noFileCompletions(command, "dry-run", "yes")
	return command
}

func newImageSyncCommand(stderr io.Writer) *cobra.Command {
	var allowDowngrade bool
	var repository string
	command := &cobra.Command{
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
			outcome, err := runImageSync(command.Context(), arguments[0], allowDowngrade, repository, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	command.Flags().BoolVar(&allowDowngrade, "allow-downgrade", false, "activate a catalog older than the newest revision seen so far")
	command.Flags().StringVarP(&repository, "repo", "r", "", "repository whose active catalog is replaced")
	noFileCompletions(command, "allow-downgrade")
	return command
}

func newImageResetCommand() *cobra.Command {
	var repository string
	command := &cobra.Command{
		Use:   "reset",
		Short: "Restore the built-in catalog",
		Long: `Reactivate the catalog embedded in this Barn binary. Downgrade protection
still remembers the newest revision ever activated.`,
		Example: `  barn image reset
  barn --json image reset`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			outcome, err := runImageReset(command.Context(), repository)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	command.Flags().StringVarP(&repository, "repo", "r", "", "repository whose active catalog is reset to the built-in one")
	return command
}

func newImageImportCommand(stderr io.Writer) *cobra.Command {
	var options imageImportCLIOptions
	command := &cobra.Command{
		Use:     "import <path>",
		Aliases: []string{"i"},
		Short:   "Import and verify a local qcow2 image",
		Long: `Copy a regular local image into the private cache, verify an optional SHA-256,
and inspect its qcow2 metadata. Supplying --name registers an immutable local-
prefixed alias and therefore also requires --boot and --source-user. Use
--arch/-A with --name for a foreign-architecture image; this records the
architecture you specify, since qcow2 metadata cannot identify the guest CPU.`,
		Example: `  barn image import ./base.qcow2
  barn image import ./base.qcow2 --sha256 <digest>
  barn image import ./base.qcow2 --name local-dev --boot uefi --source-user dba
  barn image import ./amd64.qcow2 --name local-amd64 -A amd64 --boot uefi --source-user dba`,
		Args: cobra.ExactArgs(1),
		PreRunE: func(command *cobra.Command, args []string) error {
			if err := validateOverrideFlags(command, args); err != nil {
				return err
			}
			if options.Boot == "" {
				return nil
			}
			return validateChoice("--boot", options.Boot, "bios", "uefi")
		},
		RunE: func(command *cobra.Command, arguments []string) error {
			options.Path = arguments[0]
			outcome, err := runImageImport(command.Context(), options, stderr)
			if err != nil {
				return err
			}
			return collectCommandOutcome(command.Context(), outcome)
		},
	}
	command.Flags().StringVarP(&options.ExpectedSHA256, "sha256", "s", "", "optional expected SHA-256")
	command.Flags().StringVar(&options.Name, "name", "", "optional immutable local- prefixed alias")
	addArchitectureFlag(command, &options.Arch, "guest architecture recorded with --name (defaults to native)")
	command.Flags().StringVarP(&options.Boot, "boot", "b", "", "required with --name: bios or uefi")
	command.Flags().StringVarP(&options.SourceUser, "source-user", "u", "", "required with --name: source image login user")
	command.MarkFlagsRequiredTogether("name", "boot", "source-user")
	_ = command.RegisterFlagCompletionFunc("boot", enumFlagCompletion("bios", "uefi"))
	noFileCompletions(command, "sha256", "name", "source-user")
	return command
}
