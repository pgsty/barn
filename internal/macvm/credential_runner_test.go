package macvm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialOwnerRecordCannotRedirectExecution(t *testing.T) {
	for _, mode := range []string{"traversal", "tampered", "symlink", "missing", "migration-traversal", "empty"} {
		t.Run(mode, func(t *testing.T) {
			store := testStore(t)
			id, _ := NewInstanceID()
			if _, err := store.mkdir("credentials", "owners"); err != nil {
				t.Fatal(err)
			}
			path, _ := credentialRecordPath(store.Root, id)
			hash := strings.Repeat("a", 64)
			if mode == "traversal" {
				hash = "../../external"
			}
			record := credentialRunnerRecord{Hash: hash}
			if mode == "migration-traversal" {
				record = credentialRunnerRecord{MigrationFrom: "../../external"}
			} else if mode == "empty" {
				record = credentialRunnerRecord{}
			}
			if err := writeJSON(path, record); err != nil {
				t.Fatal(err)
			}
			if mode == "tampered" || mode == "symlink" {
				dir, err := store.mkdir("credentials", "owners", hash, "Farrow Mac.app", "Contents", "MacOS")
				if err != nil {
					t.Fatal(err)
				}
				binary := filepath.Join(dir, "farrow-mac-runner")
				if mode == "tampered" {
					err = os.WriteFile(binary, []byte("must never execute"), 0700)
				} else {
					err = os.Symlink("/usr/bin/true", binary)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			r := Runner{Binary: "/never-execute"}
			if _, err := r.credentialRunner(context.Background(), store.Root, id); err == nil {
				t.Fatal("unsafe owner record accepted")
			}
		})
	}
}

func TestLegacyCredentialLookupRemainsReadOnly(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "absent")
	id, _ := NewInstanceID()
	r := Runner{Binary: "/original/custom-runner"}
	owner, err := r.credentialRunner(context.Background(), root, id)
	if err != nil || owner.Binary != r.Binary {
		t.Fatalf("legacy lookup changed: %+v %v", owner, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("read-only lookup created state")
	}
	if _, err := credentialRecordPath(root, "../../other"); err == nil {
		t.Fatal("unsafe account accepted")
	}
}
