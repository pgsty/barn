// Package setup plans the bounded host preparation performed by `barn setup`.
// It deliberately keeps package-manager selection and argv construction out of
// the CLI presentation layer so the plan can be tested without changing a host.
package setup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/pgsty/barn/internal/platform"
)

type Command struct {
	Name   string   `json:"name"`
	Binary string   `json:"binary"`
	Args   []string `json:"args"`
	Root   bool     `json:"root"`
}

type DependencyPlan struct {
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	Distribution string    `json:"distribution,omitempty"`
	Manager      string    `json:"manager,omitempty"`
	Private      bool      `json:"private"`
	Missing      []string  `json:"missing,omitempty"`
	Commands     []Command `json:"commands,omitempty"`
	Ready        bool      `json:"ready"`
	Unsupported  bool      `json:"unsupported,omitempty"`
	Resolution   string    `json:"resolution,omitempty"`
}

type DependencyProbe struct {
	GOOS        string
	GOARCH      string
	LookPath    func(string) (string, error)
	ReadFile    func(string) ([]byte, error)
	RegularFile func(string) bool
	RootCommand func(string) (string, error)
}

func (p DependencyProbe) values() (string, string) {
	goos, goarch := p.GOOS, p.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

func (p DependencyProbe) lookPath(name string) (string, error) {
	if p.LookPath != nil {
		return p.LookPath(name)
	}
	return exec.LookPath(name)
}

func (p DependencyProbe) readFile(path string) ([]byte, error) {
	if p.ReadFile != nil {
		return p.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (p DependencyProbe) regularFile(path string) bool {
	if p.RegularFile != nil {
		return p.RegularFile(path)
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func secureRootCommand(name string) (string, error) {
	if name != "apt-get" && name != "dnf" {
		return "", fmt.Errorf("unsupported root package manager %q", name)
	}
	candidate := filepath.Join("/usr/bin", name)
	canonical, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	if !rootCommandTargetAllowed(name, canonical) {
		return "", fmt.Errorf("root package manager resolves outside its fixed system path: %s", canonical)
	}
	for _, path := range []string{filepath.Dir(canonical), canonical} {
		info, statErr := os.Stat(path)
		statistics, ok := infoSyscallStat(info)
		if statErr != nil || !ok || statistics.Uid != 0 || statistics.Gid != 0 || info.Mode().Perm()&0o022 != 0 {
			return "", fmt.Errorf("root package manager path is not root-owned and non-writable: %s", path)
		}
		if path == canonical && (!info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0) {
			return "", fmt.Errorf("root package manager is not an executable regular file: %s", path)
		}
	}
	return canonical, nil
}

func rootCommandTargetAllowed(name, canonical string) bool {
	if !filepath.IsAbs(canonical) || filepath.Clean(canonical) != canonical {
		return false
	}
	if canonical == "/usr/bin/"+name || strings.HasPrefix(canonical, "/usr/bin/"+name+"-") {
		return true
	}
	// Fedora 41 switched /usr/bin/dnf to the separately named dnf5 binary.
	// Keep this privileged executable allowlist exact.
	return name == "dnf" && canonical == "/usr/bin/dnf5"
}

func infoSyscallStat(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	statistics, ok := info.Sys().(*syscall.Stat_t)
	return statistics, ok
}

func (p DependencyProbe) rootCommand(name string) (string, error) {
	if p.RootCommand != nil {
		return p.RootCommand(name)
	}
	return secureRootCommand(name)
}

func parseOSRelease(data []byte) map[string]string {
	values := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), "\"")
		values[strings.TrimSpace(key)] = value
	}
	return values
}

func linuxFamily(values map[string]string) string {
	tokens := strings.Fields(strings.ToLower(values["ID"] + " " + values["ID_LIKE"]))
	for _, token := range tokens {
		switch token {
		case "debian", "ubuntu":
			return "debian"
		case "fedora":
			return "fedora"
		case "rhel", "rocky", "almalinux", "centos", "ol":
			return "rhel"
		}
	}
	return ""
}

func (p DependencyProbe) commandExists(name string) bool {
	_, err := p.lookPath(name)
	return err == nil
}

func (p DependencyProbe) qemuExists(profile platform.Profile) bool {
	_, err := platform.FindQEMUBinary(profile, p.lookPath)
	return err == nil
}

func (p DependencyProbe) firmwareExists(profile platform.Profile) bool {
	for _, candidate := range platform.FirmwareCandidates(profile) {
		if p.regularFile(candidate.Code) && p.regularFile(candidate.Vars) {
			return true
		}
	}
	return false
}

func (p DependencyProbe) missing(profile platform.Profile, private bool) []string {
	missing := make([]string, 0, 8)
	if !p.qemuExists(profile) {
		missing = append(missing, profile.QEMUBinary)
	}
	for _, name := range []string{"qemu-img", "ssh", "ssh-keygen"} {
		if !p.commandExists(name) {
			missing = append(missing, name)
		}
	}
	// Every embedded Barn image is UEFI, including linux/amd64 where the
	// accelerator profile itself does not require UEFI.
	if !p.firmwareExists(profile) {
		missing = append(missing, "uefi-firmware")
	}
	if private && profile.OS == "linux" {
		if !p.commandExists("ip") {
			missing = append(missing, "ip")
		}
		// The bridge follows the active host-network owner. nmcli present means
		// NetworkManager can own it; only otherwise is systemd-networkd needed.
		networkOwner := p.commandExists("nmcli") || p.regularFile("/usr/lib/systemd/systemd-networkd") || p.regularFile("/lib/systemd/systemd-networkd")
		if !p.commandExists("systemctl") || !networkOwner {
			missing = append(missing, "systemd-networkd")
		}
		bridgeHelper := p.regularFile("/usr/lib/qemu/qemu-bridge-helper") || p.regularFile("/usr/libexec/qemu-bridge-helper")
		if !bridgeHelper {
			missing = append(missing, "qemu-bridge-helper")
		}
	}
	sort.Strings(missing)
	return missing
}

func debianPackages(arch string, private bool) ([]string, error) {
	var packages []string
	switch arch {
	case "amd64":
		packages = []string{"qemu-system-x86", "qemu-utils", "ovmf", "openssh-client", "iproute2"}
	case "arm64":
		packages = []string{"qemu-system-arm", "qemu-utils", "qemu-efi-aarch64", "openssh-client", "iproute2"}
	default:
		return nil, fmt.Errorf("unsupported Debian architecture %q", arch)
	}
	if private {
		packages = append(packages, "systemd")
	}
	return packages, nil
}

func rpmPackages(arch string, private bool) ([]string, error) {
	packages := []string{"qemu-kvm", "qemu-img", "openssh-clients", "iproute"}
	switch arch {
	case "amd64":
		packages = append(packages, "edk2-ovmf")
	case "arm64":
		packages = append(packages, "edk2-aarch64")
	default:
		return nil, fmt.Errorf("unsupported RPM architecture %q", arch)
	}
	if private {
		packages = append(packages, "systemd-networkd")
	}
	return packages, nil
}

// PlanDependencies is read-only. It returns exact, shell-free package-manager
// argv only when a required capability is absent.
func PlanDependencies(probe DependencyProbe, private bool) (DependencyPlan, error) {
	goos, goarch := probe.values()
	plan := DependencyPlan{OS: goos, Arch: goarch, Private: private}
	profile, err := platform.Resolve(goos, goarch)
	if err != nil {
		plan.Unsupported = true
		plan.Resolution = err.Error()
		return plan, nil
	}
	plan.Missing = probe.missing(profile, private)
	if len(plan.Missing) == 0 {
		plan.Ready = true
		return plan, nil
	}

	switch goos {
	case "darwin":
		brew, lookErr := probe.lookPath("brew")
		if lookErr != nil {
			plan.Unsupported = true
			plan.Resolution = "install Homebrew from https://brew.sh, then rerun barn setup"
			return plan, nil
		}
		plan.Manager = "homebrew"
		plan.Commands = []Command{{Name: "Install QEMU", Binary: brew, Args: []string{"install", "qemu"}}}
		return plan, nil
	case "linux":
		data, readErr := probe.readFile("/etc/os-release")
		if readErr != nil {
			return plan, fmt.Errorf("read /etc/os-release: %w", readErr)
		}
		values := parseOSRelease(data)
		plan.Distribution = values["ID"]
		family := linuxFamily(values)
		switch family {
		case "debian":
			apt, lookErr := probe.rootCommand("apt-get")
			if lookErr != nil {
				return plan, fmt.Errorf("validate Debian-family apt-get: %w", lookErr)
			}
			packages, packageErr := debianPackages(goarch, private)
			if packageErr != nil {
				return plan, packageErr
			}
			plan.Manager = "apt"
			plan.Commands = []Command{
				{Name: "Refresh package metadata", Binary: apt, Args: []string{"update"}, Root: true},
				{Name: "Install host dependencies", Binary: apt, Args: append([]string{"install", "-y"}, packages...), Root: true},
			}
		case "fedora":
			dnf, lookErr := probe.rootCommand("dnf")
			if lookErr != nil {
				return plan, fmt.Errorf("validate Fedora dnf: %w", lookErr)
			}
			needsNetworkd := false
			for _, name := range plan.Missing {
				if name == "systemd-networkd" {
					needsNetworkd = true
				}
			}
			packages, packageErr := rpmPackages(goarch, needsNetworkd)
			if packageErr != nil {
				return plan, packageErr
			}
			plan.Manager = "dnf"
			plan.Commands = []Command{{Name: "Install host dependencies", Binary: dnf, Args: append([]string{"install", "-y"}, packages...), Root: true}}
		case "rhel":
			// The RHEL family is NetworkManager-owned; the nmcli backend serves
			// private networking, so no networkd package is pulled in.
			dnf, lookErr := probe.rootCommand("dnf")
			if lookErr != nil {
				return plan, fmt.Errorf("validate RPM-family dnf: %w", lookErr)
			}
			packages, packageErr := rpmPackages(goarch, false)
			if packageErr != nil {
				return plan, packageErr
			}
			plan.Manager = "dnf"
			plan.Commands = []Command{{Name: "Install host dependencies", Binary: dnf, Args: append([]string{"install", "-y"}, packages...), Root: true}}
		default:
			plan.Unsupported = true
			plan.Resolution = fmt.Sprintf("unsupported Linux distribution %q; install QEMU, UEFI firmware, OpenSSH, and iproute manually", values["ID"])
		}
		return plan, nil
	default:
		plan.Unsupported = true
		plan.Resolution = fmt.Sprintf("unsupported host operating system %q", goos)
		return plan, nil
	}
}

func (c Command) Validate() error {
	if c.Name == "" || !filepath.IsAbs(c.Binary) || filepath.Clean(c.Binary) != c.Binary {
		return errors.New("setup command must have a name and clean absolute binary path")
	}
	for _, argument := range c.Args {
		if strings.ContainsRune(argument, 0) {
			return errors.New("setup command argument contains NUL")
		}
	}
	return nil
}
