package macvm

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/openssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var usernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`)

func ValidUsername(name string) bool {
	return usernamePattern.MatchString(name) && name != "root" && name != "daemon" && name != "nobody" && !strings.HasPrefix(name, "_")
}

func DefaultUsername() string {
	var caller *user.User
	if os.Getuid() == 0 && os.Getenv("SUDO_UID") != "" {
		caller, _ = user.LookupId(os.Getenv("SUDO_UID"))
	} else {
		caller, _ = user.Current()
	}
	if caller != nil && ValidUsername(caller.Username) {
		return caller.Username
	}
	return "farrow"
}

func ensureSSHKey(directory string) (ssh.Signer, []byte, error) {
	privatePath := filepath.Join(directory, "id_ed25519")
	data, err := os.ReadFile(privatePath)
	if errors.Is(err, os.ErrNotExist) {
		_, key, keyErr := ed25519.GenerateKey(rand.Reader)
		if keyErr != nil {
			return nil, nil, keyErr
		}
		block, keyErr := ssh.MarshalPrivateKey(key, "")
		if keyErr != nil {
			return nil, nil, keyErr
		}
		data = pem.EncodeToMemory(block)
		if err = fsutil.AtomicCreate(privatePath, data, 0600); err != nil {
			return nil, nil, err
		}
	} else if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, nil, err
	}
	public := ssh.MarshalAuthorizedKey(signer.PublicKey())
	if err = fsutil.AtomicWrite(filepath.Join(directory, "id_ed25519.pub"), public, 0644); err != nil {
		return nil, nil, err
	}
	return signer, public, nil
}

// An initialized guest accepts only its existing public key. Generating a new
// private key would silently discard the only useful local recovery material.
func (m *Manager) checkSSHMaterial(machine *Machine) error {
	if !machine.Initialized {
		return nil
	}
	_, _, err := m.sshIdentity(machine)
	return err
}

// Connection names everything OpenSSH needs to reach one machine.
type Connection struct {
	Name         string `json:"name"`
	User         string `json:"user"`
	Host         string `json:"host"`
	Port         uint16 `json:"port"`
	PrivateKey   string `json:"private_key"`
	KnownHosts   string `json:"known_hosts"`
	HostKeyAlias string `json:"host_key_alias"`
}

func (m *Manager) connection(machine *Machine) (Connection, error) {
	dir, err := m.Store.MachinePath(machine.Name)
	if err != nil {
		return Connection{}, err
	}
	return Connection{Name: machine.Name, User: machine.User, Host: machine.Network.Address, Port: 22,
		PrivateKey: filepath.Join(dir, "id_ed25519"), KnownHosts: filepath.Join(dir, "known_hosts"), HostKeyAlias: machine.HostKeyAlias()}, nil
}

// OpenSSHArgs pins the instance host key through its alias, never the address.
func (c Connection) OpenSSHArgs(interactive bool) []string {
	// OpenSSH splits -o values on whitespace, so a FARROW_HOME with spaces
	// needs the path quoted.
	knownHosts, err := openssh.QuoteConfigValue(c.KnownHosts)
	if err != nil {
		knownHosts = c.KnownHosts
	}
	args := []string{"-F", "/dev/null", "-i", c.PrivateKey,
		"-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "GlobalKnownHostsFile=/dev/null", "-o", "HostKeyAlias=" + c.HostKeyAlias,
		"-o", "ForwardAgent=no", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR"}
	if interactive {
		args = append(args, "-t")
	}
	return append(args, c.User+"@"+c.Host)
}

func (m *Manager) sshIdentity(machine *Machine) (ssh.Signer, ssh.HostKeyCallback, error) {
	dir, err := m.Store.MachinePath(machine.Name)
	if err != nil {
		return nil, nil, err
	}
	repair := fmt.Sprintf("farrow mac start %s --no-wait, then farrow mac open %s", machine.Name, machine.Name)
	data, err := os.ReadFile(filepath.Join(dir, "id_ed25519"))
	if err != nil {
		return nil, nil, fmt.Errorf("%s SSH private key is unavailable; restore %s from your backup, or repair SSH access in the guest desktop (%s); no replacement key was generated: %w", machine.Name, filepath.Join(dir, "id_ed25519"), repair, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s SSH private key is invalid; restore the original key from backup, or repair SSH access in the guest desktop (%s); no replacement key was generated: %w", machine.Name, repair, err)
	}
	verify, err := pinnedHostKey(filepath.Join(dir, "known_hosts"), machine.HostKeyAlias())
	if err != nil {
		return nil, nil, fmt.Errorf("%s pinned SSH host key is unavailable; restore %s from backup, or verify the guest host key in its desktop (%s) before repairing the pin: %w", machine.Name, filepath.Join(dir, "known_hosts"), repair, err)
	}
	return signer, verify, nil
}

// pinnedHostKey checks the server key against the instance alias. Addresses
// may change with a subnet; the pinned instance key may not.
func pinnedHostKey(path, alias string) (ssh.HostKeyCallback, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("the pin file is empty")
	}
	verify, err := knownhosts.New(path)
	if err != nil {
		return nil, err
	}
	return func(_ string, remote net.Addr, key ssh.PublicKey) error {
		return verify(net.JoinHostPort(alias, "22"), remote, key)
	}, nil
}

func permanentSSHError(machine *Machine, err error) error {
	var keyError *knownhosts.KeyError
	var revoked *knownhosts.RevokedError
	if errors.As(err, &keyError) || errors.As(err, &revoked) {
		return fmt.Errorf("%s SSH host key does not match its saved pin; inspect the guest with farrow mac open %s and verify its identity before repairing known_hosts; the saved pin was preserved: %w", machine.Name, machine.Name, err)
	}
	// x/crypto/ssh has no exported client-authentication error type. Match its
	// terminal authentication error, not disconnects or transient TCP failures.
	if strings.Contains(err.Error(), "ssh: unable to authenticate, attempted methods ") && strings.Contains(err.Error(), ", no supported methods remain") {
		return fmt.Errorf("%s SSH authentication was rejected for user %s; open farrow mac open %s to check the account and authorized_keys, and restore the original local SSH private key if it changed: %w", machine.Name, machine.User, machine.Name, err)
	}
	return nil
}

func sshWaitReason(err error, password string) string {
	reason := err.Error()
	if password != "" {
		reason = strings.ReplaceAll(reason, password, "[redacted]")
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) > 500 {
		reason = reason[:500] + "…"
	}
	return reason
}

// shellQuote preserves an argument, including an empty one, for a guest shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// dialSSH opens an SSH client to the guest through dialGuest.
func dialSSH(ctx context.Context, address, username string, auth []ssh.AuthMethod, verify ssh.HostKeyCallback) (*ssh.Client, error) {
	conn, err := dialGuest(ctx, address, 22)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	sshConn, channels, requests, err := ssh.NewClientConn(conn, net.JoinHostPort(address, "22"), &ssh.ClientConfig{User: username, Auth: auth, HostKeyCallback: verify, Timeout: 15 * time.Second})
	if err != nil {
		// A handshake that never started is a connection failure; keep the
		// relay's reason. Key and authentication errors keep their type.
		if relay, ok := conn.(*processConn); ok && (errors.Is(err, io.EOF) || strings.Contains(err.Error(), "EOF")) {
			err = relay.failure(err)
		} else if errors.Is(err, os.ErrDeadlineExceeded) {
			err = fmt.Errorf("connect to %s: the SSH service has not answered yet", net.JoinHostPort(address, "22"))
		}
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(sshConn, channels, requests), nil
}

func sshScript(ctx context.Context, client *ssh.Client, command string, input io.Reader) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// NewSession and Start also wait for remote replies. Closing the client on
	// cancellation bounds those waits, as well as a command that never exits.
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	defer func() { _ = session.Close() }()
	var stdout, stderr bytes.Buffer
	session.Stdin = input
	session.Stdout = &stdout
	session.Stderr = &stderr
	err = session.Run(command)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("guest initialization command failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

type SSHObservation struct{ User, Version, Build string }

func guestBootstrapScript(username, publicKey, name string) string {
	return `set -eu
