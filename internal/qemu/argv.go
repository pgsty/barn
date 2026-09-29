// Package qemu builds the typed argv for Barn's single QEMU backend.
package qemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pgsty/barn/internal/naming"
	"github.com/pgsty/barn/internal/platform"
)

const MiB int64 = 1024 * 1024

type Firmware struct {
	Code string `json:"code"`
	Vars string `json:"vars"`
}

type Disk struct {
	Path   string `json:"path"`
	Serial string `json:"serial"`
}

type Forward struct {
	Bind  string `json:"bind"`
	Host  uint16 `json:"host"`
	Guest uint16 `json:"guest"`
}

type PrivateNetwork struct {
	MAC          string `json:"mac"`
	StreamSocket string `json:"stream_socket,omitempty"`
	ReconnectMS  int    `json:"reconnect_ms,omitempty"`
	FD           int    `json:"fd,omitempty"`
	Bridge       string `json:"bridge,omitempty"`
	BridgeHelper string `json:"bridge_helper,omitempty"`
}

// Share is the guest-visible half of one host directory export. The host path
// is deliberately absent: callers open it as a directory descriptor and QEMU
// receives only the corresponding inherited-FD path.
type Share struct {
	Tag      string
	Readonly bool
}

type InheritedFile struct {
	FD   int    `json:"fd"`
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type Config struct {
	Profile   platform.Profile
	Binary    string
	Name      string
	UUID      string
	CPUs      int
	Memory    int64
	Firmware  *Firmware
	Root      Disk
	Data      []Disk
	Seed      string
	QMP       string
	PIDFile   string
	SerialLog string
	MgmtMAC   string
	Forwards  []Forward
	Private   *PrivateNetwork
	Shares    []Share
	Detach    bool
}

type Invocation struct {
	Binary         string          `json:"binary"`
	Args           []string        `json:"args"`
	InheritedFiles []InheritedFile `json:"inherited_files,omitempty"`
}

func (i Invocation) UsesPrivateFD3() bool {
	for _, file := range i.InheritedFiles {
		if file.Kind == "private-network" && file.FD == 3 {
			return true
		}
	}
	// Compatibility with node state written before inherited files became a
	// typed part of the invocation.
	for index := 0; index+1 < len(i.Args); index++ {
		if i.Args[index] == "-netdev" && i.Args[index+1] == "socket,id=private,fd=3" {
			return true
		}
	}
	return false
}

func (i Invocation) ShareFiles() []InheritedFile {
	result := make([]InheritedFile, 0, len(i.InheritedFiles))
	for _, file := range i.InheritedFiles {
		if file.Kind == "share" {
			result = append(result, file)
		}
	}
	return result
}

var (
	uuidPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
	shareTagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
)

func validateAbsolute(label, path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("%s must be a non-empty absolute path without NUL", label)
	}
	return nil
}

