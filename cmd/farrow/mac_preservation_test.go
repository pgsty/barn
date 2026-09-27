package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/lock"
	"github.com/pgsty/farrow/internal/spec"
	"github.com/pgsty/farrow/internal/state"
)

// Linux terminal disposal must preserve every independent Mac namespace,
// including incomplete/unreadable Mac metadata and both slots' live locks.
func TestLinuxDisposalPreservesIndependentMacData(t *testing.T) {
	for _, test := range []struct {
		name    string
		applied bool
		args    []string
	}{
		{"residual-purge", false, []string{"purge", "--json"}},
		{"applied-purge", true, []string{"purge", "--json"}},
		{"applied-destroy-purge", true, []string{"destroy", "--force", "--purge", "--json"}},
		{"applied-destroy", true, []string{"destroy", "--force", "--json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("FARROW_HOME", root)
			t.Setenv("HOME", t.TempDir())
			if test.applied {
				writeMacPreservationLinuxDeployment(t, root)
			}
			keys := filepath.Join(root, "keys")
			if err := os.Mkdir(keys, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"id_ed25519", "id_ed25519.pub", "known_hosts"} {
				if err := os.WriteFile(filepath.Join(keys, name), []byte("Linux key fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			macRoot := filepath.Join(root, "mac")
			for _, name := range []string{"config.json", "images/ipsw/26A428.ipsw", "images/ipsw/26A428.ipsw.partial", "images/base/26A428/disk.asif", "images/base/26A428/metadata.json", "slots/mac1/state.json", "slots/mac1/disk.asif", "slots/mac1/known_hosts", "slots/mac1/id_ed25519", "slots/mac2/state.json", "slots/mac2/disk.asif", "ssh/config", "runtime/state.lock", "runtime/mac1.log"} {
				path := filepath.Join(macRoot, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("Mac data must survive Linux disposal: "+name), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			held, err := lock.Acquire(context.Background(), filepath.Join(macRoot, "runtime", "state.lock"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Release() }()
			before := macPreservationSnapshot(t, macRoot)
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr); code != exitOK {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
			}
			after := macPreservationSnapshot(t, macRoot)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("Linux disposal changed Mac data\nbefore=%v\nafter=%v", before, after)
			}
			if test.args[0] == "purge" || test.name == "applied-destroy-purge" {
				if _, err := os.Stat(keys); !os.IsNotExist(err) {
					t.Fatalf("test did not exercise Linux key disposal: %v", err)
				}
			}
		})
	}
}

func writeMacPreservationLinuxDeployment(t *testing.T, root string) {
	t.Helper()
	resolved := spec.Resolved{Schema: 1, Name: "farrow", Image: "d13", Network: "private", SSHUser: "dba", Private: &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"}, Nodes: []spec.Node{{Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 2, Memory: 4 * spec.GiB, RootDisk: 64 * spec.GiB}}}
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := (state.Store{Root: root}).WriteDeployment(state.DeploymentState{Schema: state.DeploymentSchema, FarrowVersion: "test", SpecHash: hash, Resolved: resolved, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}

func macPreservationSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[rel] = fmt.Sprintf("%04o:%x", info.Mode().Perm(), sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
