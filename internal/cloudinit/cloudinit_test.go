package cloudinit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const testPublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEbV2dummydummydummydummydummydummydummy barn-test"

func testInput() Input {
	return Input{
		Node: "meta", Hostname: "meta", Generation: 1,
		SpecHash: strings.Repeat("a", 64), SSHUser: "dba", PublicKey: testPublicKey,
		MgmtMAC: "02:11:22:33:44:55",
		Disks:   []Disk{{Serial: "abcde234567abcde2345", Mount: "/data", Filesystem: "auto"}},
	}
}

func testKeyPair(t *testing.T) (string, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic))) + " barn-test", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
}

func TestRenderQuickContract(t *testing.T) {
	t.Parallel()
	files, err := Render(testInput())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files.entries() {
		if len(content) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
	userData := string(files.UserData)
	for _, want := range []string{"#cloud-config", `name: "dba"`, "uid: 88", "primary_group: admin", "create_groups: false", "/usr/sbin/groupadd --gid 88 admin", "/usr/sbin/groupmod --gid 88 admin", "lock_passwd: true", "ssh_deletekeys: false", "ntp:\n  enabled: true", "/dev/disk/by-id/", "UUID=%s", "defaults,nofail", `mountpoint -q "${mountpoint}"`, "filesystem probe failed", "previous data discarded", "e2fsck -fn", "xfs_repair -n", "/usr/local/libexec/barn-network-check", "/usr/local/libexec/barn-identity-contract", "login identity", "for attempt in 1 2 3", "timeout 10s /bin/bash", "/dev/tcp/example.com/80", "/var/lib/barn/ready.json", "/var/lib/barn/error.json", "run_optional data-disks"} {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing %q", want)
		}
	}
	if strings.Contains(userData, "BEGIN OPENSSH PRIVATE KEY") || strings.Contains(userData, "StrictHostKeyChecking no") {
		t.Fatal("quick seed contains a private key or disables host-key checking")
	}
	if strings.Contains(userData, "barn-init-shares") || strings.Contains(userData, "BARN SHARES") {
		t.Fatal("no-share quick seed contains share-specific files or markers")
	}
	if strings.Contains(string(files.NetworkConfig), "private:") {
		t.Fatal("quick network unexpectedly contains a private NIC")
	}
}

func TestGeneratedScriptsParseWithBash(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	scripts := map[string]string{
		"disk":                renderDiskScript(testInput().Disks),
		"network":             renderNetworkCheckScript(),
		"private":             renderPrivateContractScript(PrivateNetwork{MAC: "02:aa:bb:cc:dd:ee", Address: "10.10.10.10", Prefix: 24, HostAddress: "10.10.10.1"}),
		"finalizer":           renderFinalizeScript(true, true, true),
		"finalizer-no-shares": renderFinalizeScript(false, true, false),
	}
	for name, script := range scripts {
		name, script := name, script
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			command := exec.Command(bash, "-n")
			command.Stdin = strings.NewReader(script)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("generated %s script is invalid: %v\n%s\n%s", name, err, output, script)
			}
		})
	}
}

