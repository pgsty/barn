package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/config"
	"github.com/pgsty/barn/internal/doctor"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/identity"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/linuxvm"
	darwinnet "github.com/pgsty/barn/internal/network/darwin"
	linuxnet "github.com/pgsty/barn/internal/network/linux"
	netpreflight "github.com/pgsty/barn/internal/network/preflight"
	"github.com/pgsty/barn/internal/network/subnet"
	setuphost "github.com/pgsty/barn/internal/setup"
	"github.com/pgsty/barn/internal/spec"
	"golang.org/x/term"
)

type setupStep struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
	Changed bool   `json:"changed,omitempty"`
}

type setupResult struct {
	Resources         []config.ResourceResolution `json:"resources,omitempty"`
	OperationID       string                      `json:"operation_id,omitempty"`
	Schema            int                         `json:"schema"`
	OS                string                      `json:"os"`
	Arch              string                      `json:"arch"`
	GuestArch         string                      `json:"guest_arch,omitempty"`
	Profile           string                      `json:"profile"`
	Config            string                      `json:"config,omitempty"`
	Image             string                      `json:"image,omitempty"`
	DryRun            bool                        `json:"dry_run"`
	Applicable        bool                        `json:"applicable"`
	Ready             bool                        `json:"ready"`
	Blocked           bool                        `json:"blocked"`
	Changed           bool                        `json:"changed"`
	MutationUncertain bool                        `json:"mutation_uncertain,omitempty"`
	ExitCode          int                         `json:"exit_code"`
	Error             string                      `json:"error,omitempty"`
	Dependencies      setuphost.DependencyPlan    `json:"dependencies"`
	Network           *netpreflight.Report        `json:"network,omitempty"`
	NetworkCIDR       string                      `json:"network_cidr,omitempty"`
	NetworkMode       string                      `json:"network_mode,omitempty"`
	Checks            []doctor.Check              `json:"checks,omitempty"`
	Steps             []setupStep                 `json:"steps"`
	Next              string                      `json:"next"`
	NextArgv          []string                    `json:"next_argv"`
	Resolution        string                      `json:"resolution,omitempty"`
}

type setupSelection struct {
	Inputs          vmInputs
	Profile         string
	Image           string
	Arch            string
	Resolved        spec.Resolved
	File            config.File
	ConfigData      []byte
	OriginalConfig  []byte
	ConfigPath      string
	Generated       bool
	Publish         bool
	ExplicitNetwork bool
}

type setupCLIOptions struct {
	vmInputs
	FilePath     string
	CIDR         string
	Mode         string
	Repo         string
	Mirror       bool
	ModeExplicit bool
	DryRun       bool
	Yes          bool
	Applied      *spec.Resolved // internal: prepare an existing deployment from any directory
}

func (options setupCLIOptions) arguments(profileName string) []string {
	arguments := make([]string, 0, 6)
	if profileName != "" {
		arguments = append(arguments, profileName)
	}
	if options.FilePath != "" {
		arguments = append(arguments, "--file="+options.FilePath)
	}
	if options.CIDR != "" {
		arguments = append(arguments, "--cidr="+options.CIDR)
	}
	arguments = append(arguments, options.vmInputs.arguments(true)...)
	if options.Repo != "" {
		arguments = append(arguments, "--repo="+options.Repo)
	}
	if options.Mirror {
		arguments = append(arguments, "--mirror")
	}
	if options.ModeExplicit {
		arguments = append(arguments, "--mode="+options.Mode)
	}
	if options.DryRun {
		arguments = append(arguments, "--dry-run")
	}
	if options.Yes {
		arguments = append(arguments, "--yes")
	}
	return arguments
}

func loadSetupFile(path string) (config.File, spec.Resolved, error) {
	absolute, err := fsutil.UserPath(path, "")
	if err != nil {
		return config.File{}, spec.Resolved{}, err
	}
	file, err := config.LoadPath(absolute)
	if err != nil {
		return config.File{}, spec.Resolved{}, err
	}
	resolved, err := file.Resolve()
	return file, resolved, err
}

func generatedSetupProfile(name, cidr, target string, input vmInputs) (setupSelection, error) {
	input, err := input.resolve(false, true)
	if err != nil {
		return setupSelection{}, failure.New(failure.Usage, err)
	}
	data, err := config.TemplateWithOptions(name, cidr, input.templateOptions())
	if err != nil {
		return setupSelection{}, err
	}
	file, err := config.ParseInventory(data)
	if err != nil {
		return setupSelection{}, err
	}
	resolved, err := file.Resolve()
	if err != nil {
		return setupSelection{}, err
	}
	return setupSelection{
		Profile: name, Image: file.Nodes[0].Image, Arch: file.Arch, Inputs: input, Resolved: resolved, File: file,
		ConfigData: data, ConfigPath: target, Generated: true, Publish: true,
		ExplicitNetwork: cidr != "",
	}, nil
}

func resolvedHash(value spec.Resolved) (string, error) { return spec.Hash(value) }

func reconcileGeneratedTarget(selection setupSelection) (setupSelection, error) {
	info, err := os.Stat(selection.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return selection, nil
	}
	if err != nil {
		return selection, err
	}
	if !info.Mode().IsRegular() {
		return selection, fmt.Errorf("%s is not a regular file; setup will not write through it", selection.ConfigPath)
	}
	existingFile, existingResolved, err := loadSetupFile(selection.ConfigPath)
	if err != nil {
		return selection, fmt.Errorf("existing configuration is invalid and was preserved: %w", err)
	}
	wantedHash, err := resolvedHash(selection.Resolved)
	if err != nil {
		return selection, err
	}
	existingHash, err := resolvedHash(existingResolved)
	if err != nil {
		return selection, err
	}
	if wantedHash != existingHash || !sameResourceBases(existingFile.Resources, selection.File.Resources) {
		return selection, failure.New(failure.Conflict, fmt.Errorf("existing %s differs from the %s template and was kept", selection.ConfigPath, selection.Profile)).Then(fmt.Sprintf("barn setup -f %s, or run setup in another directory", shellQuote(selection.ConfigPath)))
	}
	selection.File = existingFile
	selection.Resolved = existingResolved
	selection.Publish = false
	// Once the file exists it is user-owned input. Setup may reuse it but must
	// never auto-rebase it while solving a host-network conflict.
	selection.Generated = false
	return selection, nil
}

// discoverSetupConfig returns the first existing discovery-name path in cwd.
func discoverSetupConfig(cwd string) (string, error) {
	for _, name := range config.DiscoveryNames {
		candidate := filepath.Join(cwd, name)
		info, err := os.Stat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("configuration is not a regular file: %s", candidate)
		}
		return candidate, nil
	}
	return "", nil
}

