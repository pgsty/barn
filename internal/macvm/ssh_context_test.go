package macvm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSSHScriptCancellationCoversChannelAndExecAcknowledgements(t *testing.T) {
	for _, phase := range []string{"open-channel", "exec-request", "running-command"} {
		t.Run(phase, func(t *testing.T) {
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
			waiting := make(chan struct{})
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
				request := <-channels
				if request == nil {
					serverDone <- errors.New("no channel request")
					return
				}
				if phase != "open-channel" {
					channel, requests, err := request.Accept()
					if err != nil {
						serverDone <- err
						return
					}
					defer func() { _ = channel.Close() }()
					exec := <-requests
					if exec == nil || exec.Type != "exec" {
						serverDone <- errors.New("no exec request")
						return
					}
					if phase == "running-command" {
						_ = exec.Reply(true, nil)
					}
				}
				close(waiting)
				_ = server.Wait() // The canceled client must close the transport.
				serverDone <- nil
			}()
			client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{User: "test", HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()), Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := sshScript(ctx, client, "/usr/bin/true", nil); result <- err }()
			select {
			case <-waiting:
			case err := <-serverDone:
				t.Fatalf("server did not reach %s: %v", phase, err)
			case <-time.After(3 * time.Second):
				t.Fatalf("never reached %s", phase)
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost at %s: %v", phase, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("context cancellation left SSH blocked at %s", phase)
			}
			select {
			case err := <-serverDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled script left the transport open")
			}
		})
	}
}