func TestRenderPrivateControlKeyBoundary(t *testing.T) {
	t.Parallel()
	input := testInput()
	input.Private = &PrivateNetwork{MAC: "02:aa:bb:cc:dd:ee", Address: "10.10.10.10", Prefix: 24, HostAddress: "10.10.10.1"}
	input.Hosts = []Host{{Name: "meta", Address: "10.10.10.10"}, {Name: "node-1", Address: "10.10.10.11"}}
	input.Control = true
	input.PublicKey, input.PrivateKey = testKeyPair(t)
	files, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(files.NetworkConfig), `"10.10.10.10/24"`) || !strings.Contains(string(files.NetworkConfig), "accept-ra: false") || !strings.Contains(string(files.NetworkConfig), "link-local: []") || strings.Contains(string(files.NetworkConfig), "gateway") || strings.Contains(string(files.NetworkConfig), "nameservers") {
		t.Fatalf("private network contract mismatch:\n%s", files.NetworkConfig)
	}
	if !bytes.Contains(files.UserData, []byte("barn-private-contract")) || !bytes.Contains(files.UserData, []byte("private interface ${interface} owns a default route")) || !bytes.Contains(files.UserData, []byte(`expected_mac="02:aa:bb:cc:dd:ee"`)) {
		t.Fatal("private runtime contract checker missing")
	}
	if bytes.Contains(files.NetworkConfig, []byte("set-name:")) || bytes.Contains(files.NetworkConfig, []byte("mgmt0")) || bytes.Contains(files.NetworkConfig, []byte("private0")) {
		t.Fatalf("network config still renames interfaces:\n%s", files.NetworkConfig)
	}
	if bytes.Contains(files.UserData, []byte(`ping -c`)) || !bytes.Contains(files.UserData, []byte(`printf 'barn-arp-refresh\n' >"/dev/udp/${expected_host}/9"`)) || !bytes.Contains(files.UserData, []byte(`expected_host="10.10.10.1"`)) {
		t.Fatal("private ready contract does not refresh/verify the host neighbor path")
	}
	if !bytes.Contains(files.UserData, []byte("-----BEGIN PRIVATE KEY-----")) ||
		!bytes.Contains(files.UserData, []byte("StrictHostKeyChecking no")) ||
		!bytes.Contains(files.UserData, []byte("UserKnownHostsFile /dev/null")) {
		t.Fatal("control lateral SSH contract missing")
	}
	for _, want := range []string{
		`path: "/var/lib/barn/control-id_ed25519"`,
		`path: "/var/lib/barn/control-ssh-config"`,
		`path: "/usr/local/libexec/barn-install-control-ssh"`,
		`entry=$(getent passwd "${user}")`,
		`install -d -o "${uid}" -g "${gid}" -m 0700 "${ssh_dir}"`,
		`rm -f -- /var/lib/barn/control-id_ed25519 /var/lib/barn/control-ssh-config`,
		`path: "/usr/local/libexec/barn-finalize"`,
		`- ['/usr/local/libexec/barn-finalize']`,
	} {
		if !bytes.Contains(files.UserData, []byte(want)) {
			t.Errorf("control seed missing %q", want)
		}
	}
	if bytes.Contains(files.UserData, []byte(`owner: "dba:dba"`)) ||
		bytes.Contains(files.UserData, []byte(`path: "/home/dba/.ssh/id_ed25519"`)) {
		t.Fatal("write_files refers to the login user before users-groups creates it")
	}
	finalizer := renderFinalizeScript(true, true, true)
	identityIndex := strings.Index(finalizer, "/usr/local/libexec/barn-identity-contract")
	diskIndex := strings.Index(finalizer, "/usr/local/libexec/barn-init-disks")
	shareIndex := strings.Index(finalizer, "/usr/local/libexec/barn-init-shares")
	installIndex := strings.Index(finalizer, "/usr/local/libexec/barn-install-control-ssh")
	privateIndex := strings.Index(finalizer, "/usr/local/libexec/barn-private-contract")
	readyIndex := strings.Index(finalizer, "/usr/local/libexec/barn-ready")
	if identityIndex < 0 || diskIndex <= identityIndex || shareIndex <= diskIndex || installIndex <= shareIndex || privateIndex <= installIndex || readyIndex <= privateIndex {
		t.Fatalf("private finalizer stages are out of order:\n%s", finalizer)
	}

	input.Control = false
	if _, err := Render(input); err == nil {
		t.Fatal("private key injected into non-control guest")
	}
}

func TestRenderHostsRefreshesManagedEntries(t *testing.T) {
	t.Parallel()
	script := RenderHostsScript([]Host{{Name: "node-1", Address: "10.10.10.11"}, {Name: "meta", Address: "10.10.10.10"}})
	if !strings.Contains(script, "/# barn-deployment-host$/d") {
		t.Fatalf("hosts script does not remove its managed entries:\n%s", script)
	}
	if strings.Count(script, "# barn-deployment-host\\n") != 2 {
		t.Fatalf("hosts script does not write the deployment marker exactly once per host:\n%s", script)
	}
	if strings.Index(script, "10.10.10.10 meta") > strings.Index(script, "10.10.10.11 node-1") {
		t.Fatalf("hosts script is not sorted by node name:\n%s", script)
	}
}

func TestRenderCustomUserDoesNotClaimPigstyIdentity(t *testing.T) {
	t.Parallel()
	input := testInput()
	input.SSHUser = "operator"
	files, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	userData := string(files.UserData)
	if strings.Contains(userData, "uid: 88") || strings.Contains(userData, "primary_group: admin") || strings.Contains(userData, "groupadd --gid 88 admin") {
		t.Fatalf("custom user received Pigsty identity contract:\n%s", userData)
	}
	if !strings.Contains(userData, `user="operator"`) && !strings.Contains(userData, `user='operator'`) {
		t.Fatalf("custom identity checker is missing operator:\n%s", userData)
	}
}

