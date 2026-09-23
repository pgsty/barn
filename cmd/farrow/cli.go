package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/pgsty/farrow/internal/config"
	"github.com/pgsty/farrow/internal/image"
	"github.com/pgsty/farrow/internal/spec"
	"github.com/pgsty/farrow/internal/state"
	"github.com/spf13/cobra"
)

type commandRunner func(context.Context, []string, io.Writer, io.Writer) (commandOutcome, error)

const (
	configDiscoverySummary = "farrow.yml, farrow.yaml, pigsty.yml, or pigsty.yaml"
	repositoryFlagHelp     = "image repository URL or directory; overrides --mirror and $FARROW_REPO"
	repositoryOnlyFlagHelp = "image repository URL or directory; overrides $FARROW_REPO"
	mirrorFlagHelp         = "use the China official repository at repo.pigsty.cc; overridden by --repo"
)

func init() {
	// Help follows the operational workflow: discovery and safe daily actions
	// precede destructive or recovery operations. Cobra's alphabetical default
	// would put destroy first. Set the package-global switch once, before any
	// command construction or concurrent test execution.
	cobra.EnableCommandSorting = false
}

func helpBeforeSeparator(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--" {
			return false
		}
		if argument == "-h" || argument == "--help" {
			return true
		}
	}
	return false
}

func rawOperation(use, short, long, example string, stdout, stderr io.Writer, runner commandRunner) *cobra.Command {
	command := &cobra.Command{
		Use:                   use,
		Short:                 short,
		Long:                  long,
		Example:               example,
		Args:                  cobra.ArbitraryArgs,
		DisableFlagParsing:    true,
		DisableFlagsInUseLine: true,
	}
	command.RunE = func(command *cobra.Command, arguments []string) error {
		if helpBeforeSeparator(arguments) {
			return command.Help()
		}
		outcome, err := runner(command.Context(), arguments, stdout, stderr)
		if err != nil {
			return err
		}
		return collectCommandOutcome(command.Context(), outcome)
	}
	return command
}

func enumFlagCompletion(values ...string) cobra.CompletionFunc {
	return func(_ *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
		matches := make([]string, 0, len(values))
		for _, value := range values {
			if strings.HasPrefix(value, prefix) {
				matches = append(matches, value)
			}
		}
		return matches, cobra.ShellCompDirectiveNoFileComp
	}
}

func validateChoice(option, value string, choices ...string) error {
	for _, choice := range choices {
		if value == choice {
			return nil
		}
	}
	return fmt.Errorf("%s must be one of %s, got %q", option, strings.Join(choices, ", "), value)
}

func templateArgs(command *cobra.Command, arguments []string) error {
	if err := cobra.MaximumNArgs(1)(command, arguments); err != nil {
		return err
	}
	if len(arguments) == 1 && !config.ValidTemplate(arguments[0]) {
		return fmt.Errorf("unknown lab template %q; available: %s", arguments[0], strings.Join(config.TemplateNames(), ", "))
	}
	return nil
}

func suggestingNoArgs(command *cobra.Command, arguments []string) error {
	if len(arguments) == 0 {
		return nil
	}
	message := fmt.Sprintf("unknown command %q for %q", arguments[0], command.CommandPath())
	if command.SuggestionsMinimumDistance <= 0 {
		command.SuggestionsMinimumDistance = 2
	}
	if suggestions := command.SuggestionsFor(arguments[0]); len(suggestions) > 0 {
		message += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
	}
	return errors.New(message)
}

func resolvedNodeNames(command *cobra.Command, preferConfig bool) []string {
	var resolved spec.Resolved
	if preferConfig {
		explicit := ""
		if command.Flags().Lookup("file") != nil {
			explicit, _ = command.Flags().GetString("file")
		}
		file, _, err := config.Discover("", explicit)
		if err == nil {
			candidate, resolveErr := file.Resolve()
			if resolveErr != nil {
				return nil
			}
			return sortedNodeNames(candidate)
		}
		// An explicit or discovered-but-invalid inventory must not silently
		// produce suggestions from unrelated applied state. Fall back only when
		// discovery genuinely found no desired-state file.
		if explicit != "" || !errors.Is(err, config.ErrNoConfig) {
			return nil
		}
	}
	if candidate, err := currentDeploymentResolved(); err == nil {
		resolved = candidate
	}
	return sortedNodeNames(resolved)
}

func sortedNodeNames(resolved spec.Resolved) []string {
	names := make([]string, 0, len(resolved.Nodes))
	for _, node := range resolved.Nodes {
		names = append(names, node.Name)
	}
	sort.Strings(names)
	return names
}

func nodeCompletion(preferConfig, firstOnly bool) cobra.CompletionFunc {
	return func(command *cobra.Command, arguments []string, prefix string) ([]string, cobra.ShellCompDirective) {
		if firstOnly && len(arguments) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		selected := make(map[string]struct{}, len(arguments))
		for _, argument := range arguments {
			selected[argument] = struct{}{}
		}
		matches := make([]string, 0)
		for _, name := range resolvedNodeNames(command, preferConfig) {
			if _, exists := selected[name]; !exists && strings.HasPrefix(name, prefix) {
				matches = append(matches, name)
			}
		}
		return matches, cobra.ShellCompDirectiveNoFileComp
	}
}

