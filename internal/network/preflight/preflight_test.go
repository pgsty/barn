package preflight

import (
	"net/netip"
	"testing"

	"github.com/pgsty/barn/internal/network/subnet"
)

func mustPrefix(value string) netip.Prefix { return netip.MustParsePrefix(value) }

func TestEvaluateCleanAbsentAndCustomWarning(t *testing.T) {
	layout, _ := subnet.Parse("172.31.251.0/24")
	report := Evaluate(Request{OS: "darwin", Arch: "arm64", Purpose: Install, Layout: layout}, Snapshot{Installation: Installation{Status: "absent"}})
	if !report.Ready || report.ExitCode != 0 || len(report.Findings) != 1 || report.Findings[0].Code != "network.non_default" {
		t.Fatalf("report=%#v", report)
	}
}

func TestEvaluateRouteLongestPrefixPrecedence(t *testing.T) {
	layout := subnet.Default()
	for _, test := range []struct {
		name      string
		prefix    string
		wantReady bool
	}{
		{name: "less-specific VPN exclusion", prefix: "10.0.0.0/8", wantReady: true},
		{name: "exact foreign route", prefix: "10.10.10.0/24", wantReady: false},
		{name: "more-specific foreign subnet", prefix: "10.10.10.128/25", wantReady: false},
		{name: "foreign host route", prefix: "10.10.10.99/32", wantReady: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := Evaluate(Request{OS: "linux", Arch: "amd64", Purpose: Install, Layout: layout}, Snapshot{
				Installation: Installation{Status: "absent"},
				Routes:       []Route{{Prefix: mustPrefix(test.prefix), Interface: "tun0", Evidence: test.prefix + " dev tun0"}},
			})
			if report.Ready != test.wantReady {
				t.Fatalf("ready=%t, want %t; report=%#v", report.Ready, test.wantReady, report)
			}
			if !test.wantReady && report.ExitCode != 6 {
				t.Fatalf("exit=%d, want 6; report=%#v", report.ExitCode, report)
			}
		})
	}
}

func TestEvaluateBroadInterfaceOverlapStillConflicts(t *testing.T) {
	layout := subnet.Default()
	report := Evaluate(Request{OS: "linux", Arch: "amd64", Purpose: Install, Layout: layout}, Snapshot{
		Installation: Installation{Status: "absent"},
		Interfaces:   []InterfaceAddress{{Address: netip.MustParseAddr("10.0.0.2"), Prefix: mustPrefix("10.0.0.2/8"), Interface: "eth1", Evidence: "10.0.0.2/8"}},
	})
	if report.Ready || report.ExitCode != 6 {
		t.Fatalf("report=%#v", report)
	}
}

func TestEvaluateExactInstalledAndMismatch(t *testing.T) {
	layout := subnet.Default()
	exact := Snapshot{
		Installation: Installation{Status: "exact", CIDR: layout.CIDR(), HostAddress: layout.HostAddress(), Interface: "bridge100", Healthy: true},
		Routes: []Route{
			{Prefix: layout.Prefix(), Interface: "bridge100", Evidence: "owned"},
			{Prefix: mustPrefix("10.10.10.1/32"), Interface: "lo0", Evidence: "owned host local route"},
		},
		Interfaces: []InterfaceAddress{{Address: netip.MustParseAddr("10.10.10.1"), Prefix: mustPrefix("10.10.10.1/24"), Interface: "bridge100", Evidence: "owned"}},
	}
	if report := Evaluate(Request{OS: "darwin", Arch: "arm64", Purpose: Use, Layout: layout}, exact); !report.Ready {
		t.Fatalf("exact report=%#v", report)
	}
	protected := exact
	protected.Installation.Status = "protected"
	if report := Evaluate(Request{OS: "darwin", Arch: "arm64", Purpose: Use, Layout: layout}, protected); !report.Ready {
		t.Fatalf("protected report=%#v", report)
	}
	protected.Installation.Healthy = false
	protected.Installation.Problem = "runtime socket is absent"
	if report := Evaluate(Request{OS: "darwin", Arch: "arm64", Purpose: Use, Layout: layout}, protected); report.Ready || report.ExitCode != 3 {
		t.Fatalf("protected not-ready report=%#v", report)
	}
	custom, _ := subnet.Parse("172.31.251.0/24")
	if report := Evaluate(Request{OS: "darwin", Arch: "arm64", Purpose: Use, Layout: custom}, exact); report.ExitCode != 6 {
		// State mismatch is present, but the existing interface/route also
		// overlaps only its own default subnet, not the custom request. Exit 4.
		if report.ExitCode != 4 {
			t.Fatalf("mismatch report=%#v", report)
		}
	}
}