func resolveSetupSelectionInputs(profileName, filePath, cidr string, input vmInputs, cwd string) (setupSelection, error) {
	target := filepath.Join(cwd, "barn.yml")
	if filePath != "" && profileName != "" {
		return setupSelection{}, failure.New(failure.Usage, errors.New("setup accepts either a lab template or -f, not both"))
	}
	if filePath != "" && input.any() {
		return setupSelection{}, failure.New(failure.Usage, errors.New("--image/--arch and resource flags apply only to a generated lab template; edit the existing inventory instead"))
	}
	if filePath != "" && cidr != "" {
		return setupSelection{}, failure.New(failure.Usage, errors.New("--cidr cannot change the network of an existing inventory; edit its addresses instead"))
	}
	if filePath != "" {
		absolute, err := fsutil.UserPath(filePath, "")
		if err != nil {
			return setupSelection{}, err
		}
		file, resolved, err := loadSetupFile(absolute)
		if err != nil {
			return setupSelection{}, err
		}
		return setupSelection{Resolved: resolved, File: file, ConfigPath: absolute}, nil
	}
	discovered, err := discoverSetupConfig(cwd)
	if err != nil {
		return setupSelection{}, err
	}
	if discovered != "" && profileName == "" {
		if input.any() {
			return setupSelection{}, failure.New(failure.Usage, errors.New("--image/--arch and resource flags apply only to a generated lab template; pass a template name or edit the discovered inventory"))
		}
		if cidr != "" {
			return setupSelection{}, errors.New("--cidr cannot silently rebase the discovered configuration; edit the file as one coordinated change")
		}
		file, resolved, err := loadSetupFile(discovered)
		if err != nil {
			return setupSelection{}, err
		}
		selection := setupSelection{Resolved: resolved, File: file, ConfigPath: discovered}
		return recognizeDefaultSetupTemplate(selection), nil
	}
	if profileName == "" {
		profileName = "meta"
	}
	if !config.ValidTemplate(profileName) {
		return setupSelection{}, failure.New(failure.Usage, fmt.Errorf("unknown lab template %q; available: %s", profileName, strings.Join(config.TemplateNames(), ", ")))
	}
	if discovered != "" {
		// Repeating `barn setup <template>` over the file it generated is
		// idempotent; a different existing configuration is preserved.
		target = discovered
	}
	input, err = input.resolve(true, true)
	if err != nil {
		return setupSelection{}, failure.New(failure.Usage, err)
	}
	selection, err := generatedSetupProfile(profileName, cidr, target, input)
	if err != nil {
		return setupSelection{}, err
	}
	return reconcileGeneratedTarget(selection)
}

func (selection *setupSelection) rebaseGenerated(cidr string) error {
	if !selection.Generated || selection.ExplicitNetwork {
		return errors.New("setup selection cannot be automatically rebased")
	}
	rebased, err := generatedSetupProfile(selection.Profile, cidr, selection.ConfigPath, selection.Inputs)
	if err != nil {
		return err
	}
	rebased.ExplicitNetwork = false
	if len(selection.OriginalConfig) > 0 {
		rebased.OriginalConfig = selection.OriginalConfig
		*selection = rebased
		return nil
	}
	resolved, err := reconcileGeneratedTarget(rebased)
	if err != nil {
		return err
	}
	*selection = resolved
	return nil
}

func setupAddresses(selection setupSelection) []string {
	addresses := make([]string, 0, len(selection.Resolved.Nodes))
	for _, node := range selection.Resolved.Nodes {
		if node.Address != "" {
			addresses = append(addresses, node.Address)
		}
	}
	return linuxvm.FilterRecordedAddresses(addresses)
}

func inspectSetupNetwork(ctx context.Context, selection setupSelection, runner execx.Runner) (netpreflight.Report, error) {
	if selection.Resolved.Private == nil {
		return netpreflight.Report{}, errors.New("the inventory has no Barn network")
	}
	layout, err := subnet.Parse(selection.Resolved.Private.CIDR)
	if err != nil {
		return netpreflight.Report{}, err
	}
	return netpreflight.Run(ctx, netpreflight.Request{
		OS: runtime.GOOS, Arch: runtime.GOARCH, Purpose: netpreflight.Install,
		Layout: layout, Addresses: setupAddresses(selection),
	}, netpreflight.Probe{Runner: runner}), nil
}

func reusableInstalledNetwork(report netpreflight.Report) string {
	status := report.Installation.Status
	if (status == "exact" || status == "protected") && report.Installation.CIDR != "" && report.Installation.Healthy {
		return report.Installation.CIDR
	}
	return ""
}

var setupNetworkCandidates = []string{
	"172.31.251.0/24",
	"172.31.252.0/24",
	"192.168.250.0/24",
	"192.168.251.0/24",
}

func selectSetupNetwork(ctx context.Context, selection *setupSelection, runner execx.Runner) (netpreflight.Report, error) {
	report, err := inspectSetupNetwork(ctx, *selection, runner)
	if err != nil {
		return report, err
	}
	if report.Ready {
		return report, nil
	}
	if !selection.Generated || selection.ExplicitNetwork {
		return report, nil
	}
	if installed := reusableInstalledNetwork(report); installed != "" {
		if err := selection.rebaseGenerated(installed); err != nil {
			return report, err
		}
		return inspectSetupNetwork(ctx, *selection, runner)
	}
	if report.Installation.Status != "" && report.Installation.Status != "absent" {
		return report, nil
	}
	for _, candidate := range setupNetworkCandidates {
		if candidate == report.CIDR {
			continue
		}
		original := *selection
		if err := selection.rebaseGenerated(candidate); err != nil {
			*selection = original
			continue
		}
		candidateReport, candidateErr := inspectSetupNetwork(ctx, *selection, runner)
		if candidateErr == nil && candidateReport.Ready {
			return candidateReport, nil
		}
		*selection = original
	}
	return report, nil
}

func constrainSetupNetworkMode(report netpreflight.Report, requested string, explicit bool) (netpreflight.Report, string) {
	if report.OS != "darwin" {
		return report, "bridge"
	}
	effective := requested
	installed := report.Installation.Status == "exact" || report.Installation.Status == "protected"
	if !installed || report.Installation.Mode == "" {
		return report, effective
	}
	if !explicit {
		return report, report.Installation.Mode
	}
	if report.Installation.Mode == requested {
		return report, requested
	}
	report.Findings = append(report.Findings, netpreflight.Finding{
		Code: "installation.mode_mismatch", Severity: netpreflight.Error, Class: netpreflight.State,
		Evidence: fmt.Sprintf("installed=%s requested=%s", report.Installation.Mode, requested),
		Fix:      fmt.Sprintf("rerun setup without --mode to reuse %s, or stop the deployment, uninstall the owned network, then rerun with --mode %s", report.Installation.Mode, requested),
	})
	report.Ready = false
	if report.ExitCode < exitConflict {
		report.ExitCode = exitConflict
	}
	return report, effective
}

