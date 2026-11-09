package private

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/process"
	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func resizeRecoveryFixture(t *testing.T) (state.Store, state.DeploymentState, state.NodeState) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	resolved := singlePrivateResolved()
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Root: root}
	deployment := state.DeploymentState{Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash, Resolved: resolved, UpdatedAt: time.Now().UTC()}
	if err := store.WriteDeployment(deployment); err != nil {
		t.Fatal(err)
	}
	node := resizeNodeState(t, resolved, root)
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	return store, deployment, node
}

func TestOfflineResizeReadsStateUnderLock(t *testing.T) {
	for _, phase := range []state.Phase{state.Stopped, state.Running, state.Starting} {
		t.Run(string(phase), func(t *testing.T) {
			store, deployment, node := resizeRecoveryFixture(t)
			held, err := acquireDeploymentLock(context.Background(), store.Root, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = held.Release() })
			waiting := make(chan struct{}, 1)
			manager := Manager{BarnVersion: "test", Progress: func(event activity.Event) {
				if event.Phase == "lock" {
					select {
					case waiting <- struct{}{}:
					default:
					}
				}
			}}
			target := deployment.Resolved.Nodes[0]
			target.Memory = 4 * spec.GiB
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			finished := make(chan error, 1)
			go func() { _, err := manager.resizeOffline(ctx, target); finished <- err }()
			select {
			case <-waiting:
			case <-ctx.Done():
				t.Fatal("resize did not wait for the deployment lock")
			}
			// A concurrent lifecycle command commits before resize obtains the lock.
			node.Phase = phase
			node.Invocation.Args = append(node.Invocation.Args, "-name", "latest-invocation")
			if phase == state.Running {
				// Incomplete process evidence must prevent stopping or rewriting argv.
				node.Process.ArgvHash = process.ExpectedArgvHash(node.Invocation)
			}
			if err := store.WriteNode(node); err != nil {
				t.Fatal(err)
			}
			if err := held.Release(); err != nil {
				t.Fatal(err)
			}
			err = <-finished
			updated, readErr := store.ReadNode(node.Node)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if phase != state.Stopped {
				if err == nil || !reflect.DeepEqual(updated, node) {
					t.Fatalf("resize overwrote a concurrent %s transition: err=%v node=%+v", phase, err, updated)
				}
				return
			}
			if err != nil || invocationOption(updated.Invocation.Args, "-name") != "latest-invocation" {
				t.Fatalf("resize used an invocation read before the lock: %v %+v", err, updated.Invocation)
			}
		})
	}
}

// This subprocess supplies a real, verifiable argv without starting a guest.
func TestResizeProcessHelper(t *testing.T) {
	if os.Getenv("BARN_RESIZE_TEST_PROCESS") != "1" {
		return
	}
	fmt.Println("ready")
	time.Sleep(time.Minute)
}

type resizeGuestRunner struct{}

func (resizeGuestRunner) Run(_ context.Context, _ string, args ...string) (execx.Result, error) {
	if strings.Contains(strings.Join(args, " "), "nproc") {
		return execx.Result{Stdout: []byte("1\n")}, nil
	}
	return execx.Result{Stdout: []byte("2147483648\n")}, nil
}

func startResizeProcess(t *testing.T, node *state.NodeState) {
	t.Helper()
	node.Invocation = qemu.Invocation{Binary: os.Args[0], Args: []string{
		"-test.run=^TestResizeProcessHelper$", "--", "-smp", "1,maxcpus=4", "-m", "2048M,slots=2,maxmem=8192M",
	}}
	child := exec.Command(node.Invocation.Binary, node.Invocation.Args...)
	child.Env = append(os.Environ(), "BARN_RESIZE_TEST_PROCESS=1")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if ready, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("resize process not ready: %q %v", ready, err)
	}
	identity, err := process.Capture(context.Background(), resizeGuestRunner{}, node.Invocation, child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	node.Process, node.Phase = recordProcess(identity), state.Running
}

