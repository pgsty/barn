package macvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestInitializedSSHMissingKeyNeverRegenerates(t *testing.T) {
	m, _, slot, dir := lifecycleSlot(t)
	_, err := m.readySSH(context.Background(), slot, SlotPreference{}, "")
	if err == nil || !strings.Contains(err.Error(), "restore") || !strings.Contains(err.Error(), "no replacement key") || !strings.Contains(err.Error(), "farrow mac start mac1 --no-wait") {
		t.Fatalf("missing actionable key failure: %v", err)
	}
	for _, name := range []string{"id_ed25519", "id_ed25519.pub", "known_hosts"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("readiness generated %s for initialized guest", name)
		}
	}
}

func TestSSHReadinessClassifiesRealHandshakeRejections(t *testing.T) {
	for _, mode := range []string{"host-key", "authentication"} {
		t.Run(mode, func(t *testing.T) {
			signer, _, err := ensureSSHKey(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				config := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
					return nil, errors.New("authentication rejected")
				}}
				config.AddHostKey(signer)
				server, _, _, err := ssh.NewServerConn(conn, config)
				if err == nil {
					_ = server.Close()
				}
			}()
			verify := ssh.FixedHostKey(signer.PublicKey())
			if mode == "host-key" {
				other, _, err := ensureSSHKey(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				hosts := filepath.Join(t.TempDir(), "known_hosts")
				if err := os.WriteFile(hosts, []byte(knownhosts.Line([]string{listener.Addr().String()}, other.PublicKey())+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				verify, err = knownhosts.New(hosts)
				if err != nil {
					t.Fatal(err)
				}
			}
			client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{User: "test", Auth: []ssh.AuthMethod{ssh.Password("synthetic")}, HostKeyCallback: verify, Timeout: time.Second})
			if err == nil {
				_ = client.Close()
				t.Fatal("fixture unexpectedly authenticated")
			}
			if permanentSSHError(&Slot{Name: "mac2", User: "test"}, err) == nil {
				t.Fatalf("real %s rejection would be retried: %v", mode, err)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("SSH fixture did not exit")
			}
		})
	}
}

func TestInitializedSSHMissingPinPreservesPrivateKey(t *testing.T) {
	m, _, slot, dir := lifecycleSlot(t)
	if _, _, err := ensureSSHKey(dir); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "id_ed25519"))
	if err := m.checkSSHMaterial(slot); err == nil || !strings.Contains(err.Error(), "pinned SSH host key") {
		t.Fatalf("missing actionable pin failure: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "id_ed25519"))
	if string(before) != string(after) {
		t.Fatal("pin failure changed key")
	}
}

func TestSSHReadinessDistinguishesPermanentAndTransientFailures(t *testing.T) {
	slot := &Slot{Name: "mac2", User: "testuser", Initialized: true}
	for _, err := range []error{
		fmt.Errorf("ssh: handshake failed: %w", &knownhosts.KeyError{}),
		fmt.Errorf("ssh: handshake failed: %w", &knownhosts.RevokedError{}),
		errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"),
	} {
		actionable := permanentSSHError(slot, err)
		if actionable == nil || !strings.Contains(actionable.Error(), "farrow mac open mac2") {
			t.Fatalf("permanent failure would retry: %v", err)
		}
	}
	for _, err := range []error{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, context.DeadlineExceeded, errors.New("ssh: handshake failed: EOF")} {
		if permanentSSHError(slot, err) != nil {
			t.Fatalf("transient failure became permanent: %v", err)
		}
	}
	reason := sshWaitReason(errors.New("connection refused\n\tsecret-value"), "secret-value")
	if reason != "connection refused [redacted]" {
		t.Fatalf("unexpected wait reason: %s", reason)
	}
}

func TestInitializedSSHReadinessDoesNotRewriteIdentityFiles(t *testing.T) {
	m, _, slot, dir := lifecycleSlot(t)
	signer, _, err := ensureSSHKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "known_hosts"), []byte(knownhosts.Line([]string{"10.10.20.10"}, signer.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(dir, "id_ed25519.pub")
	if err := os.Remove(publicPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	m.Runner, _, _ = uxRuntimeRunner(t, "unavailable")
	if _, err := m.readySSH(context.Background(), slot, SlotPreference{}, ""); err == nil {
		t.Fatal("readiness accepted an unavailable runtime")
	}
	if _, err := os.Stat(publicPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initialized readiness recreated a key file: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "id_ed25519"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("initialized readiness changed the private key: %v", err)
	}
}
