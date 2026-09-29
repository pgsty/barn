package private

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/state"
)

type rejectingAuditRunner struct{}

func (rejectingAuditRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	return execx.Result{}, errors.New("runner should not be called")
}

func auditNodeFixture() state.NodeState {
	return state.NodeState{Node: "meta", VMUUID: "11111111-1111-4111-8111-111111111111"}
}

func serveQMPIdentity(t *testing.T, socket, name, uuid string) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			decoder := json.NewDecoder(connection)
			encoder := json.NewEncoder(connection)
			_ = encoder.Encode(map[string]any{"QMP": map[string]any{"version": map[string]any{}, "capabilities": []any{}}})
			for requestIndex := 0; requestIndex < 2; requestIndex++ {
				var request map[string]any
				if decoder.Decode(&request) != nil {
					break
				}
				execute, _ := request["execute"].(string)
				id, _ := request["id"].(string)
				result := any(map[string]any{})
				if execute == "query-name" {
					result = map[string]any{"name": name}
				} else if execute == "query-uuid" {
					result = map[string]any{"UUID": uuid}
				}
				_ = encoder.Encode(map[string]any{"return": result, "id": id})
			}
			_ = connection.Close()
		}
	}()
}

func TestRuntimeIdentityAuditorQMPFirstAuthority(t *testing.T) {
	t.Parallel()
	// Unix socket paths are length-bounded; t.TempDir can exceed the limit.
	directory, err := os.MkdirTemp("/tmp", "barn-private-qmp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(directory, "qmp.sock"))
		_ = os.Remove(directory)
	})
	node := auditNodeFixture()
	node.Runtime = state.RuntimePaths{Directory: directory, QMP: filepath.Join(directory, "qmp.sock"), PIDFile: filepath.Join(directory, "qemu.pid")}
	serveQMPIdentity(t, node.Runtime.QMP, node.Node, node.VMUUID)
	observation, err := RuntimeIdentityAuditor(rejectingAuditRunner{}, time.Second)(context.Background(), node)
	if err != nil || !observation.Live || observation.Authority != "qmp" {
		t.Fatalf("QMP observation = %#v, %v", observation, err)
	}
}

func TestRuntimeIdentityAuditorDeadAndUnverifiedPID(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	node := auditNodeFixture()
	node.Runtime = state.RuntimePaths{Directory: directory, QMP: filepath.Join(directory, "qmp.sock"), PIDFile: filepath.Join(directory, "qemu.pid")}
	if err := os.WriteFile(node.Runtime.PIDFile, []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	auditor := RuntimeIdentityAuditor(rejectingAuditRunner{}, 100*time.Millisecond)
	observation, err := auditor(context.Background(), node)
	if err != nil || observation.Live || observation.Authority != "dead" {
		t.Fatalf("dead observation = %#v, %v", observation, err)
	}
	if err := os.WriteFile(node.Runtime.PIDFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auditor(context.Background(), node); err == nil {
		t.Fatal("unverified live pidfile was considered dead")
	}
}

func TestRuntimeIdentityAuditorNodeWithoutRuntimeIsDead(t *testing.T) {
	t.Parallel()
	observation, err := RuntimeIdentityAuditor(rejectingAuditRunner{}, time.Second)(context.Background(), auditNodeFixture())
	if err != nil || observation.Live || observation.Authority != "dead" {
		t.Fatalf("runtime-less observation = %#v, %v", observation, err)
	}
}
