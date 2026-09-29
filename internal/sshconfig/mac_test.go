package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func macEntry(name, host string) MacEntry {
	return MacEntry{Name: name, User: "barn", Host: host, Identity: "/data/mac/slots/" + name + "/id_ed25519",
		KnownHosts: "/data/mac/slots/" + name + "/known_hosts", HostKeyAlias: "barn-mac-" + name}
}

func TestMacFragmentCoexistsWithTheLinuxFragment(t *testing.T) {
	home := t.TempDir()
	if _, err := InstallMany(home, []Entry{{Name: "barn", Node: "meta", User: "vagrant", Host: "127.0.0.1", Port: 2222, Identity: "/k/id", KnownHosts: "/k/known"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallMac(home, "/data/mac", []MacEntry{macEntry("mac1", "10.10.20.10"), macEntry("dev", "10.10.30.10")}, false); err != nil {
		t.Fatal(err)
	}
	config, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if strings.Count(string(config), "# barn:include") != 1 || strings.Count(string(config), "# barn-mac:include") != 1 {
		t.Fatalf("config:\n%s", config)
	}
	fragment, _ := os.ReadFile(filepath.Join(home, ".ssh", MacFragment+"_config"))
	for _, want := range []string{"# barn-mac:root /data/mac", "Host mac1 10.10.20.10", "HostKeyAlias barn-mac-dev", "StrictHostKeyChecking yes", "ForwardAgent no"} {
		if !strings.Contains(string(fragment), want) {
			t.Errorf("fragment lacks %q:\n%s", want, fragment)
		}
	}
	// Linux install and removal leave the Mac block alone, and vice versa.
	if _, err := InstallMany(home, []Entry{{Name: "barn", Node: "meta", User: "vagrant", Host: "127.0.0.1", Port: 2223, Identity: "/k/id", KnownHosts: "/k/known"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(home, "barn"); err != nil {
		t.Fatal(err)
	}
	config, _ = os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if strings.Contains(string(config), "# barn:include") || !strings.Contains(string(config), "# barn-mac:include") {
		t.Fatalf("config after Linux removal:\n%s", config)
	}
	if installed, err := MacInstalled(home, "/data/mac"); err != nil || !installed {
		t.Fatalf("installed=%v err=%v", installed, err)
	}
	if _, err := RemoveMac(home, "/data/mac", false); err != nil {
		t.Fatal(err)
	}
	config, _ = os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if strings.Contains(string(config), "barn") {
		t.Fatalf("config after Mac removal:\n%s", config)
	}
}

func TestMacFragmentBelongsToOneHome(t *testing.T) {
	home := t.TempDir()
	if _, err := InstallMac(home, "/data/mac", []MacEntry{macEntry("mac1", "10.10.20.10")}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallMac(home, "/other/mac", []MacEntry{macEntry("dev", "10.10.30.10")}, false); !errors.Is(err, ErrMacForeign) {
		t.Fatalf("another home replaced the fragment: %v", err)
	}
	if _, err := RemoveMac(home, "/other/mac", false); !errors.Is(err, ErrMacForeign) {
		t.Fatalf("another home removed the fragment: %v", err)
	}
	if _, err := InstallMac(home, "/other/mac", []MacEntry{macEntry("dev", "10.10.30.10")}, true); err != nil {
		t.Fatalf("explicit takeover: %v", err)
	}
	// No machines left removes the fragment.
	if _, err := InstallMac(home, "/other/mac", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", MacFragment+"_config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty install left the fragment: %v", err)
	}
}

func TestMacEntriesAreValidated(t *testing.T) {
	for _, entry := range []MacEntry{
		{Name: "bad name", User: "barn", Host: "10.10.20.10", Identity: "/k", KnownHosts: "/k", HostKeyAlias: "a"},
		{Name: "mac1", User: "barn", Host: "10.10.20.10\nProxyCommand x", Identity: "/k", KnownHosts: "/k", HostKeyAlias: "a"},
		{Name: "mac1", User: "barn", Host: "10.10.20.10", Identity: "relative", KnownHosts: "/k", HostKeyAlias: "a"},
	} {
		if _, err := RenderMac("/data/mac", []MacEntry{entry}); err == nil {
			t.Errorf("accepted %+v", entry)
		}
	}
	if _, err := RenderMac("/data/mac", []MacEntry{macEntry("mac1", "10.10.20.10"), macEntry("mac1", "10.10.21.10")}); err == nil {
		t.Error("accepted duplicate names")
	}
}

func TestMacNamesTheUserAlreadyUsesStayTheUsers(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	user := "Host build\n  HostName build.example.com\n\nHost=web *.internal\n  User me\n\nHost dev-*\n  User me\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := InstallMac(home, "/data/mac", []MacEntry{macEntry("build", "10.10.20.10"), macEntry("web", "10.10.21.10"), macEntry("dev-1", "10.10.22.10")}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Shadowed, ",") != "build,web" {
		t.Fatalf("shadowed %v", result.Shadowed)
	}
	fragment, _ := os.ReadFile(filepath.Join(home, ".ssh", MacFragment+"_config"))
	for _, want := range []string{"Host 10.10.20.10\n", "Host 10.10.21.10\n", "Host dev-1 10.10.22.10\n"} {
		if !strings.Contains(string(fragment), want) {
			t.Errorf("fragment lacks %q:\n%s", want, fragment)
		}
	}
}
