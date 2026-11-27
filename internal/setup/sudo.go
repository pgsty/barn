package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
)

const sudoBinary = "/usr/bin/sudo"

// SudoRunner wraps a base execx.Runner with non-interactive sudo (-n),
// optionally preserving named proxy variables across the privilege boundary.
type SudoRunner struct {
	Base                execx.Runner
	PreserveEnvironment []string
}

func (r SudoRunner) Run(ctx context.Context, binary string, args ...string) (execx.Result, error) {
	if r.Base == nil {
		return execx.Result{}, errors.New("sudo runner has no base runner")
	}
	sudoArgs := []string{"-n"}
	if len(r.PreserveEnvironment) > 0 {
		sudoArgs = append(sudoArgs, "--preserve-env="+strings.Join(r.PreserveEnvironment, ","))
	}
	sudoArgs = append(sudoArgs, "--", binary)
	sudoArgs = append(sudoArgs, args...)
	return r.Base.Run(ctx, sudoBinary, sudoArgs...)
}

// RootRunner returns a runner that executes privileged commands via sudo when
// the current process is not already root.
func RootRunner(base execx.Runner) execx.Runner {
	if os.Geteuid() == 0 {
		return base
	}
	return SudoRunner{Base: base}
}

// PackageManagerRunner preserves only the standard proxy controls across sudo.
// Homebrew runs as the invoking user and already inherits them directly;
// APT/DNF otherwise commonly lose them at the privilege boundary.
func PackageManagerRunner(base execx.Runner) execx.Runner {
	if os.Geteuid() == 0 {
		return base
	}
	return SudoRunner{Base: base, PreserveEnvironment: ProxyEnvironmentNames()}
}

// SudoSession asks for the user's password at most once per command.
// The first privileged step announces its reason and prompts; a background
// keeper then refreshes the cached credential every minute until Close, so no
// later step prompts again even when slow downloads outlive sudo's timeout.
type SudoSession struct {
	Base         execx.Runner
	Interactive  bool
	Stdin        io.Reader
	Output       io.Writer
	Announce     func(reason string)
	BeforePrompt func() func()

	acquired bool
	stop     context.CancelFunc
}

func (s *SudoSession) Ensure(ctx context.Context, reason string) error {
	if s.BeforePrompt != nil {
		resume := s.BeforePrompt()
		defer resume()
	}
	if os.Geteuid() == 0 || s.acquired {
		return nil
	}
	if reason != "" && s.Announce != nil {
		s.Announce(reason)
	}
	if s.Interactive {
		command := exec.CommandContext(ctx, sudoBinary, "-v")
		command.Stdin = s.Stdin
		command.Stdout = s.Output
		command.Stderr = s.Output
		if err := command.Run(); err != nil {
			return failure.New(failure.Capability, fmt.Errorf("sudo authentication failed: %w", err)).Because("sudo_unavailable")
		}
	} else if _, err := s.Base.Run(ctx, sudoBinary, "-n", "-v"); err != nil {
		// `sudo -n -v` can still demand authentication under a NOPASSWD: ALL
		// policy because it validates a timestamp without running a command.
		// Prove the command path separately before rejecting automation hosts.
		if _, commandErr := s.Base.Run(ctx, sudoBinary, "-n", "--", "/usr/bin/true"); commandErr != nil {
			return failure.New(failure.Capability, errors.New("sudo needs a password, but stdin is not a terminal")).Because("sudo_unavailable").Then("rerun in a terminal, or allow passwordless sudo for this user")
		}
	}
	keeperContext, cancel := context.WithCancel(ctx)
	s.stop = cancel
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-keeperContext.Done():
				return
			case <-ticker.C:
				_, _ = s.Base.Run(keeperContext, sudoBinary, "-n", "-v")
			}
		}
	}()
	s.acquired = true
	return nil
}

func (s *SudoSession) Close() {
	if s.stop != nil {
		s.stop()
	}
}
