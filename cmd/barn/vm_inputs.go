package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pgsty/barn/internal/config"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/spec"
	"github.com/spf13/cobra"
)

type vmInputs struct {
	environmentRead                    bool
	Image, Arch, CPU, Mem, Scale, Disk string
	Sources                            map[string]string
	Resources                          config.ResourceOverrides
}

type vmInputField struct {
	name, short, environment, help string
	value                          *string
}

func (input *vmInputs) fields() []vmInputField {
	return []vmInputField{
		{"image", "I", "BARN_IMAGE", "override vm_image/vm_version for selected nodes", &input.Image},
		{"arch", "A", "BARN_ARCH", "override deployment vm_arch: native, amd64, or arm64", &input.Arch},
		{"cpu", "C", "BARN_CPU", "base vCPU count before scale", &input.CPU},
		{"mem", "M", "BARN_MEM", "base memory before scale (bare integer: MiB; e.g. 4GiB)", &input.Mem},
		{"scale", "S", "BARN_SCALE", "positive integer multiplying base CPU and memory only", &input.Scale},
		{"disk", "D", "BARN_DISK", "absolute root disk size (bare integer: GiB; e.g. 128GiB)", &input.Disk},
	}
}

func addVMFlags(command *cobra.Command, input *vmInputs, disk bool) {
	for _, field := range input.fields() {
		if field.name == "disk" && !disk {
			continue
		}
		command.Flags().StringVarP(field.value, field.name, field.short, "", field.help+"; overrides $"+field.environment)
		noFileCompletions(command, field.name)
	}
	_ = command.RegisterFlagCompletionFunc("image", imageAliasCompletion)
	_ = command.RegisterFlagCompletionFunc("arch", enumFlagCompletion("native", "amd64", "x86_64", "x64", "arm64", "aarch64", "a64"))
}

func (input vmInputs) any() bool {
	for _, field := range input.fields() {
		if *field.value != "" {
			return true
		}
	}
	return false
}

func (input vmInputs) resolve(environment, disk bool) (vmInputs, error) {
	environment = environment && !input.environmentRead
	input.environmentRead = input.environmentRead || environment
	sources := make(map[string]string)
	for key, value := range input.Sources {
		sources[key] = value
	}
	input.Sources = sources
	input.Resources = config.ResourceOverrides{Sources: sources}
	for _, field := range input.fields() {
		if field.name == "disk" && !disk {
			continue
		}
		if *field.value == "" && environment {
			if value := strings.TrimSpace(os.Getenv(field.environment)); value != "" {
				*field.value = value
				sources[field.name] = "env " + field.environment
			}
		}
		if *field.value == "" {
			continue
		}
		if sources[field.name] == "" {
			sources[field.name] = "CLI --" + field.name
		}
		value := strings.TrimSpace(*field.value)
		var err error
		switch field.name {
		case "image":
			*field.value, err = image.CanonicalReference(value)
		case "arch":
			*field.value, err = platform.NormalizeArch(value)
		case "cpu", "scale":
			var parsed int64
			parsed, err = strconv.ParseInt(value, 10, 64)
			if err == nil && (strings.Trim(value, "0123456789") != "" || parsed < 1 || parsed > 256) {
				err = fmt.Errorf("must be an integer between 1 and 256")
			}
			if field.name == "cpu" {
				input.Resources.CPU = &parsed
			} else {
				input.Resources.Scale = &parsed
			}
		case "mem", "disk":
			unit := int64(1 << 20)
			if field.name == "disk" {
				unit = spec.GiB
			}
			var parsed int64
			parsed, err = config.ParseResourceSize(value, unit)
			if err == nil && field.name == "mem" && parsed < 512<<20 {
				err = fmt.Errorf("must be at least 512 MiB")
			}
			if field.name == "mem" {
				input.Resources.Memory = &parsed
			} else {
				input.Resources.Disk = &parsed
			}
		}
		if err != nil {
			if field.name == "image" {
				return input, fmt.Errorf("invalid image reference for %s: %w", strings.TrimPrefix(sources[field.name], "CLI "), err)
			}
			return input, fmt.Errorf("%s: %w", strings.TrimPrefix(sources[field.name], "CLI "), err)
		}
	}
	return input, nil
}

func (input vmInputs) templateOptions() config.TemplateOptions {
	return config.TemplateOptions{Image: input.Image, Arch: input.Arch, Resources: input.Resources}
}

func (input vmInputs) arguments(disk bool) []string {
	var result []string
	for _, field := range input.fields() {
		if field.name == "disk" && !disk {
			continue
		}
		if *field.value != "" {
			result = append(result, "--"+field.name, *field.value)
		}
	}
	return result
}
