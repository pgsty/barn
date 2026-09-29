package sshkeys

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/execx"
)

func keyRoot(t *testing.T) string {
	t.Helper()
	// A short real path: the Ed25519 pair and known_hosts are addressed through
	// directory descriptors, and macOS temp roots are already long.
	root, err := os.MkdirTemp("", "sshkeys.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func ensured(t *testing.T, root string) (string, string, string) {
	t.Helper()
	runner := execx.OSRunner{Timeout: 30 * time.Second}
	privateKey, knownHosts, publicKey, err := EnsureKeys(context.Background(), runner, root)
	if err != nil {
		if strings.Contains(err.Error(), "ssh-keygen") {
			t.Skip("ssh-keygen is not installed")
		}
		t.Fatal(err)
	}
	return privateKey, knownHosts, publicKey
}

func TestEnsureKeysCreatesFixedModesAndIsIdempotent(t *testing.T) {
	t.Parallel()
	root := keyRoot(t)
	privateKey, knownHosts, publicKey := ensured(t, root)

	if !strings.HasPrefix(publicKey, "ssh-ed25519 ") || !strings.HasSuffix(publicKey, " barn") {
		t.Fatalf("public key = %q, want a barn-commented Ed25519 key", publicKey)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(root, "keys"): 0o700,
		privateKey:                  0o600,
		privateKey + ".pub":         0o644,
		knownHosts:                  0o600,
	} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s mode = %v (%v), want %v", path, info.Mode().Perm(), err, want)
		}
	}

	// Re-running must reuse the existing pair. Regenerating it would silently
	// invalidate every guest that already trusts the old key.
	before, err := os.ReadFile(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	_, _, second := ensured(t, root)
	after, err := os.ReadFile(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if second != publicKey || string(after) != string(before) {
		t.Fatal("EnsureKeys replaced existing deployment key material")
	}
}

func TestValidateSSHArtifactsAcceptsGeneratedMaterial(t *testing.T) {
	t.Parallel()
	root := keyRoot(t)
	privateKey, knownHosts, _ := ensured(t, root)
	// An initial no-wait start has not recorded a host key yet. OpenSSH must
	// be allowed to establish trust with accept-new on the first connection.
	if _, _, err := ValidateSSHArtifacts(root); err != nil {
		t.Fatalf("initial known_hosts blocked the first connection: %v", err)
	}
	if err := os.WriteFile(knownHosts, []byte("[127.0.0.1]:2222 ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotKey, gotHosts, err := ValidateSSHArtifacts(root)
	if err != nil || gotKey != privateKey || gotHosts != knownHosts {
		t.Fatalf("ValidateSSHArtifacts = %q, %q, %v", gotKey, gotHosts, err)
	}
}

func TestValidateSSHArtifactsRejectsUnsafeMaterial(t *testing.T) {
	t.Parallel()
	for name, corrupt := range map[string]func(t *testing.T, root, privateKey, knownHosts string){
		"relative root": func(t *testing.T, _, _, _ string) {},
		"world-readable private key": func(t *testing.T, _, privateKey, _ string) {
			if err := os.Chmod(privateKey, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"group-readable keys directory": func(t *testing.T, root, _, _ string) {
			if err := os.Chmod(filepath.Join(root, "keys"), 0o750); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked private key": func(t *testing.T, root, privateKey, _ string) {
			decoy := filepath.Join(root, "decoy")
			if err := os.WriteFile(decoy, []byte("not a key"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(privateKey); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(decoy, privateKey); err != nil {
				t.Fatal(err)
			}
		},
		"hard-linked private key": func(t *testing.T, root, privateKey, _ string) {
			if err := os.Link(privateKey, filepath.Join(root, "second-name")); err != nil {
				t.Fatal(err)
			}
		},
		"missing known_hosts": func(t *testing.T, _, _, knownHosts string) {
			if err := os.Remove(knownHosts); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := keyRoot(t)
			privateKey, knownHosts, _ := ensured(t, root)
			if err := os.WriteFile(knownHosts, []byte("[127.0.0.1]:2222 ssh-ed25519 AAAA\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			probe := root
			if name == "relative root" {
				probe = "keys"
			}
			corrupt(t, root, privateKey, knownHosts)

			_, _, err := ValidateSSHArtifacts(probe)
			if err == nil {
				t.Fatalf("%s was accepted as safe trust material", name)
			}
			// Callers map this to the integrity exit code without parsing strings.
			var integrity *SSHArtifactError
			if !errors.As(err, &integrity) {
				t.Fatalf("%s error is not an SSHArtifactError: %#v", name, err)
			}
		})
	}
}

func TestPurgeKeysPlansThenRemovesExactlyTheAllowlist(t *testing.T) {
	t.Parallel()
	root := keyRoot(t)
	ensured(t, root)
	keysDir := filepath.Join(root, "keys")
	unrelated := filepath.Join(keysDir, "not-ours.txt")
	if err := os.WriteFile(unrelated, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A foreign file in the keys directory must stop the purge rather than be
	// deleted along with it.
	if _, err := PurgeKeys(root, false); err == nil {
		t.Fatal("purge planned over an unrecognised file")
	}
	if err := os.Remove(unrelated); err != nil {
		t.Fatal(err)
	}

	plan, err := PurgeKeys(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Apply || len(plan.Actions) == 0 {
		t.Fatalf("plan = %#v", plan)
	}
	for _, action := range plan.Actions {
		if action.Applied {
			t.Fatalf("dry-run purge reported an applied action: %#v", action)
		}
		if _, err := os.Lstat(action.Path); err != nil {
			t.Fatalf("dry-run purge removed %s: %v", action.Path, err)
		}
	}

	applied, err := PurgeKeys(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Apply || len(applied.Actions) != len(plan.Actions) {
		t.Fatalf("applied purge = %#v, plan = %#v", applied, plan)
	}
	if _, err := os.Lstat(keysDir); !os.IsNotExist(err) {
		t.Fatalf("emptied keys directory survived the purge: %v", err)
	}
	// Purging an already-purged deployment is a no-op, not an error.
	if again, err := PurgeKeys(root, true); err != nil || len(again.Actions) != 0 {
		t.Fatalf("second purge = %#v, %v", again, err)
	}
}