func setupFindingError(report netpreflight.Report) error {
	lines := make([]string, 0, len(report.Findings)+1)
	for _, finding := range report.Findings {
		if finding.Severity != netpreflight.Error {
			continue
		}
		line := finding.Evidence
		if finding.Fix != "" {
			line += "; fix: " + finding.Fix
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, "the Barn network is not ready")
	}
	return errors.New(strings.Join(lines, "\n"))
}

// errSetupNeedsYes is automation forgetting the flag, not a person declining:
// a usage error, like destroy without --force on a pipe.
var errSetupNeedsYes = errors.New("setup needs --yes when stdin is not a terminal")

func confirmSetup(yes bool, mutating bool, stdin io.Reader, stderr io.Writer) error {
	resume := suspendProgress(stderr)
	defer resume()
	if yes || !mutating {
		return nil
	}
	terminal, ok := stdin.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(int(terminal.Fd())) {
		return errSetupNeedsYes
	}
	return confirmPlan("Continue with this setup? [Y/n] ", true, stdin, stderr)
}

func newSudoSession(base execx.Runner, stderr io.Writer, scope string) *setuphost.SudoSession {
	if scope == "" {
		scope = "command"
	}
	return &setuphost.SudoSession{
		Base:        base,
		Interactive: term.IsTerminal(int(os.Stdin.Fd())),
		Stdin:       os.Stdin,
		Output:      rawWriter(stderr),
		BeforePrompt: func() func() {
			return suspendProgress(stderr)
		},
		Announce: func(reason string) {
			textField(stderr, 10, "sudo", "needed: "+reason)
			textField(stderr, 10, "note", "one password prompt covers this whole "+scope)
		},
	}
}

func runSetupCommands(ctx context.Context, commands []setuphost.Command, base execx.Runner, sudo *setuphost.SudoSession, stderr io.Writer) (changed, mutationUncertain bool, err error) {
	for _, command := range commands {
		if err := command.Validate(); err != nil {
			return changed, false, err
		}
		if command.Root {
			if err := sudo.Ensure(ctx, command.Name+" (system package manager)"); err != nil {
				return changed, false, err
			}
		}
		runner := base
		if command.Root {
			runner = setuphost.PackageManagerRunner(base)
		}
		progressItem := startProgress(ctx, stderr, command.Name)
		result, err := runner.Run(ctx, command.Binary, command.Args...)
		progressItem.Stop(err)
		if err != nil {
			return changed, true, fmt.Errorf("%s: %w", strings.ToLower(command.Name[:1])+command.Name[1:], err)
		}
		changed = true
		debugf(stderr, "setup command complete name=%s duration=%s stdout=%q stderr=%q", command.Name, result.Duration.Round(time.Millisecond), strings.TrimSpace(string(result.Stdout)), strings.TrimSpace(string(result.Stderr)))
	}
	return changed, false, nil
}

func setupNeedsNetworkInstall(report netpreflight.Report) bool {
	return report.Installation.Status == "" || report.Installation.Status == "absent" || report.CanRepair()
}

func networkRepairNeedsRestart(report netpreflight.Report) bool {
	for _, finding := range report.Findings {
		if finding.Severity == netpreflight.Error && finding.Code != "installation.log_directory" {
			return true
		}
	}
	return false
}

func applySetupNetwork(ctx context.Context, mode, repo string, report netpreflight.Report, base execx.Runner, sudo *setuphost.SudoSession, stderr io.Writer) (setupStep, bool, error) {
	if !setupNeedsNetworkInstall(report) {
		tickf(stderr, "Network %s is already installed", report.CIDR)
		return setupStep{Name: "network", Status: "ready", Detail: report.CIDR}, false, nil
	}
	networkReason := "install the Barn network " + report.CIDR + " (root-owned socket_vmnet service)"
	if report.CanRepair() {
		networkReason = "restore the Barn network " + report.CIDR
	}
	if runtime.GOOS != "darwin" {
		networkReason = "install the Barn network " + report.CIDR + " (root-owned barn0 bridge)"
	}
	if runtime.GOOS != "darwin" || report.CanRepair() {
		if err := sudo.Ensure(ctx, networkReason); err != nil {
			return setupStep{}, false, err
		}
	}
	if runtime.GOOS == "darwin" {
		if report.CanRepair() {
			progressItem := startProgress(ctx, stderr, "Restoring the Barn network")
			defer progressItem.Stop(nil)
			executor := darwinnet.Executor{User: base, Root: setuphost.RootRunner(base)}
			if err := executor.Repair(ctx, mode, report.CIDR, networkRepairNeedsRestart(report)); err != nil {
				return setupStep{}, !errors.Is(err, darwinnet.ErrNoHostChanges), err
			}
			return setupStep{Name: "network", Status: "repaired", Detail: report.CIDR, Changed: true}, false, nil
		}
		executor := darwinnet.Executor{User: base, Root: setuphost.RootRunner(base)}
		authorize := func() error { return sudo.Ensure(ctx, networkReason) }
		return installSetupDarwinNetwork(ctx, mode, repo, report.CIDR, base, authorize, stderr, executor)
	}
	linuxConfig, err := linuxnet.ConfigForCIDR(report.CIDR)
	if err != nil {
		return setupStep{}, false, err
	}
	executor := linuxnet.Executor{User: base, Root: setuphost.RootRunner(base)}
	progressItem := startProgress(ctx, stderr, "Installing the Barn network")
	installReport, err := executor.InstallConfig(ctx, linuxConfig, true)
	progressItem.Stop(err)
	if err != nil {
		return setupStep{}, !errors.Is(err, linuxnet.ErrInstallRolledBack), err
	}
	return setupStep{Name: "network", Status: "installed", Detail: report.CIDR, Changed: installReport.Applied}, false, nil
}

type setupDarwinNetworkInstaller interface {
	InstallFromHomebrew(context.Context, darwinnet.LocalBinaries, string, string, string, string, bool) (darwinnet.InstallReport, error)
	InstallModeNetwork(context.Context, string, string, string, string, string, bool) (darwinnet.InstallReport, error)
}