id ` + shellQuote(username) + ` | /usr/bin/grep -q '(admin)'
home=$(/usr/bin/dscl . -read /Users/` + username + ` NFSHomeDirectory | /usr/bin/awk '{print $2}')
/bin/mkdir -p "$home/.ssh"
/bin/chmod 700 "$home/.ssh"
/usr/bin/touch "$home/.ssh/authorized_keys"
/usr/bin/grep -qxF ` + shellQuote(strings.TrimSpace(publicKey)) + ` "$home/.ssh/authorized_keys" || /usr/bin/printf '%s\n' ` + shellQuote(strings.TrimSpace(publicKey)) + ` >> "$home/.ssh/authorized_keys"
/bin/chmod 600 "$home/.ssh/authorized_keys"
/usr/sbin/chown -R ` + shellQuote(username+":staff") + ` "$home/.ssh"
/bin/mkdir -p /private/etc/sudoers.d
sudoers=$(/usr/bin/mktemp /private/etc/sudoers.d/.farrow.XXXXXX)
trap '/bin/rm -f "$sudoers"' EXIT
/usr/bin/printf '%s\n' ` + shellQuote(username+" ALL=(ALL) NOPASSWD: ALL") + ` > "$sudoers"
/bin/chmod 440 "$sudoers"
/usr/sbin/visudo -cf "$sudoers" >/dev/null
/bin/mv "$sudoers" /private/etc/sudoers.d/80-farrow
/usr/sbin/scutil --set ComputerName ` + shellQuote(name) + `
/usr/sbin/scutil --set LocalHostName ` + shellQuote("farrow-"+name) + `
/usr/bin/pmset -a sleep 0 disksleep 0 displaysleep 0
`
}

func disableSSHPasswords(ctx context.Context, client *ssh.Client) error {
	_, err := sshScript(ctx, client, "sudo -n /bin/sh -s", strings.NewReader(`set -eu
