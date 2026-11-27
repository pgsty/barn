package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/hostconfig"
	"github.com/pgsty/barn/internal/identity"
)

// CompanionHostsHelper locates the digest-matched barn-hosts-helper binary
// alongside the current executable or in its package libexec directory.
func CompanionHostsHelper() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	if canonical, canonicalErr := filepath.EvalSymlinks(executable); canonicalErr == nil {
		executable = canonical
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", err
	}
	directory := filepath.Dir(executable)
	candidates := []string{
		filepath.Join(directory, "barn-hosts-helper"),
		filepath.Join(filepath.Dir(directory), "libexec", "barn-hosts-helper"),
	}
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if _, err := hostconfig.CompanionHelperDigest(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("matching barn-hosts-helper was not found beside the CLI or in its package libexec directory")
}

// HostsHelperResult summarizes the outcome of installing or verifying the
// root-owned barn-hosts-helper binary.
type HostsHelperResult struct {
	Status  string
	Digest  string
	Changed bool
}

// EnsureHostsHelper verifies or atomically installs the digest-matched
// root-owned barn-hosts-helper under /opt/barn/libexec.
func EnsureHostsHelper(ctx context.Context, base execx.Runner, authorize func(reason string) error, runInstall func(func() error) error) (HostsHelperResult, bool, error) {
	if digest, err := hostconfig.InstalledHelperDigest(); err == nil {
		return HostsHelperResult{Status: "ready", Digest: digest}, false, nil
	}
	targetExists := false
	if _, err := os.Lstat(hostconfig.InstalledHelperPath); err == nil {
		if _, safeErr := hostconfig.RootOwnedHelperDigest(hostconfig.InstalledHelperPath); safeErr != nil {
			return HostsHelperResult{}, false, fmt.Errorf("existing privileged hosts helper is unsafe and was preserved: %w", safeErr)
		}
		targetExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return HostsHelperResult{}, false, err
	}
	source, err := CompanionHostsHelper()
	if err != nil {
		return HostsHelperResult{}, false, err
	}
	digest, err := hostconfig.CompanionHelperDigest(source)
	if err != nil {
		return HostsHelperResult{}, false, err
	}
	if err := authorize("install the /etc/hosts helper at " + hostconfig.InstalledHelperPath + " (root-owned; lets `barn hosts install` publish node names)"); err != nil {
		return HostsHelperResult{}, false, err
	}
	for _, directory := range []string{"/opt", "/opt/barn", "/opt/barn/libexec"} {
		info, statErr := os.Lstat(directory)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return HostsHelperResult{}, false, fmt.Errorf("inspect privileged helper directory %s: %w", directory, statErr)
		}
		statistics, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || statistics.Uid != 0 || statistics.Gid != 0 || info.Mode().Perm()&0o022 != 0 {
			return HostsHelperResult{}, false, fmt.Errorf("will not install the hosts helper: %s must be a root-owned directory writable only by root", directory)
		}
	}
	root := RootRunner(base)
	if _, directoryErr := root.Run(ctx, "/usr/bin/install", "-d", "-o", "root", "-g", "0", "-m", "0755", "/opt/barn", "/opt/barn/libexec"); directoryErr != nil {
		return HostsHelperResult{}, true, fmt.Errorf("prepare privileged helper directory: %w", directoryErr)
	}
	stageID, err := identity.NewUUID()
	if err != nil {
		return HostsHelperResult{}, true, err
	}
	staged := filepath.Join(filepath.Dir(hostconfig.InstalledHelperPath), ".barn-hosts-helper.next-"+stageID)
	defer func() {
		if staged != "" {
			_, _ = root.Run(context.WithoutCancel(ctx), "/bin/rm", "-f", "--", staged)
		}
	}()
	installAction := func() error {
		_, err := root.Run(ctx, "/usr/bin/install", "-o", "root", "-g", "0", "-m", "0755", source, staged)
		return err
	}
	var installErr error
	if runInstall != nil {
		installErr = runInstall(installAction)
	} else {
		installErr = installAction()
	}
	if installErr != nil {
		return HostsHelperResult{}, true, fmt.Errorf("install hosts helper: %w", installErr)
	}
	stagedDigest, stageErr := hostconfig.RootOwnedHelperDigest(staged)
	if stageErr != nil || stagedDigest != digest {
		if stageErr == nil {
			stageErr = failure.New(failure.Integrity, errors.New("staged hosts helper digest differs from the packaged CLI companion"))
		}
		return HostsHelperResult{}, true, fmt.Errorf("verify staged hosts helper: %w", stageErr)
	}
	if _, moveErr := root.Run(ctx, "/bin/mv", "-f", "--", staged, hostconfig.InstalledHelperPath); moveErr != nil {
		return HostsHelperResult{}, true, fmt.Errorf("publish hosts helper: %w", moveErr)
	}
	staged = ""
	installedDigest, verifyErr := hostconfig.InstalledHelperDigest()
	if verifyErr != nil || installedDigest != digest {
		if verifyErr == nil {
			verifyErr = failure.New(failure.Integrity, errors.New("installed hosts helper digest changed"))
		}
		return HostsHelperResult{Status: "failed", Changed: true}, true, fmt.Errorf("verify hosts helper: %w", verifyErr)
	}
	status := "installed"
	if targetExists {
		status = "updated"
	}
	return HostsHelperResult{Status: status, Digest: installedDigest, Changed: true}, false, nil
}
