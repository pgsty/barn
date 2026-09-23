package private

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/pgsty/farrow/internal/cloudinit"
	"github.com/pgsty/farrow/internal/disk"
	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/hostshare"
	"github.com/pgsty/farrow/internal/identity"
	"github.com/pgsty/farrow/internal/persistent"
	"github.com/pgsty/farrow/internal/platform"
	"github.com/pgsty/farrow/internal/qemu"
	"github.com/pgsty/farrow/internal/spec"
)

type DiskOps interface {
	CreateOverlay(context.Context, string, string, int64) (disk.Info, error)
	CreateBlank(context.Context, string, int64) (disk.Info, error)
}

type BaseImage struct {
	Path        string
	Alias       string
	Release     string
	Digest      string
	VirtualSize int64
}

type Backend struct {
	DarwinSocket      string
	ReconnectMS       int
	DarwinUseFD       bool
	LinuxBridgeHelper string
	NetworkCIDR       string
	HostAddress       string
	DHCPEnd           string
}

type PrepareConfig struct {
	DeploymentRoot string
	Resolved       spec.Resolved
	// SpecHash is the whole-deployment drift summary; NodeHashes carries each
	// node's own drift identity (see spec.NodeHash). Seeds, journals, and node
	// state bind to the node hash so peers can be added without touching them.
	SpecHash        string
	NodeHashes      map[string]string
	Plan            Plan
	Seeds           map[string]cloudinit.Files
	SeedInput       *SeedInput
	Bases           map[string]BaseImage
	SSHPorts        map[string]uint16
	Profile         platform.Profile
	QEMUBinary      string
	Firmware        platform.Firmware
	UseUEFI         bool
	Backend         Backend
	Disks           DiskOps
	OperationSource UUIDSource
	Now             func() time.Time
}