// Each QMP call opens a connection, negotiates capabilities, and executes once.
func serveResizeQMP(t *testing.T, node state.NodeState, handle func(string) (any, bool)) {
	t.Helper()
	listener, err := net.Listen("unix", node.Runtime.QMP)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { _ = listener.Close(); <-done })
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
				_ = encoder.Encode(map[string]any{"QMP": map[string]any{"version": map[string]any{}, "capabilities": []any{}}})
				for range 2 {
					var request struct {
						Execute string
						ID      string
					}
					if decoder.Decode(&request) != nil {
						return
					}
					var result any = map[string]any{}
					failed := false
					switch request.Execute {
					case "query-name":
						result = map[string]any{"name": node.Node}
					case "query-uuid":
						result = map[string]any{"UUID": node.VMUUID}
					case "qmp_capabilities":
					default:
						result, failed = handle(request.Execute)
					}
					if result == nil && !failed {
						return // the mutation succeeded, but its reply was lost
					}
					response := map[string]any{"return": result, "id": request.ID}
					if failed {
						delete(response, "return")
						response["error"] = map[string]any{"class": "GenericError", "desc": "injected resize failure"}
					}
					_ = encoder.Encode(response)
				}
			}()
		}
	}()
}

func TestOnlineResizeRetainsPartialHotplugFailure(t *testing.T) {
	for _, failAt := range []string{"memory", "second-cpu", "cancel", "lost-reply", "recovery-failure", "no-slots"} {
		t.Run(failAt, func(t *testing.T) {
			store, deployment, node := resizeRecoveryFixture(t)
			startResizeProcess(t, &node)
			node.Runtime.QMP = filepath.Join(shortRuntimeBase(t, "barn-resize-qmp-"), "q.sock")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var added atomic.Int32
			var unmarked atomic.Bool
			serveResizeQMP(t, node, func(command string) (any, bool) {
				switch command {
				case "query-memory-size-summary":
					return map[string]any{"base-memory": 2 * spec.GiB, "plugged-memory": 0}, false
				case "query-hotpluggable-cpus":
					if failAt == "no-slots" {
						return []any{}, false
					}
					return []map[string]any{
						{"type": "host-x86_64-cpu", "vcpus-count": 1, "props": map[string]any{"core-id": 1}},
						{"type": "host-x86_64-cpu", "vcpus-count": 1, "props": map[string]any{"core-id": 2}},
					}, false
				case "device_add":
					persisted, err := store.ReadNode(node.Node)
					if err != nil || !persisted.ResizeIncomplete {
						unmarked.Store(true)
					}
					if failAt == "second-cpu" && added.Load() == 1 {
						return nil, true
					}
					added.Add(1)
					if failAt == "cancel" {
						cancel()
					}
					if failAt == "lost-reply" {
						return nil, false
					}
					return map[string]any{}, false
				default:
					return nil, true
				}
			})
			if err := store.WriteNode(node); err != nil {
				t.Fatal(err)
			}
			pub, priv := testSSHKeyPair(t)
			if err := os.MkdirAll(filepath.Join(store.Root, "keys"), 0o700); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string]string{"id_ed25519": priv, "id_ed25519.pub": pub, "known_hosts": ""} {
				if err := os.WriteFile(filepath.Join(store.Root, "keys", name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			target := deployment.Resolved.Nodes[0]
			target.CPUs, target.Memory = 2, 4*spec.GiB
			if failAt == "second-cpu" {
				target.CPUs, target.Memory = 3, 2*spec.GiB
			}
			manager := Manager{BarnVersion: "test", Runner: resizeGuestRunner{}, LookPath: func(name string) (string, error) { return "/fixture/" + name, nil }}
			var applied, changed bool
			var err error
			if failAt == "recovery-failure" {
				desired := cloneResolved(deployment.Resolved)
				desired.Nodes[0] = target
				_, err = manager.Resize(ctx, desired)
				if err == nil || !strings.Contains(err.Error(), "offline recovery failed") {
					t.Fatalf("missing recovery failure: %v", err)
				}
				changed = true
			} else {
				applied, changed, err = manager.resizeOnline(ctx, target)
			}
			persisted, readErr := store.ReadNode(node.Node)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if failAt == "no-slots" {
				if applied || changed || err != nil || added.Load() != 0 || persisted.ResizeIncomplete {
					t.Fatalf("no-mutation fallback = %t %t %v", applied, changed, err)
				}
				return
			}
			if applied || !changed || err == nil || added.Load() != 1 || unmarked.Load() {
				t.Fatalf("lost partial hotplug: added=%d applied=%t changed=%t err=%v", added.Load(), applied, changed, err)
			}
			if failAt != "cancel" && failAt != "recovery-failure" && !errors.Is(err, errResizeOffline) {
				t.Fatalf("missing offline recovery cause: %v", err)
			}
			if !persisted.ResizeIncomplete || !reflect.DeepEqual(persisted.Invocation, node.Invocation) || persisted.SpecHash != node.SpecHash || persisted.PendingInvocation.Binary != "" {
				t.Fatalf("partial resize lost its recovery state or changed the applied invocation: %+v", persisted)
			}
			status, statusErr := manager.Status(context.Background())
			if statusErr == nil || len(status.Nodes) != 1 || status.Nodes[0].Runtime != "running" || !strings.Contains(status.Nodes[0].Error, "online resize did not finish") {
				t.Fatalf("partial resize was hidden by status: %+v %v", status, statusErr)
			}
			if failAt == "memory" {
				unverified := persisted
				unverified.Process.ArgvHash = "mismatched-argv"
				if err := store.WriteNode(unverified); err != nil {
					t.Fatal(err)
				}
				status, statusErr = manager.Status(context.Background())
				partial := IsolatedPartial(statusErr)
				if partial == nil || partial.Total != 1 || len(partial.Failures) != 1 || !strings.Contains(status.Nodes[0].Error, "online resize did not finish") || !strings.Contains(status.Nodes[0].Error, "process identity") {
					t.Fatalf("status miscounted a node with two problems: %+v %v", status, statusErr)
				}
				if err := store.WriteNode(persisted); err != nil {
					t.Fatal(err)
				}
			}
			// Retrying the original resources must also recover, even though the
			// applied inventory still has those CPU/memory values.
			_, names, err := resizeTargets(deployment.Resolved, deployment.Resolved, nil, store)
			if err != nil || !reflect.DeepEqual(names, []string{node.Node}) {
				t.Fatalf("retry skipped incomplete resize: %v %v", names, err)
			}
		})
	}
}

func TestResizeCommitFailureKeepsRecoveryMarker(t *testing.T) {
	store, deployment, node := resizeRecoveryFixture(t)
	node.Phase, node.ResizeIncomplete = state.Running, true
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	target := deployment.Resolved.Nodes[0]
	target.CPUs = 2
	next, err := qemu.ConfigureResources(node.Invocation, target.CPUs, target.Memory)
	if err != nil {
		t.Fatal(err)
	}
	// Make only the deployment write fail, after the updated node was saved.
	path := filepath.Join(store.Root, "state.json")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitResize(store, deployment, node, target, next, true, "test", time.Now().UTC()); err == nil {
		t.Fatal("commit ignored deployment write failure")
	}
	persisted, err := store.ReadNode(node.Node)
	if err != nil || !reflect.DeepEqual(persisted, node) {
		t.Fatalf("rollback lost pending recovery or changed applied state: %+v %v", persisted, err)
	}
}

func TestOfflineResizeCommitRefusesActiveNode(t *testing.T) {
	store, deployment, node := resizeRecoveryFixture(t)
	node.Phase = state.Running
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	target := deployment.Resolved.Nodes[0]
	target.CPUs = 2
	next, err := qemu.ConfigureResources(node.Invocation, target.CPUs, target.Memory)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitResize(store, deployment, node, target, next, false, "test", time.Now().UTC()); err == nil {
		t.Fatal("offline commit accepted a running node")
	}
	persisted, err := store.ReadNode(node.Node)
	if err != nil || !reflect.DeepEqual(persisted, node) {
		t.Fatalf("offline commit damaged live process evidence: %+v %v", persisted, err)
	}
}
