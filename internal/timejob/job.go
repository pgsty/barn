// Package timejob installs an opt-in, per-user OS timer for one-shot clock checks.
package timejob

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/lock"
)

const label = "com.pgsty.barn.time"
const unit = "barn-time-sync"
const marker = "Managed by barn time auto"

type Manager struct {
	OS         string
	Home       string
	ConfigHome string
	UID        int
	Executable string
	DataRoot   string
	Path       string
	Runner     execx.Runner
}

type Status struct {
	Installed       bool     `json:"installed"`
	Active          bool     `json:"active"`
	Scheduler       string   `json:"scheduler"`
	DataRoot        string   `json:"data_root,omitempty"`
	Files           []string `json:"files"`
	IntervalSeconds int      `json:"interval_seconds"`
}

func Current(root string) (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Manager{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return Manager{}, err
	}
	// Keep the executable's symlink spelling when it resolves to this running
	// binary so package-manager upgrades can replace the target without stranding
	// the scheduled command, while never selecting an unrelated binary from PATH.
	if candidate, err := execPath(os.Args[0]); err == nil && sameExecutable(candidate, executable) {
		executable = candidate
	}
	return Manager{OS: runtime.GOOS, Home: home, ConfigHome: os.Getenv("XDG_CONFIG_HOME"), UID: os.Getuid(), Executable: executable, DataRoot: root, Path: os.Getenv("PATH"), Runner: execx.OSRunner{Timeout: 10 * time.Second}}, nil
}

func (m Manager) paths() ([]string, error) {
	switch m.OS {
	case "darwin":
		return []string{filepath.Join(m.Home, "Library", "LaunchAgents", label+".plist")}, nil
	case "linux":
		config := m.ConfigHome
		if config == "" {
			config = filepath.Join(m.Home, ".config")
		}
		if !filepath.IsAbs(config) {
			return nil, errors.New("XDG_CONFIG_HOME must be absolute for the time synchronization timer")
		}
		base := filepath.Join(config, "systemd", "user", unit)
		return []string{base + ".service", base + ".timer"}, nil
	default:
		return nil, fmt.Errorf("time synchronization timers are unavailable on %s", m.OS)
	}
}

func validValue(value string) bool { return !strings.ContainsAny(value, "\x00\r\n") }

func xmlText(value string) string {
	var buffer bytes.Buffer
	_ = xml.EscapeText(&buffer, []byte(value))
	return buffer.String()
}

func systemdText(value string, command bool) string {
	value = strings.ReplaceAll(value, "%", "%%")
	if command {
		value = strings.ReplaceAll(value, "$", "$$")
	}
	return strconv.Quote(value)
}

