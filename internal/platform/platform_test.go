package platform

import (
	"errors"
	"reflect"
	"testing"
)

func TestResolveSupportMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		os, arch, binary, accel string
		tier                    Tier
	}{
		{"darwin", "arm64", "qemu-system-aarch64", "hvf", Tier1},
		{"darwin", "amd64", "qemu-system-x86_64", "hvf", Tier2},
		{"linux", "amd64", "qemu-system-x86_64", "kvm", Tier1},
		{"linux", "arm64", "qemu-system-aarch64", "kvm", Tier2},
	}
	for _, tt := range tests {
		profile, err := Resolve(tt.os, tt.arch)
		if err != nil {
			t.Fatalf("Resolve(%s/%s): %v", tt.os, tt.arch, err)
		}
		if profile.QEMUBinary != tt.binary || profile.Accelerator != tt.accel || profile.Tier != tt.tier {
			t.Errorf("Resolve(%s/%s) = %#v", tt.os, tt.arch, profile)
		}
	}
	if _, err := Resolve("windows", "amd64"); err == nil {
		t.Fatal("windows host unexpectedly supported")
	}
}

func TestResolveRuntimeNativeAndEmulated(t *testing.T) {
	t.Parallel()
	host, _ := Resolve("darwin", "arm64")
	native, err := ResolveRuntime(host, "native", false)
	if err != nil || native != host {
		t.Fatalf("native runtime = %#v, %v", native, err)
	}
	sameArch, err := ResolveRuntime(host, "arm64", true)
	if err != nil || !sameArch.Emulated || sameArch.QEMUBinary != "qemu-system-aarch64" || sameArch.Machine != "virt" || sameArch.Accelerator != "tcg,thread=multi" || sameArch.CPU != "cortex-a72" {
		t.Fatalf("same-arch TCG runtime = %#v, %v", sameArch, err)
	}
	foreign, err := ResolveRuntime(host, "amd64", false)
	if err != nil || !foreign.Emulated || foreign.QEMUBinary != "qemu-system-x86_64" || foreign.Machine != "q35" || foreign.RequiresUEFI || foreign.Accelerator != "tcg,thread=single" {
		t.Fatalf("foreign runtime = %#v, %v", foreign, err)
	}
	linuxAMD, _ := Resolve("linux", "amd64")
	armOnAMD, err := ResolveRuntime(linuxAMD, "arm64", false)
	if err != nil || armOnAMD.Accelerator != "tcg,thread=multi" || armOnAMD.QEMUBinary != "qemu-system-aarch64" || armOnAMD.CPU != "cortex-a72" {
		t.Fatalf("arm64-on-amd64 runtime = %#v, %v", armOnAMD, err)
	}
	if _, err := ResolveRuntime(host, "s390x", true); err == nil {
		t.Fatal("unsupported guest architecture accepted")
	}
}

func TestGuestArchitectureAliasesSelectTheSameRuntime(t *testing.T) {
	t.Parallel()
	host, _ := Resolve("darwin", "arm64")
	for alias, canonical := range map[string]string{
		"amd64": "amd64", "x86_64": "amd64", "x64": "amd64", " X64 ": "amd64",
		"arm64": "arm64", "aarch64": "arm64", "a64": "arm64", " AARCH64 ": "arm64",
		"native": "arm64", "": "arm64",
	} {
		profile, err := ResolveRuntime(host, alias, false)
		if err != nil || profile.Arch != canonical || profile.Emulated != (canonical != host.Arch) {
			t.Errorf("runtime for %q: %+v, %v", alias, profile, err)
		}
	}
}

func TestParseQEMUVersion(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]Version{
		"QEMU emulator version 10.1.0":               {10, 1, 0},
		"QEMU emulator version 8.2.1 (Homebrew)":     {8, 2, 1},
		"qemu-img version 6.2.0 (Debian 1:6.2+dfsg)": {6, 2, 0},
	} {
		got, err := ParseQEMUVersion(input)
		if err != nil {
			t.Fatalf("ParseQEMUVersion(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("ParseQEMUVersion(%q) = %s, want %s", input, got, want)
		}
	}
	if _, err := ParseQEMUVersion("not qemu"); err == nil {
		t.Fatal("invalid banner unexpectedly parsed")
	}
}

func TestVersionAtLeast(t *testing.T) {
	t.Parallel()
	if !(Version{8, 2, 1}).AtLeast(Version{8, 2, 1}) {
		t.Fatal("equal version should satisfy floor")
	}
	if (Version{8, 2, 0}).AtLeast(Version{8, 2, 1}) {
		t.Fatal("older patch should not satisfy floor")
	}
}

