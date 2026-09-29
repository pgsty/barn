package main

import (
	"context"
	"io"
)

// run is the pre-cancellation entry point kept for tests that exercise a
// command end to end without a signal context.
func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}
