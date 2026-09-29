// Package linux defines the reversible root-owned Linux private bridge plan.
// It does not execute privileged operations.
package linux

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/network/subnet"
)

const (
	BridgeName         = "barn0"
	QEMUConfigDir      = "/etc/qemu"
	BridgeConfPath     = "/etc/qemu/bridge.conf"
	NetDevPath         = "/etc/systemd/network/80-barn0.netdev"
	NetworkPath        = "/etc/systemd/network/80-barn0.network"
	NetworkManagerPath = "/etc/NetworkManager/conf.d/90-barn-unmanaged.conf"
	TmpfilesPath       = "/etc/tmpfiles.d/barn.conf"
	StatePath          = "/var/lib/barn/network.json"
	LeaseRoot          = "/run/barn"
	LeaseLockPath      = "/run/barn/private-lease.lock"
	markerBegin        = "# BEGIN BARN MANAGED: barn0"
	markerEnd          = "# END BARN MANAGED: barn0"

	// Compatibility expiry: linux-network-backend-v0 in CONTRIBUTING.md#compatibility-expiry.
	// The backend is selected by the active host-network owner: NetworkManager
	// when it is running, otherwise systemd-networkd. Legacy manifests carry no
	// backend field and mean networkd.
	BackendNetworkd       = "networkd"
	BackendNetworkManager = "networkmanager"

	// PublicStatePath is the world-readable identity of a NetworkManager-backend
	// installation. The networkd backend exposes its layout through the public
	// unit files; NM connection profiles are root-only, so this file plays the
	// same role for read-only preflight.
	PublicStateDir  = "/etc/barn"
	PublicStatePath = "/etc/barn/network.json"

	NMCLIPath = "/usr/bin/nmcli"
)

var NetworkdUnitNames = []string{
	"systemd-networkd.service",
	"systemd-networkd.socket",
	"systemd-network-generator.service",
	"systemd-networkd-wait-online.service",
}

type Family string

const (
	Debian Family = "debian"
	RPM    Family = "rpm"
)

type Override struct {
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mode  string `json:"mode"`
}

type UnitState struct {
	LoadState     string `json:"load_state"`
	UnitFileState string `json:"unit_file_state"`
	ActiveState   string `json:"active_state"`
	SubState      string `json:"sub_state"`
}