func TestValidateQEMUVersionUsesProfileMinimum(t *testing.T) {
	t.Parallel()
	profile := Profile{MinimumQEMU: Version{Major: 8, Minor: 2, Patch: 1}}
	version, err := ValidateQEMUVersion(profile, "QEMU emulator version 8.2.1")
	if err != nil || version != profile.MinimumQEMU {
		t.Fatalf("minimum version = %s, %v", version, err)
	}
	below, err := ValidateQEMUVersion(profile, "QEMU emulator version 8.2.0")
	if err == nil || below != (Version{Major: 8, Minor: 2}) {
		t.Fatalf("version below the profile minimum = %s, %v", below, err)
	}
}

func TestFirmwareCandidatesCoverNativeUEFIArchitectures(t *testing.T) {
	t.Parallel()
	amd64, _ := Resolve("linux", "amd64")
	arm64, _ := Resolve("darwin", "arm64")
	amd64Candidates := FirmwareCandidates(amd64)
	if len(amd64Candidates) == 0 || len(FirmwareCandidates(arm64)) == 0 {
		t.Fatal("UEFI candidate matrix is incomplete")
	}
	wantRPMFirmware := Firmware{Code: "/usr/share/edk2/ovmf/OVMF_CODE.fd", Vars: "/usr/share/edk2/ovmf/OVMF_VARS.fd"}
	if !containsFirmware(amd64Candidates, wantRPMFirmware) {
		t.Fatalf("amd64 firmware candidates do not cover edk2-ovmf: %#v", amd64Candidates)
	}
	if firmware, err := FindFirmwareForBoot(amd64, "bios"); err != nil || firmware != (Firmware{}) {
		t.Fatalf("amd64 BIOS selection = %#v, %v", firmware, err)
	}
	if _, err := FindFirmwareForBoot(arm64, "bios"); err == nil {
		t.Fatal("required-UEFI arm64 profile accepted BIOS")
	}
}

func containsFirmware(candidates []Firmware, want Firmware) bool {
	for _, candidate := range candidates {
		if candidate == want {
			return true
		}
	}
	return false
}

func TestFindQEMUBinaryUsesOnlyControlledLinuxFallback(t *testing.T) {
	t.Parallel()
	notFound := errors.New("not found")
	tests := []struct {
		name    string
		profile Profile
		paths   map[string]string
		want    string
		calls   []string
		wantErr bool
	}{
		{
			name:    "path binary wins",
			profile: Profile{OS: "linux", Arch: "amd64", QEMUBinary: "qemu-system-x86_64"},
			paths:   map[string]string{"qemu-system-x86_64": "/usr/bin/qemu-system-x86_64"},
			want:    "/usr/bin/qemu-system-x86_64",
			calls:   []string{"qemu-system-x86_64"},
		},
		{
			name:    "rhel libexec fallback",
			profile: Profile{OS: "linux", Arch: "arm64", QEMUBinary: "qemu-system-aarch64"},
			paths:   map[string]string{RHELQEMUBinary: RHELQEMUBinary},
			want:    RHELQEMUBinary,
			calls:   []string{"qemu-system-aarch64", RHELQEMUBinary},
		},
		{
			name:    "darwin has no libexec fallback",
			profile: Profile{OS: "darwin", Arch: "arm64", QEMUBinary: "qemu-system-aarch64"},
			calls:   []string{"qemu-system-aarch64"},
			wantErr: true,
		},
		{
			name:    "linux stops after fixed fallback",
			profile: Profile{OS: "linux", Arch: "amd64", QEMUBinary: "qemu-system-x86_64"},
			calls:   []string{"qemu-system-x86_64", RHELQEMUBinary},
			wantErr: true,
		},
		{
			name:    "custom linux binary has no fallback",
			profile: Profile{OS: "linux", Arch: "amd64", QEMUBinary: "custom-qemu"},
			calls:   []string{"custom-qemu"},
			wantErr: true,
		},
		{
			name:    "emulated linux runtime has no native libexec fallback",
			profile: Profile{OS: "linux", Arch: "arm64", QEMUBinary: "qemu-system-aarch64", Emulated: true},
			paths:   map[string]string{RHELQEMUBinary: RHELQEMUBinary},
			calls:   []string{"qemu-system-aarch64"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			got, err := FindQEMUBinary(tt.profile, func(name string) (string, error) {
				calls = append(calls, name)
				if path, ok := tt.paths[name]; ok {
					return path, nil
				}
				return "", notFound
			})
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("FindQEMUBinary() = %q, %v; want %q, error=%t", got, err, tt.want, tt.wantErr)
			}
			if !reflect.DeepEqual(calls, tt.calls) {
				t.Fatalf("lookup calls = %#v, want %#v", calls, tt.calls)
			}
		})
	}
}
