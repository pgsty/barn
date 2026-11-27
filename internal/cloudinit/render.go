// Package cloudinit renders the validated NoCloud seed payload and ISO.
package cloudinit

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pgsty/barn/internal/naming"
	"github.com/pgsty/barn/internal/spec"
	"golang.org/x/crypto/ssh"
)

var (
	//go:embed scripts/init-disks.sh
	initDisksScript string
	//go:embed scripts/init-shares.sh
	initSharesScript string
	//go:embed scripts/network-check.sh
	networkCheckScript string
	//go:embed scripts/private-contract.sh
	privateContractScript string
	//go:embed scripts/install-control-ssh.sh
	installControlSSHScript string
	//go:embed scripts/identity-contract.sh
	identityContractScript string
	//go:embed scripts/finalize.sh
	finalizeScript string
	//go:embed scripts/ready.sh
	readyScriptTemplate string
	//go:embed scripts/warning.sh
	warningScript string
)

type Host struct {
	Name    string
	Address string
}

type Disk struct {
	Fresh      bool
	Serial     string
	Mount      string
	Filesystem string
}

// Share is one QEMU virtio-9p export mounted inside the guest. Tag is a
// Barn-generated stable identity, never a user-selected mount option.
type Share struct {
	Tag      string
	Guest    string
	Readonly bool
}

type PrivateNetwork struct {
	MAC         string
	Address     string
	Prefix      int
	HostAddress string
}

type Input struct {
	Node       string
	Hostname   string
	Generation uint64
	SpecHash   string
	SSHUser    string
	PublicKey  string
	PrivateKey string
	Control    bool
	MgmtMAC    string
	Private    *PrivateNetwork
	Hosts      []Host
	Disks      []Disk
	Shares     []Share
}

// Files are the only three entries allowed at the seed root.
type Files struct {
	MetaData      []byte
	UserData      []byte
	NetworkConfig []byte
}

