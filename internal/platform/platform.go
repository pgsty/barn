// Package platform resolves the one supported QEMU backend for a native host.
package platform

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"

	"github.com/pgsty/farrow/internal/failure"
)

// Tier describes release evidence, not whether a platform can compile.
type Tier int

const (
	Unsupported Tier = iota
	Tier2
	Tier1
)

func (t Tier) String() string {
	switch t {
	case Tier1:
		return "tier1"
	case Tier2:
		return "tier2"
	default:
		return "unsupported"
	}
}

// Profile is the native QEMU machine/accelerator contract for one host tuple.
type Profile struct {
	OS           string
	Arch         string
	Tier         Tier
	QEMUBinary   string
	Machine      string
	Accelerator  string
	CPU          string
	MinimumQEMU  Version
	RequiresUEFI bool
	Emulated     bool
}

var profiles = map[string]Profile{
	"darwin/arm64": {OS: "darwin", Arch: "arm64", Tier: Tier1, QEMUBinary: "qemu-system-aarch64", Machine: "virt", Accelerator: "hvf", CPU: "host", MinimumQEMU: Version{Major: 8, Minor: 2, Patch: 1}, RequiresUEFI: true},
	"darwin/amd64": {OS: "darwin", Arch: "amd64", Tier: Tier2, QEMUBinary: "qemu-system-x86_64", Machine: "q35", Accelerator: "hvf", CPU: "host", MinimumQEMU: Version{Major: 8, Minor: 2, Patch: 1}},
	"linux/amd64":  {OS: "linux", Arch: "amd64", Tier: Tier1, QEMUBinary: "qemu-system-x86_64", Machine: "q35", Accelerator: "kvm", CPU: "host", MinimumQEMU: Version{Major: 6, Minor: 2}},
	"linux/arm64":  {OS: "linux", Arch: "arm64", Tier: Tier2, QEMUBinary: "qemu-system-aarch64", Machine: "virt", Accelerator: "kvm", CPU: "host", MinimumQEMU: Version{Major: 6, Minor: 2}, RequiresUEFI: true},
}

type Firmware struct {
	Code string
	Vars string
}

// RHELQEMUBinary is the fixed system path used by RHEL-family qemu-kvm
// packages when they do not expose qemu-system-* on PATH. It is deliberately
// the only fallback outside PATH; callers must not search arbitrary libexec
// directories or accept a different architecture's emulator.
const RHELQEMUBinary = "/usr/libexec/qemu-kvm"

// FindQEMUBinary resolves the profile's native emulator. Linux additionally
// accepts the single, distribution-defined qemu-kvm path used by RHEL-family
// packages. The injected lookup keeps resolution testable while retaining the
// same executable checks as exec.LookPath in production.
func FindQEMUBinary(profile Profile, lookup func(string) (string, error)) (string, error) {
	if profile.QEMUBinary == "" {
		return "", errors.New("QEMU binary name is empty")
	}
	if lookup == nil {
		lookup = exec.LookPath
	}
	path, primaryErr := lookup(profile.QEMUBinary)
	if primaryErr == nil {
		return path, nil
	}
	if !allowsRHELQEMUFallback(profile) {
		return "", QEMUMissing(profile.QEMUBinary, primaryErr)
	}
	path, fallbackErr := lookup(RHELQEMUBinary)
	if fallbackErr == nil {
		return path, nil
	}
	return "", QEMUMissing(profile.QEMUBinary+" or "+RHELQEMUBinary, errors.Join(primaryErr, fallbackErr))
}

// QEMUMissing reports a QEMU program that is not installed, pointing at the
// one command that installs it.
func QEMUMissing(program string, cause error) error {
	err := fmt.Errorf("QEMU is not installed (%s not found in PATH)", program)
	if !errors.Is(cause, exec.ErrNotFound) {
		err = fmt.Errorf("QEMU is not usable (%s): %w", program, cause)
	}
	return failure.New(failure.Capability, err).Because("qemu_missing").Then("farrow setup")
}

func allowsRHELQEMUFallback(profile Profile) bool {
	if profile.OS != "linux" || profile.Emulated {
		return false
	}
	switch profile.Arch {
	case "amd64":
		return profile.QEMUBinary == "qemu-system-x86_64"
	case "arm64":
		return profile.QEMUBinary == "qemu-system-aarch64"
	default:
		return false
	}
}