func installSetupDarwinNetwork(ctx context.Context, mode, repo, cidr string, base execx.Runner, authorize func() error, stderr io.Writer, executor setupDarwinNetworkInstaller) (setupStep, bool, error) {
	interfaceID, err := identity.NewUUID()
	if err != nil {
		return setupStep{}, false, err
	}
	sources := setuphost.SourcesFromEnvironment(repo)
	// Prefer the version-matched Homebrew formula when no pinned archive
	// is already local: brew honors the user's mirror and proxy
	// configuration, so nothing has to reach github.com.
	if sources.Archive == "" && !setuphost.SocketVMNetCached(runtime.GOARCH) {
		if binaries, ok := setupHomebrewSocketVMNet(ctx, base, stderr); ok {
			// Homebrew can reset sudo's timestamp. Authenticate only after
			// discovery and formula installation have finished.
			if err := authorize(); err != nil {
				return setupStep{}, false, err
			}
			progressItem := startProgress(ctx, stderr, "Installing the Barn network (socket_vmnet from Homebrew, root-owned copy)")
			installReport, err := executor.InstallFromHomebrew(ctx, binaries, interfaceID, runtime.GOARCH, mode, cidr, true)
			progressItem.Stop(err)
			if err != nil {
				return setupStep{}, !errors.Is(err, darwinnet.ErrNoHostChanges), err
			}
			return setupStep{Name: "network", Status: "installed", Detail: installReport.Plan.State.CIDR, Changed: installReport.Applied}, false, nil
		}
	}
	var downloadProgress *progress
	sources.Progress = deferredProgressReporter(&downloadProgress)
	downloadProgress = startProgress(ctx, stderr, "Fetching socket_vmnet "+darwinnet.ReleaseVersion+" (digest-pinned; sources: cache, BARN_VMNET_ARCHIVE, selected repository, github.com)")
	download, err := setuphost.DownloadPinnedSocketVMNet(ctx, runtime.GOARCH, "", nil, sources)
	downloadProgress.Stop(err)
	if err != nil {
		return setupStep{}, false, err
	}
	debugf(stderr, "socket_vmnet source=%s downloaded=%t", progressSource(download.URL), download.Downloaded)
	if err := authorize(); err != nil {
		return setupStep{}, false, err
	}
	progressItem := startProgress(ctx, stderr, "Installing the Barn network")
	installReport, err := executor.InstallModeNetwork(ctx, download.Path, interfaceID, runtime.GOARCH, mode, cidr, true)
	progressItem.Stop(err)
	if err != nil {
		return setupStep{}, !errors.Is(err, darwinnet.ErrNoHostChanges), err
	}
	return setupStep{Name: "network", Status: "installed", Detail: installReport.Plan.State.CIDR, Changed: installReport.Applied}, false, nil
}

// setupHomebrewSocketVMNet resolves version-matched socket_vmnet binaries via
// Homebrew, installing the formula (always as the user, never root) when it
// is absent. false means the Homebrew source does not apply and the caller
// falls back to the digest-pinned release download.
func setupHomebrewSocketVMNet(ctx context.Context, base execx.Runner, stderr io.Writer) (darwinnet.LocalBinaries, bool) {
	probe := darwinnet.HomebrewProbe{Runner: base}
	discovery, err := probe.Discover(ctx)
	if err != nil {
		debugf(stderr, "homebrew socket_vmnet discovery failed: %v", err)
		return darwinnet.LocalBinaries{}, false
	}
	if discovery.Status == darwinnet.HomebrewFormulaMissing {
		brew, ok := probe.Brew()
		if !ok {
			return darwinnet.LocalBinaries{}, false
		}
		progressItem := startProgress(ctx, stderr, "Installing socket_vmnet "+darwinnet.ReleaseVersion+" (brew install "+darwinnet.SocketVMNetFormula+")")
		_, installErr := base.Run(ctx, brew, "install", darwinnet.SocketVMNetFormula)
		progressItem.Stop(installErr)
		if installErr != nil {
			bestEffortf(stderr, "%s brew install %s failed (%v); falling back to the digest-pinned release download\n", styled(stderr, ansiYellow, "!"), darwinnet.SocketVMNetFormula, installErr)
			return darwinnet.LocalBinaries{}, false
		}
		discovery, err = probe.Discover(ctx)
		if err != nil {
			debugf(stderr, "homebrew socket_vmnet discovery failed: %v", err)
			return darwinnet.LocalBinaries{}, false
		}
	}
	if discovery.Status != darwinnet.HomebrewFound {
		if discovery.Reason != "" {
			debugf(stderr, "homebrew socket_vmnet unavailable: %s", discovery.Reason)
		}
		return darwinnet.LocalBinaries{}, false
	}
	return discovery.Binaries, true
}

func ensureSetupHostsHelper(ctx context.Context, base execx.Runner, sudo *setuphost.SudoSession, stderr io.Writer) (setupStep, bool, error) {
	result, uncertain, err := setuphost.EnsureHostsHelper(
		ctx,
		base,
		func(reason string) error { return sudo.Ensure(ctx, reason) },
		func(install func() error) error {
			progressItem := startProgress(ctx, stderr, "Installing the hosts helper")
			installErr := install()
			progressItem.Stop(installErr)
			return installErr
		},
	)
	if err == nil && result.Status == "ready" {
		tickf(stderr, "Hosts helper is already installed")
	}
	step := setupStep{Name: "hosts-helper", Status: result.Status, Detail: result.Digest, Changed: result.Changed}
	return step, uncertain, err
}

func setupCheckIgnored(name string, private bool) bool {
	// Setup has already run the mode-aware network preflight with the exact
	// selected node addresses (and suppresses address probes for an active
	// lease). Doctor's generic all-address network view is informational here.
	if strings.HasPrefix(name, "network-") {
		return true
	}
	if private {
		return false
	}
	return name == "linux-network-owner" || name == "bridge-helper" || name == "private-bridge"
}

func verifySetup(ctx context.Context, private bool) ([]doctor.Check, error) {
	report := (doctor.Probe{HostOnly: true}).Run(ctx)
	errorsFound := make([]string, 0)
	for _, check := range report.Checks {
		if check.Status == doctor.Error && !setupCheckIgnored(check.Name, private) {
			line := check.Evidence
			if check.Fix != "" {
				line += "; fix: " + check.Fix
			}
			errorsFound = append(errorsFound, line)
		}
	}
	if len(errorsFound) > 0 {
		return report.Checks, errors.New(strings.Join(errorsFound, "\n"))
	}
	return report.Checks, nil
}

