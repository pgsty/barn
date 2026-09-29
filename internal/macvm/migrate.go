package macvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/lock"
)

// The earlier layout (schema 1) had two fixed slots on one shared subnet,
// GUI passwords in the login Keychain behind a retained signed runner, and a
// root LaunchDaemon that owned the network. Migration keeps every disk, key,
// account and base, converts the records in place with backups, and then
// removes the daemon and the Keychain items.

type legacyConfig struct {
	SchemaVersion  int    `json:"schema_version"`
	InstallationID string `json:"installation_id"`
	Network        struct {
		Subnet  string `json:"subnet"`
		Gateway string `json:"gateway"`
	} `json:"network"`
	Slots []struct {
		Name string `json:"name"`
		IP   string `json:"ip"`
		MAC  string `json:"mac"`
	} `json:"slots"`
	DefaultBaseID string    `json:"default_base_id"`
	CreatedAt     time.Time `json:"created_at"`
}

type legacySlot struct {
	SchemaVersion   int       `json:"schema_version"`
	Name            string    `json:"name"`
	InstanceID      string    `json:"instance_id"`
	BaseID          string    `json:"base_id"`
	User            string    `json:"user"`
	Initialized     bool      `json:"initialized"`
	CPU             int       `json:"cpu"`
	MemoryBytes     int64     `json:"memory_bytes"`
	DiskBytes       int64     `json:"disk_bytes"`
	Version         string    `json:"version"`
	Build           string    `json:"build"`
	ObservedVersion string    `json:"observed_version"`
	ObservedBuild   string    `json:"observed_build"`
	CreatedAt       time.Time `json:"created_at"`
}

// MigrationMachine is one machine in a migration plan or report.
type MigrationMachine struct {
	Name       string `json:"name"`
	User       string `json:"user"`
	OldAddress string `json:"old_address"`
	Address    string `json:"address"`
	Subnet     string `json:"subnet"`
	// Password is "moved" from the Keychain, "kept" when already a file, or
	// "unavailable" when the Keychain could not provide it.
	Password string `json:"password"`
}

// MigrationPlan is shown before anything changes.
type MigrationPlan struct {
	Root          string             `json:"root"`
	Machines      []MigrationMachine `json:"machines"`
	RemoveHelper  bool               `json:"remove_helper"`
	HelperLabel   string             `json:"helper_label,omitempty"`
	KeychainItems int                `json:"keychain_items"`
}

// MigrationReport is the result. AlreadyCurrent means nothing was left to do.
type MigrationReport struct {
	MigrationPlan
	AlreadyCurrent bool     `json:"already_current"`
	HelperRemoved  bool     `json:"helper_removed"`
	KeychainErased int      `json:"keychain_erased"`
	Backups        []string `json:"backups"`
	Warnings       []string `json:"warnings"`
}

// MigrateOptions carry the confirmation hook. Nil Confirm approves.
type MigrateOptions struct {
	Confirm func(MigrationPlan) error
}

func legacyService(root string) string {
	sum := sha256.Sum256([]byte(root))
	return fmt.Sprintf("farrow.mac.%x", sum[:12])
}

func legacyLabel(uid int, installation string) string {
	return fmt.Sprintf("io.pgsty.farrow.mac-network.%d.%s", uid, strings.ToLower(installation))
}

// legacyHelperLabels lists this user's legacy network daemons by their
// LaunchDaemon files. Reading /Library/LaunchDaemons needs no privilege.
func legacyHelperLabels() []string {
	entries, err := os.ReadDir("/Library/LaunchDaemons")
	if err != nil {
		return nil
	}
	prefix := fmt.Sprintf("io.pgsty.farrow.mac-network.%d.", os.Getuid())
	var labels []string
	for _, entry := range entries {
		if name := entry.Name(); strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".plist") {
			labels = append(labels, strings.TrimSuffix(name, ".plist"))
		}
	}
	return labels
}

// legacyNetwork reports a route that the legacy daemon's bridge still holds.
func legacyNetwork(route DarwinRoute, _ netip.Prefix) bool {
	return strings.HasPrefix(route.Interface, "bridge") && len(legacyHelperLabels()) > 0
}

