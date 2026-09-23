package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/execx"
	darwinnet "github.com/pgsty/farrow/internal/network/darwin"
	"github.com/pgsty/farrow/internal/network/subnet"
)

type darwinFileInfo struct {
	mode     os.FileMode
	uid, gid uint32
}

func (f darwinFileInfo) Name() string       { return "fixture" }
func (f darwinFileInfo) Size() int64        { return 0 }
func (f darwinFileInfo) Mode() os.FileMode  { return f.mode }
func (f darwinFileInfo) ModTime() time.Time { return time.Time{} }
func (f darwinFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f darwinFileInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid, Gid: f.gid} }

type darwinNetworkRunner struct {
	args         []byte
	foreignRoute bool
	sharingCalls int
}

func (r *darwinNetworkRunner) Run(ctx context.Context, binary string, args ...string) (execx.Result, error) {
	command := binary + " " + strings.Join(args, " ")
	switch command {
	case "/usr/bin/plutil -extract ProgramArguments json -o - " + darwinnet.PlistPath:
		return execx.Result{Stdout: r.args}, nil
	case "/sbin/ifconfig ":
		return execx.Result{Stdout: []byte("bridge100: flags=1\n\tinet 10.10.10.1 netmask 0xffffff00 broadcast 10.10.10.255\n")}, nil
	case "/usr/sbin/netstat -rn -f inet":
		routes := "10.10.10/24 link#26 UC bridge100 !\n"
		if r.foreignRoute {
			routes += "10.10.10.128/25 10.0.0.1 UGSc utun9\n"
		}
		return execx.Result{Stdout: []byte(routes)}, nil
	default:
		r.sharingCalls++
		return (unreadableSharingRunner{}).Run(ctx, binary, args...)
	}
}

func installedDarwinProbe(t *testing.T, logInfo os.FileInfo) (Probe, *darwinNetworkRunner) {
	t.Helper()
	daemon, client := []byte("fixture daemon"), []byte("fixture client")
	plan, err := darwinnet.NewInstallPlanModeNetwork("arm64", "018f4b8e-1234-7abc-9def-0123456789ab", "host", subnet.DefaultCIDR)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithRecordedBinaries(darwinnet.SourceHomebrew, digestBytes(daemon), digestBytes(client))
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithBSDInterface("bridge100")
	if err != nil {
		t.Fatal(err)
	}
	plist, err := plan.Plist()
	if err != nil {
		t.Fatal(err)
	}
	marker, err := plan.InterfaceJSON()
	if err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(plan.Args)
	if err != nil {
		t.Fatal(err)
	}
	runner := &darwinNetworkRunner{args: args}
	files := map[string][]byte{
		darwinnet.DaemonPath: daemon, darwinnet.ClientPath: client,
		darwinnet.PlistPath: plist, darwinnet.InterfaceMarkerPath: marker,
	}
	return Probe{
		Runner: runner,
		Lstat: func(path string) (os.FileInfo, error) {
			switch path {
			case darwinnet.LogDir:
				if logInfo == nil {
					return nil, os.ErrNotExist
				}
				return logInfo, nil
			case darwinnet.DaemonPath, darwinnet.ClientPath:
				return darwinFileInfo{mode: 0o755}, nil
			case darwinnet.PlistPath, darwinnet.InterfaceMarkerPath:
				return darwinFileInfo{mode: 0o644}, nil
			case darwinnet.StateDir:
				return darwinFileInfo{mode: os.ModeDir | 0o700}, nil
			case darwinnet.InterfaceMarkerDir:
				return darwinFileInfo{mode: os.ModeDir | 0o755}, nil
			case darwinnet.StatePath, darwinnet.InterfaceStatePath:
				return nil, os.ErrPermission
			case darwinnet.SocketPath:
				return darwinFileInfo{mode: os.ModeSocket | 0o770, gid: 20}, nil
			default:
				return nil, fmt.Errorf("unexpected lstat: %s", path)
			}
		},
		ReadFile: func(path string) ([]byte, error) {
			if data, ok := files[path]; ok {
				return data, nil
			}
			return nil, fmt.Errorf("unexpected read: %s", path)
		},
		Dial: func(string, string, time.Duration) (net.Conn, error) {
			local, peer := net.Pipe()
			_ = peer.Close()
			return local, nil
		},
	}, runner
}