func (m Manager) render() ([][]byte, error) {
	for _, value := range []string{m.Executable, m.DataRoot, m.Path} {
		if !validValue(value) {
			return nil, errors.New("time synchronization timer paths contain a control character")
		}
	}
	if !filepath.IsAbs(m.Executable) || !filepath.IsAbs(m.DataRoot) {
		return nil, errors.New("time synchronization timer requires absolute executable and data paths")
	}
	root := "BARN_HOME_BASE64=" + base64.StdEncoding.EncodeToString([]byte(m.DataRoot))
	if m.OS == "darwin" {
		return [][]byte{fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- %s; %s -->
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>time</string><string>sync</string><string>--automatic</string><string>--timeout</string><string>45s</string></array>
<key>EnvironmentVariables</key><dict><key>BARN_HOME</key><string>%s</string><key>PATH</key><string>%s</string></dict>
<key>RunAtLoad</key><true/>
<key>StartCalendarInterval</key><dict/>
<key>ProcessType</key><string>Background</string>
<key>StandardOutPath</key><string>/dev/null</string>
<key>StandardErrorPath</key><string>/dev/null</string>
</dict></plist>
`, marker, root, label, xmlText(m.Executable), xmlText(m.DataRoot), xmlText(m.Path))}, nil
	}
	return [][]byte{
		fmt.Appendf(nil, `# %s; %s
[Unit]
Description=Check Barn guest clocks against the host
[Service]
Type=oneshot
Environment=%s
Environment=%s
ExecStart=%s time sync --automatic --timeout 45s
TimeoutStartSec=55s
`, marker, root, systemdText("BARN_HOME="+m.DataRoot, false), systemdText("PATH="+m.Path, false), systemdText(m.Executable, true)),
		fmt.Appendf(nil, `# %s; %s
[Unit]
Description=Check Barn guest clocks every minute and after resume
[Timer]
OnCalendar=*-*-* *:*:00
OnActiveSec=5s
AccuracySec=1s
Persistent=true
[Install]
WantedBy=timers.target
`, marker, root),
	}, nil
}

func readOwned(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect time synchronization job %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, fmt.Errorf("time synchronization job is not a bounded regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(data, []byte(marker+"; BARN_HOME_BASE64=")) {
		return nil, fmt.Errorf("will not change %s: it is not a Barn time synchronization job", path)
	}
	return data, nil
}

func savedRoot(data []byte) string {
	const prefix = "BARN_HOME_BASE64="
	_, tail, found := strings.Cut(string(data), prefix)
	if !found {
		return ""
	}
	fields := strings.Fields(tail)
	if len(fields) == 0 {
		return ""
	}
	root, _ := base64.StdEncoding.DecodeString(fields[0])
	return string(root)
}

func (m Manager) domain() string { return fmt.Sprintf("gui/%d", m.UID) }

func (m Manager) active(ctx context.Context) (bool, error) {
	var result execx.Result
	var err error
	if m.OS == "darwin" {
		result, err = m.Runner.Run(ctx, "launchctl", "print", m.domain()+"/"+label)
		if err != nil && (result.ExitCode == 113 || result.ExitCode == 3) {
			return false, nil
		}
	} else {
		result, err = m.Runner.Run(ctx, "systemctl", "--user", "is-active", unit+".timer")
		if err != nil && (result.ExitCode == 3 || result.ExitCode == 4) {
			return false, nil
		}
	}
	if err != nil {
		return false, fmt.Errorf("inspect time synchronization scheduler: %w", err)
	}
	return true, nil
}

func (m Manager) Status(ctx context.Context) (Status, error) {
	paths, err := m.paths()
	result := Status{Files: paths, IntervalSeconds: 60, Scheduler: "systemd user timer"}
	if err != nil {
		return result, err
	}
	if m.OS == "darwin" {
		result.Scheduler = "launchd user agent"
	}
	result.Installed = true
	for _, path := range paths {
		data, err := readOwned(path)
		if err != nil {
			return result, err
		}
		if data == nil {
			result.Installed = false
		} else if result.DataRoot == "" {
			result.DataRoot = savedRoot(data)
		}
	}
	result.Active, err = m.active(ctx)
	return result, err
}

func (m Manager) Change(ctx context.Context, enable bool) (_ Status, returnErr error) {
	paths, err := m.paths()
	if err != nil {
		return Status{}, err
	}
	parent := filepath.Dir(paths[0])
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Status{}, fmt.Errorf("create time synchronization job directory: %w", err)
	}
	held, err := lock.Acquire(ctx, filepath.Join(parent, ".barn-time.lock"), false)
	if err != nil {
		return Status{}, err
	}
	defer func() { returnErr = lock.JoinRelease(returnErr, held, "time scheduler lock") }()
	for _, path := range paths {
		if _, err := readOwned(path); err != nil {
			return Status{}, err
		}
	}
	if enable {
		info, err := os.Stat(m.Executable)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return Status{}, fmt.Errorf("time synchronization executable is unavailable: %s", m.Executable)
		}
		data, err := m.render()
		if err != nil {
			return Status{}, err
		}
		for index, path := range paths {
			if err := fsutil.AtomicWrite(path, data[index], 0o644); err != nil {
				return Status{}, err
			}
		}
	}
	if m.OS == "darwin" {
		active, err := m.active(ctx)
		if err != nil {
			return Status{}, err
		}
		if active {
			if _, err := m.Runner.Run(ctx, "launchctl", "bootout", m.domain()+"/"+label); err != nil {
				return Status{}, fmt.Errorf("unload time synchronization job: %w", err)
			}
		}
		if enable {
			if _, err := m.Runner.Run(ctx, "launchctl", "bootstrap", m.domain(), paths[0]); err != nil {
				return Status{}, fmt.Errorf("load time synchronization job: %w", err)
			}
		}
	} else {
		if enable {
			if _, err := m.Runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
				return Status{}, fmt.Errorf("reload time synchronization units: %w", err)
			}
			if _, err := m.Runner.Run(ctx, "systemctl", "--user", "enable", "--now", unit+".timer"); err != nil {
				return Status{}, fmt.Errorf("enable time synchronization timer: %w", err)
			}
		} else {
			active, err := m.active(ctx)
			if err != nil {
				return Status{}, err
			}
			_, statErr := os.Stat(paths[1])
			// disable --now also removes the timer's enablement symlink.
			if active || statErr == nil {
				if _, err := m.Runner.Run(ctx, "systemctl", "--user", "disable", "--now", unit+".timer"); err != nil {
					return Status{}, fmt.Errorf("disable time synchronization timer: %w", err)
				}
				if _, err := m.Runner.Run(ctx, "systemctl", "--user", "stop", unit+".service"); err != nil {
					return Status{}, fmt.Errorf("stop time synchronization check: %w", err)
				}
			}
		}
	}
	if !enable {
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return Status{}, fmt.Errorf("remove time synchronization job: %w", err)
			}
		}
		if m.OS == "linux" {
			if _, err := m.Runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
				return Status{}, fmt.Errorf("reload time synchronization units: %w", err)
			}
		}
	}
	return m.Status(ctx)
}
