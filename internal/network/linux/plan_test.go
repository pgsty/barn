package linux

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func debianFacts() Facts {
	return Facts{
		Family: Debian, Systemd: true, NetworkdActive: true,
		Helper:               HelperFacts{Path: "/usr/lib/qemu/qemu-bridge-helper", OwnerUID: 0, Group: "root", Mode: 0o755, Regular: true, ParentSafe: true, PackageOwned: true},
		AccessGroup:          "kvm",
		BridgeConf:           "allow virbr0\n",
		BridgeConfState:      PathState{Existed: true, Owner: "root", Group: "root", Mode: "0644"},
		QEMUConfigDirExisted: true,
		NetworkdUnits:        networkdFixture("enabled", "active"),
	}
}

func TestDebianHelperUsesInvokingUsersAccessGroup(t *testing.T) {
	t.Parallel()
	facts := debianFacts()
	facts.AccessGroup = "vonng"
	plan, err := NewInstallPlan(facts, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Manifest.AppliedOverride == nil || plan.Manifest.AppliedOverride.Group != "vonng" {
		t.Fatalf("applied override = %#v", plan.Manifest.AppliedOverride)
	}
	found := false
	for _, command := range plan.Commands {
		if command.Binary == "/usr/bin/dpkg-statoverride" {
			found = strings.Join(command.Args, " ") == "--update --add root vonng 4750 /usr/lib/qemu/qemu-bridge-helper"
		}
	}
	if !found {
		t.Fatalf("invoking-user helper override missing: %#v", plan.Commands)
	}
}

func TestSelectAccessGroupPrefersKVMThenPrimary(t *testing.T) {
	t.Parallel()
	if got, err := selectAccessGroup("vonng", []string{"sudo", "kvm", "vonng"}); err != nil || got != "kvm" {
		t.Fatalf("kvm selection = %q, %v", got, err)
	}
	if got, err := selectAccessGroup("vonng", []string{"sudo", "vonng"}); err != nil || got != "vonng" {
		t.Fatalf("primary selection = %q, %v", got, err)
	}
	if _, err := selectAccessGroup("bad group", nil); err == nil {
		t.Fatal("unsafe primary group was accepted")
	}
}

func networkdFixture(unitFile, active string) map[string]UnitState {
	result := make(map[string]UnitState, len(NetworkdUnitNames))
	for _, name := range NetworkdUnitNames {
		sub := "dead"
		if active == "active" {
			sub = "running"
		}
		result[name] = UnitState{LoadState: "loaded", UnitFileState: unitFile, ActiveState: active, SubState: sub}
	}
	return result
}

func safeInactiveNetworkdActivation() *NetworkdActivationSafety {
	return &NetworkdActivationSafety{Checked: true, Links: []NetworkdLink{
		{Name: "eth0", Type: "ether"},
		{Name: "lo", Type: "loopback"},
		{Name: BridgeName, Kind: "bridge", Type: "bridge", BarnOwned: true},
	}}
}

func testConfig() Config {
	return Config{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"}
}

func TestDebianInstallPlanIsTypedAndMarkerPreserving(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlan(debianFacts(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Manifest.AppliedOverride == nil || plan.Manifest.AppliedOverride.Mode != "4750" || len(plan.Files) != 4 || len(plan.Directories) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	for _, file := range plan.Files {
		if file.Path == LeaseLockPath || file.Path == TmpfilesPath {
			t.Fatalf("install plan still creates retired lease artifact %s", file.Path)
		}
	}
	foundOverride := false
	for _, command := range plan.Commands {
		if command.Binary == "/usr/bin/dpkg-statoverride" {
			foundOverride = strings.Join(command.Args, " ") == "--update --add root kvm 4750 /usr/lib/qemu/qemu-bridge-helper"
		}
	}
	if !foundOverride {
		t.Fatalf("typed dpkg-statoverride missing: %#v", plan.Commands)
	}
	bridge := plan.Files[0]
	for _, file := range plan.Files {
		if file.Path == BridgeConfPath {
			bridge = file
		}
	}
	if !strings.Contains(bridge.Content, "allow virbr0") || !strings.Contains(bridge.Content, managedBridgeBlock()) {
		t.Fatalf("bridge.conf was not preserved: %q", bridge.Content)
	}
}

func TestCustomSubnetPlanAndManifest(t *testing.T) {
	t.Parallel()
	config, err := ConfigForCIDR("172.31.251.0/24")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewInstallPlan(debianFacts(), config)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Manifest.CIDR != config.CIDR || plan.Manifest.HostAddress != "172.31.251.1" || plan.Manifest.DHCPEnd != "172.31.251.8" {
		t.Fatalf("manifest=%#v", plan.Manifest)
	}
	found := false
	for _, file := range plan.Files {
		if file.Path == NetworkPath {
			found = strings.Contains(file.Content, "Address=172.31.251.1/24")
		}
	}
	if !found {
		t.Fatal("custom networkd address missing")
	}
}

func TestBridgeConfInstallRemoveAndOwnershipRefusal(t *testing.T) {
	t.Parallel()
	original := "# user rule\nallow virbr0\n"
	installed, err := ReconcileBridgeConf(original, true)
	if err != nil {
		t.Fatal(err)
	}
	idempotent, err := ReconcileBridgeConf(installed, true)
	if err != nil || idempotent != installed {
		t.Fatalf("idempotent bridge block = %q, %v", idempotent, err)
	}
	removed, err := ReconcileBridgeConf(installed, false)
	if err != nil || removed != original+"\n" {
		t.Fatalf("removed bridge block = %q, %v", removed, err)
	}
	if _, err := ReconcileBridgeConf("allow barn0 # unowned\n", true); err == nil {
		t.Fatal("unmarked barn0 allow rule was adopted")
	}
	if _, err := ReconcileBridgeConf(markerBegin+"\nallow other\n"+markerEnd+"\n", false); err == nil {
		t.Fatal("modified managed block was removed")
	}
}

func TestHelperAndBridgeSafetyBoundaries(t *testing.T) {
	t.Parallel()
	facts := debianFacts()
	facts.Helper.Override = &Override{Owner: "root", Group: "unowned", Mode: "4750"}
	if _, err := NewInstallPlan(facts, testConfig()); err == nil {
		t.Fatal("non-Barn dpkg override accepted")
	}
	facts = debianFacts()
	facts.BridgeExists = true
	if _, err := NewInstallPlan(facts, testConfig()); err == nil {
		t.Fatal("unowned existing bridge accepted")
	}
	rpm := Facts{Family: RPM, Systemd: true, NetworkdActive: true, NetworkdUnits: networkdFixture("enabled", "active"), Helper: HelperFacts{Path: "/usr/libexec/qemu-bridge-helper", OwnerUID: 0, Group: "root", Mode: 0o4755, Regular: true, ParentSafe: true, PackageOwned: true}}
	plan, err := NewInstallPlan(rpm, testConfig())
	if err != nil || len(plan.Warnings) != 1 {
		t.Fatalf("RPM plan = %#v, %v", plan, err)
	}
	rpm.Helper.Mode = 0o755
	if _, err := NewInstallPlan(rpm, testConfig()); err == nil {
		t.Fatal("unsafe RPM helper mode accepted")
	}
}

func TestInactiveNetworkdPlanRecordsPrestate(t *testing.T) {
	t.Parallel()
	facts := debianFacts()
	facts.NetworkdActive = false
	facts.NetworkdUnits = networkdFixture("disabled", "inactive")
	facts.NetworkdActivation = safeInactiveNetworkdActivation()
	plan, err := NewInstallPlan(facts, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Phases) < 3 || plan.Phases[0].Name != "activate-bridge" || plan.Phases[len(plan.Phases)-1].Name != "persist-after-attach-verification" {
		t.Fatalf("ordered phases = %#v", plan.Phases)
	}
	if plan.Manifest.NetworkdUnits["systemd-networkd.service"].ActiveState != "inactive" {
		t.Fatalf("networkd prestate not retained: %#v", plan.Manifest.NetworkdUnits)
	}
	commands := ""
	for _, command := range plan.Commands {
		commands += command.Binary + " " + strings.Join(command.Args, " ") + "\n"
	}
	for _, want := range []string{"systemctl start systemd-networkd.service", "networkctl reconfigure barn0", "systemctl enable systemd-networkd.service"} {
		if !strings.Contains(commands, want) {
			t.Errorf("inactive plan missing %q:\n%s", want, commands)
		}
	}
}

func TestRepeatedInstallPreservesOriginalSnapshotAndAbsentBridgeConf(t *testing.T) {
	t.Parallel()
	facts := debianFacts()
	facts.BridgeConf = ""
	facts.BridgeConfState = PathState{}
	facts.QEMUConfigDirExisted = false
	facts.NetworkdActive = false
	facts.NetworkdUnits = networkdFixture("disabled", "inactive")
	facts.NetworkdActivation = safeInactiveNetworkdActivation()
	first, err := NewInstallPlan(facts, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	installedBridge := ""
	for _, file := range first.Files {
		if file.Path == BridgeConfPath {
			installedBridge = file.Content
		}
	}
	current := facts
	current.ExistingManifest = &first.Manifest
	current.BridgeExists = true
	current.BridgeOwned = true
	current.BridgeConf = installedBridge
	current.BridgeConfState = PathState{Existed: true, Owner: "root", Group: "root", Mode: "0644"}
	current.QEMUConfigDirExisted = true
	current.NetworkdActive = true
	current.NetworkdUnits = networkdFixture("enabled", "active")
	current.Helper.Group = "kvm"
	current.Helper.Mode = 0o4750
	current.Helper.Override = &Override{Owner: "root", Group: "kvm", Mode: "4750"}
	second, err := NewInstallPlan(current, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Manifest, second.Manifest) {
		t.Fatalf("repeated install rewrote original snapshot:\nfirst=%#v\nsecond=%#v", first.Manifest, second.Manifest)
	}
	currentFiles := make(map[string]string)
	for _, file := range first.Files {
		if file.Path != StatePath {
			currentFiles[file.Path] = file.Content
		}
	}
	applied := *first.Manifest.AppliedOverride
	uninstall, err := NewUninstallPlan(first.Manifest, UninstallFacts{CurrentFiles: currentFiles, CurrentHelper: applied, CurrentOverride: &applied})
	if err != nil {
		t.Fatal(err)
	}
	if len(uninstall.RestoreFiles) != 0 || !containsString(uninstall.RemoveFiles, BridgeConfPath) || !containsString(uninstall.RemoveDirectories, QEMUConfigDir) {
		t.Fatalf("initially absent qemu paths would leave residue: %#v", uninstall)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestStrictManifestAndOwnershipBoundedUninstall(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlan(debianFacts(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(plan.Manifest)
	manifest, err := StrictManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StrictManifest(append(append([]byte(nil), data...), []byte("\n{}")...)); err == nil {
		t.Fatal("trailing manifest JSON accepted")
	}
	unknown := append([]byte(`{"unknown":true,`), data[1:]...)
	if _, err := StrictManifest(unknown); err == nil {
		t.Fatal("unknown manifest field accepted")
	}
	currentFiles := make(map[string]string)
	for _, file := range plan.Files {
		if file.Path != StatePath {
			currentFiles[file.Path] = file.Content
		}
	}
	applied := *manifest.AppliedOverride
	uninstall, err := NewUninstallPlan(manifest, UninstallFacts{CurrentFiles: currentFiles, CurrentHelper: applied, CurrentOverride: &applied})
	if err != nil {
		t.Fatal(err)
	}
	if len(uninstall.RestoreFiles) != 1 || uninstall.RestoreFiles[0].Content != debianFacts().BridgeConf || len(uninstall.RemoveFiles) < 3 {
		t.Fatalf("uninstall plan = %#v", uninstall)
	}
	commands := ""
	for _, command := range uninstall.Commands {
		commands += command.Binary + " " + strings.Join(command.Args, " ") + "\n"
	}
	for _, required := range []string{"dpkg-statoverride --remove", "/bin/chown root:root", "/bin/chmod 0755"} {
		if !strings.Contains(commands, required) {
			t.Errorf("uninstall missing %q:\n%s", required, commands)
		}
	}
	altered := make(map[string]string, len(currentFiles))
	for pathname, content := range currentFiles {
		altered[pathname] = content
	}
	altered[NetDevPath] += "# changed\n"
	if _, err := NewUninstallPlan(manifest, UninstallFacts{CurrentFiles: altered, CurrentHelper: applied, CurrentOverride: &applied}); err == nil {
		t.Fatal("changed owned file did not block uninstall")
	}
	if _, err := NewUninstallPlan(manifest, UninstallFacts{BridgeMembers: []string{"tap0"}, CurrentFiles: currentFiles, CurrentHelper: applied, CurrentOverride: &applied}); err == nil {
		t.Fatal("bridge member did not block uninstall")
	}
}

func TestAlreadySafeDebianHelperIsNotClaimedForRestoration(t *testing.T) {
	t.Parallel()
	facts := debianFacts()
	facts.Helper.Group = "kvm"
	facts.Helper.Mode = 0o4750
	plan, err := NewInstallPlan(facts, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Manifest.AppliedOverride != nil {
		t.Fatalf("pre-existing safe helper was claimed: %#v", plan.Manifest.AppliedOverride)
	}
	for _, command := range plan.Commands {
		if command.Binary == "/usr/bin/dpkg-statoverride" {
			t.Fatal("pre-existing safe helper received a dpkg override command")
		}
	}
}
