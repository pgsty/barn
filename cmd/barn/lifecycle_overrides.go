package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/pgsty/barn/internal/failure"
	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/pgsty/barn/internal/spec"
	"github.com/spf13/cobra"
)

func validateOverrideFlags(command *cobra.Command, _ []string) error {
	for _, flag := range []string{"image", "arch", "architecture", "cpu", "mem", "scale", "disk"} {
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
	printResourceOverrides(stderr, options.vmInputs)
	if options.Image != "" {
		textField(stderr, 10, "image", options.Image+" ("+options.Sources["image"]+"; to keep it, set vm_image in the inventory and remove any superseded vm_version)")
	}
	if options.Arch != "" {
		textField(stderr, 10, "arch", options.Arch+" ("+options.Sources["arch"]+"; to keep it, set vm_arch in all.vars)")
	}
}

func lifecycleOverrideFlags(options lifecycleRetryOptions) string {
	flags := ""
	for _, argument := range options.vmInputs.arguments(true) {
		flags += " " + shellQuote(argument)
	}
	return flags
}

func lifecycleOverrideRecovery(err error, source string, nodes []string, options lifecycleRetryOptions) error {
	if !options.vmInputs.any() {
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
