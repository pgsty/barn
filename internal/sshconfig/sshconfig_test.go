package sshconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallIdempotentAndRemovePreservesUserConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(sshDir, "config")
	userContent := "Host user-owned\n  HostName example.invalid\n"
	if err := os.WriteFile(configPath, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := Entry{
		Name: "lab", Node: "meta", User: "dba", Host: "127.0.0.1", Port: 2222,
		Identity: filepath.Join(home, "key"), KnownHosts: filepath.Join(home, "known"),
	}
	first, err := Install(home, entry)
	if err != nil || !first.Changed {
		t.Fatalf("first install = %#v, %v", first, err)
	}
	second, err := Install(home, entry)
	if err != nil || second.Changed {
		t.Fatalf("idempotent install = %#v, %v", second, err)
	}
	for _, pathname := range []string{first.Fragment, first.Config} {
		info, err := os.Lstat(pathname)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("SSH config mode %s = %v, %v", pathname, info, err)
		}
	}
	fragment, _ := os.ReadFile(first.Fragment)
	if !strings.Contains(string(fragment), "Host lab-meta") || !strings.Contains(string(fragment), "StrictHostKeyChecking accept-new") {
		t.Fatalf("fragment = %s", fragment)
	}
	installedConfig, _ := os.ReadFile(configPath)
	if strings.Index(string(installedConfig), "# barn:") > strings.Index(string(installedConfig), "Host user-owned") {
		t.Fatalf("Barn Include was placed inside a user Host stanza:\n%s", installedConfig)
	}
	if ssh, err := exec.LookPath("ssh"); err == nil {
		effective, err := exec.Command(ssh, "-G", "-F", configPath, "lab-meta").CombinedOutput()
		if err != nil || !strings.Contains(string(effective), "hostname 127.0.0.1") || !strings.Contains(string(effective), "port 2222") {
			t.Fatalf("installed config is not globally effective: %v\n%s", err, effective)
		}
	}
	removed, err := Remove(home, entry.Name)
	if err != nil || !removed.Changed {
		t.Fatalf("remove = %#v, %v", removed, err)
	}
	if _, err := os.Lstat(first.Fragment); !os.IsNotExist(err) {
		t.Fatalf("fragment remains: %v", err)
	}
	config, _ := os.ReadFile(configPath)
	if string(config) != userContent {
		t.Fatalf("user config changed: %q", config)
	}
}

