package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/platform"
	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/pgsty/barn/internal/spec"
	"github.com/spf13/cobra"
)

func validateOverrideFlags(command *cobra.Command, _ []string) error {
	for _, flag := range []string{"image", "arch", "architecture"} {
		if command.Flags().Changed(flag) {
			value, _ := command.Flags().GetString(flag)
			if strings.TrimSpace(value) == "" {
				return newUsageError(fmt.Errorf("--%s must not be empty", flag))
			}
		}
	}
	return nil
}

func addArchitectureFlag(command *cobra.Command, value *string, help string) {
	command.Flags().StringVarP(value, "arch", "A", "", help+": native, amd64 (x86_64, x64), or arm64 (aarch64, a64)")
	_ = command.RegisterFlagCompletionFunc("arch", enumFlagCompletion("native", "amd64", "x86_64", "x64", "arm64", "aarch64", "a64"))
}

func normalizeLifecycleOverrides(options lifecycleOptions) (lifecycleOptions, error) {
	if options.Image != "" {
		if strings.TrimSpace(options.Image) == "" {
			return options, errors.New("--image must not be empty")
		}
		var err error
		options.Image, err = image.CanonicalReference(options.Image)
		if err != nil {
			return options, fmt.Errorf("invalid image reference for --image: %w", err)
		}
	}
	if options.Arch != "" {
		var err error
		options.Arch, err = platform.NormalizeArch(options.Arch)
		if err != nil {
			return options, fmt.Errorf("--arch: %w", err)
		}
	}
	return options, nil
}

// Overrides change desired state in memory. Keep the default image unchanged:
// setting vm_image in an inventory changes node images, not the deployment
// envelope, and scoped operations must preserve unselected peers.
func applyLifecycleOverrides(resolved spec.Resolved, nodes []string, options lifecycleOptions) spec.Resolved {
	if options.Image != "" {
		resolved.Nodes = slices.Clone(resolved.Nodes)
		for index := range resolved.Nodes {
			node := &resolved.Nodes[index]
			if len(nodes) != 0 && !slices.Contains(nodes, node.Name) {
				continue
			}
			node.Image = options.Image
			if node.Image == resolved.Image {
				node.Image = ""
			}
		}
	}
	if options.Arch != "" {
		resolved.Arch = options.Arch
		if resolved.Arch == "native" {
			resolved.Arch = ""
		}
	}
	return resolved
}

func printLifecycleOverrides(stderr io.Writer, options lifecycleOptions) {
	if options.Image != "" {
		textField(stderr, 10, "image", options.Image+" (command override; to keep it, set vm_image in the inventory and remove any superseded vm_version)")
	}
	if options.Arch != "" {
		textField(stderr, 10, "arch", options.Arch+" (command override; to keep it, set vm_arch in all.vars)")
	}
}

func lifecycleOverrideFlags(options lifecycleRetryOptions) string {
	flags := ""
	if options.Image != "" {
		flags += " --image " + shellQuote(options.Image)
	}
	if options.Arch != "" {
		flags += " --arch " + shellQuote(options.Arch)
	}
	return flags
}

func lifecycleOverrideRecovery(err error, source string, nodes []string, options lifecycleRetryOptions) error {
	if options.Image == "" && options.Arch == "" {
		return err
	}
	if errors.Is(err, privatevm.ErrWholeDeploymentRequired) {
		return failure.WithNext(err, "select all existing nodes together; review "+lifecycleNodeCommand("plan", source, nil, options)+" before recreating")
	}
	if errors.Is(err, privatevm.ErrRecreateRequired) {
		return failure.WithNext(err, lifecycleNodeCommand("plan", source, nodes, options)+", then "+lifecycleNodeCommand("recreate", source, nodes, options))
	}
	if errors.Is(err, privatevm.ErrResizeRequired) {
		return failure.WithNext(err, lifecycleNodeCommand("resize", source, nodes, options))
	}
	return err
}