var (
	dnsNamePattern  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	serialPattern   = regexp.MustCompile(`^[a-z2-7]{20}$`)
	shareTagPattern = regexp.MustCompile(`^barn-[0-9a-f]{20}$`)
	hashPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	userPattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

func normalizeInput(input Input) (Input, error) {
	input.Disks = append([]Disk(nil), input.Disks...)
	for index := range input.Disks {
		cleaned, ok := spec.NormalizeGuestPath(input.Disks[index].Mount)
		if !ok {
			return Input{}, fmt.Errorf("unsafe data disk mount %q", input.Disks[index].Mount)
		}
		input.Disks[index].Mount = cleaned
	}
	input.Shares = append([]Share(nil), input.Shares...)
	for index := range input.Shares {
		cleaned, ok := spec.NormalizeGuestPath(input.Shares[index].Guest)
		if !ok {
			return Input{}, fmt.Errorf("unsafe guest share mount %q", input.Shares[index].Guest)
		}
		input.Shares[index].Guest = cleaned
	}
	return input, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func fstabField(value string) string {
	var escaped strings.Builder
	for _, character := range value {
		switch character {
		case '\\':
			escaped.WriteString(`\134`)
		case ' ':
			escaped.WriteString(`\040`)
		case '\t':
			escaped.WriteString(`\011`)
		default:
			escaped.WriteRune(character)
		}
	}
	return escaped.String()
}

func singleLine(label, value string) error {
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s must be a non-empty single line", label)
	}
	return nil
}

func validateInput(input Input) error {
	if !naming.ValidNodeName(input.Node) || !naming.ValidNodeName(input.Hostname) {
		return errors.New("node and hostname must be DNS labels")
	}
	if input.Generation == 0 {
		return errors.New("cloud-init generation must be positive")
	}
	if !hashPattern.MatchString(input.SpecHash) {
		return errors.New("spec hash must be 64 lowercase hexadecimal characters")
	}
	if !userPattern.MatchString(input.SSHUser) {
		return fmt.Errorf("invalid SSH user %q", input.SSHUser)
	}
	if err := singleLine("SSH public key", input.PublicKey); err != nil {
		return err
	}
	if !strings.HasPrefix(input.PublicKey, "ssh-ed25519 ") {
		return errors.New("cloud-init requires an Ed25519 SSH public key")
	}
	if _, err := net.ParseMAC(input.MgmtMAC); err != nil {
		return fmt.Errorf("invalid management MAC: %w", err)
	}
	if input.Private != nil {
		if _, err := net.ParseMAC(input.Private.MAC); err != nil {
			return fmt.Errorf("invalid private MAC: %w", err)
		}
		ip := net.ParseIP(input.Private.Address)
		hostIP := net.ParseIP(input.Private.HostAddress)
		if ip == nil || hostIP == nil || input.Private.Prefix < 1 || input.Private.Prefix > 32 {
			return errors.New("invalid private IPv4 address/prefix")
		}
		if ip.To4() == nil || hostIP.To4() == nil || ip.Equal(hostIP) {
			return errors.New("private network must be IPv4")
		}
		mask := net.CIDRMask(input.Private.Prefix, 32)
		if !ip.Mask(mask).Equal(hostIP.Mask(mask)) {
			return errors.New("private guest and host addresses are not in the same subnet")
		}
	}
	seenHost := make(map[string]struct{})
	for _, host := range input.Hosts {
		if !dnsNamePattern.MatchString(host.Name) || strings.Contains(host.Name, "..") || net.ParseIP(host.Address) == nil {
			return fmt.Errorf("invalid host mapping %q=%q", host.Name, host.Address)
		}
		if _, exists := seenHost[host.Name]; exists {
			return fmt.Errorf("duplicate host name %q", host.Name)
		}
		seenHost[host.Name] = struct{}{}
	}
	seenMount := make(map[string]struct{})
	seenSerial := make(map[string]struct{})
	for _, disk := range input.Disks {
		if !serialPattern.MatchString(disk.Serial) {
			return fmt.Errorf("invalid data disk serial %q", disk.Serial)
		}
		if !spec.SafeDiskMount(disk.Mount) {
			return fmt.Errorf("unsafe data disk mount %q", disk.Mount)
		}
		if !spec.ValidFilesystem(disk.Filesystem) {
			return fmt.Errorf("unsupported filesystem %q", disk.Filesystem)
		}
		if _, exists := seenMount[disk.Mount]; exists {
			return fmt.Errorf("duplicate mount %q", disk.Mount)
		}
		if _, exists := seenSerial[disk.Serial]; exists {
			return fmt.Errorf("duplicate disk serial %q", disk.Serial)
		}
		seenMount[disk.Mount] = struct{}{}
		seenSerial[disk.Serial] = struct{}{}
	}
	seenTags := make(map[string]struct{}, len(input.Shares))
	seenGuests := make(map[string]struct{}, len(input.Shares))
	for _, share := range input.Shares {
		if !shareTagPattern.MatchString(share.Tag) {
			return fmt.Errorf("invalid Barn share tag %q", share.Tag)
		}
		if !spec.SafeShareMount(share.Guest, input.SSHUser) {
			return fmt.Errorf("unsafe guest share mount %q", share.Guest)
		}
		if _, exists := seenTags[share.Tag]; exists {
			return fmt.Errorf("duplicate Barn share tag %q", share.Tag)
		}
		for existing := range seenGuests {
			if spec.MountPathsOverlap(existing, share.Guest) {
				return fmt.Errorf("overlapping guest share mounts %q and %q", existing, share.Guest)
			}
		}
		for diskMount := range seenMount {
			if spec.MountPathsOverlap(diskMount, share.Guest) {
				return fmt.Errorf("guest share mount %q overlaps data disk mount %q", share.Guest, diskMount)
			}
		}
		seenTags[share.Tag] = struct{}{}
		seenGuests[share.Guest] = struct{}{}
	}
	if input.PrivateKey != "" {
		if !input.Control {
			return errors.New("private key may only be injected into the control guest")
		}
		if strings.ContainsRune(input.PrivateKey, '\x00') {
			return errors.New("private key contains NUL")
		}
		parsedPrivate, err := ssh.ParseRawPrivateKey([]byte(input.PrivateKey))
		if err != nil {
			return fmt.Errorf("parse control private key: %w", err)
		}
		signer, err := ssh.NewSignerFromKey(parsedPrivate)
		if err != nil || signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
			return errors.New("control private key must be Ed25519")
		}
		parsedPublic, _, _, _, err := ssh.ParseAuthorizedKey([]byte(input.PublicKey))
		if err != nil || !bytes.Equal(parsedPublic.Marshal(), signer.PublicKey().Marshal()) {
			return errors.New("control private key does not match the deployment public key")
		}
	}
	return nil
}

func yamlQuote(value string) string { return strconv.Quote(value) }