func (m *Manager) installationID() (string, error) {
	path, err := m.Store.Path("config.json")
	if err != nil {
		return "", err
	}
	var header struct {
		InstallationID string `json:"installation_id"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &header); err != nil || !validUUID(header.InstallationID) {
		return "", fmt.Errorf("mac config has no valid installation identity: %s", path)
	}
	return strings.ToLower(header.InstallationID), nil
}

// legacyHelperInstalled reports this installation's legacy daemon.
func (m *Manager) legacyHelperInstalled(ctx context.Context) (bool, error) {
	id, err := m.installationID()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	label := legacyLabel(os.Getuid(), id)
	for _, path := range []string{"/Library/LaunchDaemons/" + label + ".plist", filepath.Join("/Library/Application Support/Farrow/mac-network", strconv.Itoa(os.Getuid()), id)} {
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		}
	}
	err = exec.CommandContext(ctx, "/bin/launchctl", "print", "system/"+label).Run()
	var exit *exec.ExitError
	if err == nil {
		return true, nil
	}
	if errors.As(err, &exit) && exit.ExitCode() == 113 {
		return false, nil
	}
	return false, fmt.Errorf("inspect the legacy network service: %w", err)
}

// removeLegacyHelperScript runs as root. It removes exactly one legacy
// installation, and the shared executable only when none remains. Its
// arguments are a numeric UID and a validated lowercase UUID.
const removeLegacyHelperScript = `set -eu
uid=$1
id=$2
case $uid in ''|*[!0-9]*) echo "invalid uid" >&2; exit 64;; esac
case $id in ''|*[!0-9a-f-]*) echo "invalid installation" >&2; exit 64;; esac
[ ${#id} -eq 36 ] || { echo "invalid installation" >&2; exit 64; }
label="io.pgsty.farrow.mac-network.$uid.$id"
support="/Library/Application Support/Farrow/mac-network"
if /bin/launchctl print "system/$label" >/dev/null 2>&1; then
  /bin/launchctl bootout "system/$label" 2>/dev/null || true
  n=0
  while /bin/launchctl print "system/$label" >/dev/null 2>&1; do
    n=$((n+1))
    [ $n -le 150 ] || { echo "the legacy network service did not stop" >&2; exit 75; }
    /bin/sleep 0.1
  done
fi
/bin/rm -f "/Library/LaunchDaemons/$label.plist" "/var/run/farrow-mac/$uid-$id.sock" "/var/run/farrow-mac/$uid-$id.sock.lock" "/var/run/farrow-mac/$uid-$id.sock.admin.lock" "/var/log/farrow-mac/$uid-$id.log"
/bin/rm -rf "$support/$uid/$id"
/bin/rmdir "$support/$uid" 2>/dev/null || true
if [ -d "$support" ] && [ -z "$(/bin/ls -A "$support")" ] && ! /bin/ls /Library/LaunchDaemons/io.pgsty.farrow.mac-network.* >/dev/null 2>&1; then
  /bin/rmdir "$support"
  /bin/rmdir "/Library/Application Support/Farrow" 2>/dev/null || true
  /bin/rm -f /Library/PrivilegedHelperTools/io.pgsty.farrow.mac-network
  /bin/rm -rf /var/run/farrow-mac /var/log/farrow-mac
fi
`

func (m *Manager) removeLegacyHelper(ctx context.Context, id string) error {
	if m.Sudo != nil {
		if err := m.Sudo(ctx, "remove the legacy Farrow Mac network daemon"); err != nil {
			return err
		}
	}
	output, err := exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "--", "/bin/sh", "-c", removeLegacyHelperScript, "farrow-mac-migrate", strconv.Itoa(os.Getuid()), id).CombinedOutput()
	if err != nil {
		return failure.New(failure.Capability, fmt.Errorf("remove the legacy network daemon: %w: %s", err, strings.TrimSpace(string(output)))).
			Because("sudo_unavailable").Then("farrow mac migrate")
	}
	if installed, err := m.legacyHelperInstalled(ctx); err != nil || installed {
		return errors.Join(err, errors.New("the legacy network daemon is still registered after removal"))
	}
	return nil
}

// legacyOwner returns the retained signed runner that owns an instance's
// Keychain item, or an error when it cannot be used.
func (m *Manager) legacyOwner(ctx context.Context, instance string) (Runner, error) {
	path, err := m.Store.Path("credentials", instance+".json")
	if err != nil {
		return Runner{}, err
	}
	var record struct {
		Hash string `json:"sha256"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Runner{}, fmt.Errorf("no record of the Keychain owner: %w", err)
	}
	if err := json.Unmarshal(data, &record); err != nil || !validSHA256(record.Hash) {
		return Runner{}, errors.New("the Keychain owner record is invalid")
	}
	app, err := m.Store.Path("credentials", "owners", record.Hash, "Farrow Mac.app")
	if err != nil {
		return Runner{}, err
	}
	binary := filepath.Join(app, "Contents", "MacOS", "farrow-mac-runner")
	content, err := os.ReadFile(binary)
	if err != nil {
		return Runner{}, fmt.Errorf("the retained Keychain owner is missing: %w", err)
	}
	if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != record.Hash {
		return Runner{}, errors.New("the retained Keychain owner changed")
	}
	if err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", app).Run(); err != nil {
		return Runner{}, fmt.Errorf("the retained Keychain owner signature did not verify: %w", err)
	}
	return Runner{Binary: binary}, nil
}

