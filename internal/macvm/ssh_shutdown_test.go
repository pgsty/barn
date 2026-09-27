package macvm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/lock"
	"golang.org/x/crypto/ssh"
)

func TestShutdownSubmissionUsesNoninteractiveSudoAndAcceptsTransportExit(t *testing.T) {
	for _, mode := range []string{"exit-zero", "disconnect", "command-rejected", "unresponsive"} {
		t.Run(mode, func(t *testing.T) {
			_, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := ssh.NewSignerFromKey(private)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			command := make(chan string, 1)
			serverDone := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer func() { _ = conn.Close() }()
				config := &ssh.ServerConfig{NoClientAuth: true}
				config.AddHostKey(signer)
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					serverDone <- err
					return
				}
				defer func() { _ = server.Close() }()
				go ssh.DiscardRequests(requests)
				channelRequest := <-channels
				if channelRequest == nil {
					serverDone <- errors.New("missing session")
					return
				}
				channel, requests, err := channelRequest.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer func() { _ = channel.Close() }()
				request := <-requests
				if request == nil || request.Type != "exec" {
					serverDone <- errors.New("missing exec")
					return
				}
				var payload struct{ Command string }
				if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
					serverDone <- err
					return
				}
				command <- payload.Command
				_ = request.Reply(true, nil)
				switch mode {
				case "exit-zero":
					_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				case "command-rejected":
					_, _ = channel.Stderr().Write([]byte("sudo: a password is required\n"))
					_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
				case "unresponsive":
					_ = server.Wait()
				}
				serverDone <- nil
			}()
			client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{User: "test", HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()), Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			err = submitSSHShutdown(ctx, client)
			if mode == "command-rejected" {
				if err == nil || !strings.Contains(err.Error(), "password is required") {
					t.Fatalf("lost command failure: %v", err)
				}
			} else if mode == "unresponsive" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lost shutdown deadline: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := <-command; got != "sudo -n /sbin/shutdown -h now" {
				t.Fatalf("unexpected shutdown command: %q", got)
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStopSSHSubmissionStillRequiresStoppedRunnerAndReleasedLock(t *testing.T) {
	for _, scenario := range []string{"submitted-and-stopped", "disconnected-but-running", "stopped-but-locked"} {
		t.Run(scenario, func(t *testing.T) {
			m, _, slot, dir := lifecycleSlot(t)
			m.Runner, _ = lifecycleRunner(t, "shutdown")
			state := filepath.Join(t.TempDir(), "runtime-state")
			marker := filepath.Join(t.TempDir(), "native-stop")
			t.Setenv("FARROW_TEST_LIFECYCLE_STATE", state)
			t.Setenv("FARROW_TEST_LIFECYCLE_STOP_MARKER", marker)
			if err := os.WriteFile(state, []byte("running"), 0600); err != nil {
				t.Fatal(err)
			}
			runnerLock, err := lock.TryAcquire(filepath.Join(dir, "runner.lock"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = runnerLock.Release() }()
			called := 0
			shutdown := func(context.Context, *Slot) error {
				called++
				if scenario != "disconnected-but-running" {
					if err := os.WriteFile(state, []byte("stopped"), 0600); err != nil {
						return err
					}
				}
				if scenario == "submitted-and-stopped" {
					return runnerLock.Release()
				}
				return nil // Includes EOF/no exit status; never proves shutdown.
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err = m.stopRuntimeWithShutdown(ctx, slot, false, shutdown)
			if scenario == "submitted-and-stopped" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("false stopped success: %v", err)
			}
			if called != 1 {
				t.Fatalf("SSH submission count=%d", called)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("successful SSH submission also requested native shutdown")
			}
		})
	}
}

func TestStopSelectsNativeForUninitializedForceAndSSHFailure(t *testing.T) {
	for _, scenario := range []string{"uninitialized", "force", "ssh-unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			m, _, slot, dir := lifecycleSlot(t)
			// A real runner leaves this inode behind after releasing its lock.
			if err := os.WriteFile(filepath.Join(dir, "runner.lock"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			m.Runner, _ = lifecycleRunner(t, "shutdown")
			state := filepath.Join(t.TempDir(), "runtime-state")
			marker := filepath.Join(t.TempDir(), "native-stop")
			t.Setenv("FARROW_TEST_LIFECYCLE_STATE", state)
			t.Setenv("FARROW_TEST_LIFECYCLE_STOP_MARKER", marker)
			t.Setenv("FARROW_TEST_LIFECYCLE_AUTO_STOP", "1")
			if err := os.WriteFile(state, []byte("running"), 0600); err != nil {
				t.Fatal(err)
			}
			slot.Initialized = scenario != "uninitialized"
			called := 0
			shutdown := func(context.Context, *Slot) error { called++; return errors.New("pinned SSH unavailable") }
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := m.stopRuntimeWithShutdown(ctx, slot, scenario == "force", shutdown); err != nil {
				t.Fatal(err)
			}
			if (called == 1) != (scenario == "ssh-unavailable") {
				t.Fatalf("unexpected SSH submission count=%d", called)
			}
			wire, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			var args []string
			if err := json.Unmarshal(wire, &args); err != nil {
				t.Fatal(err)
			}
			forceSeen := false
			for _, arg := range args {
				if arg == "--force" {
					forceSeen = true
				}
			}
			if forceSeen != (scenario == "force") {
				t.Fatalf("native force mismatch: %v", args)
			}
		})
	}
}
