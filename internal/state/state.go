// Package state persists the resolved deployment, node runtime intent, and
// transaction journal as strict versioned JSON under the single data root.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/naming"
	"github.com/pgsty/farrow/internal/network/subnet"
	"github.com/pgsty/farrow/internal/qemu"
	"github.com/pgsty/farrow/internal/spec"
)

const (
	DeploymentSchema  = 2
	NodeSchema        = 2
	TransactionSchema = 2
)

type Phase string

const (
	Absent     Phase = "absent"
	Preparing  Phase = "preparing"
	Prepared   Phase = "prepared"
	Starting   Phase = "starting"
	Running    Phase = "running"
	Stopping   Phase = "stopping"
	Stopped    Phase = "stopped"
	Destroying Phase = "destroying"
)

func validPhase(phase Phase) bool {
	switch phase {
	case Absent, Preparing, Prepared, Starting, Running, Stopping, Stopped, Destroying:
		return true
	default:
		return false
	}
}

// DeploymentState is the applied desired state of the one global deployment.
type DeploymentState struct {
	Schema        int           `json:"schema"`
	FarrowVersion string        `json:"farrow_version"`
	SpecHash      string        `json:"spec_hash"`
	Resolved      spec.Resolved `json:"resolved"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

type Image struct {
	Alias       string `json:"alias"`
	Release     string `json:"release"`
	Digest      string `json:"digest"`
	VirtualSize int64  `json:"virtual_size"`
}

type DataDisk struct {
	Name                string `json:"name"`
	Path                string `json:"path"`
	Serial              string `json:"serial"`
	Size                int64  `json:"size"`
	Mount               string `json:"mount"`
	Persistent          bool   `json:"persistent"`
	RequestedFilesystem string `json:"requested_filesystem,omitempty"`
	ActualFilesystem    string `json:"actual_filesystem,omitempty"`
}

type RuntimePaths struct {
	Directory string `json:"directory"`
	QMP       string `json:"qmp"`
	PIDFile   string `json:"pidfile"`
}

type ProcessIdentity struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
	Started    string `json:"started"`
	ArgvHash   string `json:"argv_hash"`
}

// GuestWarning records a limited guest feature while management SSH remains
// usable. It is refreshed by a successful readiness check, including clearing
// old warnings after repair.
type GuestWarning struct {
	Stage  string `json:"stage"`
	Detail string `json:"detail"`
}

type NodeState struct {
	GuestWarnings []GuestWarning  `json:"-"`
	Schema        int             `json:"schema"`
	FarrowVersion string          `json:"farrow_version"`
	Node          string          `json:"node"`
	VMUUID        string          `json:"vm_uuid"`
	Phase         Phase           `json:"phase"`
	Generation    uint64          `json:"generation"`
	SpecHash      string          `json:"spec_hash"`
	Image         Image           `json:"image"`
	RootDisk      string          `json:"root_disk"`
	DataDisks     []DataDisk      `json:"data_disks,omitempty"`
	Seed          string          `json:"seed"`
	NVRAM         string          `json:"nvram,omitempty"`
	SSHPort       uint16          `json:"ssh_port"`
	Forwards      []qemu.Forward  `json:"forwards"`
	Runtime       RuntimePaths    `json:"runtime"`
	Invocation    qemu.Invocation `json:"invocation"`
	Process       ProcessIdentity `json:"process"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type Action struct {
	Name     string `json:"name"`
	Resource string `json:"resource,omitempty"`
}

type Transaction struct {
	Schema        int       `json:"schema"`
	FarrowVersion string    `json:"farrow_version"`
	OperationID   string    `json:"operation_id"`
	Node          string    `json:"node"`
	From          Phase     `json:"from"`
	To            Phase     `json:"to"`
	Completed     []Action  `json:"completed,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Store reads and writes the deployment documents under one data root.
type Store struct {
	Root string
}

func ensureDir(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("required path is not a real directory: %s", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("required directory is group/world writable: %s mode=%o", path, info.Mode().Perm())
	}
	return os.Chmod(path, mode)
}

// EnsureRoot creates the data root itself (0700) when absent.
func (s Store) EnsureRoot() error {
	if s.Root == "" || !filepath.IsAbs(s.Root) {
		return errors.New("state store root must be absolute")
	}
	return ensureDir(s.Root, 0o700)
}

// NodeDir returns <root>/nodes/<name> after validating the node name.
func (s Store) NodeDir(name string) (string, error) {
	if !naming.ValidNodeName(name) {
		return "", fmt.Errorf("invalid node name %q", name)
	}
	return filepath.Join(s.Root, "nodes", name), nil
}

// EnsureNodeDir creates <root>/nodes/<name> (0700) and its parent.
func (s Store) EnsureNodeDir(name string) (string, error) {
	directory, err := s.NodeDir(name)
	if err != nil {
		return "", err
	}
	if err := s.EnsureRoot(); err != nil {
		return "", err
	}
	if err := ensureDir(filepath.Dir(directory), 0o700); err != nil {
		return "", err
	}
	if err := ensureDir(directory, 0o700); err != nil {
		return "", err
	}
	return directory, nil
}

func strictRead(path string, destination any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("state must be a regular non-symlink file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("state contains trailing JSON data")
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(path, append(data, '\n'), 0o600)
}

func (s Store) deploymentPath() string { return filepath.Join(s.Root, "state.json") }

func (s Store) nodePath(name, filename string) (string, error) {
	directory, err := s.NodeDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, filename), nil
}

func validateDeployment(value DeploymentState) error {
	if value.Schema != DeploymentSchema {
		return fmt.Errorf("unsupported deployment state schema %d", value.Schema)
	}
	if value.FarrowVersion == "" || value.SpecHash == "" || value.UpdatedAt.IsZero() {
		return errors.New("deployment state version/hash/time is invalid")
	}
	actualHash, err := spec.Hash(value.Resolved)
	if err != nil {
		return err
	}
	if actualHash != value.SpecHash {
		return errors.New("deployment resolved spec hash mismatch")
	}
	for _, node := range value.Resolved.Nodes {
		for _, forward := range node.Forwards {
			if forward.RequestedHost != 0 && forward.RequestedHost == forward.Host {
				return fmt.Errorf("deployment resolved forward %s:%d requested_host must differ from its materialized host port", forward.Bind, forward.Host)
			}
		}
	}
	if value.Resolved.Network == "private" {
		if value.Resolved.Private == nil {
			return errors.New("private deployment state lacks its network contract")
		}
		layout, err := subnet.Parse(value.Resolved.Private.CIDR)
		if err != nil || value.Resolved.Private.HostAddress != layout.HostAddress() || value.Resolved.Private.DHCPEnd != layout.DHCPEnd() {
			return errors.New("private deployment state network contract is invalid")
		}
		for _, node := range value.Resolved.Nodes {
			if !layout.IsStatic(node.Address) {
				return fmt.Errorf("private deployment state node %s address is outside the static pool", node.Name)
			}
		}
	}
	return nil
}

func (s Store) WriteDeployment(value DeploymentState) error {
	if err := validateDeployment(value); err != nil {
		return err
	}
	if err := s.EnsureRoot(); err != nil {
		return err
	}
	return writeJSON(s.deploymentPath(), value)
}

func (s Store) ReadDeployment() (DeploymentState, error) {
	var value DeploymentState
	if err := strictRead(s.deploymentPath(), &value); err != nil {
		return DeploymentState{}, err
	}
	if err := validateDeployment(value); err != nil {
		return DeploymentState{}, err
	}
	return value, nil
}

func validateNode(value NodeState, expectedNode string) error {
	if value.Schema != NodeSchema {
		return fmt.Errorf("unsupported node state schema %d", value.Schema)
	}
	if value.FarrowVersion == "" || value.Node != expectedNode || value.VMUUID == "" || !validPhase(value.Phase) || value.Generation == 0 || value.SpecHash == "" || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return errors.New("node state version, identity, phase, generation, hash, or time is invalid")
	}
	for _, disk := range value.DataDisks {
		requested := disk.RequestedFilesystem
		if requested != "" && !spec.ValidFilesystem(requested) {
			return fmt.Errorf("data disk %s has invalid requested filesystem %q", disk.Name, requested)
		}
		actual := disk.ActualFilesystem
		if actual != "" && !spec.ValidFormattedFilesystem(actual) {
			return fmt.Errorf("data disk %s has invalid actual filesystem %q", disk.Name, actual)
		}
	}
	return nil
}

func (s Store) WriteNode(value NodeState) error {
	if err := validateNode(value, value.Node); err != nil {
		return err
	}
	if _, err := s.EnsureNodeDir(value.Node); err != nil {
		return err
	}
	path, err := s.nodePath(value.Node, "state.json")
	if err != nil {
		return err
	}
	return writeJSON(path, value)
}

func (s Store) ReadNode(name string) (NodeState, error) {
	path, err := s.nodePath(name, "state.json")
	if err != nil {
		return NodeState{}, err
	}
	var value NodeState
	if err := strictRead(path, &value); err != nil {
		return NodeState{}, err
	}
	if err := validateNode(value, name); err != nil {
		return NodeState{}, err
	}
	value.GuestWarnings = s.readGuestWarnings(value)
	return value, nil
}

func validateTransaction(value Transaction) error {
	if value.Schema != TransactionSchema || value.FarrowVersion == "" || value.OperationID == "" || value.Node == "" || !validPhase(value.From) || !validPhase(value.To) || value.StartedAt.IsZero() || value.UpdatedAt.IsZero() {
		return errors.New("transaction schema, version, or fields are invalid")
	}
	return nil
}

func (s Store) ReadTransaction(name string) (Transaction, error) {
	path, err := s.nodePath(name, "transaction.json")
	if err != nil {
		return Transaction{}, err
	}
	var value Transaction
	if err := strictRead(path, &value); err != nil {
		return Transaction{}, err
	}
	if err := validateTransaction(value); err != nil {
		return Transaction{}, err
	}
	return value, nil
}
