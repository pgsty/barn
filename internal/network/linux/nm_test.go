package linux

import (
	"encoding/json"
	"strings"
	"testing"
)

func nmFacts() Facts {
	return Facts{
		Family: RPM, Systemd: true, NetworkManagerActive: true,
		Helper:               HelperFacts{Path: "/usr/libexec/qemu-bridge-helper", OwnerUID: 0, Group: "root", Mode: 0o4755, Regular: true, ParentSafe: true, PackageOwned: true},
		BridgeConfState:      PathState{},
		QEMUConfigDirExisted: false,
	}
}

func planCommandText(plan Plan) string {
	text := ""
	for _, command := range plan.Commands {
		text += command.Binary + " " + strings.Join(command.Args, " ") + "\n"
	}
	return text
}

func TestNetworkManagerBackendPlan(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlan(nmFacts(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Manifest.Backend != BackendNetworkManager || !plan.Manifest.NetworkManager || len(plan.Manifest.NetworkdUnits) != 0 {
		t.Fatalf("NM manifest = %#v", plan.Manifest)
	}
	commands := planCommandText(plan)
	for _, want := range []string{
		"nmcli connection add type bridge con-name farrow0 ifname farrow0",
		"ipv4.addresses 10.10.10.1/24",
		"ipv6.method disabled",
		"connection.autoconnect yes",
		"nmcli connection up farrow0",
	} {
		if !strings.Contains(commands, want) {
			t.Errorf("NM plan missing %q:\n%s", want, commands)
		}
	}
	for _, forbidden := range []string{"networkctl", "systemd-networkd", "unmanaged-devices", "systemd-tmpfiles"} {
		if strings.Contains(commands, forbidden) {
			t.Errorf("NM plan must not touch networkd or retired lease runtime: %q in\n%s", forbidden, commands)
		}
	}
	paths := make(map[string]struct{}, len(plan.Files))
	for _, file := range plan.Files {
		paths[file.Path] = struct{}{}
	}
	for _, want := range []string{BridgeConfPath, PublicStatePath, StatePath} {
		if _, ok := paths[want]; !ok {
			t.Errorf("NM plan missing owned file %s: %v", want, paths)
		}
	}
	for _, forbidden := range []string{NetDevPath, NetworkPath, NetworkManagerPath, TmpfilesPath, LeaseLockPath} {
		if _, ok := paths[forbidden]; ok {
			t.Errorf("NM plan must not write networkd or retired lease file %s", forbidden)
		}
	}
	// No sudo firewalld: zone argument absent.
	if strings.Contains(commands, "connection.zone") {
		t.Fatalf("zone must be firewalld-gated:\n%s", commands)
	}
}

func TestNetworkManagerBackendFirewalldZoneAndExistingConnection(t *testing.T) {
	t.Parallel()
	facts := nmFacts()
	facts.FirewalldActive = true
	plan, err := NewInstallPlan(facts, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(planCommandText(plan), "connection.zone trusted") {
		t.Fatalf("firewalld host must assign the trusted zone:\n%s", planCommandText(plan))
	}
	// An unowned existing connection is never adopted.
	foreign := nmFacts()
	foreign.NMConnectionExists = true
	if _, err := NewInstallPlan(foreign, testConfig()); err == nil || !strings.Contains(err.Error(), "NetworkManager connection that Farrow did not create") {
		t.Fatalf("foreign connection adoption error = %v", err)
	}
}

func TestNetworkManagerManifestRoundTripAndUninstall(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlan(nmFacts(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(plan.Manifest)
	manifest, err := StrictManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if ManifestBackend(manifest) != BackendNetworkManager {
		t.Fatalf("manifest backend = %q", manifest.Backend)
	}
	currentFiles := make(map[string]string)
	for _, file := range plan.Files {
		if file.Path != StatePath {
			currentFiles[file.Path] = file.Content
		}
	}
	helper := Override{Owner: "root", Group: "root", Mode: "4755"}
	uninstall, err := NewUninstallPlan(manifest, UninstallFacts{CurrentFiles: currentFiles, CurrentHelper: helper})
	if err != nil {
		t.Fatal(err)
	}
	commands := ""
	for _, command := range uninstall.Commands {
		commands += command.Binary + " " + strings.Join(command.Args, " ") + "\n"
	}
	if !strings.Contains(commands, "nmcli connection delete farrow0") || strings.Contains(commands, "networkctl") {
		t.Fatalf("NM uninstall commands = %q", commands)
	}
	removed := strings.Join(uninstall.RemoveFiles, "\n")
	for _, want := range []string{PublicStatePath, StatePath, BridgeConfPath} {
		if !strings.Contains(removed, want) {
			t.Errorf("NM uninstall must remove %s: %v", want, uninstall.RemoveFiles)
		}
	}
	dirs := strings.Join(uninstall.RemoveDirectories, "\n")
	if !strings.Contains(dirs, PublicStateDir) || !strings.Contains(dirs, QEMUConfigDir) {
		t.Errorf("NM uninstall must prune created directories: %v", uninstall.RemoveDirectories)
	}
}

func TestBackendSwitchRequiresUninstall(t *testing.T) {
	t.Parallel()
	networkdPlan, err := NewInstallPlan(debianFacts(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	// networkd manifest + NetworkManager now active → NM backend refuses.
	nmHost := nmFacts()
	nmHost.Family = Debian
	nmHost.Helper = debianFacts().Helper
	nmHost.AccessGroup = debianFacts().AccessGroup
	nmHost.ExistingManifest = &networkdPlan.Manifest
	if _, err := NewInstallPlan(nmHost, testConfig()); err == nil || !strings.Contains(err.Error(), "systemd-networkd backend") {
		t.Fatalf("backend switch error = %v", err)
	}
	// NM manifest + networkd now the owner → networkd backend refuses.
	nmPlan, err := NewInstallPlan(nmFacts(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	networkdHost := debianFacts()
	networkdHost.Family = RPM
	networkdHost.Helper = nmFacts().Helper
	networkdHost.ExistingManifest = &nmPlan.Manifest
	if _, err := NewInstallPlan(networkdHost, testConfig()); err == nil || !strings.Contains(err.Error(), "NetworkManager backend") {
		t.Fatalf("backend switch error = %v", err)
	}
	// A ready NM install replans idempotently over its own manifest.
	repeat := nmFacts()
	repeat.NMConnectionExists = true
	repeat.BridgeExists = true
	repeat.BridgeOwned = true
	repeat.ExistingManifest = &nmPlan.Manifest
	repeat.BridgeConf = ""
	replan, err := NewInstallPlan(repeat, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(planCommandText(replan), "connection add") {
		t.Fatalf("repeat NM install must not duplicate the connection:\n%s", planCommandText(replan))
	}
}