func imageAliasCompletion(command *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	catalog := image.EmbeddedCatalog()
	aliases := make([]string, 0, len(catalog.Images)*2)
	if dataRoot, err := state.ResolveDataRoot(); err == nil {
		repository, _ := command.Flags().GetString("repo")
		mirror := false
		if command.Flags().Lookup("mirror") != nil {
			mirror, _ = command.Flags().GetBool("mirror")
		}
		repository, explicit, repositoryErr := image.ResolveRepository(repository, mirror)
		if active, _, currentErr := (image.ManifestManager{DataRoot: dataRoot, Repository: repository, AllowUnsigned: repositoryErr == nil && explicit && image.RepositoryAllowsUnsigned(repository)}).Current(); repositoryErr == nil && currentErr == nil {
			catalog = active
		}
		if localAliases, localErr := image.LocalAliasNames(dataRoot); localErr == nil {
			aliases = append(aliases, localAliases...)
		}
	}
	for name, record := range catalog.Images {
		aliases = append(aliases, name)
		aliases = append(aliases, record.Aliases...)
		for channel := range record.Channels {
			aliases = append(aliases, name+":"+channel)
		}
		for version := range record.Versions {
			aliases = append(aliases, name+"@"+version)
		}
	}
	sort.Strings(aliases)
	matches := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if strings.HasPrefix(alias, prefix) {
			matches = append(matches, alias)
		}
	}
	return matches, cobra.ShellCompDirectiveNoFileComp
}

func templateCompletion(_ *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	values := make([]string, 0)
	for _, name := range config.TemplateNames() {
		if strings.HasPrefix(name, prefix) {
			values = append(values, name)
		}
	}
	return values, cobra.ShellCompDirectiveNoFileComp
}

func configureHelpOnly(command *cobra.Command, message string, stdout, stderr io.Writer) {
	command.Args = suggestingNoArgs
	command.RunE = func(command *cobra.Command, _ []string) error {
		// A namespace invoked without a subcommand is a usage error, and a
		// presentation flag must never change that. Only the rendering differs:
		// --json emits the machine-readable failure, while the plain form still
		// prints the full help on stdout because that is what a person came for.
		// `farrow --help` remains the deliberate, successful way to ask for help.
		if command.Parent() == nil {
			// Bare `farrow` is someone looking for a starting point, not a mistake.
			actions := welcomeActions()
			return collectCommandOutcome(command.Context(), commandOutcome{payload: struct {
				Actions []welcomeAction `json:"actions"`
			}{actions}, text: func(out, _ io.Writer) error {
				printWelcome(out, actions)
				return nil
			}})
		}
		if !structuredOutput(stdout) {
			if err := command.Help(); err != nil {
				return newRuntimeError(err)
			}
		}
		return newUsageError(errors.New(message))
	}

	// RunE makes a namespace runnable so Cobra validates misspelled children.
	// Hide that implementation detail while rendering help, leaving one clean
	// "command [subcommand]" usage line instead of a duplicate bare command.
	defaultHelp := command.HelpFunc()
	command.SetHelpFunc(func(current *cobra.Command, arguments []string) {
		if current != command {
			defaultHelp(current, arguments)
			return
		}
		runE := command.RunE
		command.RunE = nil
		defer func() { command.RunE = runE }()
		defaultHelp(current, arguments)
	})
}

func subcommandGroup(use, short, long, example string, stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{
		Use:     use,
		Short:   short,
		Long:    long,
		Example: example,
	}
	configureHelpOnly(command, fmt.Sprintf("farrow %s requires a subcommand", use), stdout, stderr)
	return command
}

// commandFlagValues uses Cobra's flag definitions so presentation flags never
// consume a filename or other option value. Remote arguments stop at --.
func commandFlagValues(args []string) map[int]bool {
	command := newRootCommand(io.Discard, io.Discard)
	values := make(map[int]bool)
	for i, argument := range args {
		if values[i] {
			continue
		}
		if argument == "--" {
			break
		}
		if strings.HasPrefix(argument, "--") {
			name, _, inline := strings.Cut(strings.TrimPrefix(argument, "--"), "=")
			if flag := command.Flags().Lookup(name); flag != nil && flag.NoOptDefVal == "" && !inline {
				values[i+1] = true
			}
		} else if strings.HasPrefix(argument, "-") {
			for j := 1; j < len(argument); j++ {
				flag := command.Flags().ShorthandLookup(argument[j : j+1])
				if flag == nil {
					break
				}
				if flag.NoOptDefVal == "" {
					if j == len(argument)-1 {
						values[i+1] = true
					}
					break
				}
			}
		} else {
			for _, child := range command.Commands() {
				if child.Name() == argument || child.HasAlias(argument) {
					command = child
					break
				}
			}
		}
	}
	return values
}

func configureAliasDiscovery(command *cobra.Command) {
	mappings := make([]string, 0)
	for _, child := range command.Commands() {
		configureAliasDiscovery(child)
		if child.Hidden {
			continue
		}
		for _, alias := range child.Aliases {
			mappings = append(mappings, alias+"="+child.Name())
			command.ValidArgs = append(command.ValidArgs, alias+"\tAlias for "+child.Name())
		}
	}
	if len(mappings) != 0 {
		var summary strings.Builder
		summary.WriteString("\n\nCommand aliases:\n  ")
		lineWidth := 2
		for index, mapping := range mappings {
			separator := ""
			if index != 0 {
				separator = ", "
			}
			if lineWidth+len(separator)+len(mapping) > 88 {
				summary.WriteString(",\n  ")
				lineWidth = 2
				separator = ""
			}
			summary.WriteString(separator)
			summary.WriteString(mapping)
			lineWidth += len(separator) + len(mapping)
		}
		summary.WriteByte('.')
		command.Example = strings.TrimRight(command.Example, "\n ") + summary.String()
	}
}
