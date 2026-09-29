// barn-hosts-helper is the minimal root-owned publisher for a reviewed,
// marker-bounded native hosts-file transition.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pgsty/barn/internal/hostconfig"
)

func run(args []string) error {
	flags := flag.NewFlagSet("barn-hosts-helper", flag.ContinueOnError)
	target := flags.String("target", "", "native hosts path")
	staging := flags.String("staging", "", "reviewed staging file")
	action := flags.String("action", "", "install or uninstall")
	before := flags.String("before-sha256", "", "reviewed target digest")
	after := flags.String("after-sha256", "", "reviewed result digest")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("invalid helper arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("barn-hosts-helper does not accept positional arguments")
	}
	nativeTarget, err := hostconfig.NativePath()
	if err != nil || *target != nativeTarget || !filepath.IsAbs(*staging) {
		return errors.New("will not run for a non-native target or a relative staging path")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("barn-hosts-helper must run as root")
	}
	return hostconfig.ApplyHelper(*target, *staging, *action, *before, *after, true)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