func setupMutating(plan setuphost.DependencyPlan, selection setupSelection, report *netpreflight.Report) bool {
	if len(plan.Commands) > 0 || selection.Publish {
		return true
	}
	return report != nil && setupNeedsNetworkInstall(*report)
}

// setupNodesSummary renders "1 node: meta @ 10.10.10.10" or
// "4 nodes: meta @ 10.10.10.10 ... node-3 @ 10.10.10.13".
func setupNodesSummary(resolved spec.Resolved) string {
	nodes := resolved.Nodes
	switch len(nodes) {
	case 0:
		return "no nodes"
	case 1:
		return fmt.Sprintf("1 node: %s @ %s", nodes[0].Name, nodes[0].Address)
	case 2:
		return fmt.Sprintf("2 nodes: %s @ %s, %s @ %s", nodes[0].Name, nodes[0].Address, nodes[1].Name, nodes[1].Address)
	default:
		first, last := nodes[0], nodes[len(nodes)-1]
		return fmt.Sprintf("%d nodes: %s @ %s ... %s @ %s", len(nodes), first.Name, first.Address, last.Name, last.Address)
	}
}

// setupNetworkAction is what setup will do with the network: install, repair,
// reuse installed, or blocked. Without a report (dependencies first) it installs.
func setupNetworkAction(report *netpreflight.Report) string {
	switch {
	case report == nil:
		return "install"
	case report.CanRepair():
		return "repair"
	case !report.Ready:
		return "blocked"
	case setupNeedsNetworkInstall(*report):
		return "install"
	default:
		return "reuse installed"
	}
}

// darwinSocketVMNetSource names where a fresh macOS network install gets
// socket_vmnet, in the order installSetupDarwinNetwork tries them.
func darwinSocketVMNetSource() string {
	switch {
	case os.Getenv("BARN_VMNET_ARCHIVE") != "":
		return "BARN_VMNET_ARCHIVE"
	case setuphost.SocketVMNetCached(runtime.GOARCH):
		return "cache"
	}
	if _, ok := (darwinnet.HomebrewProbe{}).Brew(); ok {
		return "homebrew"
	}
	return "download"
}

// printSetupPlan tells the user exactly what will happen, which parts need
// root and why, and where any download would come from — before the single
// confirmation prompt.
func printSetupPlan(stderr io.Writer, plan setuphost.DependencyPlan, selection setupSelection, report *netpreflight.Report, dryRun bool) {
	printSpecificationTable(stderr, selection.Resolved, nil)
	printResourceCalculations(stderr, resourceInputSources(selection.File.Resources, selection.Inputs, nil))
	networkAction := setupNetworkAction(report)
	networkInstall := networkAction == "install" || networkAction == "repair"
	source := ""
	if networkAction == "install" && runtime.GOOS == "darwin" {
		source = darwinSocketVMNetSource()
	}
	sudoFor := make([]string, 0, 2)
	for _, command := range plan.Commands {
		if command.Root {
			sudoFor = append(sudoFor, "package installation ("+plan.Manager+")")
			break
		}
	}
	if networkInstall && runtime.GOOS == "darwin" {
		sudoFor = append(sudoFor, "network service (socket_vmnet under /opt/barn, root-owned)")
	} else if networkInstall {
		sudoFor = append(sudoFor, "network (root-owned barn0 bridge)")
	}

	rows := make([][]string, 0, 3)
	dependencyState, dependencyMessage := "ready", "QEMU, qemu-img, UEFI firmware, OpenSSH"
	if len(plan.Commands) > 0 {
		dependencyState, dependencyMessage = "planned", "install via "+plan.Manager+": "+strings.Join(plan.Missing, ", ")
	}
	rows = append(rows, []string{"dependencies", dependencyState, dependencyMessage})
	networkState := "planned"
	if networkAction == "reuse installed" {
		networkState = "ready"
	}
	if networkAction == "blocked" {
		networkState = "blocked"
	}
	networkMessage := networkAction + " " + selection.Resolved.Private.CIDR + " after dependencies"
	if report != nil {
		mode := report.Installation.Mode
		if mode != "" {
			if report.OS == "darwin" {
				mode = "vmnet " + mode
			}
			mode = " (" + mode + " mode)"
		}
		networkMessage = networkAction + " " + report.CIDR + mode
	}
	rows = append(rows, []string{"network", networkState, networkMessage})
	if selection.ConfigPath != "" {
		state, action := "ready", "use "
		if selection.Publish {
			state, action = "planned", "create "
		}
		rows = append(rows, []string{"config", state, action + selection.ConfigPath})
	}
	printTable(stderr, []string{"ITEM", "STATE", "MESSAGE"}, rows, 1)
	if len(sudoFor) > 0 {
		textField(stderr, 12, "sudo", strings.Join(sudoFor, "; "))
	}
	if !dryRun && !verboseOutput(stderr) {
		return
	}
	if dryRun {
		textField(stderr, 12, "setup", "plan only; no changes")
	}
	if selection.ConfigPath != "" {
		textField(stderr, 12, "config", selection.ConfigPath)
		textField(stderr, 12, "nodes", setupNodesSummary(selection.Resolved))
	}
	if len(plan.Commands) > 0 {
		if proxyNames := setuphost.ProxyEnvironmentNames(); len(proxyNames) > 0 {
			bestEffortf(stderr, "proxy environment: %s (values hidden)\n", strings.Join(proxyNames, ", "))
		}
	}

	switch source {
	case "BARN_VMNET_ARCHIVE":
		textField(stderr, 12, "backend", fmt.Sprintf("socket_vmnet %s from BARN_VMNET_ARCHIVE (digest-verified)", darwinnet.ReleaseVersion))
	case "cache":
		textField(stderr, 12, "backend", fmt.Sprintf("socket_vmnet %s already cached and verified; no download", darwinnet.ReleaseVersion))
	case "homebrew":
		textField(stderr, 12, "backend", fmt.Sprintf("socket_vmnet %s via Homebrew (brew install %s), copied into root-owned /opt/barn", darwinnet.ReleaseVersion, darwinnet.SocketVMNetFormula))
		textField(stderr, 12, "fallback", "digest-pinned download (selected repository, github.com/lima-vm)")
	case "download":
		textField(stderr, 12, "download", fmt.Sprintf("socket_vmnet %s (<4 MiB, SHA-256 pinned) from the selected repository or github.com/lima-vm", darwinnet.ReleaseVersion))
		textField(stderr, 12, "override", "--mirror, --repo, BARN_REPO, or BARN_VMNET_ARCHIVE=/path/to.tar.gz")
	}
	if networkAction == "install" && runtime.GOOS != "darwin" {
		textField(stderr, 12, "backend", "barn0 bridge via the active network manager; nothing is downloaded")
	}

	if len(sudoFor) == 0 {
		textField(stderr, 12, "privileges", "none; no root action in this plan")
	} else {
		textField(stderr, 12, "privileges", "sudo used for: "+strings.Join(sudoFor, "; "))
		textField(stderr, 12, "note", "at most one password prompt; the first privileged step explains itself")
	}
}