func validate(config Config) error {
	if config.Binary == "" {
		return errors.New("QEMU binary path is empty")
	}
	if !naming.ValidNodeName(config.Name) {
		return fmt.Errorf("invalid QEMU node name %q", config.Name)
	}
	if !uuidPattern.MatchString(config.UUID) {
		return fmt.Errorf("invalid QEMU UUID %q", config.UUID)
	}
	if config.CPUs < 1 || config.CPUs > 256 {
		return fmt.Errorf("CPU count %d is outside 1..256", config.CPUs)
	}
	if config.Memory < 512*MiB || config.Memory%MiB != 0 {
		return fmt.Errorf("memory %d must be at least 512 MiB and MiB-aligned", config.Memory)
	}
	if config.Profile.Machine == "" || config.Profile.Accelerator == "" || config.Profile.CPU == "" {
		return errors.New("QEMU platform profile is incomplete")
	}
	for label, path := range map[string]string{
		"root disk": config.Root.Path, "seed": config.Seed, "QMP socket": config.QMP,
		"pidfile": config.PIDFile, "serial log": config.SerialLog,
	} {
		if err := validateAbsolute(label, path); err != nil {
			return err
		}
	}
	if config.Root.Serial == "" {
		return errors.New("root disk serial is empty")
	}
	maxSocketPath := 107
	if config.Profile.OS == "darwin" {
		maxSocketPath = 103
	}
	if len(config.QMP) > maxSocketPath {
		return fmt.Errorf("QMP socket path length %d exceeds %s limit %d", len(config.QMP), config.Profile.OS, maxSocketPath)
	}
	if config.Profile.RequiresUEFI && config.Firmware == nil {
		return errors.New("platform requires UEFI firmware")
	}
	if config.Firmware != nil {
		if err := validateAbsolute("firmware code", config.Firmware.Code); err != nil {
			return err
		}
		if err := validateAbsolute("firmware vars", config.Firmware.Vars); err != nil {
			return err
		}
		if strings.Contains(config.Firmware.Code, ",") || strings.Contains(config.Firmware.Vars, ",") {
			return errors.New("firmware path containing comma is not supported by the probed pflash syntax")
		}
	}
	if _, err := net.ParseMAC(config.MgmtMAC); err != nil {
		return fmt.Errorf("invalid management MAC %q", config.MgmtMAC)
	}
	seenPaths := map[string]struct{}{config.Root.Path: {}}
	seenSerials := map[string]struct{}{config.Root.Serial: {}}
	for i, disk := range config.Data {
		if err := validateAbsolute(fmt.Sprintf("data disk %d", i), disk.Path); err != nil {
			return err
		}
		if disk.Serial == "" {
			return fmt.Errorf("data disk %d serial is empty", i)
		}
		if _, ok := seenPaths[disk.Path]; ok {
			return fmt.Errorf("duplicate disk path %q", disk.Path)
		}
		if _, ok := seenSerials[disk.Serial]; ok {
			return fmt.Errorf("duplicate disk serial %q", disk.Serial)
		}
		seenPaths[disk.Path] = struct{}{}
		seenSerials[disk.Serial] = struct{}{}
	}
	seenHost := make(map[string]struct{})
	for _, forward := range config.Forwards {
		if forward.Host == 0 || forward.Guest == 0 {
			return errors.New("forward ports must be non-zero")
		}
		address, err := netip.ParseAddr(forward.Bind)
		if err != nil {
			return fmt.Errorf("invalid forward bind address %q", forward.Bind)
		}
		if !address.Is4() {
			return fmt.Errorf("v1 forward bind address %q must be IPv4", forward.Bind)
		}
		key := net.JoinHostPort(forward.Bind, strconv.Itoa(int(forward.Host)))
		if _, ok := seenHost[key]; ok {
			return fmt.Errorf("duplicate host forward %s", key)
		}
		seenHost[key] = struct{}{}
	}
	if config.Private != nil {
		if _, err := net.ParseMAC(config.Private.MAC); err != nil {
			return fmt.Errorf("invalid private MAC %q", config.Private.MAC)
		}
		hasStream := config.Private.StreamSocket != ""
		hasFD := config.Private.FD != 0
		hasBridge := config.Private.Bridge != "" || config.Private.BridgeHelper != ""
		selected := 0
		for _, enabled := range []bool{hasStream, hasFD, hasBridge} {
			if enabled {
				selected++
			}
		}
		if selected != 1 {
			return errors.New("private network must select exactly one stream socket, inherited FD, or Linux bridge")
		}
		if hasStream {
			if err := validateAbsolute("private stream socket", config.Private.StreamSocket); err != nil {
				return err
			}
			if strings.Contains(config.Private.StreamSocket, ",") {
				return errors.New("private stream socket path containing comma is unsupported")
			}
			if len(config.Private.StreamSocket) > maxSocketPath {
				return fmt.Errorf("private stream socket path length %d exceeds %s limit %d", len(config.Private.StreamSocket), config.Profile.OS, maxSocketPath)
			}
			if config.Private.ReconnectMS < 1 || config.Private.ReconnectMS > 60000 {
				return fmt.Errorf("private reconnect-ms %d is outside 1..60000", config.Private.ReconnectMS)
			}
		} else if hasFD && config.Private.FD != 3 {
			return fmt.Errorf("private inherited FD must be 3, got %d", config.Private.FD)
		} else if hasBridge {
			if config.Private.Bridge != "barn0" {
				return fmt.Errorf("v1 private bridge must be barn0, got %q", config.Private.Bridge)
			}
			if err := validateAbsolute("qemu bridge helper", config.Private.BridgeHelper); err != nil {
				return err
			}
			if strings.Contains(config.Private.BridgeHelper, ",") {
				return errors.New("qemu bridge helper path containing comma is unsupported")
			}
		}
	}
	if len(config.Shares) > 8 {
		return errors.New("QEMU supports at most 8 host shares per node")
	}
	if len(config.Shares) != 0 && config.Profile.Machine != "q35" && config.Profile.Machine != "virt" {
		return fmt.Errorf("QEMU host shares require the non-hotpluggable pcie.0 root bus, not machine %q", config.Profile.Machine)
	}
	seenTags := make(map[string]struct{}, len(config.Shares))
	for index, share := range config.Shares {
		if !shareTagPattern.MatchString(share.Tag) {
			return fmt.Errorf("invalid QEMU share %d tag %q", index, share.Tag)
		}
		if _, duplicate := seenTags[share.Tag]; duplicate {
			return fmt.Errorf("duplicate QEMU share tag %q", share.Tag)
		}
		seenTags[share.Tag] = struct{}{}
	}
	return nil
}

