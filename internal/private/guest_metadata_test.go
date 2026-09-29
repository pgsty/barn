package private

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/cloudinit"
	"github.com/pgsty/barn/internal/spec"
)

func TestGuestMetadataExpansionRemovalAndUserConfig(t *testing.T) {
	root := t.TempDir()
	sshDir := filepath.Join(root, ".ssh")
	if err := os.Mkdir(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	hostsPath, scriptPath := filepath.Join(root, "hosts"), filepath.Join(root, "barn-hosts")
	if err := os.WriteFile(hostsPath, []byte("127.0.0.1 localhost\n192.0.2.1 custom-host\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(sshDir, "config")
	if err := os.WriteFile(configPath, []byte("ServerAliveInterval 37\nHost custom-host\n  User developer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved := singlePrivateResolved()
	apply := func() {
		t.Helper()
		command := guestMetadataCommand(resolved, true)
		original := cloudinit.RenderHostsScript(deploymentHosts(resolved))
		local := strings.ReplaceAll(original, "/etc/hosts", strconv.Quote(hostsPath))
		command = strings.ReplaceAll(command, base64.StdEncoding.EncodeToString([]byte(original)), base64.StdEncoding.EncodeToString([]byte(local)))
		command = strings.ReplaceAll(command, "sudo -n tee /usr/local/libexec/barn-hosts", "tee "+strconv.Quote(scriptPath))
		command = strings.ReplaceAll(command, "sudo -n /usr/local/libexec/barn-hosts", "bash "+strconv.Quote(scriptPath))
		command = strings.ReplaceAll(command, "$HOME/.ssh", sshDir)
		if output, err := exec.Command("bash", "-c", command).CombinedOutput(); err != nil {
			t.Fatalf("guest update: %s, %v", output, err)
		}
	}
	apply()
	resolved.Nodes = append(resolved.Nodes, spec.Node{Name: "node-1", Address: "10.10.10.11"})
	apply()
	hosts, err := os.ReadFile(hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hosts), "10.10.10.11 node-1") || !strings.Contains(string(config), "node-1") {
		t.Fatal("new peer is missing")
	}
	apply()
	again, err := os.ReadFile(configPath)
	if err != nil || string(again) != string(config) {
		t.Fatalf("refresh not idempotent: %v", err)
	}
	resolved.Nodes = resolved.Nodes[:1]
	apply()
	hosts, err = os.ReadFile(hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hosts), "node-1") || strings.Contains(string(config), "node-1") {
		t.Fatal("removed peer is still present")
	}
	if !strings.Contains(string(hosts), "custom-host") || !strings.Contains(string(config), "User developer") || strings.Count(string(config), "# BEGIN BARN") != 1 {
		t.Fatal("refresh changed user content or duplicated its managed block")
	}
	// Comments do not end an OpenSSH Host block. The refreshed fragment must
	// restore global scope for the user's leading options, not just their bytes.
	effective, err := exec.Command("ssh", "-G", "-F", configPath, "custom-host").CombinedOutput()
	if err != nil || !strings.Contains(string(effective), "serveraliveinterval 37\n") || !strings.Contains(string(effective), "user developer\n") {
		t.Fatalf("refresh changed effective user SSH options: %s, %v", effective, err)
	}
}
