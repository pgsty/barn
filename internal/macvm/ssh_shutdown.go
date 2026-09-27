package macvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// A successful return only means shutdown was submitted or the SSH transport
// closed during submission. stopRuntime must still prove the VM has stopped.
func (m *Manager) shutdownGuestSSH(ctx context.Context, slot *Slot) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	config, err := m.Store.LoadConfig()
	if err != nil {
		return fmt.Errorf("read shutdown network configuration: %w", err)
	}
	pref, err := config.Preference(slot.Name)
	if err != nil {
		return err
	}
	signer, verify, err := m.sshIdentity(slot)
	if err != nil {
		return err
	}
	var client *ssh.Client
	for {
		client, err = dialSSH(ctx, pref.IP, slot.User, []ssh.AuthMethod{ssh.PublicKeys(signer)}, verify)
		if err == nil {
			break
		}
		if ctx.Err() != nil || permanentSSHError(slot, err) != nil {
			return fmt.Errorf("connect to guest for shutdown: %w", err)
		}
		// Stop during boot may arrive just before sshd opens its listener.
		// Give it the same bounded shutdown deadline, keeping host pins intact.
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer func() { _ = client.Close() }()
	return submitSSHShutdown(ctx, client)
}

func submitSSHShutdown(ctx context.Context, client *ssh.Client) error {
	// Cover channel creation, exec acknowledgement and completion, including a
	// guest that stops answering before it closes the SSH connection.
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		return shutdownSubmissionError(ctx, err, "")
	}
	defer func() { _ = session.Close() }()
	var stderr bytes.Buffer
	session.Stdout, session.Stderr = io.Discard, &stderr
	err = session.Run("sudo -n /sbin/shutdown -h now")
	return shutdownSubmissionError(ctx, err, stderr.String())
}

func shutdownSubmissionError(ctx context.Context, err error, stderr string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var missingStatus *ssh.ExitMissingError
	if err == nil || errors.Is(err, io.EOF) || errors.As(err, &missingStatus) {
		return nil // The runner and slot lock, not SSH, prove shutdown completion.
	}
	detail := strings.TrimSpace(stderr)
	if len(detail) > 1024 {
		detail = detail[:1024]
	}
	return fmt.Errorf("submit guest shutdown: %w: %s", err, detail)
}