type OwnedArtifact struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type PrepareJournal struct {
	Schema         int             `json:"schema"`
	OperationID    string          `json:"operation_id"`
	Node           string          `json:"node"`
	VMUUID         string          `json:"vm_uuid"`
	SpecHash       string          `json:"spec_hash"`
	Completed      []OwnedArtifact `json:"completed"`
	Invocation     qemu.Invocation `json:"invocation"`
	Prepared       bool            `json:"prepared"`
	StateCommitted bool            `json:"state_committed"`
	StatePath      string          `json:"state_path,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type DataArtifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Serial string `json:"serial"`
	Size   int64  `json:"size"`
	Mount  string `json:"mount"`
}

type NodeArtifacts struct {
	Name        string          `json:"name"`
	NodeDir     string          `json:"node_dir"`
	Journal     string          `json:"journal"`
	Root        string          `json:"root"`
	RootSerial  string          `json:"root_serial"`
	Data        []DataArtifact  `json:"data"`
	Seed        string          `json:"seed"`
	NVRAM       string          `json:"nvram,omitempty"`
	SerialLog   string          `json:"serial_log"`
	QEMULog     string          `json:"qemu_log"`
	Invocation  qemu.Invocation `json:"invocation"`
	ImageDigest string          `json:"image_digest"`
}

type PrepareOutcome struct {
	Node      string         `json:"node"`
	Artifacts *NodeArtifacts `json:"artifacts,omitempty"`
	Error     string         `json:"error,omitempty"`
	Reason    string         `json:"reason,omitempty"`
}

func (config PrepareConfig) now() time.Time {
	if config.Now != nil {
		return config.Now().UTC()
	}
	return time.Now().UTC()
}

func validatePrepareConfig(config PrepareConfig) error {
	if config.DeploymentRoot == "" || !filepath.IsAbs(config.DeploymentRoot) || config.QEMUBinary == "" || !filepath.IsAbs(config.QEMUBinary) || config.Disks == nil || len(config.Plan.Nodes) != len(config.Resolved.Nodes) || len(config.Seeds) != len(config.Resolved.Nodes) || len(config.SpecHash) != 64 {
		return errors.New("prepare deployment, QEMU, disks, plan, or seeds are incomplete")
	}
	for _, node := range config.Resolved.Nodes {
		if len(config.NodeHashes[node.Name]) != 64 {
			return fmt.Errorf("prepare node hash missing for node %s", node.Name)
		}
	}
	info, err := os.Lstat(config.DeploymentRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("prepare deployment root must be a real mode-0700 directory")
	}
	if (config.UseUEFI || config.Profile.RequiresUEFI) && (config.Firmware.Code == "" || config.Firmware.Vars == "") {
		return errors.New("prepare platform requires firmware code/vars")
	}
	switch config.Profile.OS {
	case "darwin":
		if !filepath.IsAbs(config.Backend.DarwinSocket) || (!config.Backend.DarwinUseFD && config.Backend.ReconnectMS <= 0) {
			return errors.New("darwin private prepare requires a socket and a selected stream/FD policy")
		}
	case "linux":
		if !filepath.IsAbs(config.Backend.LinuxBridgeHelper) {
			return errors.New("linux private prepare requires an absolute bridge helper")
		}
	default:
		return errors.New("prepare supports Darwin/Linux profiles only")
	}
	return nil
}

func nodeSpec(resolved spec.Resolved, name string) (spec.Node, bool) {
	for _, node := range resolved.Nodes {
		if node.Name == name {
			return node, true
		}
	}
	return spec.Node{}, false
}

func writePrepareJournal(path string, value PrepareJournal) error {
	if err := validatePrepareJournal(path, value); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(path, append(data, '\n'), 0o600)
}

func appendCompleted(path string, journal *PrepareJournal, artifact OwnedArtifact, now time.Time) error {
	journal.Completed = append(journal.Completed, artifact)
	journal.UpdatedAt = now
	return writePrepareJournal(path, *journal)
}

func copyFirmwareVars(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("firmware vars source is not a regular non-symlink file")
	}
	temporary, _, err := fsutil.CopyToTemp(source, filepath.Dir(target), ".nvram-*.partial", 0o600, 512<<20)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temporary)
		}
	}()
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	keep = true
	return fsutil.SyncDir(filepath.Dir(target))
}

func privateBackend(config PrepareConfig, node NodePlan) *qemu.PrivateNetwork {
	if config.Profile.OS == "darwin" {
		if config.Backend.DarwinUseFD {
			return &qemu.PrivateNetwork{MAC: node.PrivateMAC, FD: 3}
		}
		return &qemu.PrivateNetwork{MAC: node.PrivateMAC, StreamSocket: config.Backend.DarwinSocket, ReconnectMS: config.Backend.ReconnectMS}
	}
	return &qemu.PrivateNetwork{MAC: node.PrivateMAC, Bridge: "farrow0", BridgeHelper: config.Backend.LinuxBridgeHelper}
}

func PrepareNode(ctx context.Context, config PrepareConfig, name string) (NodeArtifacts, error) {
	if err := validatePrepareConfig(config); err != nil {
		return NodeArtifacts{}, err
	}
	nodePlan, ok := config.Plan.Node(name)
	if !ok {
		return NodeArtifacts{}, fmt.Errorf("plan has no node %s", name)
	}
	definition, ok := nodeSpec(config.Resolved, name)
	if !ok {
		return NodeArtifacts{}, fmt.Errorf("resolved spec has no node %s", name)
	}
	if err := hostshare.Validate(config.DeploymentRoot, definition.Shares); err != nil {
		return NodeArtifacts{}, fmt.Errorf("validate host shares for node %s: %w", name, err)
	}
	baseAlias := definition.Image
	if baseAlias == "" {
		baseAlias = config.Resolved.Image
	}
	base, ok := config.Bases[baseAlias]
	if !ok || !filepath.IsAbs(base.Path) || base.Digest == "" {
		return NodeArtifacts{}, fmt.Errorf("node %s has no validated base image", name)
	}
	seedFiles, ok := config.Seeds[name]
	if !ok {
		return NodeArtifacts{}, fmt.Errorf("node %s has no rendered seed", name)
	}
	sshPort := config.SSHPorts[name]
	if sshPort == 0 {
		return NodeArtifacts{}, fmt.Errorf("node %s has no management SSH port", name)
	}
	persistentIdentities, err := privatePersistentIdentities(privatePrepareDeployment(config), config.Resolved)
	if err != nil {
		return NodeArtifacts{}, err
	}
	if _, err := persistent.ValidateDesired(privatePrepareDeployment(config).Root, persistentIdentities); err != nil {
		return NodeArtifacts{}, err
	}
	operationID, err := identity.NewUUID()
	if config.OperationSource != nil {
		operationID, err = config.OperationSource()
	}
	if err != nil || !identity.ValidUUID(operationID) {
		return NodeArtifacts{}, errors.New("prepare operation UUID is invalid")
	}
	nodesDir := filepath.Join(config.DeploymentRoot, "nodes")
	if err := os.Mkdir(nodesDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return NodeArtifacts{}, err
	}
	if info, err := os.Lstat(nodesDir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return NodeArtifacts{}, errors.New("nodes directory is unsafe")
	}
	nodeDir := filepath.Join(nodesDir, name)
	if err := os.Mkdir(nodeDir, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return NodeArtifacts{}, fmt.Errorf("create new private node directory: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return NodeArtifacts{}, err
		}
		info, err := os.Lstat(nodeDir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			return NodeArtifacts{}, fmt.Errorf("preserved unsafe unfinished node directory %s", nodeDir)
		}
		metadata, ok := info.Sys().(*syscall.Stat_t)
		if !ok || metadata.Uid != uint32(os.Geteuid()) {
			return NodeArtifacts{}, fmt.Errorf("preserved unfinished node directory %s owned by another user", nodeDir)
		}
		// An offline prepare may leave a journal after a disk/tool failure.
		// Reuse the existing rollback boundary before preparing again: it refuses
		// committed state, runtime artifacts and any unrecognized files.
		journal, err := ReadPrepareJournal(filepath.Join(nodeDir, "private-prepare.json"))
		if err != nil || journal.Node != name || journal.SpecHash != config.NodeHashes[name] {
			return NodeArtifacts{}, fmt.Errorf("unfinished node directory %s cannot be recovered automatically; preserve it and inspect its prepare journal", nodeDir)
		}
		for _, path := range []string{nodePlan.Runtime.QMP, nodePlan.Runtime.PIDFile} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return NodeArtifacts{}, fmt.Errorf("unfinished node %s has runtime artifacts; run farrow status before retrying", name)
			}
		}
		if _, err := RollbackPrepared(privatePrepareDeployment(config), name, true); err != nil {
			return NodeArtifacts{}, fmt.Errorf("preserved unfinished node %s: %w", name, err)
		}
		if err := os.Mkdir(nodeDir, 0o700); err != nil {
			return NodeArtifacts{}, fmt.Errorf("create recovered private node directory: %w", err)
		}
	}
	now := config.now()
	journalPath := filepath.Join(nodeDir, "private-prepare.json")
	journal := PrepareJournal{Schema: 1, OperationID: operationID, Node: name, VMUUID: nodePlan.VMUUID, SpecHash: config.NodeHashes[name], StartedAt: now, UpdatedAt: now, Completed: []OwnedArtifact{}}
	if err := writePrepareJournal(journalPath, journal); err != nil {
		return NodeArtifacts{}, err
	}
	artifacts := NodeArtifacts{Name: name, NodeDir: nodeDir, Journal: journalPath, Root: filepath.Join(nodeDir, "root.qcow2"), Seed: filepath.Join(nodeDir, "seed.iso"), SerialLog: filepath.Join(nodeDir, "serial.log"), QEMULog: filepath.Join(nodeDir, "qemu.log"), ImageDigest: base.Digest}
	artifacts.RootSerial, err = identity.DiskSerial(name, "root")
	if err != nil {
		return artifacts, err
	}
	if _, err := config.Disks.CreateOverlay(ctx, base.Path, artifacts.Root, definition.RootDisk); err != nil {
		return artifacts, err
	}
	if err := appendCompleted(journalPath, &journal, OwnedArtifact{Kind: "root-overlay", Path: artifacts.Root}, config.now()); err != nil {
		return artifacts, err
	}
	qemuData := make([]qemu.Disk, 0, len(definition.Disks))
	freshDisks := make(map[string]bool)
	for _, diskSpec := range definition.Disks {
		serial, err := identity.DiskSerial(name, diskSpec.Name)
		if err != nil {
			return artifacts, err
		}
		path, created, err := resolvePrivateDataDisk(ctx, config, nodeDir, diskSpec, serial)
		if err != nil {
			return artifacts, err
		}
		artifacts.Data = append(artifacts.Data, DataArtifact{Name: diskSpec.Name, Path: path, Serial: serial, Size: diskSpec.Size, Mount: diskSpec.Mount})
		qemuData = append(qemuData, qemu.Disk{Path: path, Serial: serial})
		freshDisks[serial] = created
		if created {
			if err := appendCompleted(journalPath, &journal, OwnedArtifact{Kind: "data-disk", Path: path}, config.now()); err != nil {
				return artifacts, err
			}
		}
	}
	if config.SeedInput != nil {
		input := *config.SeedInput
		input.FreshDisks = map[string]map[string]bool{name: freshDisks}
		seeds, err := RenderSeeds(config.Resolved, config.Plan, input)
		if err != nil {
			return artifacts, err
		}
		seedFiles = seeds[name]
	}
	if err := cloudinit.BuildISO(artifacts.Seed, seedFiles); err != nil {
		return artifacts, err
	}
	if err := appendCompleted(journalPath, &journal, OwnedArtifact{Kind: "seed", Path: artifacts.Seed}, config.now()); err != nil {
		return artifacts, err
	}
	var firmware *qemu.Firmware
	if config.UseUEFI || config.Profile.RequiresUEFI {
		artifacts.NVRAM = filepath.Join(nodeDir, "nvram.fd")
		if err := copyFirmwareVars(config.Firmware.Vars, artifacts.NVRAM); err != nil {
			return artifacts, err
		}
		if err := appendCompleted(journalPath, &journal, OwnedArtifact{Kind: "nvram", Path: artifacts.NVRAM}, config.now()); err != nil {
			return artifacts, err
		}
		firmware = &qemu.Firmware{Code: config.Firmware.Code, Vars: artifacts.NVRAM}
	}
	forwards := []qemu.Forward{{Bind: "127.0.0.1", Host: sshPort, Guest: 22}}
	for _, forward := range definition.Forwards {
		forwards = append(forwards, qemu.Forward{Bind: forward.Bind, Host: forward.Host, Guest: forward.Guest})
	}
	artifacts.Invocation, err = qemu.Build(qemu.Config{
		Profile: config.Profile, Binary: config.QEMUBinary, Name: name, UUID: nodePlan.VMUUID,
		CPUs: definition.CPUs, Memory: definition.Memory, Firmware: firmware,
		Root: qemu.Disk{Path: artifacts.Root, Serial: artifacts.RootSerial}, Data: qemuData, Seed: artifacts.Seed,
		QMP: nodePlan.Runtime.QMP, PIDFile: nodePlan.Runtime.PIDFile, SerialLog: artifacts.SerialLog,
		MgmtMAC: nodePlan.ManagementMAC, Forwards: forwards, Private: privateBackend(config, nodePlan), Shares: hostshare.QEMU(definition.Shares), Detach: true,
	})
	if err != nil {
		return artifacts, err
	}
	journal.Invocation = artifacts.Invocation
	journal.Prepared = true
	journal.UpdatedAt = config.now()
	if err := writePrepareJournal(journalPath, journal); err != nil {
		return artifacts, err
	}
	return artifacts, nil
}

func PrepareSelected(ctx context.Context, config PrepareConfig, names []string, concurrency int) []PrepareOutcome {
	if concurrency <= 0 {
		concurrency = 4
	}
	if concurrency > 16 {
		concurrency = 16
	}
	outcomes := make([]PrepareOutcome, len(names))
	jobs := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				name := names[index]
				outcomes[index].Node = name
				if err := ctx.Err(); err != nil {
					outcomes[index].Error = err.Error()
					continue
				}
				artifacts, err := PrepareNode(ctx, config, name)
				if err != nil {
					outcomes[index].Error, outcomes[index].Reason = err.Error(), failureReason(err)
					continue
				}
				outcomes[index].Artifacts = &artifacts
			}
		}()
	}
	for index := range names {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	return outcomes
}

func PreparedNames(outcomes []PrepareOutcome) []string {
	names := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.Artifacts != nil && outcome.Error == "" {
			names = append(names, outcome.Node)
		}
	}
	sort.Strings(names)
	return names
}
