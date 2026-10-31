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

const (
	defaultMemorySlots = 8
	minimumMaxCPUs     = 8
	maximumMaxCPUs     = 256
)

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
	Profile platform.Profile
	Binary  string
	Name    string
	UUID    string
	CPUs    int
	Memory  int64
	// MaxCPUs, MaxMemory, and MemorySlots are optional launch-time capacity
	// reservations for later resource changes. Zero selects Barn's conservative
	// defaults, so inventories do not need to carry a second resource model.
	MaxCPUs     int
	MaxMemory   int64
	MemorySlots int
	Firmware    *Firmware
	Root        Disk
	Data        []Disk
	Seed        string
	QMP         string
	PIDFile     string
	SerialLog   string
	MgmtMAC     string
	Forwards    []Forward
	Private     *PrivateNetwork
	Shares      []Share
	Detach      bool
}

type Invocation struct {
	Binary         string          `json:"binary"`
	Args           []string        `json:"args"`
	InheritedFiles []InheritedFile `json:"inherited_files,omitempty"`
}

// Resources describes the CPU and memory values encoded in a QEMU
// invocation. MaxCPUs and MaxMemory are zero when an older invocation did not
// reserve hotplug capacity.
type Resources struct {
	CPUs        int
	MaxCPUs     int
	Memory      int64
	MaxMemory   int64
	MemorySlots int
}

func defaultMaxCPUs(cpus int) int {
	max := cpus * 4
	if max < minimumMaxCPUs {
		max = minimumMaxCPUs
	}
	if max > maximumMaxCPUs {
		max = maximumMaxCPUs
	}
	return max
}

func defaultMaxMemory(memory int64) int64 {
	max := memory * 4
	if max < 16*1024*MiB {
		max = 16 * 1024 * MiB
	}
	return max
}

func formatMemory(memory int64) string {
	return strconv.FormatInt(memory/MiB, 10) + "M"
}

func parseMemory(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("QEMU memory value is empty")
	}
	value = strings.TrimSuffix(strings.TrimSuffix(value, "B"), "b")
	// QEMU's short numeric form for -m/maxmem is in megabytes. Barn stores
	// memory in bytes, so keep the conversion compatible with invocations
	// written by versions that emitted `-m 4096`.
	unit := MiB
	if len(value) > 0 {
		switch value[len(value)-1] {
		case 'k', 'K':
			unit, value = 1024, value[:len(value)-1]
		case 'm', 'M':
			unit, value = MiB, value[:len(value)-1]
		case 'g', 'G':
			unit, value = 1024*MiB, value[:len(value)-1]
		case 't', 'T':
			unit, value = 1024*1024*MiB, value[:len(value)-1]
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		if err == nil {
			err = errors.New("must be positive")
		}
		return 0, fmt.Errorf("invalid QEMU memory value %q: %w", value, err)
	}
	if parsed > (1<<63-1)/unit {
		return 0, fmt.Errorf("QEMU memory value %q overflows int64", value)
	}
	return parsed * unit, nil
}

func optionParts(value string) []string {
	parts := strings.Split(value, ",")
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
	}
	return parts
}

func namedOption(parts []string, name string) (int, string, bool) {
	prefix := name + "="
	for index, part := range parts {
		if strings.HasPrefix(part, prefix) {
			return index, strings.TrimPrefix(part, prefix), true
		}
	}
	return -1, "", false
}

func parseSMP(value string) (Resources, []string, error) {
	parts := optionParts(value)
	if len(parts) == 0 || parts[0] == "" {
		return Resources{}, nil, errors.New("QEMU -smp value is empty")
	}
	result := Resources{}
	first := parts[0]
	if !strings.Contains(first, "=") {
		parsed, err := strconv.Atoi(first)
		if err != nil || parsed < 1 {
			return Resources{}, nil, fmt.Errorf("invalid QEMU CPU value %q", first)
		}
		result.CPUs = parsed
	} else if index, value, ok := namedOption(parts, "cpus"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return Resources{}, nil, fmt.Errorf("invalid QEMU cpus value %q", value)
		}
		_ = index
		result.CPUs = parsed
	} else {
		return Resources{}, nil, errors.New("QEMU -smp value has no cpus field")
	}
	if _, value, ok := namedOption(parts, "maxcpus"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < result.CPUs {
			return Resources{}, nil, fmt.Errorf("invalid QEMU maxcpus value %q", value)
		}
		result.MaxCPUs = parsed
	}
	return result, parts, nil
}

func parseMemoryOption(value string) (Resources, []string, error) {
	parts := optionParts(value)
	if len(parts) == 0 || parts[0] == "" {
		return Resources{}, nil, errors.New("QEMU -m value is empty")
	}
	result := Resources{}
	first := parts[0]
	first = strings.TrimPrefix(first, "size=")
	memory, err := parseMemory(first)
	if err != nil {
		return Resources{}, nil, err
	}
	result.Memory = memory
	if _, value, ok := namedOption(parts, "slots"); ok {
		result.MemorySlots, err = strconv.Atoi(value)
		if err != nil || result.MemorySlots < 1 {
			return Resources{}, nil, fmt.Errorf("invalid QEMU memory slots value %q", value)
		}
	}
	if _, value, ok := namedOption(parts, "maxmem"); ok {
		result.MaxMemory, err = parseMemory(value)
		if err != nil || result.MaxMemory < result.Memory {
			if err == nil {
				err = errors.New("must be at least current memory")
			}
			return Resources{}, nil, fmt.Errorf("invalid QEMU maxmem value %q: %w", value, err)
		}
	}
	return result, parts, nil
}

