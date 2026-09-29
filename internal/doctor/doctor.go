// Package doctor performs read-only host capability checks.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/execx"
	netpreflight "github.com/pgsty/barn/internal/network/preflight"
	"github.com/pgsty/barn/internal/platform"
)

type Status string

const (
	OK    Status = "ok"
	Warn  Status = "warn"
	Error Status = "error"
)

// ClassNetwork marks checks about host-global private-network readiness. A
// host that simply has not run `barn setup` yet is not broken, so these
// checks inform the report without failing the capability verdict.
const ClassNetwork = "network"

type Check struct {
	Name     string `json:"name"`
	Status   Status `json:"status"`
	Evidence string `json:"evidence"`
	Fix      string `json:"fix,omitempty"`
	Class    string `json:"class,omitempty"`
}

type Report struct {
	OS     string  `json:"os"`
	Arch   string  `json:"arch"`
	Tier   string  `json:"tier"`
	Checks []Check `json:"checks"`
}

// HasErrors reports capability errors only. Network-class findings are
// surfaced by NetworkReady and never turn the whole host verdict into exit 3.
func (r Report) HasErrors() bool {
	for _, check := range r.Checks {
		if check.Status == Error && check.Class != ClassNetwork {
			return true
		}
	}
	return false
}

// NetworkReady reports whether every network-class check passed.
func (r Report) NetworkReady() bool {
	for _, check := range r.Checks {
		if check.Class == ClassNetwork && check.Status == Error {
			return false
		}
	}
	return true
}

func markClass(checks []Check, class string) []Check {
	for index := range checks {
		checks[index].Class = class
	}
	return checks
}

type Probe struct {
	HostOnly         bool
	Runner           execx.Runner
	LookPath         func(string) (string, error)
	AcceleratorSmoke func(context.Context, string, platform.Profile) (string, error)
}

func (p Probe) acceleratorSmoke(ctx context.Context, qemuPath string, profile platform.Profile) (string, error) {
	if p.AcceleratorSmoke != nil {
		return p.AcceleratorSmoke(ctx, qemuPath, profile)
	}
	return acceleratorSmoke(ctx, qemuPath, profile)
}

func (p Probe) runner() execx.Runner {
	if p.Runner != nil {
		return p.Runner
	}
	return execx.OSRunner{Timeout: 10 * time.Second}
}

func (p Probe) lookPath(name string) (string, error) {
	if p.LookPath != nil {
		return p.LookPath(name)
	}
	return exec.LookPath(name)
}

func qemuInstallFix(goos string) string {
	if goos == "darwin" {
		return "run barn setup (or brew install qemu)"
	}
	return "run barn setup (or install the distribution QEMU and qemu-img packages)"
}

func combined(result execx.Result) string {
	return strings.TrimSpace(string(result.Stdout) + "\n" + string(result.Stderr))
}

func (p Probe) capability(ctx context.Context, binary, name string, args []string, needles ...string) Check {
	result, err := p.runner().Run(ctx, binary, args...)
	if err != nil {
		return Check{Name: name, Status: Error, Evidence: err.Error(), Fix: "inspect the exact QEMU binary and its help output"}
	}
	output := combined(result)
	missing := make([]string, 0)
	for _, needle := range needles {
		if !strings.Contains(output, needle) {
			missing = append(missing, needle)
		}
	}
	if len(missing) > 0 {
		return Check{Name: name, Status: Error, Evidence: "missing: " + strings.Join(missing, ", "), Fix: "use a QEMU build exposing all required native devices/options"}
	}
	return Check{Name: name, Status: OK, Evidence: "found " + strings.Join(needles, ", ")}
}