func (m *Manager) legacyPassword(ctx context.Context, instance string) (string, error) {
	owner, err := m.legacyOwner(ctx, instance)
	if err != nil {
		return "", err
	}
	var result struct {
		Password string `json:"password"`
	}
	if err := owner.Call(ctx, nil, &result, "secret", "get", "--service", legacyService(m.Store.Root), "--account", instance); err != nil {
		return "", err
	}
	if result.Password == "" || strings.ContainsAny(result.Password, "\r\n\x00") {
		return "", errors.New("the Keychain returned an invalid password")
	}
	return result.Password, nil
}

func (m *Manager) eraseLegacyPassword(ctx context.Context, instance string) error {
	owner, err := m.legacyOwner(ctx, instance)
	if err != nil {
		return err
	}
	return owner.Call(ctx, nil, nil, "secret", "delete", "--service", legacyService(m.Store.Root), "--account", instance)
}

type migrationItem struct {
	slot     legacySlot
	mac      string
	oldIP    string
	network  MachineNetwork
	password string
	status   string
	current  *Machine
}

// Migrate converts the earlier layout. It is safe to repeat: finished steps
// are skipped, and the daemon and Keychain cleanup resume where they stopped.
func (m *Manager) Migrate(ctx context.Context, options MigrateOptions) (report MigrationReport, retErr error) {
	report.Root, report.Machines, report.Backups, report.Warnings = m.Store.Root, []MigrationMachine{}, []string{}, []string{}
	if err := HostSupported(); err != nil {
		return report, err
	}
	if os.Geteuid() == 0 {
		return report, failure.New(failure.Usage, errors.New("run farrow mac migrate as your normal macOS login user, not as root"))
	}
	configPath, err := m.Store.Path("config.json")
	if err != nil {
		return report, err
	}
	schema, err := schemaOf(configPath)
	if errors.Is(err, os.ErrNotExist) {
		report.AlreadyCurrent = true
		return report, nil
	}
	if err != nil {
		return report, err
	}
	held, err := m.acquireState(ctx, "mac migrate")
	if err != nil {
		return report, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac migrate") }()
	var legacy legacyConfig
	if schema == legacySchemaVersion {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return report, err
		}
		if err := json.Unmarshal(data, &legacy); err != nil || !validUUID(legacy.InstallationID) {
			return report, fmt.Errorf("the earlier Mac config is unreadable: %s", configPath)
		}
	} else if schema != SchemaVersion {
		return report, fmt.Errorf("unsupported mac config schema %d", schema)
	}
	id, err := m.installationID()
	if err != nil {
		return report, err
	}
	items, err := m.migrationItems(ctx, legacy)
	if err != nil {
		return report, err
	}
	helper, err := m.legacyHelperInstalled(ctx)
	if err != nil {
		return report, err
	}
	report.RemoveHelper = helper
	if helper {
		report.HelperLabel = legacyLabel(os.Getuid(), id)
	}
	for _, item := range items {
		report.Machines = append(report.Machines, MigrationMachine{Name: item.slot.Name, User: item.slot.User, OldAddress: item.oldIP,
			Address: item.network.Address, Subnet: item.network.Subnet, Password: item.status})
		if item.status != "unavailable" {
			if _, err := m.legacyOwner(ctx, item.slot.InstanceID); err == nil {
				report.KeychainItems++
			}
		}
	}
	converted := schema == SchemaVersion
	for _, item := range items {
		converted = converted && item.current != nil
	}
	if converted && !helper && report.KeychainItems == 0 && !m.legacyLeftovers() {
		report.AlreadyCurrent = true
		return report, nil
	}
	if options.Confirm != nil {
		if err := options.Confirm(report.MigrationPlan); err != nil {
			return report, err
		}
	}
	// Records first, config last: until config.json is converted every other
	// command keeps refusing the data and points back here.
	for _, item := range items {
		if item.current != nil {
			continue
		}
		backups, err := m.convertMachine(item)
		report.Backups = append(report.Backups, backups...)
		if err != nil {
			return report, fmt.Errorf("convert %s: %w", item.slot.Name, err)
		}
	}
	if schema == legacySchemaVersion {
		backup := filepath.Join(filepath.Dir(configPath), "config.v1.json")
		if err := copyPrivate(configPath, backup); err != nil {
			return report, err
		}
		report.Backups = append(report.Backups, backup)
		config := &Config{SchemaVersion: SchemaVersion, InstallationID: strings.ToLower(legacy.InstallationID), DefaultBaseID: legacy.DefaultBaseID, CreatedAt: legacy.CreatedAt}
		if err := config.Validate(); err != nil {
			return report, err
		}
		if err := writeJSON(configPath, config); err != nil {
			return report, err
		}
	}
	for _, item := range items {
		if item.status == "unavailable" {
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s: the GUI password could not be read from the Keychain, so farrow mac password will not show it; SSH access is unaffected", item.slot.Name))
		}
	}
	if helper {
		m.report("network", "Removing the legacy root network daemon")
		if err := m.removeLegacyHelper(ctx, id); err != nil {
			report.Warnings = append(report.Warnings, err.Error())
			return report, failure.New(failure.Partial, fmt.Errorf("the machines were converted, but the legacy network daemon is still installed: %w", err)).Then("farrow mac migrate")
		}
		report.HelperRemoved = true
	}
	for _, item := range items {
		if _, err := m.Store.Password(item.slot.Name); err != nil {
			continue
		}
		if _, err := m.legacyOwner(ctx, item.slot.InstanceID); err != nil {
			continue
		}
		if err := m.eraseLegacyPassword(ctx, item.slot.InstanceID); err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s: the old Keychain item remains: %v", item.slot.Name, err))
			continue
		}
		report.KeychainErased++
		if path, err := m.Store.Path("credentials", item.slot.InstanceID+".json"); err == nil {
			_ = os.Remove(path)
		}
	}
	m.removeLegacyLeftovers()
	return report, nil
}

