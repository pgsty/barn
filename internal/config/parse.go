// Package config contains the strict user-facing scalar parsers shared by the
// CLI and inventory decoding.
package config

import (
	"fmt"
	"strconv"
	"strings"
)

var sizeUnits = []struct {
	name       string
	multiplier int64
}{
	{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
	{"B", 1},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
}

func ParseSize(value string) (int64, error) {
	value = strings.TrimSpace(value)
	for _, unit := range sizeUnits {
		if len(value) < len(unit.name) || !strings.EqualFold(value[len(value)-len(unit.name):], unit.name) {
			continue
		}
		number := strings.TrimSpace(value[:len(value)-len(unit.name)])
		if number == "" || strings.ContainsAny(number, ".+-") {
			return 0, fmt.Errorf("size %q must be a positive integer plus unit", value)
		}
		parsed, err := strconv.ParseInt(number, 10, 64)
		if err != nil || parsed <= 0 || parsed > (1<<63-1)/unit.multiplier {
			return 0, fmt.Errorf("invalid or overflowing size %q", value)
		}
		return parsed * unit.multiplier, nil
	}
	return 0, fmt.Errorf("size %q requires B, K/M/G/T, KiB/MiB/GiB/TiB, or KB/MB/GB/TB (case-insensitive)", value)
}

func scaleSize(value, multiplier int64) (int64, error) {
	if value <= 0 || value > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("invalid or overflowing size %d", value)
	}
	return value * multiplier, nil
}