// Run performs no installation or host mutation.
func (p Probe) Run(ctx context.Context) Report {
	report := Report{OS: runtime.GOOS, Arch: runtime.GOARCH}
	profile, err := platform.Native()
	if err != nil {
		report.Tier = platform.Unsupported.String()
		report.Checks = append(report.Checks, Check{Name: "host", Status: Error, Evidence: err.Error()})
		return report
	}
	report.Tier = profile.Tier.String()
	report.Checks = append(report.Checks, Check{Name: "host", Status: OK, Evidence: fmt.Sprintf("%s/%s %s", profile.OS, profile.Arch, profile.Tier)})

	qemuPath, err := platform.FindQEMUBinary(profile, p.lookPath)
	if err != nil {
		report.Checks = append(report.Checks, Check{Name: "qemu", Status: Error, Evidence: profile.QEMUBinary + " not found", Fix: qemuInstallFix(profile.OS)})
	} else {
		report.Checks = append(report.Checks, Check{Name: "qemu-path", Status: OK, Evidence: qemuPath})
		qemuVersion := ""
		result, versionErr := p.runner().Run(ctx, qemuPath, "--version")
		if versionErr != nil {
			report.Checks = append(report.Checks, Check{Name: "qemu-version", Status: Error, Evidence: versionErr.Error(), Fix: qemuInstallFix(profile.OS)})
		} else if version, parseErr := platform.ParseQEMUVersion(combined(result)); parseErr != nil {
			report.Checks = append(report.Checks, Check{Name: "qemu-version", Status: Error, Evidence: parseErr.Error(), Fix: qemuInstallFix(profile.OS)})
		} else if !version.AtLeast(profile.MinimumQEMU) {
			report.Checks = append(report.Checks, Check{Name: "qemu-version", Status: Error, Evidence: version.String() + " is below " + profile.MinimumQEMU.String(), Fix: qemuInstallFix(profile.OS)})
		} else {
			qemuVersion = version.String()
			report.Checks = append(report.Checks, Check{Name: "qemu-version", Status: OK, Evidence: qemuVersion})
		}
		staticChecks := []Check{
			p.capability(ctx, qemuPath, "accelerator", []string{"-accel", "help"}, profile.Accelerator),
			p.capability(ctx, qemuPath, "machine", []string{"-machine", "help"}, profile.Machine),
			p.capability(ctx, qemuPath, "cpu", []string{"-cpu", "help"}, profile.CPU),
			p.capability(ctx, qemuPath, "devices", []string{"-device", "help"}, "virtio-blk-pci", "virtio-net-pci", "virtio-scsi-pci"),
			p.capability(ctx, qemuPath, "netdev", []string{"-machine", "none", "-netdev", "help"}, "user"),
		}
		report.Checks = append(report.Checks, staticChecks...)
		if evidence, smokeErr := p.acceleratorSmoke(ctx, qemuPath, profile); smokeErr != nil {
			report.Checks = append(report.Checks, Check{Name: profile.Accelerator + "-real-smoke", Status: Error, Evidence: smokeErr.Error(), Fix: "verify the native accelerator entitlement/device and retry doctor"})
		} else {
			report.Checks = append(report.Checks, Check{Name: profile.Accelerator + "-real-smoke", Status: OK, Evidence: evidence})
		}
	}

	qemuImgPath, err := p.lookPath("qemu-img")
	if err != nil {
		report.Checks = append(report.Checks, Check{Name: "qemu-img", Status: Error, Evidence: "qemu-img not found", Fix: qemuInstallFix(profile.OS)})
	} else {
		result, versionErr := p.runner().Run(ctx, qemuImgPath, "--version")
		if versionErr != nil {
			report.Checks = append(report.Checks, Check{Name: "qemu-img", Status: Error, Evidence: versionErr.Error(), Fix: qemuInstallFix(profile.OS)})
		} else {
			report.Checks = append(report.Checks, Check{Name: "qemu-img", Status: OK, Evidence: qemuImgPath + ": " + strings.Split(combined(result), "\n")[0]})
		}
	}

	// All embedded Barn images boot with UEFI. Checking only profiles whose
	// machine mandates UEFI would let amd64 doctor pass and the default image
	// fail later during up.
	firmware, firmwareErr := platform.FindFirmwareForBoot(profile, "uefi")
	if firmwareErr != nil {
		report.Checks = append(report.Checks, Check{Name: "firmware", Status: Error, Evidence: "no matching UEFI code/vars pair found", Fix: qemuInstallFix(profile.OS)})
	} else {
		report.Checks = append(report.Checks, Check{Name: "firmware", Status: OK, Evidence: firmware.Code + " + " + firmware.Vars})
	}

	if profile.OS == "linux" {
		file, openErr := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if openErr != nil {
			report.Checks = append(report.Checks, Check{Name: "kvm", Status: Error, Evidence: openErr.Error(), Fix: "enable KVM and grant the invoking user read/write access to /dev/kvm"})
		} else {
			_ = file.Close()
			report.Checks = append(report.Checks, Check{Name: "kvm", Status: OK, Evidence: "/dev/kvm is readable and writable"})
		}
	}

	if sshPath, sshErr := p.lookPath("ssh"); sshErr != nil {
		report.Checks = append(report.Checks, Check{Name: "ssh", Status: Error, Evidence: "OpenSSH client not found", Fix: "install an OpenSSH client"})
	} else {
		report.Checks = append(report.Checks, Check{Name: "ssh", Status: OK, Evidence: filepath.Clean(sshPath)})
	}
	if p.HostOnly {
		return report
	}
	report.Checks = append(report.Checks, p.deploymentChecks()...)
	if profile.OS == "darwin" {
		report.Checks = append(report.Checks, markClass(networkPreflightChecks(ctx, profile, netpreflight.Probe{Runner: p.runner()}), ClassNetwork)...)
	} else if profile.OS == "linux" {
		report.Checks = append(report.Checks, markClass(p.linuxPrivateChecks(ctx), ClassNetwork)...)
		report.Checks = append(report.Checks, markClass(networkPreflightChecks(ctx, profile, netpreflight.Probe{Runner: p.runner()}), ClassNetwork)...)
	}
	return report
}
