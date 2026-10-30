package config

import (
	"testing"

	"github.com/pgsty/barn/internal/spec"
)

func TestParseSize(t *testing.T) {
	t.Parallel()
	for input, expected := range map[string]int64{
		"4GiB":    4 * spec.GiB,
		"4gib":    4 * spec.GiB,
		"8G":      8 * spec.GiB,
		"512m":    512 << 20,
		"64GB":    64_000_000_000,
		"64gb":    64_000_000_000,
		"1TB":     1_000_000_000_000,
		"1TiB":    1 << 40,
		"1024KiB": 1024 << 10,
	} {
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
