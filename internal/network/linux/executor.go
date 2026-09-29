package linux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/identity"
	"github.com/pgsty/barn/internal/network/subnet"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/process"
	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/qmp"
)

type Executor struct {
	// InUse, when set, refuses uninstall while the deployment still has
	// live or recorded VMs.
	InUse         func(context.Context) error
	User          execx.Runner
	Root          execx.Runner
	LookPath      func(string) (string, error)
	StagingParent string
}

type InstallReport struct {
	Plan     Plan              `json:"plan"`
	Applied  bool              `json:"applied"`
	Checks   map[string]string `json:"checks,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
}

type UninstallReport struct {
	Plan    UninstallPlan     `json:"plan"`
	Applied bool              `json:"applied"`
	Checks  map[string]string `json:"checks,omitempty"`
}

// ErrInstallRolledBack means installation failed after mutation began, but
// the executor verified that its owned host changes were removed/restored.
var ErrInstallRolledBack = errors.New("installing the Barn network failed and was rolled back")

func (e Executor) validate() error {
	if e.User == nil || e.Root == nil {
		return errors.New("linux network executor requires user and privileged runners")
	}
	return nil
}

func (e Executor) lookPath(name string) (string, error) {
	if e.LookPath != nil {
		return e.LookPath(name)
	}
	return exec.LookPath(name)
}

func (e Executor) PlanInstallConfig(ctx context.Context, config Config) (Plan, error) {
	if err := e.validate(); err != nil {
		return Plan{}, err
	}
	facts, err := DiscoverFacts(ctx, e.User, e.Root)
	if err != nil {
		return Plan{}, err
	}
	return NewInstallPlan(facts, config)
}

func stagedFileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<20 {
		return "", fmt.Errorf("staged network source is unsafe: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fileDigest(string(data)), nil
}

func (e Executor) installTarget(ctx context.Context, targetPath string, target StagedTarget) error {
	digest, err := stagedFileDigest(target.Source)
	if err != nil {
		return err
	}
	if digest != target.SHA256 || target.Owner != "root:root" {
		return fmt.Errorf("staged target identity mismatch: %s", targetPath)
	}
	_, err = e.Root.Run(ctx, "/usr/bin/install", "-o", "root", "-g", "root", "-m", target.Mode, target.Source, targetPath)
	return err
}

func (e Executor) waitBridge(ctx context.Context, config Config) error {
	expected := config.HostAddress + "/24"
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		result, err := e.User.Run(ctx, "/usr/sbin/ip", "-4", "-o", "address", "show", "dev", BridgeName)
		if err == nil && strings.Contains(string(result.Stdout), expected) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("barn0 did not acquire %s", expected)
}

func (e Executor) runInstallPhases(ctx context.Context, phases []CommandPhase, persistOnly bool, config Config) error {
	for _, phase := range phases {
		isPersist := phase.Name == "persist-after-attach-verification"
		if isPersist != persistOnly {
			continue
		}
		for _, command := range phase.Commands {
			if command.Binary == "/usr/bin/networkctl" && len(command.Args) == 2 && command.Args[0] == "reconfigure" {
				if err := e.waitBridge(ctx, config); err != nil {
					return err
				}
			}
			if _, err := e.Root.Run(ctx, command.Binary, command.Args...); err != nil {
				return fmt.Errorf("install phase %s: %w", phase.Name, err)
			}
		}
	}
	return nil
}

func waitQMPIdentity(ctx context.Context, client *qmp.Client, socket, name, uuid string) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		actualName, nameErr := client.QueryName(ctx, socket)
		actualUUID, uuidErr := client.QueryUUID(ctx, socket)
		if nameErr == nil && uuidErr == nil && actualName.Name == name && strings.EqualFold(actualUUID.UUID, uuid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("the test VM did not answer on QMP")
}

func readSmokePID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, errors.New("the test VM wrote an invalid pidfile")
	}
	return pid, nil
}

func (e Executor) helperAttachSmoke(ctx context.Context, helper string) (returnErr error) {
	profile, err := platform.Native()
	if err != nil || profile.OS != "linux" {
		return errors.New("the bridge attach test requires native Linux")
	}
	qemuPath, err := platform.FindQEMUBinary(profile, e.lookPath)
	if err != nil {
		return err
	}
	runtimeDir, err := os.MkdirTemp("/tmp", "barn-network-smoke-")
	if err != nil {
		return err
	}
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		return err
	}
	qmpPath := filepath.Join(runtimeDir, "qmp.sock")
	pidfile := filepath.Join(runtimeDir, "qemu.pid")
	uuid, err := identity.NewUUID()
	if err != nil {
		return err
	}
	mac, err := identity.MAC("169.254.250.250", identity.NICPrivate)
	if err != nil {
		return err
	}
	name := "barn-network-smoke"
	invocation := qemu.Invocation{Binary: qemuPath, Args: []string{
		"-name", name, "-uuid", uuid, "-machine", profile.Machine,
		"-accel", profile.Accelerator, "-cpu", profile.CPU, "-m", "128",
		"-display", "none", "-nodefaults", "-no-user-config", "-S",
		"-qmp", "unix:" + qmpPath + ",server=on,wait=off", "-pidfile", pidfile, "-daemonize",
		"-netdev", "bridge,id=private,br=" + BridgeName + ",helper=" + helper,
		"-device", "virtio-net-pci,netdev=private,mac=" + mac,
	}}
	client := &qmp.Client{Timeout: 5 * time.Second}
	pid := 0
	defer func() {
		if pid > 0 && process.Alive(pid) {
			actualName, nameErr := client.QueryName(context.Background(), qmpPath)
			actualUUID, uuidErr := client.QueryUUID(context.Background(), qmpPath)
			if nameErr == nil && uuidErr == nil && actualName.Name == name && strings.EqualFold(actualUUID.UUID, uuid) {
				_ = client.Quit(context.Background(), qmpPath)
			}
			deadline := time.Now().Add(10 * time.Second)
			for process.Alive(pid) && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
			}
			if process.Alive(pid) && returnErr == nil {
				returnErr = errors.New("could not confirm that the test VM exited")
				return
			}
		}
		for _, path := range []string{qmpPath, pidfile} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && returnErr == nil {
				returnErr = err
			}
		}
		if err := os.Remove(runtimeDir); err != nil && !errors.Is(err, os.ErrNotExist) && returnErr == nil {
			returnErr = err
		}
	}()
	if _, err := e.User.Run(ctx, invocation.Binary, invocation.Args...); err != nil {
		return err
	}
	if err := waitQMPIdentity(ctx, client, qmpPath, name, uuid); err != nil {
		return err
	}
	pid, err = readSmokePID(pidfile)
	if err != nil {
		return err
	}
	identityValue, err := process.Capture(ctx, e.User, invocation, pid)
	if err != nil || !process.MatchesLive(ctx, e.User, identityValue, invocation) {
		return errors.New("could not verify the test VM process")
	}
	members, err := e.User.Run(ctx, "/usr/sbin/ip", "-o", "link", "show", "master", BridgeName)
	if err != nil || !strings.Contains(string(members.Stdout), "tap") {
		return errors.New("qemu-bridge-helper attached no tap device to barn0")
	}
	if err := client.Quit(ctx, qmpPath); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for process.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if process.Alive(pid) {
		return errors.New("the test VM did not exit after QMP quit")
	}
	pid = 0
	return nil
}

func (e Executor) InstallConfig(ctx context.Context, config Config, apply bool) (_ InstallReport, returnErr error) {
	plan, err := e.PlanInstallConfig(ctx, config)
	if err != nil {
		return InstallReport{}, err
	}
	report := InstallReport{Plan: plan, Checks: make(map[string]string)}
	if layout, layoutErr := subnet.Parse(config.CIDR); layoutErr == nil {
		if warning := layout.Warning(); warning != "" {
			report.Warnings = []string{warning}
		}
	}
	if !apply {
		return report, nil
	}
	parent := e.StagingParent
	if parent == "" {
		parent = os.TempDir()
	}
	staging, err := os.MkdirTemp(parent, "barn-linux-network-")
	if err != nil {
		return report, err
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		return report, err
	}
	defer func() {
		if err := os.RemoveAll(staging); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove Linux network staging: %w", err))
		}
	}()
	staged, err := StageInstallPlan(plan, staging)
	if err != nil {
		return report, err
	}
	for _, directory := range plan.Directories {
		if directory.Owner != "root:root" {
			return report, fmt.Errorf("unexpected directory owner in Linux network plan: %s", directory.Path)
		}
		if _, err := e.Root.Run(ctx, "/usr/bin/install", "-d", "-o", "root", "-g", "root", "-m", directory.Mode, directory.Path); err != nil {
			return report, err
		}
	}
	stateTarget := staged.Targets[StatePath]
	if err := e.installTarget(ctx, StatePath, stateTarget); err != nil {
		return report, err
	}
	for _, file := range plan.Files {
		if file.Path == StatePath {
			continue
		}
		if err := e.installTarget(ctx, file.Path, staged.Targets[file.Path]); err != nil {
			return report, err
		}
	}
	if err := e.runInstallPhases(ctx, plan.Phases, false, config); err != nil {
		return e.rollbackInstall(report, err)
	}
	if err := e.waitBridge(ctx, config); err != nil {
		return e.rollbackInstall(report, err)
	}
	if err := e.helperAttachSmoke(ctx, plan.Manifest.HelperPath); err != nil {
		return e.rollbackInstall(report, fmt.Errorf("test that a non-root VM can attach to barn0: %w", err))
	}
	if err := e.runInstallPhases(ctx, plan.Phases, true, config); err != nil {
		return e.rollbackInstall(report, err)
	}
	// The non-persistence phases are intentionally idempotent and ran twice;
	// this also exercises repeated reload/reconfigure without recapturing state.
	report.Applied = true
	report.Checks["bridge"] = "barn0 " + config.HostAddress + "/24"
	report.Checks["helper-attach"] = "a non-root test VM attached to barn0"
	return report, nil
}

func (e Executor) rollbackInstall(report InstallReport, installErr error) (InstallReport, error) {
	rollbackContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rollback, rollbackErr := e.Uninstall(rollbackContext, true)
	if rollbackErr != nil || !rollback.Applied {
		if rollbackErr == nil {
			rollbackErr = errors.New("rollback did not report an applied uninstall")
		}
		return report, fmt.Errorf("%w; automatic rollback also failed: %v", installErr, rollbackErr)
	}
	return report, fmt.Errorf("%w: %w", ErrInstallRolledBack, installErr)
}

func (e Executor) currentUninstallFacts(ctx context.Context, manifest Manifest) (UninstallFacts, error) {
	currentFiles := make(map[string]string, len(manifest.Files))
	for path := range manifest.Files {
		data, err := os.ReadFile(path)
		if err != nil {
			return UninstallFacts{}, err
		}
		currentFiles[path] = string(data)
	}
	facts, err := discoverFacts(ctx, e.User, e.Root, false)
	if err != nil {
		return UninstallFacts{}, err
	}
	currentHelper := Override{Owner: "root", Group: facts.Helper.Group, Mode: fmt.Sprintf("%04o", facts.Helper.Mode)}
	membersResult, err := e.User.Run(ctx, "/usr/sbin/ip", "-o", "link", "show", "master", BridgeName)
	if err != nil {
		return UninstallFacts{}, err
	}
	members := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(membersResult.Stdout)), "\n") {
		if line != "" {
			members = append(members, line)
		}
	}
	return UninstallFacts{BridgeMembers: members, CurrentFiles: currentFiles, CurrentHelper: currentHelper, CurrentOverride: facts.Helper.Override}, nil
}

func (e Executor) PlanUninstall(ctx context.Context) (UninstallPlan, Manifest, error) {
	if err := e.validate(); err != nil {
		return UninstallPlan{}, Manifest{}, err
	}
	facts, err := discoverFacts(ctx, e.User, e.Root, false)
	if err != nil {
		return UninstallPlan{}, Manifest{}, err
	}
	if facts.ExistingManifest == nil {
		return UninstallPlan{}, Manifest{}, errors.New("the Barn network is not installed")
	}
	manifest := *facts.ExistingManifest
	uninstallFacts, err := e.currentUninstallFacts(ctx, manifest)
	if err != nil {
		return UninstallPlan{}, Manifest{}, err
	}
	plan, err := NewUninstallPlan(manifest, uninstallFacts)
	return plan, manifest, err
}

func (e Executor) rootUnlink(ctx context.Context, path string) error {
	_, err := e.Root.Run(ctx, "/usr/bin/unlink", path)
	return err
}

func (e Executor) rootRmdir(ctx context.Context, path string) error {
	if _, err := e.Root.Run(ctx, "/usr/bin/rmdir", path); err != nil {
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

func (e Executor) Uninstall(ctx context.Context, apply bool) (_ UninstallReport, returnErr error) {
	plan, manifest, err := e.PlanUninstall(ctx)
	if err != nil {
		return UninstallReport{}, err
	}
	report := UninstallReport{Plan: plan, Checks: make(map[string]string)}
	if !apply {
		return report, nil
	}
	if e.InUse != nil {
		if err := e.InUse(ctx); err != nil {
			return report, fmt.Errorf("will not remove the Barn network: %w", err)
		}
	}
	// Re-run all ownership/member/hash checks immediately before mutation.
	uninstallFacts, err := e.currentUninstallFacts(ctx, manifest)
	if err != nil {
		return report, err
	}
	if _, err := NewUninstallPlan(manifest, uninstallFacts); err != nil {
		return report, err
	}
	if ManifestBackend(manifest) == BackendNetworkManager {
		return e.applyNetworkManagerUninstall(ctx, report, plan)
	}
	if _, err := e.Root.Run(ctx, "/usr/bin/networkctl", "delete", BridgeName); err != nil {
		return report, err
	}
	for _, path := range []string{NetDevPath, NetworkPath} {
		if err := e.rootUnlink(ctx, path); err != nil {
			return report, err
		}
	}
	if _, err := e.Root.Run(ctx, "/usr/bin/networkctl", "reload"); err != nil {
		return report, err
	}
	if manifest.AppliedOverride != nil {
		for _, command := range []Command{
			{Binary: "/usr/bin/dpkg-statoverride", Args: []string{"--remove", manifest.HelperPath}},
			{Binary: "/bin/chown", Args: []string{manifest.OriginalHelper.Owner + ":" + manifest.OriginalHelper.Group, manifest.HelperPath}},
			{Binary: "/bin/chmod", Args: []string{manifest.OriginalHelper.Mode, manifest.HelperPath}},
		} {
			if _, err := e.Root.Run(ctx, command.Binary, command.Args...); err != nil {
				return report, err
			}
		}
	}
	if manifest.OriginalBridgePath.Existed {
		staging, err := os.MkdirTemp(e.StagingParent, "barn-bridge-restore-")
		if err != nil {
			return report, err
		}
		defer func() {
			if err := os.RemoveAll(staging); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("remove Linux bridge-restore staging: %w", err))
			}
		}()
		source := filepath.Join(staging, "bridge.conf")
		if err := os.WriteFile(source, []byte(manifest.OriginalBridgeConf), 0o600); err != nil {
			return report, err
		}
		ownerParts := strings.Split(manifest.OriginalBridgePath.Owner+":"+manifest.OriginalBridgePath.Group, ":")
		if len(ownerParts) != 2 {
			return report, errors.New("original bridge.conf ownership is invalid")
		}
		if _, err := e.Root.Run(ctx, "/usr/bin/install", "-o", ownerParts[0], "-g", ownerParts[1], "-m", manifest.OriginalBridgePath.Mode, source, BridgeConfPath); err != nil {
			return report, err
		}
	} else {
		if err := e.rootUnlink(ctx, BridgeConfPath); err != nil {
			return report, err
		}
	}
	for _, command := range restoreNetworkdCommands(manifest.NetworkdUnits) {
		if _, err := e.Root.Run(ctx, command.Binary, command.Args...); err != nil {
			return report, err
		}
	}
	if err := e.rootUnlink(ctx, StatePath); err != nil {
		return report, err
	}
	for _, directory := range plan.RemoveDirectories {
		if err := e.rootRmdir(ctx, directory); err != nil {
			return report, err
		}
	}
	report.Applied = true
	report.Checks["restored"] = "helper, networkd units, paths and bridge"
	return report, nil
}

// applyNetworkManagerUninstall executes the NM-backend uninstall plan exactly:
// delete the owned bridge connection, restore helper prestate, then remove or
// restore the owned files and prune Barn-created directories.
func (e Executor) applyNetworkManagerUninstall(ctx context.Context, report UninstallReport, plan UninstallPlan) (UninstallReport, error) {
	for _, phase := range plan.Phases {
		for _, command := range phase.Commands {
			if _, err := e.Root.Run(ctx, command.Binary, command.Args...); err != nil {
				return report, fmt.Errorf("uninstall phase %s: %w", phase.Name, err)
			}
		}
	}
	for _, file := range plan.RestoreFiles {
		staging, err := os.MkdirTemp(e.StagingParent, "barn-bridge-restore-")
		if err != nil {
			return report, err
		}
		source := filepath.Join(staging, filepath.Base(file.Path))
		if err := os.WriteFile(source, []byte(file.Content), 0o600); err != nil {
			_ = os.RemoveAll(staging)
			return report, err
		}
		ownerParts := strings.Split(file.Owner, ":")
		if len(ownerParts) != 2 {
			_ = os.RemoveAll(staging)
			return report, errors.New("restore file ownership is invalid")
		}
		if _, err := e.Root.Run(ctx, "/usr/bin/install", "-o", ownerParts[0], "-g", ownerParts[1], "-m", file.Mode, source, file.Path); err != nil {
			_ = os.RemoveAll(staging)
			return report, err
		}
		_ = os.RemoveAll(staging)
	}
	for _, path := range plan.RemoveFiles {
		if err := e.rootUnlink(ctx, path); err != nil {
			return report, err
		}
	}
	for _, directory := range plan.RemoveDirectories {
		if err := e.rootRmdir(ctx, directory); err != nil {
			return report, err
		}
	}
	report.Applied = true
	report.Checks["restored"] = "helper, bridge connection and paths"
	return report, nil
}