/bin/mkdir -p /etc/ssh/sshd_config.d
/usr/bin/printf '%s\n' 'PasswordAuthentication no' 'KbdInteractiveAuthentication no' 'PermitRootLogin no' > /etc/ssh/sshd_config.d/000-farrow.conf
/usr/sbin/sshd -t
`))
	return err
}

func observeSSH(ctx context.Context, client *ssh.Client, expectedUser string) (SSHObservation, error) {
	output, err := sshScript(ctx, client, "/usr/bin/id -un; /usr/bin/sw_vers -productVersion; /usr/bin/sw_vers -buildVersion; sudo -n /usr/bin/id -u", nil)
	if err != nil {
		return SSHObservation{}, err
	}
	lines := strings.Split(output, "\n")
	if len(lines) != 4 || lines[0] != expectedUser || lines[3] != "0" {
		return SSHObservation{}, errors.New("guest account or passwordless sudo verification failed")
	}
	return SSHObservation{User: lines[0], Version: lines[1], Build: lines[2]}, nil
}

// readySSH waits for the guest's SSH service. On first boot it authenticates
// with the instance password, pins the host key, installs the SSH public key,
// passwordless sudo and hostname, then disables SSH passwords.
//
// First-contact trust comes from the network: this machine's private network
// exists only inside its own runner process, so apart from the host the guest
// is the only peer that can answer at its reserved address.
func (m *Manager) readySSH(ctx context.Context, machine *Machine, password string) (SSHObservation, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	directory, err := m.Store.MachinePath(machine.Name)
	if err != nil {
		return SSHObservation{}, err
	}
	var signer ssh.Signer
	var public []byte
	var verify ssh.HostKeyCallback
	if machine.Initialized {
		signer, verify, err = m.sshIdentity(machine)
	} else {
		signer, public, err = ensureSSHKey(directory)
	}
	if err != nil {
		return SSHObservation{}, err
	}
	hosts := filepath.Join(directory, "known_hosts")
	alias := machine.HostKeyAlias()
	var last, lastReported string
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	attempt := 0
	for {
		before, err := m.status(ctx, machine)
		if err != nil {
			return SSHObservation{}, fmt.Errorf("VM identity unavailable during SSH readiness: %w", err)
		}
		if before.State != "running" || before.PID <= 0 {
			return SSHObservation{}, fmt.Errorf("VM is %s during SSH readiness", before.State)
		}
		var captured ssh.PublicKey
		if !machine.Initialized {
			if _, statErr := os.Stat(hosts); statErr == nil {
				verify, err = pinnedHostKey(hosts, alias)
				if err != nil {
					return SSHObservation{}, err
				}
			} else if errors.Is(statErr, os.ErrNotExist) {
				verify = func(_ string, _ net.Addr, key ssh.PublicKey) error {
					captured = key
					return nil
				}
			} else {
				return SSHObservation{}, fmt.Errorf("read pinned guest SSH host key: %w", statErr)
			}
		}
		auth := []ssh.AuthMethod{ssh.PublicKeys(signer)}
		if !machine.Initialized && password != "" {
			auth = append(auth, ssh.Password(password), ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = password
				}
				return answers, nil
			}))
		}
		client, dialErr := dialSSH(ctx, machine.Network.Address, machine.User, auth, verify)
		if dialErr == nil {
			after, statusErr := m.status(ctx, machine)
			if statusErr != nil {
				_ = client.Close()
				return SSHObservation{}, statusErr
			}
			if after.State != "running" || after.PID != before.PID {
				_ = client.Close()
				return SSHObservation{}, errors.New("VM process changed during SSH readiness; refusing first SSH trust")
			}
			if captured != nil {
				line := knownhosts.Line([]string{alias}, captured) + "\n"
				if err = fsutil.AtomicCreate(hosts, []byte(line), 0600); err != nil {
					_ = client.Close()
					return SSHObservation{}, err
				}
			}
			if !machine.Initialized {
				script := guestBootstrapScript(machine.User, string(public), machine.Name)
				if _, sudoErr := sshScript(ctx, client, "sudo -n /usr/bin/true", nil); sudoErr == nil {
					_, err = sshScript(ctx, client, "sudo -n /bin/sh -s", strings.NewReader(script))
				} else {
					_, err = sshScript(ctx, client, "sudo -k -S -p '' /bin/sh -s", strings.NewReader(password+"\n"+script))
				}
				_ = client.Close()
				if err != nil {
					return SSHObservation{}, err
				}
				verify, err = pinnedHostKey(hosts, alias)
				if err != nil {
					return SSHObservation{}, err
				}
				client, err = dialSSH(ctx, machine.Network.Address, machine.User, []ssh.AuthMethod{ssh.PublicKeys(signer)}, verify)
				if err != nil {
					return SSHObservation{}, fmt.Errorf("verify installed SSH public key: %w", err)
				}
				if err = disableSSHPasswords(ctx, client); err != nil {
					_ = client.Close()
					return SSHObservation{}, err
				}
			}
			observation, err := observeSSH(ctx, client, machine.User)
			_ = client.Close()
			return observation, err
		}
		if permanent := permanentSSHError(machine, dialErr); permanent != nil {
			return SSHObservation{}, permanent
		}
		last = sshWaitReason(dialErr, password)
		attempt++
		if attempt == 1 || attempt%15 == 0 || last != lastReported {
			m.report("guest-ready", "Waiting for %s SSH at %s: %s", machine.Name, machine.Network.Address, last)
			lastReported = last
		}
		select {
		case <-ctx.Done():
			return SSHObservation{}, fmt.Errorf("SSH readiness did not complete: %w; last attempt: %v", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}