func setupOutcome(result setupResult) commandOutcome {
	return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
		if result.DryRun {
			// The complete plan was already printed before this outcome.
			if result.Next != "" {
				textField(stdout, 10, "next", result.Next)
			}
			return nil
		}
		if !lifecycleProgressFrozen(stdout) {
			rows := make([][]string, 0, len(result.Steps))
			for _, step := range result.Steps {
				rows = append(rows, []string{step.Name, step.Status, step.Detail})
			}
			if len(rows) == 0 {
				rows = append(rows, []string{"dependencies", "pending", "checking host dependencies"})
				rows = append(rows, []string{"network", "pending", result.NetworkCIDR + " (pending dependencies)"})
			}
			if len(rows) > 0 {
				printTable(stdout, []string{"ITEM", "STATE", "MESSAGE"}, rows, 1)
			}
		}
		if verboseOutput(stdout) {
			textField(stdout, 10, "host", result.OS+"/"+result.Arch)
			if result.Network != nil {
				textField(stdout, 10, "network", result.Network.CIDR)
			}
		}
		if result.Config != "" {
			textField(stdout, 10, "config", result.Config)
		}
		if result.MutationUncertain {
			textField(stdout, 10, "mutation", "uncertain after a failed host command; inspect before retrying")
		}
		if result.Next != "" {
			textField(stdout, 10, "next", result.Next)
		}
		return nil
	}}
}

func failSetup(result *setupResult, code int, failure error) (commandOutcome, error) {
	if errors.Is(failure, context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	boundary := newCommandError(code, failure).withOperation(result.OperationID)
	result.Ready = false
	result.ExitCode = boundary.code
	result.Error = failure.Error()
	if result.Resolution == "" {
		result.Resolution = failure.Error()
		result.Next = "fix the error, then rerun barn setup"
		if boundary.failure.Next != "" {
			result.Next = boundary.failure.Next
		}
		result.NextArgv = nil
	}
	return commandOutcome{}, boundary.withPayload(*result)
}

// failSetupRendered is for blockers the stderr plan has already described: the
// error line carries the evidence and fix, and stdout only says what to do next.
func failSetupRendered(result *setupResult, code int, failure error) (commandOutcome, error) {
	_, err := failSetup(result, code, failure)
	if boundary, ok := err.(*commandBoundaryError); ok {
		next := result.Next
		boundary.text = func(stdout, _ io.Writer) error {
			textField(stdout, 14, "next", next)
			return nil
		}
	}
	return commandOutcome{}, err
}

func shellQuote(argument string) string { return fsutil.ShellQuote(argument) }

func setupNextCommand(selection setupSelection, cwd string) (string, []string) {
	arguments := []string{"barn", "up"}
	discoverable := false
	for _, name := range config.DiscoveryNames {
		if selection.ConfigPath == filepath.Join(cwd, name) {
			discoverable = true
		}
	}
	if selection.ConfigPath != "" && !discoverable {
		arguments = append(arguments, "-f", selection.ConfigPath)
	}
	return formatSetupCommand(arguments)
}

func setupApplyCommand(arguments []string, format outputFormat, verbose bool) (string, []string) {
	apply := []string{"barn", "setup"}
	for _, argument := range arguments {
		if argument == "--dry-run" || argument == "-dry-run" ||
			strings.HasPrefix(argument, "--dry-run=") || strings.HasPrefix(argument, "-dry-run=") {
			continue
		}
		apply = append(apply, argument)
	}
	switch format {
	case outputJSON:
		apply = append(apply, "--json")
	case outputYAML:
		apply = append(apply, "--yaml")
	}
	if verbose {
		apply = append(apply, "--verbose")
	}
	return formatSetupCommand(apply)
}

func formatSetupCommand(arguments []string) (string, []string) {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = shellQuote(argument)
	}
	return strings.Join(quoted, " "), arguments
}

type setupSession struct {
	result       *setupResult
	selection    *setupSelection
	options      setupCLIOptions
	base         execx.Runner
	sudo         *setuphost.SudoSession
	stderr       io.Writer
	progressItem *progress
	activeStep   string
	networkMode  string
}

func (s *setupSession) working(name, message string) {
	s.activeStep = name
	s.progressItem.Report(activity.Event{Phase: "setup", Item: name, State: "running", Message: message})
}

func (s *setupSession) recordStep(steps ...setupStep) {
	s.result.Steps = append(s.result.Steps, steps...)
	for _, step := range steps {
		if s.options.DryRun {
			continue
		}
		s.progressItem.Report(activity.Event{Phase: "setup", Item: step.Name, State: step.Status, Message: step.Detail, Done: true})
		if s.activeStep == step.Name {
			s.activeStep = ""
		}
	}
}

// chooseNetwork selects the network the inventory will use (a fresh default
// template may move off a taken /24) and records it in result.
func (s *setupSession) chooseNetwork(ctx context.Context) (netpreflight.Report, error) {
	report, err := selectSetupNetwork(ctx, s.selection, s.base)
	if err != nil {
		return report, err
	}
	report, s.networkMode = constrainSetupNetworkMode(report, s.options.Mode, s.options.ModeExplicit)
	s.result.Network, s.result.NetworkMode = &report, s.networkMode
	if s.selection.Resolved.Private != nil {
		s.result.NetworkCIDR = s.selection.Resolved.Private.CIDR
	}
	return report, nil
}

// blockedNetwork records a network that setup cannot install or repair.
func (s *setupSession) blockedNetwork(report netpreflight.Report) (int, error) {
	err := setupFindingError(report)
	s.result.Blocked = true
	s.result.Resolution = err.Error()
	s.result.Next = "fix the network conflict, then rerun barn setup"
	s.result.NextArgv = nil
	if report.ExitCode == exitOK {
		return exitConflict, err
	}
	return report.ExitCode, err
}

