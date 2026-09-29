package linux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeNetworkdTestFile(t *testing.T, pathname, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(pathname), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathname, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func activationTestLinks() []NetworkdLink {
	return []NetworkdLink{
		{Name: "eth0", Type: "ether"},
		{Name: "lo", Type: "loopback"},
		{Name: BridgeName, Kind: "bridge", Type: "bridge", BarnOwned: true},
	}
}

func TestInactiveNetworkdRejectsConfigThatCouldMatchEth0(t *testing.T) {
	directory := filepath.Join("testdata", "networkd-match-eth0")
	safety := inspectNetworkdActivation([]string{directory}, activationTestLinks(), nil, false)
	if len(safety.Conflicts) == 0 {
		t.Fatal("eth0-matching networkd fixture produced no activation conflict")
	}
	foundEth0 := false
	for _, conflict := range safety.Conflicts {
		if strings.HasSuffix(conflict.Path, "10-eth0.network") && conflict.Link == "eth0" {
			foundEth0 = true
		}
	}
	if !foundEth0 {
		t.Fatalf("activation conflicts omit eth0: %#v", safety.Conflicts)
	}

	facts := debianFacts()
	facts.NetworkdActive = false
	facts.NetworkdUnits = networkdFixture("disabled", "inactive")
	facts.NetworkdActivation = &safety
	if _, err := NewInstallPlan(facts, testConfig()); err == nil || !strings.Contains(err.Error(), "eth0") {
		t.Fatalf("inactive networkd plan did not fail closed on eth0 fixture: %v", err)
	}
}

func TestInactiveNetworkdAcceptsDisjointUbuntuVendorFixture(t *testing.T) {
	directory := filepath.Join("testdata", "networkd-ubuntu-vendor")
	safety := inspectNetworkdActivation([]string{directory}, activationTestLinks(), nil, false)
	if len(safety.Configurations) != 7 || len(safety.Conflicts) != 0 {
		t.Fatalf("Ubuntu vendor activation proof = %#v", safety)
	}

	facts := debianFacts()
	facts.NetworkdActive = false
	facts.NetworkdUnits = networkdFixture("disabled", "inactive")
	facts.NetworkdActivation = &safety
	plan, err := NewInstallPlan(facts, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if plan.NetworkdActivation == nil || !plan.NetworkdActivation.Checked {
		t.Fatalf("plan omitted activation proof: %#v", plan.NetworkdActivation)
	}
}

func TestInactiveNetworkdRejectsUnprovenAndNetdevActivation(t *testing.T) {
	facts := debianFacts()
	facts.NetworkdActive = false
	facts.NetworkdUnits = networkdFixture("disabled", "inactive")
	if _, err := NewInstallPlan(facts, testConfig()); err == nil || !strings.Contains(err.Error(), "cannot prove that starting it leaves existing links untouched") {
		t.Fatalf("inactive networkd without discovery proof was accepted: %v", err)
	}

	directory := t.TempDir()
	writeNetworkdTestFile(t, filepath.Join(directory, "20-existing.netdev"), "[NetDev]\nName=existing0\nKind=bridge\n")
	safety := inspectNetworkdActivation([]string{directory}, activationTestLinks(), nil, false)
	facts.NetworkdActivation = &safety
	if _, err := NewInstallPlan(facts, testConfig()); err == nil || !strings.Contains(err.Error(), ".netdev") {
		t.Fatalf("inactive networkd accepted an existing netdev: %v", err)
	}
}

func TestNetworkdActivationHonorsMasksAndRejectsDropIns(t *testing.T) {
	highPriority := t.TempDir()
	lowPriority := t.TempDir()
	writeNetworkdTestFile(t, filepath.Join(lowPriority, "10-eth0.network"), "[Match]\nName=eth0\n")
	writeNetworkdTestFile(t, filepath.Join(highPriority, "10-eth0.network"), "")

	safety := inspectNetworkdActivation([]string{highPriority, lowPriority}, activationTestLinks(), nil, false)
	if len(safety.Conflicts) != 0 || len(safety.Configurations) != 0 {
		t.Fatalf("higher-priority empty mask did not disable lower config: %#v", safety)
	}

	writeNetworkdTestFile(t, filepath.Join(highPriority, "20-safe.network"), "[Match]\nName=not-present\n")
	writeNetworkdTestFile(t, filepath.Join(lowPriority, "20-safe.network.d", "50-override.conf"), "[Match]\nName=eth0\n")
	safety = inspectNetworkdActivation([]string{highPriority, lowPriority}, activationTestLinks(), nil, false)
	if len(safety.Conflicts) == 0 || !strings.Contains(safety.Conflicts[0].Reason, "drop-in") {
		t.Fatalf("networkd drop-in did not fail closed: %#v", safety.Conflicts)
	}
}

func TestNetworkdActivationMatchesAlternativeLinkNames(t *testing.T) {
	directory := t.TempDir()
	writeNetworkdTestFile(t, filepath.Join(directory, "10-uplink.network"), "[Match]\nName=uplink0\n")
	links := activationTestLinks()
	links[0].AlternativeNames = []string{"uplink0"}
	safety := inspectNetworkdActivation([]string{directory}, links, nil, false)
	if len(safety.Conflicts) == 0 || safety.Conflicts[0].Link != "eth0" {
		t.Fatalf("alternative-name match was not treated as an eth0 conflict: %#v", safety.Conflicts)
	}
}
