package cloudinit

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"
)

func TestRepairUpdatesHelpersWithoutReplacingGuestIdentityOrKeys(t *testing.T) {
	input := testInput()
	input.Control = true
	input.Shares = []Share{{Tag: "barn-0123456789abcdef0123", Guest: "/shared"}}
	script, err := RetryScript(input)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("script syntax: %v %s", err, output)
	}
	for _, name := range []string{"barn-warning", "barn-init-disks", "barn-init-shares", "barn-install-control-ssh", "barn-ready", "barn-finalize"} {
		if !strings.Contains(script, "/usr/local/libexec/"+name) {
			t.Fatalf("missing helper %s", name)
		}
	}
	for _, forbidden := range []string{"cloud-init clean", "reboot", "authorized_keys", "BEGIN OPENSSH PRIVATE KEY"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("unexpected repair action %s", forbidden)
		}
	}
	ready, err := renderReadyScript(input)
	if err != nil || !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte(ready))) {
		t.Fatal("repair loses original readiness identity")
	}
	input.PrivateKey = "private-key-must-not-travel-in-argv"
	if _, err := RetryScript(input); err == nil {
		t.Fatal("repair accepted a private key")
	}
}