func (s *setupSession) applyDependencies(ctx context.Context, dependencyPlan setuphost.DependencyPlan) error {
	s.working("dependencies", "checking and installing host dependencies")
	if len(dependencyPlan.Commands) > 0 {
		changed, uncertain, err := runSetupCommands(ctx, dependencyPlan.Commands, s.base, s.sudo, s.stderr)
		s.result.Changed = s.result.Changed || changed
		s.result.MutationUncertain = s.result.MutationUncertain || uncertain
		if err != nil {
			verified, verifyErr := setuphost.PlanDependencies(setuphost.DependencyProbe{}, true)
			if verifyErr != nil || !verified.Ready {
				s.recordStep(setupStep{Name: "dependencies", Status: "failed", Changed: changed})
				_, failErr := failSetup(s.result, exitCapability, err)
				return failErr
			}
			debugf(s.stderr, "package manager returned an error, but all required capabilities are installed: %v", err)
		}
		s.recordStep(setupStep{Name: "dependencies", Status: "installed", Changed: changed})
	} else {
		tickf(s.stderr, "Dependencies ready (QEMU, qemu-img, UEFI firmware, OpenSSH)")
		s.recordStep(setupStep{Name: "dependencies", Status: "ready"})
	}
	s.working("dependencies", "verifying installed dependencies")
	verifiedDependencies, err := setuphost.PlanDependencies(setuphost.DependencyProbe{}, true)
	if err != nil || !verifiedDependencies.Ready {
		if err == nil {
			err = fmt.Errorf("dependencies remain unavailable: %s", strings.Join(verifiedDependencies.Missing, ", "))
		}
		_, failErr := failSetup(s.result, exitCapability, err)
		return failErr
	}
	s.result.Dependencies = verifiedDependencies
	s.progressItem.Report(activity.Event{Phase: "setup", Item: "dependencies", State: "ready", Message: "QEMU, qemu-img, firmware and OpenSSH ready", Done: true})
	return nil
}

func (s *setupSession) verifyCapabilities(ctx context.Context) error {
	s.working("capabilities", "verifying native QEMU")
	capabilityProgress := startProgress(ctx, s.stderr, "Verifying native QEMU")
	capabilityChecks, capabilityErr := verifySetup(ctx, false)
	capabilityProgress.Stop(capabilityErr)
	s.result.Checks = capabilityChecks
	if capabilityErr != nil {
		s.recordStep(setupStep{Name: "capabilities", Status: "failed"})
		_, failErr := failSetup(s.result, exitCapability, capabilityErr)
		return failErr
	}
	s.recordStep(setupStep{Name: "capabilities", Status: "ready"})
	return nil
}

func (s *setupSession) applyNetworkPhase(ctx context.Context, repository, plannedCIDR string) error {
	s.working("network", "preparing the Barn network")
	report, reportErr := s.chooseNetwork(ctx)
	if reportErr != nil {
		_, failErr := failSetup(s.result, exitCapability, reportErr)
		return failErr
	}
	if s.result.NetworkCIDR != plannedCIDR {
		infof(s.stderr, "using network %s: %s is not available on this host", s.result.NetworkCIDR, plannedCIDR)
		// The confirmed plan named another network; ask again unless --yes.
		if !s.options.Yes {
			resume := suspendProgress(s.stderr)
			err := confirmPlan("Continue with "+s.result.NetworkCIDR+"? [Y/n] ", true, os.Stdin, s.stderr)
			resume()
			if err != nil {
				if errors.Is(err, ErrCancelled) {
					return err
				}
				_, failErr := failSetup(s.result, exitRuntime, err)
				return failErr
			}
		}
	}
	if !report.Ready && !report.CanRepair() {
		code, err := s.blockedNetwork(report)
		_, failErr := failSetup(s.result, code, err)
		return failErr
	}
	step, uncertain, installErr := applySetupNetwork(ctx, s.networkMode, repository, report, s.base, s.sudo, s.stderr)
	if installErr != nil {
		s.result.MutationUncertain = s.result.MutationUncertain || uncertain
		s.recordStep(setupStep{Name: "network", Status: "failed"})
		_, failErr := failSetup(s.result, exitRuntime, installErr)
		return failErr
	}
	s.recordStep(step)
	s.result.Changed = s.result.Changed || step.Changed
	s.working("verify", "verifying installed network")
	finalReport, finalErr := inspectSetupNetwork(ctx, *s.selection, s.base)
	finalReport, _ = constrainSetupNetworkMode(finalReport, s.networkMode, true)
	if finalErr != nil || !finalReport.Ready || !finalReport.Installation.Healthy {
		if finalErr == nil {
			finalErr = setupFindingError(finalReport)
		}
		_, failErr := failSetup(s.result, exitRuntime, fmt.Errorf("verify the Barn network: %w", finalErr))
		return failErr
	}
	s.result.Network = &finalReport
	s.recordStep(setupStep{Name: "hosts-helper", Status: "on-demand"}, setupStep{Name: "verify", Status: "ready"})
	return nil
}

func (s *setupSession) publishConfig() error {
	if s.selection.Publish {
		s.working("config", "writing "+s.selection.ConfigPath)
		if len(bytes.TrimSpace(s.selection.ConfigData)) == 0 {
			_, failErr := failSetup(s.result, exitRuntime, errors.New("generated setup configuration is empty"))
			return failErr
		}
		if err := publishSetupConfig(*s.selection); err != nil {
			code := exitRuntime
			if errors.Is(err, os.ErrExist) {
				code = exitConflict
			}
			s.recordStep(setupStep{Name: "config", Status: "failed", Detail: s.selection.ConfigPath})
			_, failErr := failSetup(s.result, code, fmt.Errorf("publish setup configuration without replacing concurrent content: %w", err))
			return failErr
		}
		s.result.Config = s.selection.ConfigPath
		s.result.Changed = true
		tickf(s.stderr, "Wrote %s", s.selection.ConfigPath)
		s.recordStep(setupStep{Name: "config", Status: "created", Detail: s.selection.ConfigPath, Changed: true})
	} else if s.selection.ConfigPath != "" {
		s.result.Config = s.selection.ConfigPath
		tickf(s.stderr, "Using %s", s.selection.ConfigPath)
		s.recordStep(setupStep{Name: "config", Status: "ready", Detail: s.selection.ConfigPath})
	}
	return nil
}

