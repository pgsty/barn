package config

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
)

// ResourceOverrides are applied to inventory bases before scaling. Nil means
// absent; CLI/environment parsers reject explicit zero before constructing them.
type ResourceOverrides struct {
	CPU, Memory, Scale, Disk *int64
	Sources                  map[string]string
}

type InventoryOptions struct {
	Resources ResourceOverrides
	Nodes     []string
}

// ResourceResolution explains input values without changing VM identity or
// applied-state hashes. CPU and Memory remain bases even after evaluation.
type ResourceResolution struct {
	Node            string            `json:"node"`
	CPU             int64             `json:"base_cpu"`
	Memory          int64             `json:"base_memory_bytes"`
	Scale           int64             `json:"scale"`
	Disk            int64             `json:"root_disk_bytes"`
	EffectiveCPU    int64             `json:"cpus"`
	EffectiveMemory int64             `json:"memory_bytes"`
	Sources         map[string]string `json:"sources"`
}

func ParseResourceSize(value string, bareUnit int64) (int64, error) {
	value = strings.TrimSpace(value)
	if digits := strings.Trim(value, "0123456789"); value != "" && digits == "" {
		number, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid or overflowing size %q", value)
		}
		return scaleSize(number, bareUnit)
	}
	return ParseSize(value)
}

func ResolveResources(base ResourceResolution, overrides ResourceOverrides) (ResourceResolution, error) {
	base.Sources = maps.Clone(base.Sources)
	if base.Sources == nil {
		base.Sources = make(map[string]string)
	}
	for _, field := range []struct {
		name   string
		value  *int64
		target *int64
	}{
		{"cpu", overrides.CPU, &base.CPU}, {"mem", overrides.Memory, &base.Memory},
		{"scale", overrides.Scale, &base.Scale}, {"disk", overrides.Disk, &base.Disk},
	} {
		if field.value != nil {
			*field.target = *field.value
			base.Sources[field.name] = overrides.Sources[field.name]
		}
	}
	if base.CPU < 1 || base.CPU > maxVirtualCPUs || base.Scale < 1 || base.Scale > maxVirtualCPUs {
		return base, fmt.Errorf("node %s vm_cpu and vm_scale must be integers between 1 and %d", base.Node, maxVirtualCPUs)
	}
	if base.CPU > maxVirtualCPUs/base.Scale {
		return base, fmt.Errorf("node %s effective vm_cpu is %d, above the %d vCPU limit; reduce vm_cpu or vm_scale", base.Node, base.CPU*base.Scale, maxVirtualCPUs)
	}
	if base.Memory < minMemory || base.Disk <= 0 {
		return base, fmt.Errorf("node %s vm_mem must be at least 512 MiB and vm_disk must be positive", base.Node)
	}
	memory, err := scaleSize(base.Memory, base.Scale)
	if err != nil {
		return base, fmt.Errorf("node %s effective vm_mem overflows; reduce vm_mem or vm_scale", base.Node)
	}
	base.EffectiveCPU, base.EffectiveMemory = base.CPU*base.Scale, memory
	return base, nil
}