func indent(content string, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

func renderDiskScript(disks []Disk) string {
	var calls strings.Builder
	for _, disk := range disks {
		fmt.Fprintf(&calls, `disk_mount=%s
disk_err=$(mktemp /var/lib/barn/disk.err.XXXXXX)
set +e
( set -euo pipefail; init_disk %s "${disk_mount}" %s %t virtio-%s %s ) 2>"${disk_err}"
disk_status=$?
set -e
if (( disk_status != 0 )); then
  if [[ "${check_only}" == true ]]; then rm -f -- "${disk_err}"; exit 1; fi
  detail=$(tail -n 1 "${disk_err}")
  /usr/local/libexec/barn-warning data-disks "${disk_mount}: ${detail:-disk setup unavailable}"
fi
cat "${disk_err}" >&2
rm -f -- "${disk_err}"
`, shellQuote(disk.Mount), disk.Serial, shellQuote(disk.Filesystem), disk.Fresh, disk.Serial, shellQuote(fstabField(disk.Mount)))
	}
	return strings.Replace(initDisksScript, "# __BARN_DISK_CALLS__\n", calls.String(), 1)
}

func renderShareScript(user string, shares []Share) string {
	if len(shares) == 0 {
		return ""
	}
	const (
		beginMarker = "# BEGIN BARN SHARES"
		endMarker   = "# END BARN SHARES"
		baseOptions = "version=9p2000.L,trans=virtio,cache=none,msize=262144,access=client,nofail,nodev,nosuid"
	)
	var config, fstab, calls strings.Builder
	fmt.Fprintf(&config, "share_user=%s\n", shellQuote(user))
	fmt.Fprintf(&config, "begin_marker=%s\n", shellQuote(beginMarker))
	fmt.Fprintf(&config, "end_marker=%s\n", shellQuote(endMarker))
	config.WriteString("is_desired_share() {\n  case \"$1|$2\" in\n")
	for _, share := range shares {
		fmt.Fprintf(&config, "    %s) return 0 ;;\n", shellQuote(share.Tag+"|"+share.Guest))
		options := baseOptions + ",rw"
		if share.Readonly {
			options = baseOptions + ",ro"
		}
		line := fmt.Sprintf("%s %s 9p %s 0 0", share.Tag, fstabField(share.Guest), options)
		fmt.Fprintf(&fstab, "printf '%%s\\n' %s >> \"${fstab_tmp}\"\n", shellQuote(line))
		// Run each mount in an independent strict subshell. Putting a shell
		// function in an if/|| condition would disable errexit inside it.
		fmt.Fprintf(&calls, `share_err=$(mktemp /var/lib/barn/share.err.XXXXXX)
set +e
( set -Eeuo pipefail; init_share %s %s %t ) 2>"${share_err}"
share_status=$?
set -e
if (( share_status != 0 )); then
  detail=$(tail -n 1 "${share_err}")
  /usr/local/libexec/barn-warning shares "${detail:-share mount unavailable}"
fi
cat "${share_err}" >&2
rm -f -- "${share_err}"
`, shellQuote(share.Tag), shellQuote(share.Guest), share.Readonly)
	}
	config.WriteString("    *) return 1 ;;\n  esac\n}\n\n")
	replacer := strings.NewReplacer(
		"# __BARN_SHARE_CONFIG__\n", config.String(),
		"# __BARN_SHARE_FSTAB__\n", fstab.String(),
		"# __BARN_SHARE_CALLS__\n", calls.String(),
	)
	return replacer.Replace(initSharesScript)
}

func RenderHostsScript(hosts []Host) string {
	sorted := append([]Host(nil), hosts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var out strings.Builder
	out.WriteString("#!/bin/bash\nset -euo pipefail\n")
	out.WriteString("sed -i.bak '/# barn-deployment-host$/d' /etc/hosts\n")
	for _, host := range sorted {
		fmt.Fprintf(&out, "printf '%%s %%s # barn-deployment-host\\n' %s %s >> /etc/hosts\n", host.Address, host.Name)
	}
	return out.String()
}

// RenderControlSSHConfig is shared by initial cloud-init and later topology updates.
func RenderControlSSHConfig(user string, hosts []Host) string {
	// Local lab nodes are routinely recreated at the same names and addresses.
	return "# BEGIN BARN\nHost " + strings.Join(hostNames(hosts), " ") + "\n  User " + user + "\n  IdentityFile ~/.ssh/id_ed25519\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  LogLevel ERROR\nHost *\n# END BARN\n"
}

func renderPrivateContractScript(network PrivateNetwork) string {
	vars := fmt.Sprintf("expected=%q\nexpected_host=%q\nexpected_mac=%q\n",
		fmt.Sprintf("%s/%d", network.Address, network.Prefix), network.HostAddress, strings.ToLower(network.MAC))
	return strings.Replace(privateContractScript, "# __BARN_PRIVATE_VARS__\n", vars, 1)
}

func renderControlSSHInstallScript(user string) string {
	return strings.Replace(installControlSSHScript, "# __BARN_CONTROL_SSH_VARS__\n", fmt.Sprintf("user=%q\n", user), 1)
}

func renderIdentityContractScript(user string) string {
	contract := "[[ \"${uid}\" =~ ^[0-9]+$ && \"${gid}\" =~ ^[0-9]+$ ]]\n[[ \"${home}\" == /home/* && \"${shell}\" == /bin/bash ]]\n"
	if user == "dba" {
		contract = `[[ "${uid}" == 88 && "${gid}" == 88 ]]
[[ "${home}" == /home/dba && "${shell}" == /bin/bash ]]
group=$(getent group admin)
IFS=: read -r group_name _ group_gid _ <<<"${group}"
[[ "${group_name}" == admin && "${group_gid}" == 88 ]]
[[ "$(stat -c '%u:%g' "${home}")" == 88:88 ]]
`
	}
	replacer := strings.NewReplacer(
		"# __BARN_IDENTITY_USER__\n", fmt.Sprintf("user=%q\n", user),
		"# __BARN_IDENTITY_CONTRACT__\n", contract,
	)
	return replacer.Replace(identityContractScript)
}

func renderFinalizeScript(controlSSH, privateNetwork, shares bool) string {
	var optional strings.Builder
	if shares {
		optional.WriteString("run_optional shares 30s /usr/local/libexec/barn-init-shares\n")
	}
	if controlSSH {
		optional.WriteString("run_optional control-ssh 30s /usr/local/libexec/barn-install-control-ssh\n")
	}
	if privateNetwork {
		optional.WriteString("run_optional private-network 30s /usr/local/libexec/barn-private-contract\n")
	}
	return strings.Replace(finalizeScript, "# __BARN_FINALIZE_OPTIONAL__\n", optional.String(), 1)
}

func renderReadyScript(input Input) (string, error) {
	ready := struct {
		Node         string `json:"node"`
		Generation   uint64 `json:"generation"`
		SpecHash     string `json:"spec_hash"`
		SetupVersion string `json:"setup_version"`
	}{input.Node, input.Generation, input.SpecHash, SetupVersion}
	data, err := json.Marshal(ready)
	if err != nil {
		return "", err
	}
	writeLine := fmt.Sprintf("printf '%%s,\"warnings\":[%%s]}\\n' %s \"${warnings}\" > /var/lib/barn/ready.json.tmp\n", yamlQuote(strings.TrimSuffix(string(data), "}")))
	return strings.Replace(readyScriptTemplate, "# __BARN_READY_WRITE__\n", writeLine, 1), nil
}

func renderMetaData(input Input) []byte {
	return []byte(fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", yamlQuote(fmt.Sprintf("barn-%s-g%d", input.Node, input.Generation)), yamlQuote(input.Hostname)))
}

func renderNetwork(input Input) []byte {
	var out strings.Builder
	out.WriteString("version: 2\nethernets:\n  management:\n    match:\n      macaddress: ")
	out.WriteString(yamlQuote(strings.ToLower(input.MgmtMAC)))
	out.WriteString("\n    dhcp4: true\n    dhcp6: false\n    dhcp4-overrides:\n      route-metric: 100\n")
	if input.Private != nil {
		out.WriteString("  private:\n    match:\n      macaddress: ")
		out.WriteString(yamlQuote(strings.ToLower(input.Private.MAC)))
		out.WriteString("\n    dhcp4: false\n    dhcp6: false\n    accept-ra: false\n    link-local: []\n    addresses:\n      - ")
		out.WriteString(yamlQuote(fmt.Sprintf("%s/%d", input.Private.Address, input.Private.Prefix)))
		out.WriteString("\n")
	}
	return []byte(out.String())
}

func renderUserData(input Input) ([]byte, error) {
	readyScript, err := renderReadyScript(input)
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	out.WriteString("#cloud-config\npreserve_hostname: false\nhostname: ")
	out.WriteString(yamlQuote(input.Hostname))
	if input.SSHUser == "dba" {
		out.WriteString("\nbootcmd:\n  - ['/bin/sh', '-c', 'if /usr/bin/getent group admin >/dev/null; then current=$(/usr/bin/getent group admin | /usr/bin/cut -d: -f3); if test \"${current}\" != 88; then ! /usr/bin/getent group 88 >/dev/null; /usr/sbin/groupmod --gid 88 admin; fi; else ! /usr/bin/getent group 88 >/dev/null; /usr/sbin/groupadd --gid 88 admin; fi']")
	}
	out.WriteString("\nssh_pwauth: false\ndisable_root: true\nssh_deletekeys: false\nusers:\n  - default\n  - name: ")
	out.WriteString(yamlQuote(input.SSHUser))
	if input.SSHUser == "dba" {
		out.WriteString("\n    uid: 88\n    primary_group: admin\n    create_groups: false")
	}
	out.WriteString("\n    shell: /bin/bash\n    lock_passwd: true\n    sudo: ALL=(ALL) NOPASSWD:ALL\n    ssh_authorized_keys:\n      - ")
	out.WriteString(yamlQuote(input.PublicKey))
	out.WriteString("\ngrowpart:\n  mode: auto\n  devices: ['/']\nresize_rootfs: true\nntp:\n  enabled: true\nwrite_files:\n")

	writeFile := func(path, owner, permissions, content string) {
		out.WriteString("  - path: ")
		out.WriteString(yamlQuote(path))
		out.WriteString("\n    owner: ")
		out.WriteString(yamlQuote(owner))
		out.WriteString("\n    permissions: ")
		out.WriteString(yamlQuote(permissions))
		out.WriteString("\n    content: |\n")
		out.WriteString(indent(content, 6))
	}
	writeFile("/usr/local/libexec/barn-init-disks", "root:root", "0755", renderDiskScript(input.Disks))
	writeFile("/usr/local/libexec/barn-warning", "root:root", "0755", warningScript)
	if len(input.Shares) != 0 {
		writeFile("/usr/local/libexec/barn-init-shares", "root:root", "0755", renderShareScript(input.SSHUser, input.Shares))
	}
	writeFile("/usr/local/libexec/barn-hosts", "root:root", "0755", RenderHostsScript(input.Hosts))
	writeFile("/usr/local/libexec/barn-network-check", "root:root", "0755", networkCheckScript)
	writeFile("/usr/local/libexec/barn-identity-contract", "root:root", "0755", renderIdentityContractScript(input.SSHUser))
	if input.Private != nil {
		writeFile("/usr/local/libexec/barn-private-contract", "root:root", "0755", renderPrivateContractScript(*input.Private))
	}
	writeFile("/usr/local/libexec/barn-ready", "root:root", "0755", readyScript)
	if input.PrivateKey != "" {
		// write_files runs before users-groups on supported cloud-init images.
		// Stage the secret as root, then install it only after the login user
		// exists. Referring to the future user as write_files owner makes the
		// entire module fail and can leave a deceptively usable guest.
		writeFile("/var/lib/barn/control-id_ed25519", "root:root", "0600", input.PrivateKey)
		sshConfig := RenderControlSSHConfig(input.SSHUser, input.Hosts)
		writeFile("/var/lib/barn/control-ssh-config", "root:root", "0600", sshConfig)
		writeFile("/usr/local/libexec/barn-install-control-ssh", "root:root", "0700", renderControlSSHInstallScript(input.SSHUser))
	}
	writeFile("/usr/local/libexec/barn-finalize", "root:root", "0755", renderFinalizeScript(input.PrivateKey != "", input.Private != nil, len(input.Shares) != 0))
	out.WriteString("runcmd:\n  - ['/usr/local/libexec/barn-finalize']\n")
	return []byte(out.String()), nil
}

func hostNames(hosts []Host) []string {
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.Name)
	}
	sort.Strings(names)
	return names
}

// Render validates all interpolated values before producing YAML/shell text.
func Render(input Input) (Files, error) {
	normalized, err := normalizeInput(input)
	if err != nil {
		return Files{}, err
	}
	input = normalized
	if err := validateInput(input); err != nil {
		return Files{}, err
	}
	userData, err := renderUserData(input)
	if err != nil {
		return Files{}, err
	}
	return Files{MetaData: renderMetaData(input), UserData: userData, NetworkConfig: renderNetwork(input)}, nil
}
