package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const (
	exitOK         = 0
	exitRuntime    = 1
	exitUsage      = 2
	exitCapability = 3
	exitConflict   = 4
	exitPartial    = 5
	exitResource   = 6
	exitIntegrity  = 7
	exitCancelled  = 130
)

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	prepared, preparedStdout, preparedStderr, err := prepareOutput(args, stdout, stderr)
	if err != nil {
		errorf(stderr, "%v", err)
		return exitUsage
	}
	if len(prepared) > 0 {
		debugf(preparedStderr, "command=%s format=%s stdout_tty=%t stderr_tty=%t", prepared[0], outputFormatFor(preparedStdout), writerTTY(preparedStdout), writerTTY(preparedStderr))
	}
	return executeCLI(ctx, prepared, preparedStdout, preparedStderr)
}

// interruptSignals is the one delivery channel for SIGINT, SIGHUP, and SIGTERM. It is
// package-level so a child that must own SIGINT for a while (an SSH session)
// can suspend and re-arm the same registration.
var interruptSignals = make(chan os.Signal, 1)

// interruptContext cancels the returned context on the first interrupt,
// hangup, or termination signal. The stop function unregisters the signals and
// cancels the context.
func interruptContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	signal.Notify(interruptSignals, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		select {
		case <-interruptSignals:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(interruptSignals)
		cancel()
	}
}

func main() {
	ctx, stop := interruptContext(context.Background())
	code := runContext(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
