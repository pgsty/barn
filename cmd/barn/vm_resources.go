package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/pgsty/barn/internal/config"
	"github.com/pgsty/barn/internal/spec"
)

// Non-MiB sizes retain their exact byte count rather than a rounded GiB label.
func compactSpec(cpu int, memory int64) string {
	if cpu <= 0 || memory <= 0 {
		return "-"
	}
	if memory%(1<<20) != 0 {
		return fmt.Sprintf("%dc%db", cpu, memory)
	}
	return strconv.Itoa(cpu) + "c" + strconv.FormatFloat(float64(memory/(1<<20))/1024, 'f', -1, 64) + "g"
}

func resourceInputSources(resources []config.ResourceResolution, input vmInputs, nodes []string) []config.ResourceResolution {
	var result []config.ResourceResolution
	for _, resource := range resources {
		if len(nodes) != 0 && !slices.Contains(nodes, resource.Node) {
			continue
		}
		resource.Sources = maps.Clone(resource.Sources)
		if resource.Sources == nil {
			resource.Sources = make(map[string]string)
		}
		for key, source := range input.Sources {
			resource.Sources[key] = source
		}
		result = append(result, resource)
	}
	return result
}

func applyStateResources(resolved spec.Resolved, nodes []string, input vmInputs) (spec.Resolved, []config.ResourceResolution, error) {
	if input.Resources.Scale != nil {
		return resolved, nil, fmt.Errorf("--scale or BARN_SCALE requires an inventory with base resources; pass -f <file>")
	}
	resolved.Nodes = slices.Clone(resolved.Nodes)
	var resources []config.ResourceResolution
	for index := range resolved.Nodes {
		node := &resolved.Nodes[index]
		if len(nodes) != 0 && !slices.Contains(nodes, node.Name) {
			continue
		}
		sources := make(map[string]string)
		for _, name := range []string{"cpu", "mem", "disk", "scale", "image", "arch"} {
			sources[name] = "applied state"
		}
		value, err := config.ResolveResources(config.ResourceResolution{Node: node.Name, CPU: int64(node.CPUs), Memory: node.Memory, Scale: 1, Disk: node.RootDisk, Sources: sources}, input.Resources)
		if err != nil {
			return resolved, nil, err
		}
		node.CPUs, node.Memory, node.RootDisk = int(value.EffectiveCPU), value.EffectiveMemory, value.Disk
		resources = append(resources, value)
	}
	return resolved, resourceInputSources(resources, input, nodes), nil
}

func printSpecificationTable(out io.Writer, resolved spec.Resolved, nodes []string) {
	var rows [][]string
	for _, node := range resolved.Nodes {
		if len(nodes) != 0 && !slices.Contains(nodes, node.Name) {
			continue
		}
		rows = append(rows, []string{node.Name, node.Address, compactSpec(node.CPUs, node.Memory), planSize(node.RootDisk)})
	}
	if len(rows) > 0 {
		printTable(out, []string{"NAME", "ADDRESS", "SPEC", "ROOT"}, rows, -1)
	}
}

func printResourceCalculations(out io.Writer, resources []config.ResourceResolution) {
	for _, resource := range resources {
		if resource.Scale == 1 && !verboseOutput(out) {
			continue
		}
		textField(out, 12, "resources", fmt.Sprintf("%s: %s × %d = %s (cpu: %s; mem: %s; scale: %s)", resource.Node,
			compactSpec(int(resource.CPU), resource.Memory), resource.Scale, compactSpec(int(resource.EffectiveCPU), resource.EffectiveMemory),
			resource.Sources["cpu"], resource.Sources["mem"], resource.Sources["scale"]))
	}
}

func sameResourceBases(left, right []config.ResourceResolution) bool {
	if len(left) != len(right) {
		return false
	}
	byName := make(map[string]config.ResourceResolution)
	for _, value := range left {
		byName[value.Node] = value
	}
	for _, value := range right {
		other, ok := byName[value.Node]
		if !ok || other.CPU != value.CPU || other.Memory != value.Memory || other.Scale != value.Scale || other.Disk != value.Disk {
			return false
		}
	}
	return true
}

func templateResourceOverrides(resources []config.ResourceResolution) config.ResourceOverrides {
	if len(resources) == 0 {
		return config.ResourceOverrides{}
	}
	first := resources[0]
	return config.ResourceOverrides{CPU: &first.CPU, Memory: &first.Memory, Scale: &first.Scale, Disk: &first.Disk}
}

func templateVMInputs(resources []config.ResourceResolution, image, arch string) vmInputs {
	input := vmInputs{Image: image, Arch: arch}
	if len(resources) != 0 {
		first := resources[0]
		input.CPU, input.Mem, input.Scale, input.Disk = strconv.FormatInt(first.CPU, 10), strconv.FormatInt(first.Memory, 10)+"B", strconv.FormatInt(first.Scale, 10), strconv.FormatInt(first.Disk, 10)+"B"
	}
	return input
}

func printResourceOverrides(out io.Writer, input vmInputs) {
	for _, field := range input.fields() {
		if field.name == "image" || field.name == "arch" || *field.value == "" {
			continue
		}
		textField(out, 10, field.name, *field.value+" ("+strings.TrimSpace(input.Sources[field.name])+")")
	}
}
