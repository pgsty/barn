package macvm

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/lock"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "farrow"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func holdStore(t *testing.T, s *Store) *lock.File {
	t.Helper()
	held, err := (&Manager{Store: s}).acquireState(context.Background(), "test state")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := held.Release(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error(err)
		}
	})
	return held
}

// testBase publishes a ready base whose files satisfy validateBase.
func testBase(t *testing.T, s *Store, held *lock.File, id string) *BaseImage {
	t.Helper()
	b := &BaseImage{SchemaVersion: imageSchemaVersion, ID: id, Version: "27.0", Build: "26A428", HardwareModelHash: strings.Repeat("a", 64),
		InstallerSHA256: strings.Repeat("b", 64), State: "ready", DiskBytes: DefaultDiskBytes, RecipeVersion: 1, CreatedAt: time.Now().UTC()}
	if err := s.SaveBase(held, b); err != nil {
		t.Fatal(err)
	}
	dir, err := s.BasePath(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"disk.asif", "hardware-model.bin", "auxiliary-storage.bin", "machine-id.bin"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture "+name), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "base.json"), []byte(`{"ready":true,"first_boot":false}`), 0o400); err != nil {
		t.Fatal(err)
	}
	return b
}

// testManager returns a manager whose store has a config and a ready base.
// Host routes and limits are fixed so tests never read the real host.
func testManager(t *testing.T) (*Manager, *BaseImage) {
	t.Helper()
	store := testStore(t)
	m := &Manager{Store: store, SSHHome: t.TempDir(), protocolOK: true}
	m.hostRoutes = func(context.Context) ([]DarwinRoute, error) { return nil, nil }
	m.hostLimits = func(context.Context) (int, int64, error) { return 16, 64 << 30, nil }
	held, err := m.acquireState(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	config, err := NewConfig()
	if err != nil {
		t.Fatal(err)
	}
	base := testBase(t, store, held, "26A428-fixture")
	config.DefaultBaseID = base.ID
	if err := store.SaveConfig(held, config); err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	directory, err := RuntimeDir(store.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return m, base
}

// testMachine saves a machine with its password and overlay files.
func testMachine(t *testing.T, m *Manager, name, subnet string, initialized bool) *Machine {
	t.Helper()
	held, err := m.acquireState(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	config, err := m.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	id, _ := NewInstanceID()
	mac, _ := newMAC()
	machine := &Machine{SchemaVersion: SchemaVersion, Name: name, InstanceID: id, BaseID: config.DefaultBaseID, State: "stopped", User: "farrow",
		Initialized: initialized, CPU: DefaultCPU, MemoryBytes: DefaultMemoryBytes, DiskBytes: DefaultDiskBytes, MAC: mac,
		Network: NetworkFor(netip.MustParsePrefix(subnet)), Clipboard: true, Version: "27.0", Build: "26A428", CreatedAt: time.Now().UTC()}
	if !initialized {
		machine.State = "prepared"
	}
	if err := m.Store.SavePassword(name, "fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SaveMachine(held, machine); err != nil {
		t.Fatal(err)
	}
	dir, _ := m.Store.MachinePath(name)
	for _, file := range []string{"disk.asif", "machine-id.bin", "auxiliary-storage.bin"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("overlay"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if initialized {
		if _, _, err := ensureSSHKey(dir); err != nil {
			t.Fatal(err)
		}
		public, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ssh.NewPublicKey(public)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "known_hosts"), []byte(knownhosts.Line([]string{machine.HostKeyAlias()}, key)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// A machine that booted has a request marker and the runner's lock file.
		for file, content := range map[string]string{"boot-requested": id + "\n", "runner.lock": ""} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return machine
}

// fakeRuntime answers the runner RPC for one machine and holds its runner
// lock while "running", like the real runner process.
type fakeRuntime struct {
	mu       sync.Mutex
	state    string
	requests []string
	// ignoreGraceful keeps running after a normal stop request.
	ignoreGraceful bool
	// refuseGraceful fails a normal stop request, like a VM still starting.
	refuseGraceful bool
	listener       net.Listener
	held           *lock.File
	lockPath       string
}

func startFakeRuntime(t *testing.T, m *Manager, machine *Machine) *fakeRuntime {
	t.Helper()
	socket, err := m.socket(machine.Name, true)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := m.Store.MachinePath(machine.Name)
	f := &fakeRuntime{state: "running", listener: listener, lockPath: filepath.Join(dir, "runner.lock")}
	if f.held, err = lock.TryAcquire(f.lockPath, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.exit() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, machine.InstanceID)
		}
	}()
	return f
}

func (f *fakeRuntime) serve(conn net.Conn, instance string) {
	defer func() { _ = conn.Close() }()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var request struct {
		Instance string `json:"instance"`
		Method   string `json:"method"`
		Force    bool   `json:"force"`
	}
	_ = json.Unmarshal(line, &request)
	f.mu.Lock()
	f.requests = append(f.requests, request.Method+map[bool]string{true: ":force", false: ""}[request.Force])
	reply := map[string]any{"ok": true, "instance": instance, "state": f.state, "pid": os.Getpid()}
	switch {
	case request.Method == "stop" && !request.Force && f.refuseGraceful:
		reply = map[string]any{"ok": false, "error": map[string]string{"code": "stop_failed", "message": "the VM cannot stop normally yet"}}
	case request.Method == "stop" && (request.Force || !f.ignoreGraceful):
		f.state = "stopped"
		reply["state"] = f.state
		go func() { time.Sleep(50 * time.Millisecond); f.exit() }()
	}
	f.mu.Unlock()
	data, _ := json.Marshal(reply)
	_, _ = conn.Write(append(data, '\n'))
}

// exit closes the socket and releases the lock, like a runner process exit.
func (f *fakeRuntime) exit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listener != nil {
		_ = f.listener.Close()
		f.listener = nil
	}
	if f.held != nil {
		_ = f.held.Release()
		f.held = nil
	}
}

func (f *fakeRuntime) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func requireMac(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || HostSupported() != nil {
		t.Skip("requires macOS 27 on Apple Silicon")
	}
}

func holdRunnerLockFile(t *testing.T, path string) *lock.File {
	t.Helper()
	held, err := lock.TryAcquire(path, false)
	if err != nil {
		t.Fatal(err)
	}
	return held
}
