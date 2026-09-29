package qemu

import (
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/platform"
)

func testConfig(t *testing.T, goos, arch string) Config {
	t.Helper()
	profile, err := platform.Resolve(goos, arch)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		Profile: profile, Binary: "/opt/qemu/bin/" + profile.QEMUBinary,
		Name: "meta", UUID: "018f4b8e-1234-7abc-9def-0123456789ab",
		CPUs: 2, Memory: 4 * 1024 * MiB,
		Root: Disk{Path: "/data/root.qcow2", Serial: "root0000000000000001"},
		Data: []Disk{{Path: "/data/data.qcow2", Serial: "abcde234567abcde2345"}},
		Seed: "/data/seed.iso", QMP: "/tmp/barn/p/m/qmp.sock",
		PIDFile: "/tmp/barn/p/m/qemu.pid", SerialLog: "/data/serial.log",
		MgmtMAC:  "02:11:22:33:44:55",
		Forwards: []Forward{{Bind: "127.0.0.1", Host: 2222, Guest: 22}, {Bind: "127.0.0.1", Host: 15432, Guest: 5432}},
		Detach:   true,
	}
	if profile.RequiresUEFI {
		config.Firmware = &Firmware{Code: "/opt/qemu/share/edk2-aarch64-code.fd", Vars: "/data/nvram.fd"}
	}
	return config
}

