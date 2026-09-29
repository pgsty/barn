package darwin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/network/subnet"
)

func TestPinnedRelease(t *testing.T) {
	t.Parallel()
	release, err := PinnedRelease("arm64")
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "1.2.2" || release.SHA256 != "c7bf62308fbcfdc29bdfb8373c9b1951f7ac2396446e4390919796a94972e6dc" || release.SocketSHA256 == "" {
		t.Fatalf("release = %#v", release)
	}
	if _, err := PinnedRelease("riscv64"); err == nil {
		t.Fatal("unsupported artifact architecture accepted")
	}
}

func fixtureArchive(t *testing.T, names []string) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range names {
		content := []byte("fixture")
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestArchiveInspectionRejectsTraversal(t *testing.T) {
	t.Parallel()
	archive := fixtureArchive(t, []string{"../../escape", "opt/socket_vmnet/bin/socket_vmnet", "opt/socket_vmnet/bin/socket_vmnet_client"})
	if _, err := inspectArchive(bytes.NewReader(archive)); err == nil {
		t.Fatal("traversal archive unexpectedly accepted")
	}
}

func TestArchiveInspectionRequiresExecutables(t *testing.T) {
	t.Parallel()
	archive := fixtureArchive(t, []string{"opt/socket_vmnet/bin/socket_vmnet"})
	if _, err := inspectArchive(bytes.NewReader(archive)); err == nil {
		t.Fatal("incomplete archive unexpectedly accepted")
	}
}

func TestInstallPlanContract(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlan("arm64", "018f4b8e-1234-7abc-9def-0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	plist, err := plan.Plist()
	if err != nil {
		t.Fatal(err)
	}
	text := string(plist)
	for _, expected := range []string{DaemonPath, "--vmnet-mode=host", "--vmnet-gateway=10.10.10.1", "--vmnet-dhcp-end=10.10.10.8", "--vmnet-interface-id=018f4b8e-1234-7abc-9def-0123456789ab", SocketPath, "<string>root</string>"} {
		if !strings.Contains(text, expected) {
			t.Errorf("plist missing %q", expected)
		}
	}
	if strings.Contains(text, "network-identifier") || strings.Contains(text, "isolation") {
		t.Fatal("plan enables isolated network")
	}
	state, err := plan.StateJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"mode": "host"`, `"cidr": "10.10.10.0/24"`, `"dhcp_end": "10.10.10.8"`} {
		if !strings.Contains(string(state), expected) {
			t.Errorf("state missing %q", expected)
		}
	}
}

func TestSharedInstallPlanKeepsSameSubnetWithoutIsolation(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlanMode("arm64", "018f4b8e-1234-7abc-9def-0123456789ab", "shared")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Args, "\n")
	for _, expected := range []string{"--vmnet-mode=shared", "--vmnet-gateway=10.10.10.1", "--vmnet-dhcp-end=10.10.10.8"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("shared plan missing %q", expected)
		}
	}
	if plan.State.Mode != "shared" || strings.Contains(joined, "network-identifier") {
		t.Fatalf("shared plan = %#v args=%s", plan.State, joined)
	}
	state, err := plan.StateJSON()
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := StrictNetworkState(state); err != nil || parsed.Mode != "shared" {
		t.Fatalf("strict shared state=%#v err=%v", parsed, err)
	}
	if _, err := NewInstallPlanMode("arm64", "018f4b8e-1234-7abc-9def-0123456789ab", "bridged"); err == nil {
		t.Fatal("unsupported mode accepted")
	}
}

func TestCustomSubnetPlanKeepsHostAndDHCPPolicy(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlanModeNetwork("arm64", "018f4b8e-1234-7abc-9def-0123456789ab", "shared", "172.31.251.0/24")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Args, "\n")
	for _, expected := range []string{"--vmnet-mode=shared", "--vmnet-gateway=172.31.251.1", "--vmnet-dhcp-end=172.31.251.8"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("custom plan missing %q", expected)
		}
	}
	state, err := plan.StateJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := StrictNetworkState(state)
	if err != nil || parsed.CIDR != "172.31.251.0/24" || parsed.HostAddress != "172.31.251.1" {
		t.Fatalf("state=%#v err=%v", parsed, err)
	}
}

func TestInterfaceMarkerStrictContractAndPinnedBytes(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlan("arm64", "018f4b8e-1234-7abc-9def-0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithBSDInterface("bridge100")
	if err != nil {
		t.Fatal(err)
	}
	data, err := plan.InterfaceJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := StrictInterfaceMarker(data)
	if err != nil || parsed != plan.Interface {
		t.Fatalf("marker=%#v err=%v", parsed, err)
	}
	for _, expected := range []string{
		`"interface_id": "018f4b8e-1234-7abc-9def-0123456789ab"`,
		`"cidr": "10.10.10.0/24"`, `"host_address": "10.10.10.1"`, `"bsd_name": "bridge100"`,
	} {
		if !strings.Contains(string(data), expected) {
			t.Errorf("interface marker missing %q", expected)
		}
	}
	invalid := [][]byte{
		bytes.Replace(data, []byte(`"schema": 1`), []byte(`"schema": 2`), 1),
		bytes.Replace(data, []byte(`"host_address": "10.10.10.1"`), []byte(`"host_address": "10.10.11.1"`), 1),
		bytes.Replace(data, []byte(`"bsd_name": "bridge100"`), []byte(`"bsd_name": "vboxnet0/../../"`), 1),
		append(append([]byte(nil), data...), []byte("{}")...),
		[]byte(`{"schema":1,"interface_id":"018f4b8e-1234-7abc-9def-0123456789ab","cidr":"10.10.10.0/24","host_address":"10.10.10.1","bsd_name":"bridge100","extra":true}`),
	}
	for index, candidate := range invalid {
		if _, err := StrictInterfaceMarker(candidate); err == nil {
			t.Errorf("invalid marker %d accepted: %s", index, candidate)
		}
	}

	publicWithDifferentBytes := append([]byte(" "), data...)
	if _, err := bindInterfaceEvidence(plan, data, publicWithDifferentBytes); err == nil {
		t.Fatal("semantically equivalent marker with different bytes accepted")
	}
	if rebound, err := bindInterfaceEvidence(plan, data, data); err != nil || rebound.Interface != plan.Interface {
		t.Fatalf("exact public/protected evidence rejected: plan=%#v err=%v", rebound, err)
	}
	foreignPlan, err := NewInstallPlanModeNetwork("arm64", "018f4b8e-1234-7abc-9def-0123456789ab", "host", "172.31.251.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bindInterfaceEvidence(foreignPlan, data, data); err == nil {
		t.Fatal("marker from a different CIDR accepted by protected plan")
	}
}

func TestRecoverPlanFromExactInterfaceEvidence(t *testing.T) {
	t.Parallel()
	plan, err := NewInstallPlanModeNetwork("arm64", "018f4b8e-1234-7abc-9def-0123456789ab", "host", subnet.DefaultCIDR)
	if err == nil {
		plan, err = plan.WithBSDInterface("bridge100")
	}
	if err != nil {
		t.Fatal(err)
	}
	marker, err := plan.InterfaceJSON()
	if err != nil {
		t.Fatal(err)
	}
	plist, err := plan.Plist()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverPlanFromInterfaceEvidence("arm64", marker, marker, plist, plan.SocketDigest(), plan.ClientDigest())
	if err != nil || !reflect.DeepEqual(recovered, plan) {
		t.Fatalf("recovered=%#v err=%v want=%#v", recovered, err, plan)
	}
	if _, err := recoverPlanFromInterfaceEvidence("arm64", append([]byte(" "), marker...), marker, plist, plan.SocketDigest(), plan.ClientDigest()); err == nil {
		t.Fatal("different protected/public evidence was accepted")
	}
	modifiedPlist := bytes.Replace(plist, []byte("--vmnet-gateway=10.10.10.1"), []byte("--vmnet-gateway=10.10.10.99"), 1)
	if _, err := recoverPlanFromInterfaceEvidence("arm64", marker, marker, modifiedPlist, plan.SocketDigest(), plan.ClientDigest()); err == nil {
		t.Fatal("modified launchd plist was accepted")
	}
	if _, err := recoverPlanFromInterfaceEvidence("arm64", marker, marker, plist, strings.Repeat("0", 64), plan.ClientDigest()); err == nil {
		t.Fatal("wrong installed binary digest was accepted")
	}
}

func TestExactHostInterfaceDeltaDoesNotAdoptForeignVBox(t *testing.T) {
	t.Parallel()
	layout := subnet.Default()
	beforeOutput := []byte(`vboxnet0: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet 10.10.10.1 netmask 0xffffff00 broadcast 10.10.10.255
bridge98: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet 10.10.10.1 netmask 0xffff0000 broadcast 10.10.255.255
bridge99: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet 10.10.10.2 netmask 0xffffff00 broadcast 10.10.10.255
`)
	afterOutput := append(append([]byte(nil), beforeOutput...), []byte(`bridge100: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet 10.10.10.1 netmask 255.255.255.0 broadcast 10.10.10.255
`)...)
	before, err := exactHostInterfaces(beforeOutput, layout)
	if err != nil {
		t.Fatal(err)
	}
	after, err := exactHostInterfaces(afterOutput, layout)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("before exact interfaces = %#v", before)
	}
	if _, ok := before["vboxnet0"]; !ok {
		t.Fatalf("foreign vboxnet0 was not captured in baseline: %#v", before)
	}
	created := newExactHostInterfaces(before, after)
	if len(created) != 1 || created[0] != "bridge100" {
		t.Fatalf("new exact interfaces = %#v, want only bridge100", created)
	}
	after["bridge101"] = struct{}{}
	created = newExactHostInterfaces(before, after)
	if len(created) != 2 || created[0] != "bridge100" || created[1] != "bridge101" {
		t.Fatalf("multiple new interfaces not retained deterministically: %#v", created)
	}
	if created := newExactHostInterfaces(before, before); len(created) != 0 {
		t.Fatalf("pre-existing interface treated as newly created: %#v", created)
	}
}

type recordingRunner struct {
	calls []string
}

func (r *recordingRunner) Run(_ context.Context, binary string, args ...string) (execx.Result, error) {
	r.calls = append(r.calls, strings.Join(append([]string{binary}, args...), "\x00"))
	return execx.Result{}, nil
}

func TestPartialInstallRollbackIsExactAndIncludesInterfaceEvidence(t *testing.T) {
	runner := &recordingRunner{}
	executor := Executor{Root: runner}
	created := []string{StateDir, InterfaceMarkerDir}
	if err := executor.rollbackFreshInstall(context.Background(), true, created); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, expected := range []string{
		"/bin/launchctl\x00bootout\x00system/" + ServiceID,
		"/bin/rm\x00-f\x00" + InterfaceStatePath,
		"/bin/rm\x00-f\x00" + InterfaceMarkerPath,
		"/bin/rmdir\x00" + InterfaceMarkerDir,
		"/bin/rmdir\x00" + StateDir,
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("rollback missing exact command %q; calls:\n%s", expected, joined)
		}
	}
	if strings.Contains(joined, "\x00-r") || strings.Contains(joined, "\x00-rf") {
		t.Fatalf("rollback used recursive removal: %s", joined)
	}
	markerDirIndex := strings.Index(joined, "/bin/rmdir\x00"+InterfaceMarkerDir)
	stateDirIndex := strings.Index(joined, "/bin/rmdir\x00"+StateDir)
	if markerDirIndex < 0 || stateDirIndex < 0 || markerDirIndex > stateDirIndex {
		t.Fatalf("created directories were not removed in reverse order: %s", joined)
	}
}

func TestInterfaceEvidencePathsArePublicProtectedAndTargeted(t *testing.T) {
	t.Parallel()
	if InterfaceMarkerPath != "/Library/Application Support/io.pgsty.barn/network-interface.json" ||
		InterfaceStatePath != "/private/var/db/barn/network-interface.json" {
		t.Fatalf("interface evidence paths changed: public=%q protected=%q", InterfaceMarkerPath, InterfaceStatePath)
	}
	targets := darwinTargets()
	if targets[InterfaceMarkerDir] != "root:wheel 0755" || targets[InterfaceMarkerPath] != "root:wheel 0644" ||
		targets[InterfaceStatePath] != "root:wheel 0600" {
		t.Fatalf("interface evidence metadata targets = %#v", targets)
	}
	if _, exists := targets[LeaseRoot]; exists {
		t.Fatalf("current install contract still creates retired lease root: %#v", targets)
	}
}