// NetworkdLink is the bounded link identity used to prove that starting an
// inactive systemd-networkd cannot claim a pre-existing non-Barn link. The
// planned barn0 identity is included even when the bridge does not yet exist
// so an earlier, unowned .network file cannot win systemd's first-match rule.
type NetworkdLink struct {
	Name             string   `json:"name"`
	AlternativeNames []string `json:"alternative_names,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	Type             string   `json:"type"`
	BarnOwned        bool     `json:"barn_owned,omitempty"`
}

// NetworkdConfiguration records an effective .network or .netdev identity and
// the supported positive match predicates from .network files. A predicate is
// retained only when Barn can parse it exactly; unsupported predicates can
// never be used as safety proof.
type NetworkdConfiguration struct {
	Path       string   `json:"path"`
	Kind       string   `json:"kind"`
	MatchNames []string `json:"match_names,omitempty"`
	MatchTypes []string `json:"match_types,omitempty"`
}

type NetworkdActivationConflict struct {
	Path   string `json:"path"`
	Link   string `json:"link,omitempty"`
	Reason string `json:"reason"`
}

// NetworkdActivationSafety is populated only by pre-mutation discovery when
// systemd-networkd is inactive. Conflicts are deliberately typed so the plan
// can fail closed without starting networkd and attempting post-hoc repair.
type NetworkdActivationSafety struct {
	Checked        bool                         `json:"checked"`
	Links          []NetworkdLink               `json:"links"`
	Configurations []NetworkdConfiguration      `json:"configurations,omitempty"`
	Conflicts      []NetworkdActivationConflict `json:"conflicts,omitempty"`
}

type PathState struct {
	Existed bool   `json:"existed"`
	Owner   string `json:"owner,omitempty"`
	Group   string `json:"group,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

type HelperFacts struct {
	Path         string
	OwnerUID     int
	Group        string
	Mode         uint32
	Regular      bool
	Symlink      bool
	ParentSafe   bool
	PackageOwned bool
	Override     *Override
}

type Facts struct {
	Family               Family
	Systemd              bool
	NetworkdActive       bool
	NetworkdUnits        map[string]UnitState
	NetworkdActivation   *NetworkdActivationSafety
	NetworkManagerActive bool
	NMConnectionExists   bool
	FirewalldActive      bool
	PublicDirExisted     bool
	BridgeExists         bool
	BridgeOwned          bool
	BridgeConf           string
	BridgeConfState      PathState
	QEMUConfigDirExisted bool
	Helper               HelperFacts
	// AccessGroup is the invoking user's preferred group for the Debian
	// qemu-bridge-helper setuid boundary. It is kvm when the user is already a
	// member, otherwise the user's primary group.
	AccessGroup      string
	ExistingManifest *Manifest
}

type Config struct {
	CIDR        string
	HostAddress string
	DHCPEnd     string
}

func ConfigForCIDR(cidr string) (Config, error) {
	layout, err := subnet.Parse(cidr)
	if err != nil {
		return Config{}, err
	}
	return Config{CIDR: layout.CIDR(), HostAddress: layout.HostAddress(), DHCPEnd: layout.DHCPEnd()}, nil
}

type File struct {
	Path    string `json:"path"`
	Owner   string `json:"owner"`
	Mode    string `json:"mode"`
	Content string `json:"content"`
}

type Directory struct {
	Path  string `json:"path"`
	Owner string `json:"owner"`
	Mode  string `json:"mode"`
}

type Command struct {
	Binary string   `json:"binary"`
	Args   []string `json:"args"`
}

type Manifest struct {
	Schema             int                  `json:"schema"`
	Family             Family               `json:"family"`
	Backend            string               `json:"backend,omitempty"`
	Bridge             string               `json:"bridge"`
	CIDR               string               `json:"cidr"`
	HostAddress        string               `json:"host_address"`
	DHCPEnd            string               `json:"dhcp_end"`
	HelperPath         string               `json:"helper_path"`
	OriginalHelper     Override             `json:"original_helper"`
	OriginalBridgeConf string               `json:"original_bridge_conf"`
	OriginalBridgePath PathState            `json:"original_bridge_path"`
	QEMUConfigCreated  bool                 `json:"qemu_config_created"`
	PublicDirCreated   bool                 `json:"public_dir_created,omitempty"`
	NetworkdUnits      map[string]UnitState `json:"networkd_units"`
	AppliedOverride    *Override            `json:"applied_override,omitempty"`
	Files              map[string]string    `json:"files"`
	NetworkManager     bool                 `json:"network_manager"`
	LeaseRoot          string               `json:"lease_root"`
}

type Plan struct {
	Manifest           Manifest                  `json:"manifest"`
	NetworkdActivation *NetworkdActivationSafety `json:"networkd_activation,omitempty"`
	Directories        []Directory               `json:"directories"`
	Files              []File                    `json:"files"`
	Commands           []Command                 `json:"commands"`
	Phases             []CommandPhase            `json:"phases"`
	Warnings           []string                  `json:"warnings,omitempty"`
}

type CommandPhase struct {
	Name     string    `json:"name"`
	Commands []Command `json:"commands"`
}

func validateConfig(config Config) error {
	layout, err := subnet.Parse(config.CIDR)
	if err != nil {
		return err
	}
	if config.HostAddress != layout.HostAddress() || config.DHCPEnd != layout.DHCPEnd() {
		return fmt.Errorf("the Barn network %s requires host %s and DHCP end %s", layout.CIDR(), layout.HostAddress(), layout.DHCPEnd())
	}
	return nil
}

func validateNetworkdUnits(units map[string]UnitState) error {
	if len(units) != len(NetworkdUnitNames) {
		return errors.New("networkd prestate must contain the four exact units")
	}
	for _, name := range NetworkdUnitNames {
		state, ok := units[name]
		if !ok || state.LoadState != "loaded" || state.SubState == "" {
			return fmt.Errorf("networkd unit %s has incomplete prestate", name)
		}
		if state.UnitFileState != "enabled" && state.UnitFileState != "disabled" {
			return fmt.Errorf("networkd unit %s has unsupported unit-file state %q", name, state.UnitFileState)
		}
		if state.ActiveState != "active" && state.ActiveState != "inactive" {
			return fmt.Errorf("networkd unit %s has unsupported active state %q", name, state.ActiveState)
		}
	}
	return nil
}

func validateNetworkdActivationSafety(safety *NetworkdActivationSafety) error {
	if safety == nil || !safety.Checked {
		return errors.New("will not start the inactive systemd-networkd: Barn cannot prove that starting it leaves existing links untouched")
	}
	if len(safety.Links) == 0 {
		return errors.New("networkd activation safety proof contains no link inventory")
	}
	plannedBridge := false
	seenLinks := make(map[string]struct{}, len(safety.Links))
	for _, link := range safety.Links {
		if link.Name == "" || link.Type == "" {
			return errors.New("networkd activation safety proof contains an incomplete link identity")
		}
		if _, exists := seenLinks[link.Name]; exists {
			return fmt.Errorf("networkd activation safety proof repeats link %s", link.Name)
		}
		seenLinks[link.Name] = struct{}{}
		if link.Name == BridgeName && link.BarnOwned {
			plannedBridge = true
		}
	}
	if !plannedBridge {
		return errors.New("networkd activation safety proof omits the planned Barn bridge identity")
	}
	for _, configuration := range safety.Configurations {
		if configuration.Path == "" || (configuration.Kind != "network" && configuration.Kind != "netdev") {
			return errors.New("networkd activation safety proof contains an invalid configuration identity")
		}
		if configuration.Kind == "netdev" {
			return fmt.Errorf("will not start the inactive systemd-networkd: %s may create or change a virtual link", configuration.Path)
		}
		for _, patterns := range [][]string{configuration.MatchNames, configuration.MatchTypes} {
			for _, pattern := range patterns {
				if !supportedMatchPattern(pattern) {
					return fmt.Errorf("networkd activation safety proof contains an unsupported match pattern in %s", configuration.Path)
				}
			}
		}
		for _, link := range safety.Links {
			if configurationCouldMatch(configuration, link) {
				return fmt.Errorf("will not start the inactive systemd-networkd: %s could affect link %s", configuration.Path, link.Name)
			}
		}
	}
	if len(safety.Conflicts) != 0 {
		conflict := safety.Conflicts[0]
		if conflict.Path == "" || conflict.Reason == "" {
			return errors.New("networkd activation safety proof contains an incomplete conflict")
		}
		if conflict.Link != "" {
			return fmt.Errorf("will not start the inactive systemd-networkd: %s could affect link %s: %s", conflict.Path, conflict.Link, conflict.Reason)
		}
		return fmt.Errorf("will not start the inactive systemd-networkd: %s: %s", conflict.Path, conflict.Reason)
	}
	return nil
}

func copyUnitStates(source map[string]UnitState) map[string]UnitState {
	result := make(map[string]UnitState, len(source))
	for name, state := range source {
		result[name] = state
	}
	return result
}

func validateBridgePathState(state PathState) error {
	if !state.Existed {
		if state.Owner != "" || state.Group != "" || state.Mode != "" {
			return errors.New("absent bridge.conf path has unexpected metadata")
		}
		return nil
	}
	if state.Owner != "root" || state.Group == "" || state.Mode == "" {
		return errors.New("existing bridge.conf must have root ownership metadata")
	}
	return nil
}

func managedBridgeBlock() string {
	return markerBegin + "\nallow " + BridgeName + "\n" + markerEnd + "\n"
}

func ReconcileBridgeConf(existing string, install bool) (string, error) {
	beginCount := strings.Count(existing, markerBegin)
	endCount := strings.Count(existing, markerEnd)
	if beginCount != endCount || beginCount > 1 {
		return "", errors.New("qemu bridge.conf has malformed or duplicate Barn markers")
	}
	block := managedBridgeBlock()
	if beginCount == 1 {
		if !strings.Contains(existing, block) {
			return "", errors.New("qemu bridge.conf Barn block was modified")
		}
		if install {
			return existing, nil
		}
		return strings.Replace(existing, block, "", 1), nil
	}
	if !install {
		return existing, nil
	}
	for _, line := range strings.Split(existing, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "allow" && fields[1] == BridgeName {
			return "", errors.New("will not take over an existing \"allow barn0\" bridge rule that Barn did not write")
		}
	}
	result := existing
	if result != "" && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	if result != "" && !strings.HasSuffix(result, "\n\n") {
		result += "\n"
	}
	return result + block, nil
}

func fileDigest(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

var accessGroupPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,63}$`)

func validateHelper(facts Facts) (*Override, []Command, []string, error) {
	helper := facts.Helper
	if (helper.Path != "/usr/lib/qemu/qemu-bridge-helper" && helper.Path != "/usr/libexec/qemu-bridge-helper") || !filepath.IsAbs(helper.Path) || helper.OwnerUID != 0 || !helper.Regular || helper.Symlink || !helper.ParentSafe || !helper.PackageOwned {
		return nil, nil, nil, errors.New("qemu-bridge-helper must be the package-owned regular non-symlink file below safe root-owned parents")
	}
	switch facts.Family {
	case Debian:
		if !accessGroupPattern.MatchString(facts.AccessGroup) {
			return nil, nil, nil, errors.New("cannot select a safe invoking-user group for qemu-bridge-helper")
		}
		desired := &Override{Owner: "root", Group: facts.AccessGroup, Mode: "4750"}
		if helper.Override != nil {
			if facts.ExistingManifest == nil || facts.ExistingManifest.AppliedOverride == nil || *helper.Override != *facts.ExistingManifest.AppliedOverride {
				return nil, nil, nil, errors.New("will not change qemu-bridge-helper permissions: a dpkg-statoverride that Barn did not write already exists")
			}
			if helper.Override.Group != facts.AccessGroup {
				return nil, nil, nil, fmt.Errorf("qemu-bridge-helper is scoped to group %s, but this user requires group %s; uninstall the Barn network as its owner before switching users", helper.Override.Group, facts.AccessGroup)
			}
			return helper.Override, nil, nil, nil
		}
		if helper.Group == facts.AccessGroup && helper.Mode == 0o4750 {
			return nil, nil, nil, nil
		}
		command := Command{Binary: "/usr/bin/dpkg-statoverride", Args: []string{"--update", "--add", "root", facts.AccessGroup, "4750", helper.Path}}
		return desired, []Command{command}, nil, nil
	case RPM:
		if helper.Mode != 0o4755 {
			return nil, nil, nil, fmt.Errorf("RPM helper mode is %04o, want distribution-owned 4755", helper.Mode)
		}
		return nil, nil, []string{"RPM qemu-bridge-helper mode 4755 permits every local user to request an allowed bridge attach"}, nil
	default:
		return nil, nil, nil, fmt.Errorf("unsupported linux family %q", facts.Family)
	}
}

// Compatibility expiry: linux-network-backend-v0 in CONTRIBUTING.md#compatibility-expiry.
// ManifestBackend normalizes the backend of a manifest; legacy manifests
// without the field are systemd-networkd installations.
func ManifestBackend(manifest Manifest) string {
	if manifest.Backend == "" {
		return BackendNetworkd
	}
	return manifest.Backend
}

// PublicNetworkState is the world-readable identity file of a
// NetworkManager-backend installation.
type PublicNetworkState struct {
	Schema      int    `json:"schema"`
	Backend     string `json:"backend"`
	Bridge      string `json:"bridge"`
	CIDR        string `json:"cidr"`
	HostAddress string `json:"host_address"`
	DHCPEnd     string `json:"dhcp_end"`
}

func RenderPublicState(config Config) (string, error) {
	state := PublicNetworkState{Schema: 1, Backend: BackendNetworkManager, Bridge: BridgeName, CIDR: config.CIDR, HostAddress: config.HostAddress, DHCPEnd: config.DHCPEnd}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return "", err
	}
	return string(append(data, '\n')), nil
}

func ParsePublicState(data []byte) (PublicNetworkState, error) {
	if len(data) == 0 || len(data) > 1<<20 {
		return PublicNetworkState{}, errors.New("linux public network state size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state PublicNetworkState
	if err := decoder.Decode(&state); err != nil {
		return PublicNetworkState{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return PublicNetworkState{}, errors.New("linux public network state has trailing JSON data")
	}
	if state.Schema != 1 || state.Backend != BackendNetworkManager || state.Bridge != BridgeName {
		return PublicNetworkState{}, errors.New("linux public network state identity is invalid")
	}
	if err := validateConfig(Config{CIDR: state.CIDR, HostAddress: state.HostAddress, DHCPEnd: state.DHCPEnd}); err != nil {
		return PublicNetworkState{}, err
	}
	return state, nil
}

// NMConnectionAddArgs is the exact bridge-connection creation argv. The
// firewalld zone is assigned at creation time so guest traffic to the host
// .1 is not filtered by the default zone on firewalld hosts.
func NMConnectionAddArgs(config Config, firewalld bool) []string {
	args := []string{
		"connection", "add", "type", "bridge",
		"con-name", BridgeName, "ifname", BridgeName,
		"ipv4.method", "manual", "ipv4.addresses", config.HostAddress + "/24",
		"ipv6.method", "disabled", "bridge.stp", "no",
		"connection.autoconnect", "yes",
	}
	if firewalld {
		args = append(args, "connection.zone", "trusted")
	}
	return args
}

func newNetworkManagerInstallPlan(facts Facts, config Config) (Plan, error) {
	if err := validateBridgePathState(facts.BridgeConfState); err != nil {
		return Plan{}, err
	}
	if facts.BridgeExists && !facts.BridgeOwned {
		return Plan{}, errors.New("will not take over an existing barn0 bridge that Barn did not create")
	}
	if facts.NMConnectionExists && facts.ExistingManifest == nil {
		return Plan{}, errors.New("will not take over an existing barn0 NetworkManager connection that Barn did not create")
	}
	bridgeConf, err := ReconcileBridgeConf(facts.BridgeConf, true)
	if err != nil {
		return Plan{}, err
	}
	appliedOverride, helperCommands, warnings, err := validateHelper(facts)
	if err != nil {
		return Plan{}, err
	}
	publicState, err := RenderPublicState(config)
	if err != nil {
		return Plan{}, err
	}
	files := []File{
		{Path: BridgeConfPath, Owner: "root:root", Mode: "0644", Content: bridgeConf},
		{Path: PublicStatePath, Owner: "root:root", Mode: "0644", Content: publicState},
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	originalHelper := Override{Owner: "root", Group: facts.Helper.Group, Mode: fmt.Sprintf("%04o", facts.Helper.Mode)}
	originalBridgeConf := facts.BridgeConf
	originalBridgePath := facts.BridgeConfState
	qemuConfigCreated := !facts.QEMUConfigDirExisted
	publicDirCreated := !facts.PublicDirExisted
	if facts.ExistingManifest != nil {
		if err := validateManifest(*facts.ExistingManifest); err != nil {
			return Plan{}, fmt.Errorf("existing Linux network manifest: %w", err)
		}
		existing := facts.ExistingManifest
		if ManifestBackend(*existing) != BackendNetworkManager {
			return Plan{}, failure.WithNext(errors.New("the Barn network was installed with systemd-networkd; remove it before switching backends"), "barn network uninstall --yes")
		}
		if existing.Family != facts.Family || existing.CIDR != config.CIDR || existing.HostAddress != config.HostAddress || existing.DHCPEnd != config.DHCPEnd || existing.HelperPath != facts.Helper.Path {
			return Plan{}, errors.New("existing Linux network manifest does not match requested install")
		}
		originalHelper = existing.OriginalHelper
		originalBridgeConf = existing.OriginalBridgeConf
		originalBridgePath = existing.OriginalBridgePath
		qemuConfigCreated = existing.QEMUConfigCreated
		publicDirCreated = existing.PublicDirCreated
		appliedOverride = existing.AppliedOverride
	}
	manifest := Manifest{
		Schema: 1, Family: facts.Family, Backend: BackendNetworkManager, Bridge: BridgeName, CIDR: config.CIDR,
		HostAddress: config.HostAddress, DHCPEnd: config.DHCPEnd, HelperPath: facts.Helper.Path,
		OriginalHelper: originalHelper, OriginalBridgeConf: originalBridgeConf, OriginalBridgePath: originalBridgePath,
		QEMUConfigCreated: qemuConfigCreated, PublicDirCreated: publicDirCreated, NetworkdUnits: map[string]UnitState{},
		AppliedOverride: appliedOverride, Files: make(map[string]string), NetworkManager: true,
	}
	for _, file := range files {
		manifest.Files[file.Path] = fileDigest(file.Content)
	}
	stateBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Plan{}, err
	}
	files = append(files, File{Path: StatePath, Owner: "root:root", Mode: "0600", Content: string(append(stateBytes, '\n'))})
	create := make([]Command, 0, 2)
	if !facts.NMConnectionExists {
		create = append(create, Command{Binary: NMCLIPath, Args: NMConnectionAddArgs(config, facts.FirewalldActive)})
	}
	create = append(create, Command{Binary: NMCLIPath, Args: []string{"connection", "up", BridgeName}})
	phases := []CommandPhase{{Name: "create-bridge-connection", Commands: create}}
	privilege := append([]Command(nil), helperCommands...)
	phases = append(phases, CommandPhase{Name: "helper-and-runtime-boundary", Commands: privilege})
	commands := make([]Command, 0)
	for _, phase := range phases {
		commands = append(commands, phase.Commands...)
	}
	directories := []Directory{{Path: "/var/lib/barn", Owner: "root:root", Mode: "0700"}}
	if !facts.PublicDirExisted {
		directories = append([]Directory{{Path: PublicStateDir, Owner: "root:root", Mode: "0755"}}, directories...)
	}
	if !facts.QEMUConfigDirExisted {
		directories = append([]Directory{{Path: QEMUConfigDir, Owner: "root:root", Mode: "0755"}}, directories...)
	}
	return Plan{Manifest: manifest, Directories: directories, Files: files, Commands: commands, Phases: phases, Warnings: warnings}, nil
}

func NewInstallPlan(facts Facts, config Config) (Plan, error) {
	if err := validateConfig(config); err != nil {
		return Plan{}, err
	}
	if !facts.Systemd {
		return Plan{}, errors.New("the Barn network on Linux requires systemd")
	}
	if facts.NetworkManagerActive {
		return newNetworkManagerInstallPlan(facts, config)
	}
	if err := validateNetworkdUnits(facts.NetworkdUnits); err != nil {
		return Plan{}, err
	}
	if err := validateBridgePathState(facts.BridgeConfState); err != nil {
		return Plan{}, err
	}
	if facts.BridgeExists && !facts.BridgeOwned {
		return Plan{}, errors.New("will not take over an existing barn0 bridge that Barn did not create")
	}
	serviceState := facts.NetworkdUnits["systemd-networkd.service"]
	if serviceState.ActiveState != "active" {
		if err := validateNetworkdActivationSafety(facts.NetworkdActivation); err != nil {
			return Plan{}, err
		}
	}
	bridgeConf, err := ReconcileBridgeConf(facts.BridgeConf, true)
	if err != nil {
		return Plan{}, err
	}
	appliedOverride, helperCommands, warnings, err := validateHelper(facts)
	if err != nil {
		return Plan{}, err
	}
	netdev := "[NetDev]\nName=barn0\nKind=bridge\n"
	network := fmt.Sprintf("[Match]\nName=barn0\n\n[Network]\nAddress=%s/%s\nConfigureWithoutCarrier=yes\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n\n[Link]\nRequiredForOnline=no\n", config.HostAddress, strings.Split(config.CIDR, "/")[1])
	files := []File{
		{Path: BridgeConfPath, Owner: "root:root", Mode: "0644", Content: bridgeConf},
		{Path: NetDevPath, Owner: "root:root", Mode: "0644", Content: netdev},
		{Path: NetworkPath, Owner: "root:root", Mode: "0644", Content: network},
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	originalHelper := Override{Owner: "root", Group: facts.Helper.Group, Mode: fmt.Sprintf("%04o", facts.Helper.Mode)}
	originalBridgeConf := facts.BridgeConf
	originalBridgePath := facts.BridgeConfState
	qemuConfigCreated := !facts.QEMUConfigDirExisted
	originalUnits := copyUnitStates(facts.NetworkdUnits)
	if facts.ExistingManifest != nil {
		if err := validateManifest(*facts.ExistingManifest); err != nil {
			return Plan{}, fmt.Errorf("existing Linux network manifest: %w", err)
		}
		existing := facts.ExistingManifest
		if ManifestBackend(*existing) != BackendNetworkd {
			return Plan{}, failure.WithNext(errors.New("the Barn network was installed with NetworkManager; remove it before switching backends"), "barn network uninstall --yes")
		}
		if existing.Family != facts.Family || existing.CIDR != config.CIDR || existing.HostAddress != config.HostAddress || existing.DHCPEnd != config.DHCPEnd || existing.HelperPath != facts.Helper.Path || existing.NetworkManager {
			return Plan{}, errors.New("existing Linux network manifest does not match requested install")
		}
		originalHelper = existing.OriginalHelper
		originalBridgeConf = existing.OriginalBridgeConf
		originalBridgePath = existing.OriginalBridgePath
		qemuConfigCreated = existing.QEMUConfigCreated
		originalUnits = copyUnitStates(existing.NetworkdUnits)
		appliedOverride = existing.AppliedOverride
	}
	manifest := Manifest{
		Schema: 1, Family: facts.Family, Bridge: BridgeName, CIDR: config.CIDR,
		HostAddress: config.HostAddress, DHCPEnd: config.DHCPEnd, HelperPath: facts.Helper.Path,
		OriginalHelper: originalHelper, OriginalBridgeConf: originalBridgeConf, OriginalBridgePath: originalBridgePath,
		QEMUConfigCreated: qemuConfigCreated, NetworkdUnits: originalUnits,
		AppliedOverride: appliedOverride, Files: make(map[string]string), NetworkManager: false,
	}
	for _, file := range files {
		manifest.Files[file.Path] = fileDigest(file.Content)
	}
	stateBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Plan{}, err
	}
	stateContent := string(append(stateBytes, '\n'))
	files = append(files, File{Path: StatePath, Owner: "root:root", Mode: "0600", Content: stateContent})
	phases := make([]CommandPhase, 0, 4)
	activate := make([]Command, 0, 3)
	if serviceState.ActiveState != "active" {
		activate = append(activate, Command{Binary: "/usr/bin/systemctl", Args: []string{"start", "systemd-networkd.service"}})
	}
	activate = append(activate, Command{Binary: "/usr/bin/networkctl", Args: []string{"reload"}}, Command{Binary: "/usr/bin/networkctl", Args: []string{"reconfigure", BridgeName}})
	phases = append(phases, CommandPhase{Name: "activate-bridge", Commands: activate})
	privilege := append([]Command(nil), helperCommands...)
	phases = append(phases, CommandPhase{Name: "helper-and-runtime-boundary", Commands: privilege})
	if serviceState.UnitFileState != "enabled" {
		phases = append(phases, CommandPhase{Name: "persist-after-attach-verification", Commands: []Command{{Binary: "/usr/bin/systemctl", Args: []string{"enable", "systemd-networkd.service"}}}})
	}
	commands := make([]Command, 0)
	for _, phase := range phases {
		commands = append(commands, phase.Commands...)
	}
	directories := []Directory{{Path: "/var/lib/barn", Owner: "root:root", Mode: "0700"}}
	if !facts.QEMUConfigDirExisted {
		directories = append([]Directory{{Path: QEMUConfigDir, Owner: "root:root", Mode: "0755"}}, directories...)
	}
	return Plan{Manifest: manifest, NetworkdActivation: facts.NetworkdActivation, Directories: directories, Files: files, Commands: commands, Phases: phases, Warnings: warnings}, nil
}

func validateManifest(manifest Manifest) error {
	backend := ManifestBackend(manifest)
	if manifest.Schema != 1 || manifest.Bridge != BridgeName || (manifest.LeaseRoot != "" && manifest.LeaseRoot != LeaseRoot) || (manifest.Family != Debian && manifest.Family != RPM) || (manifest.HelperPath != "/usr/lib/qemu/qemu-bridge-helper" && manifest.HelperPath != "/usr/libexec/qemu-bridge-helper") {
		return errors.New("linux network ownership manifest identity is invalid")
	}
	if backend != BackendNetworkd && backend != BackendNetworkManager {
		return errors.New("linux network manifest backend is invalid")
	}
	if err := validateConfig(Config{CIDR: manifest.CIDR, HostAddress: manifest.HostAddress, DHCPEnd: manifest.DHCPEnd}); err != nil {
		return err
	}
	if manifest.OriginalHelper.Owner != "root" || manifest.OriginalHelper.Group == "" || len(manifest.OriginalHelper.Mode) != 4 {
		return errors.New("linux network manifest lacks original helper state")
	}
	if err := validateBridgePathState(manifest.OriginalBridgePath); err != nil {
		return err
	}
	var allowed map[string]struct{}
	var required []string
	if backend == BackendNetworkManager {
		if !manifest.NetworkManager {
			return errors.New("NetworkManager-backend manifest must record NetworkManager ownership")
		}
		if len(manifest.NetworkdUnits) != 0 {
			return errors.New("NetworkManager-backend manifest must not carry networkd prestate")
		}
		// TmpfilesPath and LeaseLockPath survive only in pre-simplification
		// manifests; new installs no longer write them.
		allowed = map[string]struct{}{BridgeConfPath: {}, TmpfilesPath: {}, LeaseLockPath: {}, PublicStatePath: {}}
		required = []string{BridgeConfPath, PublicStatePath}
	} else {
		if err := validateNetworkdUnits(manifest.NetworkdUnits); err != nil {
			return err
		}
		allowed = map[string]struct{}{BridgeConfPath: {}, NetDevPath: {}, NetworkPath: {}, TmpfilesPath: {}, LeaseLockPath: {}}
		if manifest.NetworkManager {
			allowed[NetworkManagerPath] = struct{}{}
		}
		required = []string{BridgeConfPath, NetDevPath, NetworkPath}
	}
	for _, pathname := range required {
		if _, ok := manifest.Files[pathname]; !ok {
			return errors.New("linux network manifest lacks a required managed file")
		}
	}
	for pathname, digest := range manifest.Files {
		if _, ok := allowed[pathname]; !ok || len(digest) != 64 {
			return errors.New("linux network manifest file path/digest is invalid")
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return err
		}
	}
	return nil
}

func StrictManifest(data []byte) (Manifest, error) {
	if len(data) == 0 || len(data) > 1<<20 {
		return Manifest{}, errors.New("linux network manifest size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("linux network manifest has trailing JSON data")
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

type UninstallFacts struct {
	BridgeMembers   []string
	CurrentFiles    map[string]string
	CurrentHelper   Override
	CurrentOverride *Override
}

type UninstallPlan struct {
	RestoreFiles      []File         `json:"restore_files"`
	RemoveFiles       []string       `json:"remove_files"`
	RemoveDirectories []string       `json:"remove_directories"`
	Commands          []Command      `json:"commands"`
	Phases            []CommandPhase `json:"phases"`
}

func restoreNetworkdCommands(units map[string]UnitState) []Command {
	commands := []Command{{Binary: "/usr/bin/systemctl", Args: []string{"disable", "systemd-networkd.service"}}}
	if units["systemd-networkd.service"].UnitFileState == "enabled" {
		commands = append(commands, Command{Binary: "/usr/bin/systemctl", Args: []string{"enable", "systemd-networkd.service"}})
	}
	for _, name := range NetworkdUnitNames[1:] {
		verb := "disable"
		if units[name].UnitFileState == "enabled" {
			verb = "enable"
		}
		commands = append(commands, Command{Binary: "/usr/bin/systemctl", Args: []string{verb, name}})
	}
	for _, name := range NetworkdUnitNames {
		verb := "stop"
		if units[name].ActiveState == "active" {
			verb = "start"
		}
		commands = append(commands, Command{Binary: "/usr/bin/systemctl", Args: []string{verb, name}})
	}
	return commands
}

func NewUninstallPlan(manifest Manifest, facts UninstallFacts) (UninstallPlan, error) {
	if err := validateManifest(manifest); err != nil {
		return UninstallPlan{}, err
	}
	if len(facts.BridgeMembers) > 0 {
		return UninstallPlan{}, failure.New(failure.Conflict, fmt.Errorf("will not remove the Barn network while barn0 still has members: %s", strings.Join(facts.BridgeMembers, ", "))).Then("stop the VMs that use it (barn stop for Barn nodes), then retry")
	}
	for pathname, expectedDigest := range manifest.Files {
		content, ok := facts.CurrentFiles[pathname]
		if !ok || fileDigest(content) != expectedDigest {
			return UninstallPlan{}, fmt.Errorf("owned Linux network file changed or missing: %s", pathname)
		}
	}
	if ManifestBackend(manifest) == BackendNetworkManager {
		return newNetworkManagerUninstallPlan(manifest, facts)
	}
	phases := []CommandPhase{{Name: "delete-bridge-before-owned-files", Commands: []Command{{Binary: "/usr/bin/networkctl", Args: []string{"delete", BridgeName}}}}}
	helperCommands := make([]Command, 0, 3)
	if manifest.AppliedOverride != nil {
		if facts.CurrentOverride == nil || *facts.CurrentOverride != *manifest.AppliedOverride || facts.CurrentHelper != *manifest.AppliedOverride {
			return UninstallPlan{}, errors.New("qemu-bridge-helper override/current state no longer matches Barn manifest")
		}
		helperCommands = append(helperCommands,
			Command{Binary: "/usr/bin/dpkg-statoverride", Args: []string{"--remove", manifest.HelperPath}},
			Command{Binary: "/bin/chown", Args: []string{manifest.OriginalHelper.Owner + ":" + manifest.OriginalHelper.Group, manifest.HelperPath}},
			Command{Binary: "/bin/chmod", Args: []string{manifest.OriginalHelper.Mode, manifest.HelperPath}},
		)
	}
	if len(helperCommands) > 0 {
		phases = append(phases, CommandPhase{Name: "restore-helper-after-vm-detach", Commands: helperCommands})
	}
	phases = append(phases, CommandPhase{Name: "reload-after-network-file-removal", Commands: []Command{{Binary: "/usr/bin/networkctl", Args: []string{"reload"}}}})
	if manifest.NetworkManager {
		phases = append(phases, CommandPhase{Name: "reload-network-manager-after-dropin-removal", Commands: []Command{{Binary: "/usr/bin/nmcli", Args: []string{"general", "reload"}}}})
	}
	phases = append(phases, CommandPhase{Name: "restore-networkd-prestate", Commands: restoreNetworkdCommands(manifest.NetworkdUnits)})
	removeFiles := make([]string, 0, len(manifest.Files)+1)
	for _, pathname := range []string{NetDevPath, NetworkPath, NetworkManagerPath, TmpfilesPath, LeaseLockPath} {
		if _, owned := manifest.Files[pathname]; owned {
			removeFiles = append(removeFiles, pathname)
		}
	}
	restoreFiles := make([]File, 0, 1)
	if manifest.OriginalBridgePath.Existed {
		restoreFiles = append(restoreFiles, File{Path: BridgeConfPath, Owner: manifest.OriginalBridgePath.Owner + ":" + manifest.OriginalBridgePath.Group, Mode: manifest.OriginalBridgePath.Mode, Content: manifest.OriginalBridgeConf})
	} else {
		removeFiles = append(removeFiles, BridgeConfPath)
	}
	removeFiles = append(removeFiles, StatePath)
	removeDirectories := []string{"/var/lib/barn"}
	if manifest.LeaseRoot != "" {
		removeDirectories = append([]string{manifest.LeaseRoot}, removeDirectories...)
	}
	if manifest.QEMUConfigCreated {
		removeDirectories = append(removeDirectories, QEMUConfigDir)
	}
	commands := make([]Command, 0)
	for _, phase := range phases {
		commands = append(commands, phase.Commands...)
	}
	return UninstallPlan{
		RestoreFiles: restoreFiles, RemoveFiles: removeFiles, RemoveDirectories: removeDirectories, Commands: commands, Phases: phases,
	}, nil
}

func helperRestoreCommands(manifest Manifest, facts UninstallFacts) ([]Command, error) {
	if manifest.AppliedOverride == nil {
		return nil, nil
	}
	if facts.CurrentOverride == nil || *facts.CurrentOverride != *manifest.AppliedOverride || facts.CurrentHelper != *manifest.AppliedOverride {
		return nil, errors.New("qemu-bridge-helper override/current state no longer matches Barn manifest")
	}
	return []Command{
		{Binary: "/usr/bin/dpkg-statoverride", Args: []string{"--remove", manifest.HelperPath}},
		{Binary: "/bin/chown", Args: []string{manifest.OriginalHelper.Owner + ":" + manifest.OriginalHelper.Group, manifest.HelperPath}},
		{Binary: "/bin/chmod", Args: []string{manifest.OriginalHelper.Mode, manifest.HelperPath}},
	}, nil
}

func newNetworkManagerUninstallPlan(manifest Manifest, facts UninstallFacts) (UninstallPlan, error) {
	phases := []CommandPhase{{Name: "delete-bridge-connection", Commands: []Command{{Binary: NMCLIPath, Args: []string{"connection", "delete", BridgeName}}}}}
	helperCommands, err := helperRestoreCommands(manifest, facts)
	if err != nil {
		return UninstallPlan{}, err
	}
	if len(helperCommands) > 0 {
		phases = append(phases, CommandPhase{Name: "restore-helper-after-vm-detach", Commands: helperCommands})
	}
	removeFiles := make([]string, 0, len(manifest.Files)+1)
	for _, pathname := range []string{TmpfilesPath, LeaseLockPath, PublicStatePath} {
		if _, owned := manifest.Files[pathname]; owned {
			removeFiles = append(removeFiles, pathname)
		}
	}
	restoreFiles := make([]File, 0, 1)
	if manifest.OriginalBridgePath.Existed {
		restoreFiles = append(restoreFiles, File{Path: BridgeConfPath, Owner: manifest.OriginalBridgePath.Owner + ":" + manifest.OriginalBridgePath.Group, Mode: manifest.OriginalBridgePath.Mode, Content: manifest.OriginalBridgeConf})
	} else {
		removeFiles = append(removeFiles, BridgeConfPath)
	}
	removeFiles = append(removeFiles, StatePath)
	removeDirectories := []string{"/var/lib/barn"}
	if manifest.LeaseRoot != "" {
		removeDirectories = append([]string{manifest.LeaseRoot}, removeDirectories...)
	}
	if manifest.PublicDirCreated {
		removeDirectories = append(removeDirectories, PublicStateDir)
	}
	if manifest.QEMUConfigCreated {
		removeDirectories = append(removeDirectories, QEMUConfigDir)
	}
	commands := make([]Command, 0)
	for _, phase := range phases {
		commands = append(commands, phase.Commands...)
	}
	return UninstallPlan{RestoreFiles: restoreFiles, RemoveFiles: removeFiles, RemoveDirectories: removeDirectories, Commands: commands, Phases: phases}, nil
}