type fileBlockdev struct {
	Driver   string `json:"driver"`
	Filename string `json:"filename"`
	NodeName string `json:"node-name"`
	ReadOnly bool   `json:"read-only"`
}

type formatBlockdev struct {
	Driver   string `json:"driver"`
	File     string `json:"file"`
	NodeName string `json:"node-name"`
	ReadOnly bool   `json:"read-only"`
}

func appendBlockdev(args []string, path, id, format string, readOnly bool) ([]string, error) {
	fileJSON, err := json.Marshal(fileBlockdev{Driver: "file", Filename: path, NodeName: id + "-file", ReadOnly: readOnly})
	if err != nil {
		return nil, err
	}
	formatJSON, err := json.Marshal(formatBlockdev{Driver: format, File: id + "-file", NodeName: id, ReadOnly: readOnly})
	if err != nil {
		return nil, err
	}
	return append(args, "-blockdev", string(fileJSON), "-blockdev", string(formatJSON)), nil
}

// Build constructs a complete headless user-network invocation. It has no
// arbitrary user-supplied QEMU argv field.
func Build(config Config) (Invocation, error) {
	if err := validate(config); err != nil {
		return Invocation{}, err
	}
	args := []string{
		"-name", config.Name,
		"-uuid", strings.ToLower(config.UUID),
		"-machine", config.Profile.Machine,
		"-accel", config.Profile.Accelerator,
		"-cpu", config.Profile.CPU,
		"-smp", strconv.Itoa(config.CPUs),
		"-m", strconv.FormatInt(config.Memory/MiB, 10),
		"-display", "none",
		"-nodefaults",
		"-no-user-config",
		"-qmp", "unix:" + config.QMP + ",server=on,wait=off",
		"-pidfile", config.PIDFile,
		"-serial", "file:" + config.SerialLog,
	}
	var inheritedFiles []InheritedFile
	if config.Detach {
		args = append(args, "-daemonize")
	}
	if config.Firmware != nil {
		args = append(args,
			"-drive", "if=pflash,format=raw,readonly=on,file="+config.Firmware.Code,
			"-drive", "if=pflash,format=raw,file="+config.Firmware.Vars,
		)
	}

	var err error
	args, err = appendBlockdev(args, config.Root.Path, "root", "qcow2", false)
	if err != nil {
		return Invocation{}, fmt.Errorf("encode root blockdev: %w", err)
	}
	args = append(args, "-device", "virtio-blk-pci,drive=root,serial="+config.Root.Serial+",bootindex=1")
	for i, disk := range config.Data {
		id := "data" + strconv.Itoa(i)
		args, err = appendBlockdev(args, disk.Path, id, "qcow2", false)
		if err != nil {
			return Invocation{}, fmt.Errorf("encode data blockdev %d: %w", i, err)
		}
		args = append(args, "-device", "virtio-blk-pci,drive="+id+",serial="+disk.Serial)
	}

	args = append(args, "-device", "virtio-scsi-pci,id=seed-scsi")
	args, err = appendBlockdev(args, config.Seed, "seed", "raw", true)
	if err != nil {
		return Invocation{}, fmt.Errorf("encode seed blockdev: %w", err)
	}
	args = append(args, "-device", "scsi-cd,drive=seed,bus=seed-scsi.0")

	netdev := []string{"user", "id=mgmt"}
	for _, forward := range config.Forwards {
		netdev = append(netdev, fmt.Sprintf("hostfwd=tcp:%s:%d-:%d", forward.Bind, forward.Host, forward.Guest))
	}
	args = append(args,
		"-netdev", strings.Join(netdev, ","),
		"-device", "virtio-net-pci,netdev=mgmt,mac="+strings.ToLower(config.MgmtMAC),
	)
	if config.Private != nil {
		privateNetdev := ""
		if config.Private.StreamSocket != "" {
			privateNetdev = fmt.Sprintf("stream,id=private,server=off,addr.type=unix,addr.path=%s,reconnect-ms=%d", config.Private.StreamSocket, config.Private.ReconnectMS)
		} else if config.Private.FD != 0 {
			privateNetdev = fmt.Sprintf("socket,id=private,fd=%d", config.Private.FD)
			inheritedFiles = append(inheritedFiles, InheritedFile{FD: config.Private.FD, Kind: "private-network", ID: "private"})
		} else {
			privateNetdev = fmt.Sprintf("bridge,id=private,br=%s,helper=%s", config.Private.Bridge, config.Private.BridgeHelper)
		}
		args = append(args,
			"-netdev", privateNetdev,
			"-device", "virtio-net-pci,netdev=private,mac="+strings.ToLower(config.Private.MAC),
		)
	}
	shareFD := 3
	if config.Private != nil && config.Private.FD == 3 {
		shareFD = 4
	}
	for index, share := range config.Shares {
		id := "share" + strconv.Itoa(index)
		fdPath := "/proc/self/fd/" + strconv.Itoa(shareFD)
		if config.Profile.OS == "darwin" {
			fdPath = "/dev/fd/" + strconv.Itoa(shareFD)
		}
		fsdev := fmt.Sprintf("local,id=%s,path=%s,security_model=mapped-xattr,multidevs=remap,fmode=0600,dmode=0700", id, fdPath)
		if share.Readonly {
			fsdev += ",readonly=on"
		}
		args = append(args,
			"-fsdev", fsdev,
			// Security invariant: pcie.0 on both supported machines is a
			// cold-plug-only root bus. Do not move 9p behind a hotpluggable
			// root port; guest-triggered device eject has caused 9p UAFs.
			"-device", fmt.Sprintf("virtio-9p-pci,id=%sdev,bus=pcie.0,fsdev=%s,mount_tag=%s", id, id, share.Tag),
		)
		inheritedFiles = append(inheritedFiles, InheritedFile{FD: shareFD, Kind: "share", ID: share.Tag})
		shareFD++
	}
	return Invocation{Binary: config.Binary, Args: args, InheritedFiles: inheritedFiles}, nil
}
