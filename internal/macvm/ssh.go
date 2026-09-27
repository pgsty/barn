package macvm

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pgsty/farrow/internal/fsutil"
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

func NewPassword() (string, error) {
	data := make([]byte, 30)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "Fr9!" + base64.RawURLEncoding.EncodeToString(data), nil
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
func (m *Manager) checkSSHMaterial(slot *Slot) error {
	if !slot.Initialized {
		return nil
	}
	_, _, err := m.sshIdentity(slot)
	return err
}

func (m *Manager) sshIdentity(slot *Slot) (ssh.Signer, ssh.HostKeyCallback, error) {
	dir, err := m.Store.Path("slots", slot.Name)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "id_ed25519"))
	if err != nil {
		return nil, nil, fmt.Errorf("%s SSH private key is unavailable; restore %s from your backup, or run 'farrow mac start %s --no-wait' then 'farrow mac open %s' to repair SSH access in the guest; no replacement key was generated: %w", slot.Name, filepath.Join(dir, "id_ed25519"), slot.Name, slot.Name, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s SSH private key is invalid; restore the original key from backup, or run 'farrow mac start %s --no-wait' then 'farrow mac open %s' to repair SSH access in the guest; no replacement key was generated: %w", slot.Name, slot.Name, slot.Name, err)
	}
	hosts := filepath.Join(dir, "known_hosts")
	data, err = os.ReadFile(hosts)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return nil, nil, fmt.Errorf("%s pinned SSH host key is unavailable; restore %s from backup, or run 'farrow mac start %s --no-wait' then 'farrow mac open %s' and verify the guest host key before repairing the pin", slot.Name, hosts, slot.Name, slot.Name)
	}
	verify, err := knownhosts.New(hosts)
	if err != nil {
		return nil, nil, fmt.Errorf("%s pinned SSH host key file is invalid; restore %s from backup, or run 'farrow mac start %s --no-wait' then 'farrow mac open %s' and verify the guest host key before repairing the pin: %w", slot.Name, hosts, slot.Name, slot.Name, err)
	}
	return signer, verify, nil
}

