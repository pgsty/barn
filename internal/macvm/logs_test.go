package macvm

import (
	"strings"
	"testing"
)

func TestLogTailHonorsRequestedLines(t *testing.T) {
	longLine := strings.Repeat("x", (1<<20)+7)
	for _, test := range []struct {
		name, input, want string
		lines             int
	}{
		{"empty", "", "", 100},
		{"terminated", "one\ntwo\nthree\n", "two\nthree\n", 2},
		{"unterminated", "one\ntwo\nthree", "two\nthree\n", 2},
		{"blank", "one\n\n", "\n", 1},
		{"more than file", "one\ntwo\n", "one\ntwo\n", 100},
		{"chunk boundary", strings.Repeat("x", (32<<10)-1) + "\ntail\n", "tail\n", 1},
		{"long diagnostic", "older\n" + longLine + "\nlast\n", longLine + "\nlast\n", 2},
		{"over old line cap", strings.Repeat("line\n", 12000), strings.Repeat("line\n", 11000), 11000},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := tailLog(strings.NewReader(test.input), int64(len(test.input)), test.lines)
			if err != nil || got != test.want {
				t.Fatalf("tail: got %d bytes, want %d bytes; %v", len(got), len(test.want), err)
			}
		})
	}
}