func runSetupCommand(parent context.Context, profileName string, options setupCLIOptions, format outputFormat, verbose bool, stderr io.Writer) (_ commandOutcome, returnErr error) {
	result := setupResult{
		Schema: 1, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Profile: profileName, DryRun: options.DryRun,
		Steps: make([]setupStep, 0, 6), NextArgv: nil,
	}

	cwd, err := os.Getwd()
	if err != nil {
		return failSetup(&result, exitRuntime, err)
	}
	useEnvironment := options.Applied == nil && options.FilePath == ""
	if useEnvironment && profileName == "" {
		discovered, discoverErr := discoverSetupConfig(cwd)
		if discoverErr != nil {
			return failSetup(&result, exitUsage, discoverErr)
		}
		useEnvironment = discovered == ""
	}
	options.vmInputs, err = options.vmInputs.resolve(useEnvironment, true)
	if err != nil {
		return failSetup(&result, exitUsage, err)
	}

	parent, operationID, operationErr := operationContext(parent)
	if operationErr != nil {
		return commandOutcome{}, newRuntimeError(operationErr)
	}
	if !options.DryRun {
		recordOperationPhase(parent, operationID, "setup", "begin", nil, stderr)
		defer func() { recordOperationPhase(parent, operationID, "setup", "complete", returnErr, stderr) }()
	}
	operation := "setup"
	if options.DryRun {
		operation = "setup plan"
	}
	progressItem := startOperationProgress(parent, stderr, "Preparing host", operation)
	session := &setupSession{
		result:       &result,
		options:      options,
		stderr:       stderr,
		progressItem: progressItem,
		networkMode:  "bridge",
	}
	if runtime.GOOS == "darwin" {
		session.networkMode = options.Mode
	}
	defer func() {
		if returnErr != nil && session.activeStep != "" {
			progressItem.Report(activity.Event{Phase: "setup", Item: session.activeStep, State: "failed", Message: returnErr.Error(), Done: true})
		}
		progressItem.Stop(returnErr)
	}()
	result.OperationID = operationID
	if options.DryRun && options.Yes {
		return failSetup(&result, exitUsage, errors.New("--dry-run and --yes are mutually exclusive"))
	}
	if options.Mode != "host" && options.Mode != "shared" {
		return failSetup(&result, exitUsage, errors.New("--mode must be host or shared"))
	}
	if runtime.GOOS != "darwin" && options.Mode != "host" {
		return failSetup(&result, exitUsage, errors.New("--mode shared is available only on Darwin"))
	}

	repository, _, err := image.ResolveRepository(options.Repo, options.Mirror)
	if err != nil {
		return failSetup(&result, exitUsage, err)
	}
	var selection setupSelection
	if options.Applied != nil {
		if options.vmInputs.any() {
			return failSetup(&result, exitUsage, errors.New("--image/--arch cannot be used while preparing an already-applied deployment"))
		}
		selection = setupSelection{Resolved: *options.Applied, ExplicitNetwork: true}
	} else if selection, err = resolveSetupSelectionInputs(profileName, options.FilePath, options.CIDR, options.vmInputs, cwd); err != nil {
		code := exitConflict
		if class, _, _ := failure.Classify(err); class != "" {
			code = classExit(class)
		}
		return failSetup(&result, code, err)
	}
	session.selection = &selection
	result.Resources = resourceInputSources(selection.File.Resources, selection.Inputs, nil)
	result.Profile = selection.Profile
	result.Config = selection.ConfigPath
	result.Image = selection.Image
	result.GuestArch = lifecycleImageArch(selection.Resolved)
	result.Next, result.NextArgv = setupNextCommand(selection, cwd)
	dependencyPlan, err := setuphost.PlanDependencies(setuphost.DependencyProbe{}, true)
	if err != nil {
		return failSetup(&result, exitCapability, err)
	}
	result.Dependencies = dependencyPlan
	if dependencyPlan.Unsupported {
		return failSetup(&result, exitCapability, errors.New(dependencyPlan.Resolution))
	}
	base := execx.OSRunner{Timeout: 20 * time.Minute, OutputLimit: 64 << 10}
	ctx, cancel := context.WithTimeout(parent, 45*time.Minute)
	defer cancel()
	sudoSession := newSudoSession(base, stderr, "setup run")
	defer sudoSession.Close()
	session.base, session.sudo = base, sudoSession

	var networkReport *netpreflight.Report
	if dependencyPlan.Ready {
		report, reportErr := session.chooseNetwork(ctx)
		if reportErr != nil {
			return failSetup(&result, exitCapability, reportErr)
		}
		networkReport = &report
	} else {
		result.NetworkMode = session.networkMode
		if selection.Resolved.Private != nil {
			result.NetworkCIDR = selection.Resolved.Private.CIDR
		}
	}
	printSetupPlan(stderr, dependencyPlan, selection, networkReport, options.DryRun)
	plannedCIDR := result.NetworkCIDR
	result.Config = selection.ConfigPath
	result.Dependencies = dependencyPlan
	if networkReport != nil && !networkReport.Ready && !networkReport.CanRepair() {
		code, blockedErr := session.blockedNetwork(*networkReport)
		return failSetupRendered(&result, code, blockedErr)
	}
	if options.DryRun {
		result.Applicable = true
		session.recordStep(
			setupStep{Name: "dependencies", Status: "planned"},
			setupStep{Name: "network", Status: "planned"},
			setupStep{Name: "hosts-helper", Status: "on-demand"})
		if selection.Publish {
			session.recordStep(setupStep{Name: "config", Status: "planned", Detail: selection.ConfigPath})
		}
		if setupMutating(dependencyPlan, selection, networkReport) {
			result.Next, result.NextArgv = setupApplyCommand(options.arguments(profileName), format, verbose)
		}
		return setupOutcome(result), nil
	}
	if err := confirmSetup(options.Yes, setupMutating(dependencyPlan, selection, networkReport), os.Stdin, stderr); err != nil {
		if errors.Is(err, ErrCancelled) {
			return commandOutcome{}, err
		}
		if errors.Is(err, errSetupNeedsYes) {
			return failSetup(&result, exitUsage, err)
		}
		return failSetup(&result, exitConflict, err)
	}
	for _, name := range []string{"dependencies", "capabilities", "network", "verify", "hosts-helper"} {
		progressItem.Report(activity.Event{Phase: "setup", Item: name, State: "queued", Message: "waiting"})
	}
	if selection.Publish || selection.ConfigPath != "" {
		progressItem.Report(activity.Event{Phase: "setup", Item: "config", State: "queued", Message: selection.ConfigPath})
	}
	if err := session.applyDependencies(ctx, dependencyPlan); err != nil {
		return commandOutcome{}, err
	}
	if err := session.verifyCapabilities(ctx); err != nil {
		return commandOutcome{}, err
	}
	if err := session.applyNetworkPhase(ctx, repository, plannedCIDR); err != nil {
		return commandOutcome{}, err
	}
	if err := session.publishConfig(); err != nil {
		return commandOutcome{}, err
	}
	result.Applicable = true
	result.Ready = true
	return setupOutcome(result), nil
}
