package darwin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pgsty/barn/internal/execx"
)

// SocketVMNetFormula is the Homebrew formula whose keg provides the pinned
// socket_vmnet version. brew always runs as the invoking user, never as root.
const SocketVMNetFormula = "socket_vmnet"

// A Homebrew bottle rebuild appends _<n> without changing the upstream
// version, so 1.2.2 and 1.2.2_1 both satisfy the 1.2.2 pin.
var homebrewKegVersionPattern = regexp.MustCompile(`^` + regexp.QuoteMeta(ReleaseVersion) + `(_[0-9]+)?$`)

type HomebrewProbe struct {
	Runner       execx.Runner
	LookPath     func(string) (string, error)
	EvalSymlinks func(string) (string, error)
}

func (p HomebrewProbe) lookPath(name string) (string, error) {
	if p.LookPath != nil {
		return p.LookPath(name)
	}
	return exec.LookPath(name)
}

func (p HomebrewProbe) evalSymlinks(path string) (string, error) {
	if p.EvalSymlinks != nil {
		return p.EvalSymlinks(path)
	}
	return filepath.EvalSymlinks(path)
}

// Brew returns the user's brew executable, or ok=false when Homebrew is not
// installed.
func (p HomebrewProbe) Brew() (string, bool) {
	brew, err := p.lookPath("brew")
	return brew, err == nil && filepath.IsAbs(brew)
}

type HomebrewStatus int

const (
	// HomebrewFound means the discovered binaries are usable.
	HomebrewFound HomebrewStatus = iota
	// HomebrewMissing means brew itself is not installed.
	HomebrewMissing
	// HomebrewFormulaMissing means brew exists but the formula is not
	// installed; `brew install socket_vmnet` (as the user) would resolve it.
	HomebrewFormulaMissing
	// HomebrewUnusable means the formula is installed but cannot serve the
	// pin (version drift, broken keg); Reason explains.
	HomebrewUnusable
)

type HomebrewDiscovery struct {
	Binaries LocalBinaries
	Status   HomebrewStatus
	Reason   string
}

// Discover locates the binaries of an installed socket_vmnet formula whose
// keg version matches the pinned release. A non-Found status is not an
// error; Reason explains why the Homebrew source does not apply.
func (p HomebrewProbe) Discover(ctx context.Context) (HomebrewDiscovery, error) {
	if p.Runner == nil {
		return HomebrewDiscovery{}, fmt.Errorf("homebrew probe requires a runner")
	}
	unusable := func(reason string) HomebrewDiscovery {
		return HomebrewDiscovery{Status: HomebrewUnusable, Reason: reason}
	}
	brew, ok := p.Brew()
	if !ok {
		return HomebrewDiscovery{Status: HomebrewMissing, Reason: "Homebrew is not installed"}, nil
	}
	result, err := p.Runner.Run(ctx, brew, "--prefix", SocketVMNetFormula)
	if err != nil {
		return HomebrewDiscovery{Status: HomebrewFormulaMissing, Reason: "the " + SocketVMNetFormula + " formula is not installed"}, nil
	}
	prefix := strings.TrimSpace(string(result.Stdout))
	if !filepath.IsAbs(prefix) {
		return HomebrewDiscovery{}, fmt.Errorf("brew --prefix %s returned a non-absolute path %q", SocketVMNetFormula, prefix)
	}
	keg, err := p.evalSymlinks(prefix)
	if err != nil {
		// brew --prefix <formula> succeeds and prints the opt path even for
		// formulas that were never installed; only the opt symlink of an
		// installed keg actually resolves.
		if errors.Is(err, os.ErrNotExist) {
			return HomebrewDiscovery{Status: HomebrewFormulaMissing, Reason: "the " + SocketVMNetFormula + " formula is not installed"}, nil
		}
		return unusable("the " + SocketVMNetFormula + " formula prefix does not resolve"), nil
	}
	if version := filepath.Base(keg); !homebrewKegVersionPattern.MatchString(version) {
		return unusable(fmt.Sprintf("installed formula version %s differs from the pinned %s", version, ReleaseVersion)), nil
	}
	binaries := LocalBinaries{
		Socket: filepath.Join(keg, "bin", "socket_vmnet"),
		Client: filepath.Join(keg, "bin", "socket_vmnet_client"),
	}
	for _, path := range []string{binaries.Socket, binaries.Client} {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			return unusable("the " + SocketVMNetFormula + " keg is missing its executables"), nil
		}
	}
	return HomebrewDiscovery{Binaries: binaries, Status: HomebrewFound}, nil
}
