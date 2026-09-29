package sshkeys

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/execx"
)

func TestEnsureKeysRestoresPublicKeyWithoutChangingIdentity(t *testing.T) {
	root := keyRoot(t)
	private, _, public := ensured(t, root)
	before, err := os.ReadFile(private)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(private + ".pub"); err != nil {
		t.Fatal(err)
	}
	// Recovery should not need ssh-keygen or replace the guest's trusted key.
	t.Setenv("PATH", t.TempDir())
	_, _, recovered, err := EnsureKeys(context.Background(), execx.OSRunner{}, root)
	if err != nil || recovered != public {
		t.Fatalf("public key recovery = %q, %v", recovered, err)
	}
	after, err := os.ReadFile(private)
	if err != nil || string(after) != string(before) {
		t.Fatal("public key recovery changed the private key")
	}
	info, err := os.Lstat(private + ".pub")
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("restored public key mode: %v, %v", info, err)
	}
}

func TestEnsureKeysDoesNotReplaceUnsafeOrInvalidKeys(t *testing.T) {
	t.Parallel()
	for _, problem := range []string{"invalid private", "symlink private", "symlink public"} {
		t.Run(problem, func(t *testing.T) {
			root := keyRoot(t)
			private, _, _ := ensured(t, root)
			if err := os.Remove(private + ".pub"); err != nil {
				t.Fatal(err)
			}
			decoy := filepath.Join(root, "decoy")
			if err := os.WriteFile(decoy, []byte("keep this file"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch problem {
			case "invalid private":
				if err := os.WriteFile(private, []byte("invalid key"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink private":
				if err := os.Remove(private); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(decoy, private); err != nil {
					t.Fatal(err)
				}
			case "symlink public":
				if err := os.Symlink(decoy, private+".pub"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(private)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := EnsureKeys(context.Background(), execx.OSRunner{}, root); err == nil {
				t.Fatal("unsafe or invalid material was accepted")
			}
			after, err := os.ReadFile(private)
			if err != nil || string(before) != string(after) {
				t.Fatal("private material was replaced")
			}
			if problem != "symlink public" {
				if _, err := os.Lstat(private + ".pub"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("a public key was created from unsafe material: %v", err)
				}
			}
			if data, err := os.ReadFile(decoy); err != nil || string(data) != "keep this file" {
				t.Fatal("unrelated file was changed")
			}
		})
	}
}

func TestEnsureKeysDoesNotReplaceLostPrivateIdentity(t *testing.T) {
	root := keyRoot(t)
	private, _, public := ensured(t, root)
	if err := os.Remove(private); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	_, _, _, err := EnsureKeys(context.Background(), execx.OSRunner{}, root)
	if err == nil || !strings.Contains(err.Error(), "restore "+private+" from backup") {
		t.Fatalf("missing private key has no recovery guidance: %v", err)
	}
	data, err := os.ReadFile(private + ".pub")
	if err != nil || strings.TrimSpace(string(data)) != public {
		t.Fatal("lost private identity caused a replacement key pair")
	}
	if _, err := os.Lstat(private); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a replacement private key was generated: %v", err)
	}
}