// FirmwareCandidates are distribution/package locations to probe. The chosen
// path is evidence and must be persisted by the caller.
func FirmwareCandidates(profile Profile) []Firmware {
	if profile.Arch == "amd64" {
		return []Firmware{
			{Code: "/opt/homebrew/share/qemu/edk2-x86_64-code.fd", Vars: "/opt/homebrew/share/qemu/edk2-i386-vars.fd"},
			{Code: "/usr/local/share/qemu/edk2-x86_64-code.fd", Vars: "/usr/local/share/qemu/edk2-i386-vars.fd"},
			{Code: "/usr/share/OVMF/OVMF_CODE_4M.fd", Vars: "/usr/share/OVMF/OVMF_VARS_4M.fd"},
			{Code: "/usr/share/OVMF/OVMF_CODE.fd", Vars: "/usr/share/OVMF/OVMF_VARS.fd"},
			{Code: "/usr/share/edk2/ovmf/OVMF_CODE.fd", Vars: "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
		}
	}
	if profile.Arch == "arm64" {
		return []Firmware{
			{Code: "/opt/homebrew/share/qemu/edk2-aarch64-code.fd", Vars: "/opt/homebrew/share/qemu/edk2-arm-vars.fd"},
			{Code: "/usr/local/share/qemu/edk2-aarch64-code.fd", Vars: "/usr/local/share/qemu/edk2-arm-vars.fd"},
			{Code: "/usr/share/AAVMF/AAVMF_CODE.fd", Vars: "/usr/share/AAVMF/AAVMF_VARS.fd"},
			{Code: "/usr/share/edk2/aarch64/QEMU_EFI-pflash.raw", Vars: "/usr/share/edk2/aarch64/vars-template-pflash.raw"},
		}
	}
	return nil
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func FindFirmwareForBoot(profile Profile, boot string) (Firmware, error) {
	if boot == "bios" {
		if profile.RequiresUEFI {
			return Firmware{}, fmt.Errorf("BIOS image is incompatible with required UEFI guest architecture %s", profile.Arch)
		}
		return Firmware{}, nil
	}
	if boot != "uefi" {
		return Firmware{}, fmt.Errorf("unsupported image boot mode %q", boot)
	}
	for _, candidate := range FirmwareCandidates(profile) {
		if regularFile(candidate.Code) && regularFile(candidate.Vars) {
			return candidate, nil
		}
	}
	return Firmware{}, fmt.Errorf("no matching UEFI code/vars pair found for %s/%s", profile.OS, profile.Arch)
}

// GuestArch resolves the deployment-wide guest architecture. An empty value or
// "native" follows the host; explicit values are stable desired state.
func GuestArch(requested, hostArch string) (string, error) {
	if requested == "" || requested == "native" {
		requested = hostArch
	}
	if requested != "amd64" && requested != "arm64" {
		return "", fmt.Errorf("unsupported guest architecture %q", requested)
	}
	return requested, nil
}

// ResolveRuntime selects the QEMU system binary and machine for one guest
// architecture on a supported host. Acceleration is policy, never a user
// supplied QEMU argument: native uses the host hypervisor, while compatibility
// mode uses multi-threaded TCG with QEMU's max CPU model.
func ResolveRuntime(host Profile, guestArch string, emulate bool) (Profile, error) {
	guestArch, err := GuestArch(guestArch, host.Arch)
	if err != nil {
		return Profile{}, err
	}
	if guestArch == host.Arch && !emulate {
		return host, nil
	}
	accelerator := "tcg,thread=multi"
	if host.Arch == "arm64" && guestArch == "amd64" {
		// A single translation thread preserves x86's stronger memory-ordering
		// contract on an Arm host. Compatibility takes precedence over throughput.
		accelerator = "tcg,thread=single"
	}
	runtimeProfile := Profile{
		OS: host.OS, Arch: guestArch, Tier: Tier2,
		Accelerator: accelerator, CPU: "max",
		MinimumQEMU: host.MinimumQEMU, RequiresUEFI: guestArch == "arm64", Emulated: true,
	}
	switch guestArch {
	case "amd64":
		runtimeProfile.QEMUBinary = "qemu-system-x86_64"
		runtimeProfile.Machine = "q35"
	case "arm64":
		runtimeProfile.QEMUBinary = "qemu-system-aarch64"
		runtimeProfile.Machine = "virt"
	}
	return runtimeProfile, nil
}

// Native resolves the current process host.
func Native() (Profile, error) { return Resolve(runtime.GOOS, runtime.GOARCH) }

// Resolve returns a supported native profile. It does not select TCG or a
// foreign guest architecture.
func Resolve(goos, goarch string) (Profile, error) {
	profile, ok := profiles[goos+"/"+goarch]
	if !ok {
		return Profile{}, fmt.Errorf("unsupported host %s/%s: Farrow v1 supports native darwin/linux arm64/amd64 only", goos, goarch)
	}
	return profile, nil
}

// Version is a comparable semantic prefix from QEMU's version output.
type Version struct {
	Major int
	Minor int
	Patch int
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

func (v Version) AtLeast(min Version) bool {
	if v.Major != min.Major {
		return v.Major > min.Major
	}
	if v.Minor != min.Minor {
		return v.Minor > min.Minor
	}
	return v.Patch >= min.Patch
}

var qemuVersionPattern = regexp.MustCompile(`(?i)(?:QEMU (?:emulator|disk image utility)|qemu-img) version\s+([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)

// ParseQEMUVersion parses both qemu-system and qemu-img version banners.
func ParseQEMUVersion(output string) (Version, error) {
	match := qemuVersionPattern.FindStringSubmatch(output)
	if len(match) == 0 {
		return Version{}, errors.New("QEMU version banner not found")
	}
	values := [3]int{}
	for i := 0; i < 3; i++ {
		if match[i+1] == "" {
			continue
		}
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			return Version{}, fmt.Errorf("parse QEMU version component %q: %w", match[i+1], err)
		}
		values[i] = value
	}
	return Version{Major: values[0], Minor: values[1], Patch: values[2]}, nil
}

// ValidateQEMUVersion parses a QEMU banner and enforces the selected host
// profile's supported minimum.
func ValidateQEMUVersion(profile Profile, output string) (Version, error) {
	version, err := ParseQEMUVersion(output)
	if err != nil {
		return Version{}, err
	}
	if !version.AtLeast(profile.MinimumQEMU) {
		return version, fmt.Errorf("unsupported QEMU version %s; minimum is %s", version, profile.MinimumQEMU)
	}
	return version, nil
}