func TestDarwinLogPermissionsPreserveNetworkOwnership(t *testing.T) {
	request := Request{OS: "darwin", Arch: "arm64", Purpose: Use, Layout: subnet.Default()}
	for _, test := range []struct {
		name                string
		info                os.FileInfo
		code, evidence, fix string
		repairable          bool
	}{
		{name: "launchd 0744", info: darwinFileInfo{mode: os.ModeDir | 0o744}, code: "installation.log_directory", evidence: "mode 0744; expected 0755", fix: "sudo chmod 0755 " + darwinnet.LogDir, repairable: true},
		{name: "missing log directory", code: "installation.log_directory", evidence: "is missing", fix: "sudo install -d", repairable: true},
		{name: "already correct", info: darwinFileInfo{mode: os.ModeDir | 0o755}},
		{name: "unsafe symlink", info: darwinFileInfo{mode: os.ModeSymlink | 0o755}, code: "installation.log_directory_unsafe", evidence: "will not repair it automatically", fix: "ls -ld"},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe, runner := installedDarwinProbe(t, test.info)
			report := Run(context.Background(), request, probe)
			if report.Installation.Status != "protected" || report.Installation.Interface != "bridge100" || !report.Installation.Healthy {
				t.Fatalf("lost intact network ownership: %+v", report)
			}
			if report.Ready != (test.code == "") || report.CanRepair() != test.repairable || runner.sharingCalls != 0 {
				t.Fatalf("report=%+v sharing probes=%d", report, runner.sharingCalls)
			}
			if test.code == "" {
				if len(report.Findings) != 0 {
					t.Fatalf("unexpected findings: %+v", report.Findings)
				}
				return
			}
			if len(report.Findings) != 1 {
				t.Fatalf("cascading findings: %+v", report.Findings)
			}
			finding := report.Findings[0]
			if finding.Code != test.code || !strings.Contains(finding.Evidence, test.evidence) || !strings.Contains(finding.Fix, test.fix) {
				t.Fatalf("finding=%+v", finding)
			}
		})
	}
	probe, runner := installedDarwinProbe(t, darwinFileInfo{mode: os.ModeDir | 0o744})
	runner.foreignRoute = true
	if report := Run(context.Background(), request, probe); report.CanRepair() || len(report.Findings) != 2 || report.ExitCode != 6 {
		t.Fatalf("real foreign route did not block automatic repair: %+v", report)
	}
}

func TestDarwinSharingPermissionErrorHasRelevantResolution(t *testing.T) {
	probe := Probe{
		Runner: &darwinNetworkRunner{},
		Lstat:  func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	}
	report := Run(context.Background(), Request{OS: "darwin", Arch: "arm64", Purpose: Inspect, Layout: subnet.Default()}, probe)
	for _, finding := range report.Findings {
		if finding.Code == "vmnet.sharing_probe" {
			if !strings.Contains(finding.Evidence, "permission denied") || !strings.Contains(finding.Fix, "sudo plutil -p") || report.Ready || report.CanRepair() {
				t.Fatalf("finding=%+v report=%+v", finding, report)
			}
			return
		}
	}
	t.Fatalf("missing vmnet permission finding: %+v", report)
}

func TestDarwinOrphanLogDirectoryStillBlocksFreshInstall(t *testing.T) {
	probe := Probe{
		Runner: &darwinNetworkRunner{},
		Lstat: func(path string) (os.FileInfo, error) {
			if path == darwinnet.LogDir {
				return darwinFileInfo{mode: os.ModeDir | 0o755}, nil
			}
			return nil, os.ErrNotExist
		},
	}
	report := Run(context.Background(), Request{OS: "darwin", Arch: "arm64", Purpose: Install, Layout: subnet.Default()}, probe)
	if report.Installation.Status != "partial" || report.Ready || report.CanRepair() {
		t.Fatalf("orphan log directory was treated as an absent installation: %+v", report)
	}
}
