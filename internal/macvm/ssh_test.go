package macvm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestOpenSSHArgsPinTheInstanceNotTheAddress(t *testing.T) {
	connection := Connection{Name: "dev", User: "farrow", Host: "10.10.30.10", Port: 22, PrivateKey: "/VM Lab/id_ed25519", KnownHosts: "/VM Lab/known_hosts", HostKeyAlias: "farrow-mac-abc"}
	args := connection.OpenSSHArgs(true)
	joined := strings.Join(args, " ")
	// OpenSSH splits -o values on whitespace: the path must arrive quoted.
	for _, want := range []string{"-F /dev/null", "StrictHostKeyChecking=yes", `UserKnownHostsFile="/VM Lab/known_hosts"`, "GlobalKnownHostsFile=/dev/null", "HostKeyAlias=farrow-mac-abc", "IdentitiesOnly=yes", "BatchMode=yes", "ForwardAgent=no", "-t"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if args[len(args)-1] != "farrow@10.10.30.10" {
		t.Fatalf("destination %q", args[len(args)-1])
	}
	if slices.Contains(connection.OpenSSHArgs(false), "-t") {
		t.Fatal("a command session requested a terminal")
	}
}

func TestPinnedHostKeyFollowsTheAlias(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	otherPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewPublicKey(otherPublic)
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(knownhosts.Line([]string{"farrow-mac-abc"}, key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	verify, err := pinnedHostKey(path, "farrow-mac-abc")
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"10.10.20.10:22", "10.10.99.10:22"} {
		remote, _ := net.ResolveTCPAddr("tcp", address)
		if err := verify(address, remote, key); err != nil {
			t.Fatalf("pinned key rejected at %s: %v", address, err)
		}
	}
	remote, _ := net.ResolveTCPAddr("tcp", "10.10.20.10:22")
	var keyErr *knownhosts.KeyError
	if err := verify("10.10.20.10:22", remote, other); !errors.As(err, &keyErr) {
		t.Fatalf("a different host key was accepted: %v", err)
	}
	if permanentSSHError(&Machine{Name: "dev", User: "farrow"}, verify("x", remote, other)) == nil {
		t.Fatal("a host key mismatch was retried as transient")
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pinnedHostKey(path, "farrow-mac-abc"); err == nil {
		t.Fatal("an empty pin file was accepted")
	}
}

func TestAliasKnownHostsRekeysLegacyPins(t *testing.T) {
	legacy := "10.10.20.10 ecdsa-sha2-nistp256 AAAAE2VjZHNh\n# comment\n@revoked * ssh-ed25519 AAAA\n"
	got := aliasKnownHosts(legacy, "farrow-mac-953f3ce8")
	want := "farrow-mac-953f3ce8 ecdsa-sha2-nistp256 AAAAE2VjZHNh\n# comment\n@revoked * ssh-ed25519 AAAA\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestGuestBootstrapNamesTheMachine(t *testing.T) {
	script := guestBootstrapScript("farrow", "ssh-ed25519 AAAA test", "dev")
	for _, want := range []string{"--set ComputerName 'dev'", "--set LocalHostName 'farrow-dev'", "NOPASSWD: ALL", "visudo -cf", "authorized_keys"} {
		if !strings.Contains(script, want) {
			t.Errorf("bootstrap lacks %q", want)
		}
	}
}

func TestSSHMaterialIsNeverRegenerated(t *testing.T) {
	m, _ := testManager(t)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	dir, _ := m.Store.MachinePath("dev")
	if err := os.Remove(filepath.Join(dir, "id_ed25519")); err != nil {
		t.Fatal(err)
	}
	if err := m.checkSSHMaterial(machine); err == nil || !strings.Contains(err.Error(), "no replacement key was generated") {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "id_ed25519")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a replacement key was generated")
	}
}

func TestGuestTransportRelaysThroughAppleNetcat(t *testing.T) {
	if _, err := os.Stat(netcatPath); err != nil || runtime.GOOS != "darwin" {
		t.Skip("requires Apple's " + netcatPath)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buffer := make([]byte, 5)
		if _, err := io.ReadFull(conn, buffer); err == nil {
			_, _ = conn.Write(append([]byte("echo:"), buffer...))
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	conn, err := dialGuest(context.Background(), "127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, reply); err != nil || string(reply) != "echo:hello" {
		t.Fatalf("reply %q %v", reply, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	// A refused port reports the relay's reason rather than a bare EOF.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	relay, err := dialGuest(ctx, "127.0.0.1", closedPort)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	if _, err := relay.Read(buffer); err == nil {
		t.Fatal("read from a refused connection")
	}
	if err := relay.(*processConn).failure(io.EOF); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("relay failure: %v", err)
	}
	_ = relay.Close()
}