func TestEvaluateInstalledRequiresOnlyExactOwnedRouteAndAddress(t *testing.T) {
	layout := subnet.Default()
	installation := Installation{Status: "exact", CIDR: layout.CIDR(), HostAddress: layout.HostAddress(), Interface: "barn0", Healthy: true}
	missing := Evaluate(Request{OS: "linux", Arch: "amd64", Purpose: Use, Layout: layout}, Snapshot{
		Installation: installation,
		Routes:       []Route{{Prefix: mustPrefix("10.0.0.0/8"), Interface: "tun0", Evidence: "less-specific VPN route"}},
		Interfaces:   []InterfaceAddress{{Address: netip.MustParseAddr("10.10.10.1"), Prefix: mustPrefix("10.10.10.1/24"), Interface: "barn0", Evidence: "owned"}},
	})
	if missing.Ready || missing.ExitCode != 3 {
		t.Fatalf("missing exact route report=%#v", missing)
	}
	blackhole := Evaluate(Request{OS: "linux", Arch: "amd64", Purpose: Use, Layout: layout}, Snapshot{
		Installation: installation,
		Routes:       []Route{{Prefix: layout.Prefix(), Interface: "barn0", Kind: "blackhole", Evidence: "blackhole 10.10.10.0/24"}},
		Interfaces:   []InterfaceAddress{{Address: netip.MustParseAddr("10.10.10.1"), Prefix: mustPrefix("10.10.10.1/24"), Interface: "barn0", Evidence: "owned"}},
	})
	if blackhole.Ready || blackhole.ExitCode != 6 {
		t.Fatalf("blackhole exact route report=%#v", blackhole)
	}

	covering := Evaluate(Request{OS: "linux", Arch: "amd64", Purpose: Use, Layout: layout}, Snapshot{
		Installation: installation,
		Routes: []Route{
			{Prefix: layout.Prefix(), Interface: "barn0", Evidence: "owned exact route"},
			{Prefix: mustPrefix("10.0.0.0/8"), Interface: "en7", Evidence: "VPN exclusion through the physical gateway"},
		},
		Interfaces: []InterfaceAddress{{Address: netip.MustParseAddr("10.10.10.1"), Prefix: mustPrefix("10.10.10.1/24"), Interface: "barn0", Evidence: "owned exact address"}},
	})
	if !covering.Ready || covering.ExitCode != 0 {
		t.Fatalf("less-specific covering route report=%#v", covering)
	}

	conflict := Evaluate(Request{OS: "linux", Arch: "amd64", Purpose: Use, Layout: layout}, Snapshot{
		Installation: installation,
		Routes:       []Route{{Prefix: layout.Prefix(), Interface: "barn0", Evidence: "owned exact route"}},
		Interfaces: []InterfaceAddress{
			{Address: netip.MustParseAddr("10.10.10.1"), Prefix: mustPrefix("10.10.10.1/24"), Interface: "barn0", Evidence: "owned exact address"},
			{Address: netip.MustParseAddr("10.10.10.2"), Prefix: mustPrefix("10.10.10.2/24"), Interface: "barn0", Evidence: "unexpected extra address"},
		},
	})
	if conflict.Ready || conflict.ExitCode != 6 {
		t.Fatalf("unexpected extra interface address report=%#v", conflict)
	}
}

func TestEvaluateDarwinSharingBusyIsResourceConflict(t *testing.T) {
	report := Evaluate(Request{OS: "darwin", Arch: "arm64", Purpose: Install, Layout: subnet.Default()}, Snapshot{
		Installation: Installation{Status: "absent"},
		SharingBusy:  "com.apple.NetworkSharing is active for 10.10.10.0/24",
	})
	if report.Ready || report.ExitCode != 6 {
		t.Fatalf("sharing report=%#v", report)
	}
}

func TestEvaluateIntegrityWins(t *testing.T) {
	report := Evaluate(Request{OS: "darwin", Arch: "arm64", Purpose: Use, Layout: subnet.Default()}, Snapshot{
		Installation: Installation{Status: "partial", Problem: "plist without state"},
		Routes:       []Route{{Prefix: mustPrefix("10.10.10.0/24"), Interface: "utun9", Evidence: "conflict"}},
	})
	if report.ExitCode != 7 || report.Ready {
		t.Fatalf("report=%#v", report)
	}
}
