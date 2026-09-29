package macvm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/sshconfig"
)

func TestSSHConfigCoversInitializedMachinesOnly(t *testing.T) {
	m, _ := testManager(t)
	dev := testMachine(t, m, "dev", "10.10.30.0/24", true)
	testMachine(t, m, "fresh", "10.10.31.0/24", false)
	text, err := m.SSHConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Host dev 10.10.30.10\n") || strings.Contains(text, "fresh") || !strings.Contains(text, "HostKeyAlias "+dev.HostKeyAlias()) || !strings.Contains(text, "StrictHostKeyChecking yes") {
		t.Fatalf("fragment:\n%s", text)
	}
	result, err := m.InstallSSHConfig(context.Background())
	if err != nil || !result.Changed {
		t.Fatalf("install %+v %v", result, err)
	}
	config, _ := os.ReadFile(filepath.Join(m.SSHHome, ".ssh", "config"))
	if !strings.Contains(string(config), "# barn-mac:include\nInclude ") {
		t.Fatalf("config %q", config)
	}
	if warning := m.refreshSSHConfig(); warning != "" {
		t.Fatal(warning)
	}
	if installed, err := sshconfig.MacInstalled(m.SSHHome, m.Store.Root); err != nil || !installed {
		t.Fatalf("installed=%v err=%v", installed, err)
	}
	// Another Barn home never takes over the fragment implicitly.
	other, _ := testManager(t)
	other.SSHHome = m.SSHHome
	testMachine(t, other, "other", "10.10.40.0/24", true)
	if warning := other.refreshSSHConfig(); warning != "" {
		t.Fatalf("foreign refresh warned: %s", warning)
	}
	fragment, _ := os.ReadFile(filepath.Join(m.SSHHome, ".ssh", sshconfig.MacFragment+"_config"))
	if strings.Contains(string(fragment), "Host other") {
		t.Fatal("another home replaced the fragment during a refresh")
	}
	if _, err := m.RemoveSSHConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	config, _ = os.ReadFile(filepath.Join(m.SSHHome, ".ssh", "config"))
	if strings.Contains(string(config), "barn-mac") {
		t.Fatalf("include remains: %q", config)
	}
}

func TestSSHConfigRemovalLastsUntilInstall(t *testing.T) {
	m, _ := testManager(t)
	testMachine(t, m, "dev", "10.10.30.0/24", true)
	ctx := context.Background()
	if warning := m.refreshSSHConfig(); warning != "" {
		t.Fatal(warning)
	}
	if _, err := m.RemoveSSHConfig(ctx); err != nil {
		t.Fatal(err)
	}
	// A later start or destroy must not add the entries back.
	if warning := m.refreshSSHConfig(); warning != "" {
		t.Fatal(warning)
	}
	if installed, err := sshconfig.MacInstalled(m.SSHHome, m.Store.Root); err != nil || installed {
		t.Fatalf("a refresh reinstalled removed entries: installed=%v err=%v", installed, err)
	}
	if _, err := m.InstallSSHConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if installed, err := sshconfig.MacInstalled(m.SSHHome, m.Store.Root); err != nil || !installed {
		t.Fatalf("install after removal: installed=%v err=%v", installed, err)
	}
}

func TestSSHConfigInstallWithoutReadyMachinesKeepsOtherHomes(t *testing.T) {
	other, _ := testManager(t)
	testMachine(t, other, "other", "10.10.40.0/24", true)
	if warning := other.refreshSSHConfig(); warning != "" {
		t.Fatal(warning)
	}
	m, _ := testManager(t)
	m.SSHHome = other.SSHHome
	testMachine(t, m, "fresh", "10.10.31.0/24", false)
	result, err := m.InstallSSHConfig(context.Background())
	if err != nil || result.Action != "pending" {
		t.Fatalf("install with no ready machine: %+v %v", result, err)
	}
	if installed, err := sshconfig.MacInstalled(other.SSHHome, other.Store.Root); err != nil || !installed {
		t.Fatalf("the other home's entries were removed: installed=%v err=%v", installed, err)
	}
}