func TestBuildDarwinArm64UserInvocation(t *testing.T) {
	t.Parallel()
	invocation, err := Build(testConfig(t, "darwin", "arm64"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(invocation.Args, "\n")
	for _, want := range []string{
		"qemu-system-aarch64", "virt", "hvf", "-daemonize",
		`"driver":"qcow2"`, `"filename":"/data/root.qcow2"`,
		"virtio-blk-pci,drive=data0,serial=abcde234567abcde2345",
		"scsi-cd,drive=seed,bus=seed-scsi.0",
		"hostfwd=tcp:127.0.0.1:2222-:22",
		"hostfwd=tcp:127.0.0.1:15432-:5432",
	} {
		if !strings.Contains(invocation.Binary+"\n"+joined, want) {
			t.Errorf("invocation missing %q:\n%s\n%s", want, invocation.Binary, joined)
		}
	}
}

func TestBuildLinuxAMD64DoesNotAddFirmware(t *testing.T) {
	t.Parallel()
	invocation, err := Build(testConfig(t, "linux", "amd64"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(invocation.Args, "\n")
	if !strings.Contains(joined, "kvm") || strings.Contains(joined, "pflash") {
		t.Fatalf("unexpected linux invocation:\n%s", joined)
	}
	if invocation.InheritedFiles != nil {
		t.Fatalf("legacy invocation without inherited files must retain nil metadata: %#v", invocation.InheritedFiles)
	}
}

func TestBuildUsesSelectedTCGRuntimeVerbatim(t *testing.T) {
	t.Parallel()
	host, _ := platform.Resolve("darwin", "arm64")
	runtimeProfile, err := platform.ResolveRuntime(host, "amd64", true)
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig(t, "darwin", "amd64")
	config.Profile = runtimeProfile
	config.Binary = "/opt/homebrew/bin/qemu-system-x86_64"
	invocation, err := Build(config)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(invocation.Args, " ")
	for _, want := range []string{"-machine q35", "-accel tcg,thread=single", "-cpu max"} {
		if !strings.Contains(joined, want) {
			t.Errorf("TCG invocation missing %q: %s", want, joined)
		}
	}
}

func TestBuildRejectsIPv6ForwardBindBeforeArgvConstruction(t *testing.T) {
	t.Parallel()
	for _, bind := range []string{"::1", "2001:db8::1", "::ffff:127.0.0.1"} {
		config := testConfig(t, "linux", "amd64")
		config.Forwards[0].Bind = bind
		if _, err := Build(config); err == nil || !strings.Contains(err.Error(), "must be IPv4") {
			t.Errorf("IPv6 forward bind %q was not rejected clearly: %v", bind, err)
		}
	}
}

func TestBuildPrivateStreamAndFDBackends(t *testing.T) {
	t.Parallel()
	stream := testConfig(t, "darwin", "arm64")
	stream.Private = &PrivateNetwork{MAC: "02:aa:bb:cc:dd:ee", StreamSocket: "/private/var/run/barn-vmnet.sock", ReconnectMS: 1000}
	invocation, err := Build(stream)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(invocation.Args, "\n")
	if !strings.Contains(joined, "stream,id=private,server=off,addr.type=unix,addr.path=/private/var/run/barn-vmnet.sock,reconnect-ms=1000") || !strings.Contains(joined, "virtio-net-pci,netdev=private,mac=02:aa:bb:cc:dd:ee") {
		t.Fatalf("private stream missing:\n%s", joined)
	}

	fd := testConfig(t, "darwin", "arm64")
	fd.Private = &PrivateNetwork{MAC: "02:aa:bb:cc:dd:ef", FD: 3}
	invocation, err = Build(fd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(invocation.Args, "\n"), "socket,id=private,fd=3") || !invocation.UsesPrivateFD3() {
		t.Fatal("private FD fallback missing")
	}
}

func TestBuildLinuxPrivateBridgeBackend(t *testing.T) {
	t.Parallel()
	config := testConfig(t, "linux", "amd64")
	config.Private = &PrivateNetwork{MAC: "02:aa:bb:cc:dd:f0", Bridge: "barn0", BridgeHelper: "/usr/libexec/qemu-bridge-helper"}
	invocation, err := Build(config)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(invocation.Args, "\n")
	if !strings.Contains(joined, "bridge,id=private,br=barn0,helper=/usr/libexec/qemu-bridge-helper") {
		t.Fatalf("Linux private bridge missing:\n%s", joined)
	}
}

func TestBuildHostSharesUseAnchoredFDsAndColdPlugRootBus(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		goos, arch string
		firstFD    string
	}{
		{goos: "darwin", arch: "arm64", firstFD: "/dev/fd/3"},
		{goos: "linux", arch: "amd64", firstFD: "/proc/self/fd/3"},
	} {
		config := testConfig(t, test.goos, test.arch)
		config.Shares = []Share{{Tag: "barn-0123456789abcdefabcd", Readonly: true}, {Tag: "barn-fedcba9876543210fedc"}}
		invocation, err := Build(config)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(invocation.Args, "\n")
		for _, want := range []string{
			"local,id=share0,path=" + test.firstFD + ",security_model=mapped-xattr,multidevs=remap,fmode=0600,dmode=0700,readonly=on",
			"virtio-9p-pci,id=share0dev,bus=pcie.0,fsdev=share0,mount_tag=barn-0123456789abcdefabcd",
			"virtio-9p-pci,id=share1dev,bus=pcie.0,fsdev=share1,mount_tag=barn-fedcba9876543210fedc",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s/%s invocation missing %q:\n%s", test.goos, test.arch, want, joined)
			}
		}
		if strings.Contains(joined, "pcie-root-port") || len(invocation.ShareFiles()) != 2 || invocation.ShareFiles()[0].FD != 3 || invocation.ShareFiles()[1].FD != 4 {
			t.Fatalf("unsafe or inconsistent share topology: %#v\n%s", invocation.InheritedFiles, joined)
		}
	}
}

func TestBuildPrivateFDReservesFD3BeforeShares(t *testing.T) {
	t.Parallel()
	config := testConfig(t, "darwin", "arm64")
	config.Private = &PrivateNetwork{MAC: "02:aa:bb:cc:dd:ef", FD: 3}
	config.Shares = []Share{{Tag: "barn-0123456789abcdefabcd", Readonly: true}}
	invocation, err := Build(config)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(invocation.Args, "\n")
	if !strings.Contains(joined, "path=/dev/fd/4") || len(invocation.InheritedFiles) != 2 || invocation.InheritedFiles[0].Kind != "private-network" || invocation.InheritedFiles[1].FD != 4 {
		t.Fatalf("private/share inherited FD layout is wrong: %#v\n%s", invocation.InheritedFiles, joined)
	}
}

func TestBuildRejectsUnsafeOrAmbiguousInput(t *testing.T) {
	t.Parallel()
	tests := []func(*Config){
		func(c *Config) { c.Name = "../meta" },
		func(c *Config) { c.UUID = "not-a-uuid" },
		func(c *Config) { c.Memory = 511 * MiB },
		func(c *Config) { c.QMP = "/" + strings.Repeat("q", 110) },
		func(c *Config) { c.Data = append(c.Data, c.Data[0]) },
		func(c *Config) { c.Forwards = append(c.Forwards, c.Forwards[0]) },
		func(c *Config) { c.Firmware = nil },
		func(c *Config) { c.Private = &PrivateNetwork{MAC: "02:aa:bb:cc:dd:ee", FD: 4} },
		func(c *Config) {
			c.Private = &PrivateNetwork{MAC: "02:aa:bb:cc:dd:ee", StreamSocket: "/tmp/a", ReconnectMS: 0}
		},
		func(c *Config) { c.Private = &PrivateNetwork{MAC: "bad", FD: 3} },
		func(c *Config) {
			c.Private = &PrivateNetwork{MAC: "02:aa:bb:cc:dd:ee", Bridge: "br0", BridgeHelper: "/usr/libexec/qemu-bridge-helper"}
		},
	}
	for i, mutate := range tests {
		config := testConfig(t, "darwin", "arm64")
		mutate(&config)
		if _, err := Build(config); err == nil {
			t.Errorf("unsafe config %d unexpectedly accepted", i)
		}
	}
}