func permanentSSHError(slot *Slot, err error) error {
	var keyError *knownhosts.KeyError
	var revoked *knownhosts.RevokedError
	if errors.As(err, &keyError) || errors.As(err, &revoked) {
		return fmt.Errorf("%s SSH host key does not match its saved trust; inspect 'farrow mac open %s' and verify the guest identity before repairing known_hosts; the saved pin was preserved: %w", slot.Name, slot.Name, err)
	}
	// x/crypto/ssh has no exported client-authentication error type. Match its
	// terminal authentication error, not disconnects or transient TCP failures.
	if strings.Contains(err.Error(), "ssh: unable to authenticate, attempted methods ") && strings.Contains(err.Error(), ", no supported methods remain") {
		return fmt.Errorf("%s SSH authentication was rejected for user %s; open 'farrow mac open %s' to check the account and authorized_keys, and restore the original local SSH private key if it changed; use 'farrow mac password %s' only for GUI login: %w", slot.Name, slot.User, slot.Name, slot.Name, err)
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

// shellQuote preserves argument boundaries, including empty strings, for SSH.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func QuoteCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func dialSSH(ctx context.Context, address, username string, auth []ssh.AuthMethod, verify ssh.HostKeyCallback) (*ssh.Client, error) {
	dialer := net.Dialer{Timeout: 8 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, "22"))
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

func guestBootstrapScript(username, publicKey, slot string) string {
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
/usr/sbin/scutil --set ComputerName ` + shellQuote("farrow-"+slot) + `
/usr/sbin/scutil --set LocalHostName ` + shellQuote("farrow-"+slot) + `
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

func (m *Manager) readySSH(ctx context.Context, slot *Slot, pref SlotPreference, password string) (SSHObservation, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	directory, err := m.Store.Path("slots", slot.Name)
	if err != nil {
		return SSHObservation{}, err
	}
	var signer ssh.Signer
	var public []byte
	var verify ssh.HostKeyCallback
	if slot.Initialized {
		signer, verify, err = m.sshIdentity(slot)
	} else {
		signer, public, err = ensureSSHKey(directory)
	}
	if err != nil {
		return SSHObservation{}, err
	}
	hosts := filepath.Join(directory, "known_hosts")
	var last string
	var lastReported string
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	attempt := 0
	for {
		before, err := m.status(ctx, slot)
		if err != nil {
			return SSHObservation{}, fmt.Errorf("VM identity unavailable during SSH readiness: %w", err)
		}
		if before.State != "running" || before.PID <= 0 {
			return SSHObservation{}, fmt.Errorf("VM is %s during SSH readiness", before.State)
		}
		var captured ssh.PublicKey
		if !slot.Initialized {
			_, statErr := os.Stat(hosts)
			if statErr == nil {
				verify, err = knownhosts.New(hosts)
				if err != nil {
					return SSHObservation{}, err
				}
			} else if errors.Is(statErr, os.ErrNotExist) {
				verify = func(_ string, _ net.Addr, key ssh.PublicKey) error {
					if err := m.verifyFirstSSHSource(ctx, slot, pref); err != nil {
						return err
					}
					captured = key
					return nil
				}
			} else {
				return SSHObservation{}, fmt.Errorf("read pinned guest SSH host key: %w", statErr)
			}
		}
		auth := []ssh.AuthMethod{ssh.PublicKeys(signer)}
		if !slot.Initialized && password != "" {
			auth = append(auth, ssh.Password(password), ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = password
				}
				return answers, nil
			}))
		}
		client, dialErr := dialSSH(ctx, pref.IP, slot.User, auth, verify)
		if dialErr == nil {
			after, statusErr := m.status(ctx, slot)
			if statusErr != nil {
				_ = client.Close()
				return SSHObservation{}, statusErr
			}
			if after.State != "running" || after.PID != before.PID {
				_ = client.Close()
				return SSHObservation{}, errors.New("VM process changed during SSH readiness; refusing first SSH trust")
			}
			if captured != nil {
				line := knownhosts.Line([]string{pref.IP}, captured) + "\n"
				if err = fsutil.AtomicCreate(hosts, []byte(line), 0600); err != nil {
					_ = client.Close()
					return SSHObservation{}, err
				}
			}
			if !slot.Initialized {
				script := guestBootstrapScript(slot.User, string(public), slot.Name)
				if _, sudoErr := sshScript(ctx, client, "sudo -n /usr/bin/true", nil); sudoErr == nil {
					_, err = sshScript(ctx, client, "sudo -n /bin/sh -s", strings.NewReader(script))
				} else {
					_, err = sshScript(ctx, client, "sudo -k -S -p '' /bin/sh -s", strings.NewReader(password+"\n"+script))
				}
				_ = client.Close()
				if err != nil {
					return SSHObservation{}, err
				}
				verify, err = knownhosts.New(hosts)
				if err != nil {
					return SSHObservation{}, err
				}
				client, err = dialSSH(ctx, pref.IP, slot.User, []ssh.AuthMethod{ssh.PublicKeys(signer)}, verify)
				if err != nil {
					return SSHObservation{}, fmt.Errorf("verify installed SSH public key: %w", err)
				}
				if err = disableSSHPasswords(ctx, client); err != nil {
					_ = client.Close()
					return SSHObservation{}, err
				}
			}
			observation, err := observeSSH(ctx, client, slot.User)
			_ = client.Close()
			return observation, err
		}
		if permanent := permanentSSHError(slot, dialErr); permanent != nil {
			return SSHObservation{}, permanent
		}
		last = sshWaitReason(dialErr, password)
		attempt++
		if attempt == 1 || attempt%10 == 0 || last != lastReported {
			m.progress("Waiting for %s SSH at %s (%s): %s", slot.Name, pref.IP, time.Now().Format("15:04:05"), last)
			lastReported = last
		}
		select {
		case <-ctx.Done():
			return SSHObservation{}, fmt.Errorf("SSH readiness did not complete: %w; last attempt: %v", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

func (m *Manager) SSHCommand(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer, interactive bool) (int, error) {
	slot, err := m.Store.LoadSlot(name)
	if err != nil {
		return 0, err
	}
	if !slot.Initialized {
		return 0, fmt.Errorf("%s is not initialized; run farrow mac up %s", name, name)
	}
	if err := m.checkSSHMaterial(slot); err != nil {
		return 0, err
	}
	if _, err = m.status(ctx, slot); err != nil {
		return 0, fmt.Errorf("%s is offline; run farrow mac start %s: %w", name, name, err)
	}
	config, err := m.Store.LoadConfig()
	if err != nil {
		return 0, err
	}
	pref, err := config.Preference(name)
	if err != nil {
		return 0, err
	}
	dir, err := m.Store.Path("slots", name)
	if err != nil {
		return 0, err
	}
	sshArgs := []string{"-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + filepath.Join(dir, "known_hosts"), "-o", "GlobalKnownHostsFile=/dev/null", "-o", "ForwardAgent=no", "-o", "ConnectTimeout=10", "-i", filepath.Join(dir, "id_ed25519")}
	if interactive {
		sshArgs = append(sshArgs, "-t")
	}
	sshArgs = append(sshArgs, slot.User+"@"+pref.IP)
	if len(args) > 0 {
		sshArgs = append(sshArgs, QuoteCommand(args))
	}
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err = cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 0, err
	}
	return 0, nil
}