func TestWriteUserDataForExternalSchemaValidation(t *testing.T) {
	output := os.Getenv("BARN_CLOUD_CONFIG_OUTPUT")
	if output == "" {
		t.Skip("set BARN_CLOUD_CONFIG_OUTPUT to write a test-only cloud-config fixture")
	}
	files, err := Render(testInput())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, files.UserData, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRenderRejectsUnsafeMounts(t *testing.T) {
	t.Parallel()
	for _, mount := range []string{"/data/../../etc", "/data/..", "/data//nested", "/proc/data", "/var/lib/barn/data"} {
		input := testInput()
		input.Disks[0].Mount = mount
		if _, err := Render(input); err == nil {
			t.Errorf("unsafe mount %q was accepted", mount)
		}
	}
}

func TestRenderShareContract(t *testing.T) {
	t.Parallel()
	input := testInput()
	input.Shares = []Share{
		{Tag: "barn-0123456789abcdef0123", Guest: "/src", Readonly: false},
		{Tag: "barn-fedcba9876543210fedc", Guest: "/reference", Readonly: true},
	}
	files, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	userData := string(files.UserData)
	for _, want := range []string{
		`path: "/usr/local/libexec/barn-init-shares"`,
		`# BEGIN BARN SHARES`,
		`# END BARN SHARES`,
		`barn-0123456789abcdef0123 /src 9p version=9p2000.L,trans=virtio,cache=none,msize=262144,access=client,nofail,nodev,nosuid,rw 0 0`,
		`barn-fedcba9876543210fedc /reference 9p version=9p2000.L,trans=virtio,cache=none,msize=262144,access=client,nofail,nodev,nosuid,ro 0 0`,
		`runuser -u "${share_user}" -- mktemp`,
		`has_mount_option "${mounted_options}" ro`,
		`has_mount_option "${mounted_options}" rw`,
		`refuse non-empty Barn share mountpoint`,
		`mv -f -- "${fstab_tmp}" "${fstab}"`,
		`printf '{"exit_status":%d,"stage":"%s","detail":"%s"}\n'`,
	} {
		if !strings.Contains(userData, want) {
			t.Errorf("share contract missing %q", want)
		}
	}
	if strings.Contains(userData, "chown -R") {
		t.Fatal("share contract recursively changes host-backed ownership")
	}
	script := renderShareScript(input.SSHUser, input.Shares)
	publishIndex := strings.Index(script, `mv -f -- "${fstab_tmp}" "${fstab}"`)
	mountIndex := strings.Index(script, `mount "${mountpoint}"`)
	if publishIndex < 0 || mountIndex <= publishIndex {
		t.Fatalf("share script mounts before atomically publishing fstab:\n%s", script)
	}
	if !strings.Contains(script, "# A previous boot may have downgraded this mount.") {
		t.Fatalf("share script does not idempotently verify an existing mount:\n%s", script)
	}
	for _, want := range []string{
		`if [[ "${mounted_type}" == 9p ]] && is_desired_share "${mounted_source}" "${old_guest}"; then`,
		`if [[ "${mounted_source}" != "${old_tag}" || "${mounted_type}" != 9p ]] || is_desired_share "${old_tag}" "${old_guest}"; then`,
		`umount "${old_guest}"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("share script does not safely distinguish expected and removed marker mounts; missing %q:\n%s", want, script)
		}
	}
	emptyScript := renderShareScript(input.SSHUser, nil)
	if emptyScript != "" {
		t.Fatalf("empty share script changed the no-share path:\n%s", emptyScript)
	}
}

func TestRenderRejectsUnsafeShareTagsAndMountOverlap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		shares []Share
	}{
		{name: "user selected tag", shares: []Share{{Tag: "source", Guest: "/src"}}},
		{name: "reserved ancestor", shares: []Share{{Tag: "barn-0123456789abcdef0123", Guest: "/var"}}},
		{name: "ssh ancestor", shares: []Share{{Tag: "barn-0123456789abcdef0123", Guest: "/home/dba"}}},
		{name: "ssh descendant", shares: []Share{{Tag: "barn-0123456789abcdef0123", Guest: "/home/dba/.ssh/cache"}}},
		{name: "disk nested", shares: []Share{{Tag: "barn-0123456789abcdef0123", Guest: "/data/src"}}},
		{name: "duplicate tag", shares: []Share{{Tag: "barn-0123456789abcdef0123", Guest: "/src"}, {Tag: "barn-0123456789abcdef0123", Guest: "/work"}}},
		{name: "duplicate guest", shares: []Share{{Tag: "barn-0123456789abcdef0123", Guest: "/src"}, {Tag: "barn-fedcba9876543210fedc", Guest: "/src"}}},
		{name: "nested guests", shares: []Share{{Tag: "barn-0123456789abcdef0123", Guest: "/src"}, {Tag: "barn-fedcba9876543210fedc", Guest: "/src/nested"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := testInput()
			input.Shares = test.shares
			if _, err := Render(input); err == nil {
				t.Fatal("unsafe share contract was accepted")
			}
		})
	}
	input := testInput()
	input.Disks[0].Mount = "/data/nested"
	input.Shares = []Share{{Tag: "barn-0123456789abcdef0123", Guest: "/data"}}
	if _, err := Render(input); err == nil {
		t.Fatal("share ancestor of a data disk mount was accepted")
	}
}

func TestRenderRejectsMismatchedControlKey(t *testing.T) {
	t.Parallel()
	input := testInput()
	input.Private = &PrivateNetwork{MAC: "02:aa:bb:cc:dd:ee", Address: "10.10.10.10", Prefix: 24, HostAddress: "10.10.10.1"}
	input.Hosts = []Host{{Name: "meta", Address: "10.10.10.10"}, {Name: "node-1", Address: "10.10.10.11"}}
	input.Control = true
	_, input.PrivateKey = testKeyPair(t)
	if _, err := Render(input); err == nil {
		t.Fatal("mismatched control private key was accepted")
	}
}

func TestDiskScriptPublishesFstabAfterSuccessfulDirectMount(t *testing.T) {
	t.Parallel()
	script := renderDiskScript([]Disk{{Serial: "abcde234567abcde2345", Mount: "/data", Filesystem: "auto"}})
	fstabIndex := strings.Index(script, `mv -f -- "${fstab_tmp}" /etc/fstab`)
	mountIndex := strings.LastIndex(script, `mount -t "${fstype}" -o defaults "${dev}" "${mountpoint}"`)
	if mountIndex < 0 || fstabIndex <= mountIndex {
		t.Fatalf("disk script publishes fstab before validating a direct mount:\n%s", script)
	}
}

func TestBuildISORoundTrip(t *testing.T) {
	t.Parallel()
	files, err := Render(testInput())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "seed.iso")
	if err := BuildISO(target, files); err != nil {
		t.Fatalf("BuildISO: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("seed mode = %o, want 600", info.Mode().Perm())
	}
	label, contents, err := readISO(target)
	if err != nil {
		t.Fatalf("ReadISO: %v", err)
	}
	if label != "CIDATA" {
		t.Fatalf("volume label = %q, want CIDATA", label)
	}
	for name, want := range files.entries() {
		if !bytes.Equal(contents[name], want) {
			t.Errorf("round-trip mismatch for %s", name)
		}
	}
}

func TestBuildISORejectsExistingTarget(t *testing.T) {
	t.Parallel()
	files, err := Render(testInput())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "seed.iso")
	if err := os.WriteFile(target, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := BuildISO(target, files); err == nil {
		t.Fatal("existing target unexpectedly overwritten")
	}
}

// Exercise the generated finalizer with local stand-ins for guest operations:
// optional failures must not skip independent stages or guest readiness.
func TestFinalizeDegradesOptionalStagesAndKeepsCoreFailures(t *testing.T) {
	for _, failure := range []string{"", "identity-contract", "init-disks", "hosts", "init-shares", "install-control-ssh", "private-contract"} {
		t.Run("failure-"+failure, func(t *testing.T) {
			dir := t.TempDir()
			trace := filepath.Join(dir, "trace")
			replace := strings.NewReplacer("/var/lib/barn", dir, "/usr/local/libexec", dir,
				"install -d -o root -g root -m 0755", "mkdir -p", "chown root:root", "true",
				"timeout --kill-after=5s \"${budget}\" ", "", "flock -n 9", "true")
			for _, name := range []string{"identity-contract", "hosts", "init-disks", "init-shares", "install-control-ssh", "private-contract", "network-check"} {
				script := "#!/bin/bash\nprintf '%s\\n' " + name + " >> '" + trace + "'\n"
				if name == failure && name == "identity-contract" {
					// A tab and an escape sequence in the last stderr line must
					// not break the JSON marker that carries it to the host.
					script += "printf 'mkfs:\\tfixture \\033[1m\"failed\"\\n' >&2\nexit 23\n"
				} else if name == failure || name == "network-check" {
					script += "echo 'fixture \"unavailable\"' >&2\nexit 23\n"
				}
				if err := os.WriteFile(filepath.Join(dir, "barn-"+name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ready, err := renderReadyScript(testInput())
			if err != nil {
				t.Fatal(err)
			}
			for name, script := range map[string]string{"warning": renderWarningScript(), "ready": ready} {
				if err := os.WriteFile(filepath.Join(dir, "barn-"+name), []byte(replace.Replace(script)), 0700); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("bash")
			command.Stdin = strings.NewReader(replace.Replace(renderFinalizeScript(true, true, true)))
			output, err := command.CombinedOutput()
			coreFailure := failure == "identity-contract"
			if (err != nil) != coreFailure {
				t.Fatalf("finalizer err=%v output=%s", err, output)
			}
			data, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "network-check") {
				t.Fatal("Internet blocks bootstrap")
			}
			marker, readyErr := os.ReadFile(filepath.Join(dir, "ready.json"))
			if coreFailure {
				if !os.IsNotExist(readyErr) {
					t.Fatalf("unsafe ready marker: %s, %v", marker, readyErr)
				}
				marker, err = os.ReadFile(filepath.Join(dir, "error.json"))
				var failed struct{ Stage, Detail string }
				if err != nil || json.Unmarshal(marker, &failed) != nil {
					t.Fatalf("lost failure: %s %v", marker, err)
				}
				if failed.Stage != "identity" || failed.Detail != `mkfs: fixture  [1m"failed"` {
					t.Fatalf("failure marker = %+v", failed)
				}
				return
			}
			var report struct {
				Warnings []struct{ Stage, Detail string }
			}
			if readyErr != nil || json.Unmarshal(marker, &report) != nil {
				t.Fatalf("invalid ready report %s %v", marker, readyErr)
			}
			if (len(report.Warnings) > 0) != (failure != "") {
				t.Fatalf("warnings=%+v", report)
			}
			if failure != "" && report.Warnings[0].Detail != "fixture \"unavailable\"" {
				t.Fatalf("lost warning detail: %+v", report)
			}
			want := "identity-contract\nhosts\ninit-disks\ninit-shares\ninstall-control-ssh\nprivate-contract\n"
			if string(data) != want {
				t.Fatalf("skipped usable features: %s", data)
			}
		})
	}
}

func TestFinalizeRetryKeepsUnselectedStagesAndWarnings(t *testing.T) {
	dir := t.TempDir()
	replace := strings.NewReplacer("/var/lib/barn", dir, "/usr/local/libexec", dir,
		"install -d -o root -g root -m 0755", "mkdir -p", "chown root:root", "true",
		"timeout --kill-after=5s \"${budget}\" ", "", "flock -n 9", "true")
	for _, stage := range []string{"identity-contract", "hosts", "init-disks", "init-shares", "install-control-ssh", "private-contract"} {
		script := "#!/bin/bash\nprintf '%s\\n' " + stage + " >> '" + filepath.Join(dir, "trace") + "'\n"
		if err := os.WriteFile(filepath.Join(dir, "barn-"+stage), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ready, err := renderReadyScript(testInput())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "barn-ready"), []byte(replace.Replace(ready)), 0700); err != nil {
		t.Fatal(err)
	}
	warnings := `{"stage":"shares","detail":"retry me"}
{"stage":"data-disks","detail":"keep me"}
{"stage":"disk-reset","detail":"old notice"}
`
	if err := os.WriteFile(filepath.Join(dir, "warnings.jsonl"), []byte(warnings), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-s", "--", "shares", "ready")
	cmd.Stdin = strings.NewReader(replace.Replace(renderFinalizeScript(true, true, true)))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("retry failed: %v %s", err, output)
	}
	trace, err := os.ReadFile(filepath.Join(dir, "trace"))
	if err != nil || string(trace) != "identity-contract\ninit-shares\n" {
		t.Fatalf("reran unrelated stages: %s %v", trace, err)
	}
	marker, err := os.ReadFile(filepath.Join(dir, "ready.json"))
	var report struct {
		Warnings []struct{ Stage, Detail string }
	}
	if err != nil || json.Unmarshal(marker, &report) != nil || len(report.Warnings) != 1 || report.Warnings[0].Detail != "keep me" {
		t.Fatalf("lost unrelated limitation or retained resolved warning: %s %v", marker, err)
	}
}
