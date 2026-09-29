package macvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pgsty/farrow/internal/lock"
	"github.com/pgsty/farrow/internal/sshconfig"
)

func (m *Manager) sshHome() (string, error) {
	if m.SSHHome != "" {
		return filepath.Abs(m.SSHHome)
	}
	return os.UserHomeDir()
}

// sshEntries lists every initialized machine; the others have no SSH yet.
func (m *Manager) sshEntries() ([]sshconfig.MacEntry, error) {
	machines, err := m.Store.usableMachines()
	if err != nil {
		return nil, err
	}
	entries := []sshconfig.MacEntry{}
	for i := range machines {
		if !machines[i].Initialized {
			continue
		}
		connection, err := m.connection(&machines[i])
		if err != nil {
			return nil, err
		}
		entries = append(entries, sshconfig.MacEntry{Name: connection.Name, User: connection.User, Host: connection.Host,
			Identity: connection.PrivateKey, KnownHosts: connection.KnownHosts, HostKeyAlias: connection.HostKeyAlias})
	}
	return entries, nil
}

// SSHConfig renders the fragment that makes ssh, scp and editors reach each
// machine by name with its own key and pinned host key.
func (m *Manager) SSHConfig() (string, error) {
	entries, err := m.sshEntries()
	if err != nil {
		return "", err
	}
	return sshconfig.RenderMac(m.Store.Root, entries)
}

// InstallSSHConfig publishes the fragment and one Include in ~/.ssh/config,
// replacing a fragment written for another Farrow home, and lets lifecycle
// commands keep it current again. With no ready machine it changes nothing in
// ~/.ssh: the first machine to become ready installs it.
func (m *Manager) InstallSSHConfig(ctx context.Context) (sshconfig.Result, error) {
	if err := m.setSSHConfigOff(ctx, false); err != nil {
		return sshconfig.Result{}, err
	}
	home, err := m.sshHome()
	if err != nil {
		return sshconfig.Result{}, err
	}
	entries, err := m.sshEntries()
	if err != nil || len(entries) == 0 {
		return sshconfig.Result{Action: "pending"}, err
	}
	return sshconfig.InstallMac(home, m.Store.Root, entries, true)
}

// RemoveSSHConfig removes only what InstallSSHConfig wrote, and lifecycle
// commands stop adding it back until InstallSSHConfig.
func (m *Manager) RemoveSSHConfig(ctx context.Context) (sshconfig.Result, error) {
	if err := m.setSSHConfigOff(ctx, true); err != nil {
		return sshconfig.Result{}, err
	}
	home, err := m.sshHome()
	if err != nil {
		return sshconfig.Result{}, err
	}
	return sshconfig.RemoveMac(home, m.Store.Root, true)
}

func (m *Manager) setSSHConfigOff(ctx context.Context, off bool) (retErr error) {
	held, err := m.acquireState(ctx, "mac ssh-config")
	if err != nil {
		return err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac ssh-config") }()
	config, err := m.ensureConfig(held)
	if err != nil || config.SSHConfigOff == off {
		return err
	}
	config.SSHConfigOff = off
	return m.Store.SaveConfig(held, config)
}

// refreshSSHConfig keeps an installed fragment current after a lifecycle
// change and installs it after the first machine becomes ready, unless the
// user removed it. A fragment written for another Farrow home is left alone.
// It returns a warning, never an error: the machine is already in its new
// state.
func (m *Manager) refreshSSHConfig() string {
	if config, err := m.Store.LoadConfig(); err == nil && config.SSHConfigOff {
		return ""
	}
	home, err := m.sshHome()
	if err != nil {
		return "SSH config was not updated: " + err.Error()
	}
	entries, err := m.sshEntries()
	var result sshconfig.Result
	if err == nil {
		result, err = sshconfig.InstallMac(home, m.Store.Root, entries, false)
	}
	switch {
	case err == nil && result.Changed && len(result.Shadowed) > 0:
		return ShadowedWarning(result.Shadowed)
	case err == nil || errors.Is(err, sshconfig.ErrMacForeign):
		return ""
	}
	return "SSH config was not updated: " + err.Error()
}

// ShadowedWarning explains machine names that ~/.ssh/config already uses.
func ShadowedWarning(names []string) string {
	return fmt.Sprintf("~/.ssh/config already uses %s for another host, so the Farrow entry answers only to the machine's address; farrow mac ssh NAME always works", strings.Join(names, ", "))
}
