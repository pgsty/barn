package macvm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"golang.org/x/crypto/ssh"
	"net"
	"strings"
	"testing"
	"time"
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
			err = submitSSHShutdown(ctx, client, "/sbin/shutdown -h now")
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