// migrationItems reads each earlier slot, proves it stopped and plans its
// new network. The first machine keeps the shared subnet, so its address
// stays; every other machine moves to its own subnet at the same .10 host.
func (m *Manager) migrationItems(ctx context.Context, legacy legacyConfig) ([]migrationItem, error) {
	root, err := m.Store.Path("slots")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	routes, err := m.routes(ctx)
	if err != nil {
		return nil, err
	}
	legacySubnet, _ := netip.ParsePrefix(legacy.Network.Subnet)
	var occupied []netip.Prefix
	for _, route := range routes {
		// The legacy daemon's own bridge goes away with the daemon.
		if !(legacySubnet.IsValid() && route.Prefix == legacySubnet && strings.HasPrefix(route.Interface, "bridge")) {
			occupied = append(occupied, route.Prefix)
		}
	}
	var items []migrationItem
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !ValidName(name) {
			continue
		}
		path, err := m.Store.machineFile(name, "state.json")
		if err != nil {
			return nil, err
		}
		schema, err := schemaOf(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if schema == SchemaVersion {
			machine, err := m.Store.LoadMachine(name)
			if err != nil {
				return nil, err
			}
			if p, err := netip.ParsePrefix(machine.Network.Subnet); err == nil {
				occupied = append(occupied, p)
			}
			items = append(items, migrationItem{slot: legacySlot{Name: name, InstanceID: machine.InstanceID, User: machine.User}, network: machine.Network, oldIP: machine.Network.Address, status: "kept", current: machine})
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var slot legacySlot
		if err := json.Unmarshal(data, &slot); err != nil || slot.SchemaVersion != legacySchemaVersion || slot.Name != name || !validUUID(slot.InstanceID) {
			return nil, fmt.Errorf("%s has an unreadable earlier record: %s", name, path)
		}
		// The earlier runner held the same lock for the VM's lifetime.
		dir, err := m.Store.MachinePath(name)
		if err != nil {
			return nil, err
		}
		if proof, err := lock.TryAcquire(filepath.Join(dir, "runner.lock"), false); err != nil {
			return nil, failure.New(failure.Conflict, fmt.Errorf("%s is still running under the earlier Farrow; shut it down from its desktop, or with the earlier farrow mac stop %s, then retry", name, name)).Because("mac_running")
		} else if err := proof.Release(); err != nil {
			return nil, err
		}
		item := migrationItem{slot: slot, status: "moved"}
		for _, pref := range legacy.Slots {
			if pref.Name == name {
				item.mac, item.oldIP = pref.MAC, pref.IP
			}
		}
		if !validMAC(item.mac) {
			if item.mac, err = newMAC(); err != nil {
				return nil, err
			}
		}
		items = append(items, item)
	}
	// Assign networks in name order: mac1 keeps the shared subnet.
	for i := range items {
		if items[i].current != nil {
			continue
		}
		subnet := ""
		if legacySubnet.IsValid() && CheckSubnet(legacySubnet.String(), occupied) == nil {
			subnet = legacySubnet.String()
		}
		if subnet == "" {
			if subnet, err = SelectSubnet("", occupied); err != nil {
				return nil, err
			}
		}
		prefix, err := ValidateSubnet(subnet)
		if err != nil {
			return nil, err
		}
		occupied = append(occupied, prefix)
		items[i].network = NetworkFor(prefix)
		if password, err := m.Store.Password(items[i].slot.Name); err == nil {
			items[i].password, items[i].status = password, "kept"
		} else if password, err := m.legacyPassword(ctx, items[i].slot.InstanceID); err == nil {
			items[i].password = password
		} else {
			items[i].status = "unavailable"
		}
	}
	return items, nil
}

// convertMachine writes one machine's new record, password file and host key
// pin, keeping backups of every file it replaces.
func (m *Manager) convertMachine(item migrationItem) ([]string, error) {
	slot := item.slot
	machine := &Machine{SchemaVersion: SchemaVersion, Name: slot.Name, InstanceID: strings.ToLower(slot.InstanceID), BaseID: slot.BaseID,
		State: "stopped", User: slot.User, Initialized: slot.Initialized, CPU: slot.CPU, MemoryBytes: slot.MemoryBytes, DiskBytes: slot.DiskBytes,
		MAC: item.mac, Network: item.network, Clipboard: true, Version: slot.Version, Build: slot.Build,
		ObservedVersion: slot.ObservedVersion, ObservedBuild: slot.ObservedBuild, CreatedAt: slot.CreatedAt, UpdatedAt: time.Now().UTC()}
	if !machine.Initialized {
		machine.State = "prepared"
	}
	if err := validateMachine(machine); err != nil {
		return nil, err
	}
	var backups []string
	if item.password != "" {
		if err := m.Store.SavePassword(slot.Name, item.password); err != nil {
			return backups, err
		}
	}
	dir, err := m.Store.MachinePath(slot.Name)
	if err != nil {
		return backups, err
	}
	hosts := filepath.Join(dir, "known_hosts")
	if data, err := os.ReadFile(hosts); err == nil {
		backup := hosts + ".v1"
		if err := copyPrivate(hosts, backup); err != nil {
			return backups, err
		}
		backups = append(backups, backup)
		if err := fsutil.AtomicWrite(hosts, []byte(aliasKnownHosts(string(data), machine.HostKeyAlias())), 0o600); err != nil {
			return backups, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return backups, err
	}
	state := filepath.Join(dir, "state.json")
	backup := filepath.Join(dir, "state.v1.json")
	if err := copyPrivate(state, backup); err != nil {
		return backups, err
	}
	backups = append(backups, backup)
	return backups, writeJSON(state, machine)
}

// aliasKnownHosts re-keys every plain pin line to the instance alias.
func aliasKnownHosts(data, alias string) string {
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(data, "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && !strings.HasPrefix(fields[0], "#") && !strings.HasPrefix(fields[0], "@") {
			line = alias + " " + strings.Join(fields[1:], " ")
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func copyPrivate(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return nil // Keep the first backup of a repeated migration.
	}
	return fsutil.AtomicWrite(target, data, 0o600)
}

// legacyLeftovers reports files only the earlier layout used.
func (m *Manager) legacyLeftovers() bool {
	for _, name := range []string{"credentials", "network-helper.json"} {
		if path, err := m.Store.Path(name); err == nil {
			if _, err := os.Lstat(path); err == nil {
				return true
			}
		}
	}
	return false
}

// removeLegacyLeftovers deletes the retained Keychain owner once no Keychain
// item depends on it, and the earlier network record.
func (m *Manager) removeLegacyLeftovers() {
	if path, err := m.Store.Path("network-helper.json"); err == nil {
		_ = os.Remove(path)
	}
	root, err := m.Store.Path("credentials")
	if err != nil {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			return // An item still needs its owner.
		}
	}
	_ = os.RemoveAll(root)
}
