package private

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRecoveryKeys(t *testing.T, root string) (string, string) {
	t.Helper()
	public, private := testSSHKeyPair(t)
	dir := filepath.Join(root, "keys")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"id_ed25519": private, "id_ed25519.pub": public, "known_hosts": ""} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "id_ed25519"), private
}

func TestExistingStartRestoresPublicKeyWithoutChangingIdentity(t *testing.T) {
	fixture, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	key, original := writeRecoveryKeys(t, fixture.Deployment.Root)
	if err := os.Remove(key + ".pub"); err != nil {
		t.Fatal(err)
	}
	manager := privateShareCapabilityManager(t, fixture, unavailableSharePeerRunner{})
	manager.DialSSHAddress = func(string, string) (net.Conn, error) { return nil, errors.New("unused") }
	_, _ = manager.Start(context.Background()) // The fake runtime fails after key recovery.
	if _, err := os.Stat(key + ".pub"); err != nil {
		t.Fatalf("start did not recover the public key: %v", err)
	}
	if data, err := os.ReadFile(key); err != nil || string(data) != original {
		t.Fatalf("private identity changed: %v", err)
	}
}

func TestExistingDeploymentNeverGeneratesReplacementPrivateKey(t *testing.T) {
	fixture, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	manager := privateShareCapabilityManager(t, fixture, unavailableSharePeerRunner{})
	_, _, _, err := manager.ensureKeys(context.Background(), fixture.Deployment)
	if err == nil || !strings.Contains(err.Error(), "restore") || !strings.Contains(err.Error(), "private SSH key") {
		t.Fatalf("missing identity must require original backup, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.Deployment.Root, "keys", "id_ed25519")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created replacement key: %v", err)
	}
}