// InvocationResources extracts CPU and memory values without making any
// assumptions about the other QEMU devices in the invocation.
func InvocationResources(inv Invocation) (Resources, error) {
	var result Resources
	for index := 0; index < len(inv.Args); index++ {
		switch inv.Args[index] {
		case "-smp":
			if index+1 >= len(inv.Args) {
				return Resources{}, errors.New("QEMU invocation has -smp without a value")
			}
			parsed, _, err := parseSMP(inv.Args[index+1])
			if err != nil {
				return Resources{}, err
			}
			result.CPUs, result.MaxCPUs = parsed.CPUs, parsed.MaxCPUs
		case "-m":
			if index+1 >= len(inv.Args) {
				return Resources{}, errors.New("QEMU invocation has -m without a value")
			}
			parsed, _, err := parseMemoryOption(inv.Args[index+1])
			if err != nil {
				return Resources{}, err
			}
			result.Memory, result.MaxMemory, result.MemorySlots = parsed.Memory, parsed.MaxMemory, parsed.MemorySlots
		}
	}
	if result.CPUs < 1 || result.Memory < 512*MiB {
		return Resources{}, errors.New("QEMU invocation does not contain valid CPU and memory resources")
	}
	return result, nil
}

func setNamedOption(parts []string, name, value string) []string {
	prefix := name + "="
	for index, part := range parts {
		if strings.HasPrefix(part, prefix) {
			parts[index] = prefix + value
			return parts
		}
	}
	return append(parts, prefix+value)
}

// ConfigureResources changes only the -smp and -m resource fields. It also
// adds conservative hotplug capacity when an older invocation did not have
// any, which makes an offline resize a one-time upgrade for future changes.
func ConfigureResources(inv Invocation, cpus int, memory int64) (Invocation, error) {
	if cpus < 1 || cpus > maximumMaxCPUs {
		return Invocation{}, fmt.Errorf("CPU count %d is outside 1..%d", cpus, maximumMaxCPUs)
	}
	if memory < 512*MiB || memory%MiB != 0 {
		return Invocation{}, fmt.Errorf("memory %d must be at least 512 MiB and MiB-aligned", memory)
	}
	current, err := InvocationResources(inv)
	if err != nil {
		return Invocation{}, err
	}
	maxCPUs := current.MaxCPUs
	if maxCPUs < cpus {
		maxCPUs = defaultMaxCPUs(cpus)
	}
	if maxCPUs < current.CPUs {
		maxCPUs = current.CPUs
	}
	maxMemory := current.MaxMemory
	if maxMemory < memory {
		maxMemory = defaultMaxMemory(memory)
	}
	if maxMemory < current.Memory {
		maxMemory = current.Memory
	}
	slots := current.MemorySlots
	if slots < 1 {
		slots = defaultMemorySlots
	}
	args := append([]string(nil), inv.Args...)
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "-smp":
			if index+1 >= len(args) {
				return Invocation{}, errors.New("QEMU invocation has -smp without a value")
			}
			_, parts, err := parseSMP(args[index+1])
			if err != nil {
				return Invocation{}, err
			}
			if parts[0] != "" && !strings.Contains(parts[0], "=") {
				parts[0] = "cpus=" + strconv.Itoa(cpus)
			} else {
				parts = setNamedOption(parts, "cpus", strconv.Itoa(cpus))
			}
			parts = setNamedOption(parts, "maxcpus", strconv.Itoa(maxCPUs))
			args[index+1] = strings.Join(parts, ",")
		case "-m":
			if index+1 >= len(args) {
				return Invocation{}, errors.New("QEMU invocation has -m without a value")
			}
			_, parts, err := parseMemoryOption(args[index+1])
			if err != nil {
				return Invocation{}, err
			}
			if strings.HasPrefix(parts[0], "size=") {
				parts[0] = "size=" + formatMemory(memory)
			} else {
				parts[0] = formatMemory(memory)
			}
			parts = setNamedOption(parts, "slots", strconv.Itoa(slots))
			parts = setNamedOption(parts, "maxmem", formatMemory(maxMemory))
			args[index+1] = strings.Join(parts, ",")
		}
	}
	inv.Args = args
	return inv, nil
}

func (i Invocation) UsesPrivateFD3() bool {
	for _, file := range i.InheritedFiles {
		if file.Kind == "private-network" && file.FD == 3 {
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
	maxCPUs := config.MaxCPUs
	if maxCPUs < config.CPUs {
		maxCPUs = defaultMaxCPUs(config.CPUs)
	}
	maxMemory := config.MaxMemory
	if maxMemory < config.Memory {
		maxMemory = defaultMaxMemory(config.Memory)
	}
	memorySlots := config.MemorySlots
	if memorySlots < 1 {
		memorySlots = defaultMemorySlots
	}
	args := []string{
		"-name", config.Name,
		"-uuid", strings.ToLower(config.UUID),
		"-machine", config.Profile.Machine,
		"-accel", config.Profile.Accelerator,
		"-cpu", config.Profile.CPU,
		"-smp", fmt.Sprintf("cpus=%d,maxcpus=%d", config.CPUs, maxCPUs),
		"-m", fmt.Sprintf("%s,slots=%d,maxmem=%s", formatMemory(config.Memory), memorySlots, formatMemory(maxMemory)),
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
