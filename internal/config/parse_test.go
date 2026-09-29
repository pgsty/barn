package config

import (
	"testing"

	"github.com/pgsty/barn/internal/spec"
)

func TestParseSize(t *testing.T) {
	t.Parallel()
	for input, expected := range map[string]int64{"4GiB": 4 * spec.GiB, "64GB": 64_000_000_000, "512MiB": 512 << 20} {
		got, err := ParseSize(input)
		if err != nil || got != expected {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", input, got, err, expected)
		}
	}
	for _, input := range []string{"", "4", "1.5GiB", "-1GiB", "0GiB", "999999999999999999999GiB"} {
		if _, err := ParseSize(input); err == nil {
			t.Errorf("invalid size %q accepted", input)
		}
	}
}