func TestInstallAndRemoveRefuseUnownedFragment(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fragment := filepath.Join(sshDir, "lab_config")
	if err := os.WriteFile(fragment, []byte("Host user-owned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := Entry{Name: "lab", Node: "meta", User: "dba", Host: "127.0.0.1", Port: 2222, Identity: filepath.Join(home, "key"), KnownHosts: filepath.Join(home, "known")}
	if _, err := Install(home, entry); err == nil {
		t.Fatal("unowned fragment was overwritten")
	}
	if _, err := Remove(home, entry.Name); err == nil {
		t.Fatal("unowned fragment was removed")
	}
	data, _ := os.ReadFile(fragment)
	if string(data) != "Host user-owned\n" {
		t.Fatalf("unowned fragment changed: %q", data)
	}
}

func TestInstallManyRendersNodeAndPrivateAddressAliases(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	identity := filepath.Join(home, "id_ed25519")
	knownHosts := filepath.Join(home, "known_hosts")
	entries := []Entry{
		{Name: "lab", Node: "meta", Aliases: []string{"meta", "10.10.10.10"}, User: "dba", Host: "127.0.0.1", Port: 2222, Identity: identity, KnownHosts: knownHosts},
		{Name: "lab", Node: "node-1", Aliases: []string{"node-1", "10.10.10.11"}, User: "dba", Host: "127.0.0.1", Port: 2223, Identity: identity, KnownHosts: knownHosts},
	}
	result, err := InstallMany(home, entries)
	if err != nil || !result.Changed {
		t.Fatalf("install many = %#v, %v", result, err)
	}
	data, err := os.ReadFile(result.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{
		"Host lab-meta meta 10.10.10.10\n",
		"Host lab-node-1 node-1 10.10.10.11\n",
		"Port 2222\n",
		"Port 2223\n",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("multi-node fragment missing %q:\n%s", expected, text)
		}
	}
	second, err := InstallMany(home, entries)
	if err != nil || second.Changed {
		t.Fatalf("idempotent install many = %#v, %v", second, err)
	}
}

func TestInstallManyRejectsDuplicateNodeAndUnsafeAlias(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	base := Entry{Name: "lab", Node: "meta", User: "dba", Host: "127.0.0.1", Port: 2222, Identity: filepath.Join(home, "key"), KnownHosts: filepath.Join(home, "known")}
	if _, err := InstallMany(home, []Entry{base, base}); err == nil {
		t.Fatal("duplicate node was accepted")
	}
	base.Aliases = []string{"meta\nInclude /tmp/evil"}
	if _, err := InstallMany(home, []Entry{base}); err == nil {
		t.Fatal("unsafe Host alias was accepted")
	}
}

func TestInstallRefusesMalformedOwnedInclude(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := Entry{Name: "lab", Node: "meta", User: "dba", Host: "127.0.0.1", Port: 2222, Identity: filepath.Join(home, "key"), KnownHosts: filepath.Join(home, "known")}
	marker := "# barn:include\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(marker+"Host unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(home, entry); err == nil {
		t.Fatal("malformed owned Include block was adopted")
	}
}

func TestInstallRejectsOpenSSHTokenExpansionPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	home := filepath.Join(root, "home%h")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := Entry{Name: "lab", Node: "meta", User: "dba", Host: "127.0.0.1", Port: 2222, Identity: filepath.Join(root, "key"), KnownHosts: filepath.Join(root, "known")}
	if _, err := Install(home, entry); err == nil {
		t.Fatal("OpenSSH percent-token home was accepted")
	}
	entry.Identity = filepath.Join(root, "${HOME}-key")
	if _, err := Install(root, entry); err == nil {
		t.Fatal("OpenSSH environment-token identity path was accepted")
	}
	entry.Identity = filepath.Join(root, "key")
	entry.Aliases = []string{"%h"}
	if _, err := Install(root, entry); err == nil {
		t.Fatal("OpenSSH token Host alias was accepted")
	}
}

func TestRemoveValidatesFragmentBeforeChangingConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	entry := Entry{Name: "lab", Node: "meta", User: "dba", Host: "127.0.0.1", Port: 2222, Identity: filepath.Join(home, "key"), KnownHosts: filepath.Join(home, "known")}
	installed, err := Install(home, entry)
	if err != nil {
		t.Fatal(err)
	}
	configBefore, _ := os.ReadFile(installed.Config)
	if err := os.WriteFile(installed.Fragment, []byte("Host user-owned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(home, entry.Name); err == nil {
		t.Fatal("malformed fragment removal succeeded")
	}
	configAfter, _ := os.ReadFile(installed.Config)
	if string(configAfter) != string(configBefore) {
		t.Fatalf("config changed before fragment validation:\n%s", configAfter)
	}
}

func TestLinkedConfigIsNeverEditedAndAcceptsManualInclude(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(home, "dotfiles-ssh-config")
	userContent := "Host user-owned\n  HostName example.invalid\n"
	if err := os.WriteFile(managed, []byte(userContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(managed, filepath.Join(sshDir, "config")); err != nil {
		t.Fatal(err)
	}
	entry := Entry{Name: "barn", Node: "meta", User: "dba", Host: "127.0.0.1", Port: 2222, Identity: filepath.Join(home, "key"), KnownHosts: filepath.Join(home, "known")}
	result, err := Install(home, entry)
	if err == nil || !strings.Contains(err.Error(), "managed outside Barn") || !result.Changed {
		t.Fatalf("linked install = %#v, %v", result, err)
	}
	if data, _ := os.ReadFile(managed); string(data) != userContent {
		t.Fatalf("linked config was edited: %q", data)
	}
	if _, err := os.Stat(result.Fragment); err != nil {
		t.Fatalf("fragment not published: %v", err)
	}
	if err := os.WriteFile(managed, []byte("Include ~/.ssh/barn_config\n"+userContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(home, entry); err != nil {
		t.Fatalf("manual Include must satisfy install: %v", err)
	}
	if _, err := Remove(home, "barn"); err != nil {
		t.Fatalf("remove with linked config: %v", err)
	}
	if data, _ := os.ReadFile(managed); !strings.HasPrefix(string(data), "Include ~/.ssh/barn_config") {
		t.Fatalf("linked config was edited on remove: %q", data)
	}
}
