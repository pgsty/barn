// Package sshconfig owns the narrowly marked OpenSSH Include integration.
package sshconfig

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/openssh"
)

const maxConfigBytes = 1 << 20

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
var aliasPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,252}$`)

// ValidName reports whether value is safe as both an SSH Host prefix and the
// basename of a Farrow-owned SSH configuration fragment.
func ValidName(value string) bool { return namePattern.MatchString(value) }

func safeOpenSSHPath(value string) bool {
	return filepath.IsAbs(value) && !strings.ContainsAny(value, "\r\n\x00$%*?[]")
}

type Entry struct {
	Name         string
	Node         string
	Aliases      []string
	User         string
	Host         string
	Port         uint16
	Identity     string
	KnownHosts   string
	HostKeyAlias string
}

type Result struct {
	Fragment string `json:"fragment"`
	Config   string `json:"config"`
	Changed  bool   `json:"changed"`
	Action   string `json:"action"`
	// Shadowed names are published by address only; see InstallMac.
	Shadowed []string `json:"shadowed,omitempty"`
}

func validateEntry(entry Entry) error {
	if entry.HostKeyAlias != "" && !aliasPattern.MatchString(entry.HostKeyAlias) {
		return errors.New("SSH host key alias is invalid")
	}
	if !namePattern.MatchString(entry.Name) || !namePattern.MatchString(entry.Node) || !namePattern.MatchString(entry.User) || strings.ContainsAny(entry.Host, "\r\n\x00$%") || entry.Host == "" || entry.Port == 0 || !safeOpenSSHPath(entry.Identity) || !safeOpenSSHPath(entry.KnownHosts) {
		return errors.New("SSH config entry name, connection, or paths are invalid")
	}
	for _, alias := range entry.Aliases {
		if !aliasPattern.MatchString(alias) {
			return fmt.Errorf("SSH config alias %q is invalid", alias)
		}
	}
	return nil
}

// marks separate independently managed fragments. The Linux deployment and
// the Mac machines each own one fragment and one Include block, so neither
// can adopt, rewrite or remove the other.
type marks struct{ begin, end, include string }

var (
	linuxMarks = marks{"# farrow:begin", "# farrow:end", "# farrow:include"}
	macMarks   = marks{"# farrow-mac:begin", "# farrow-mac:end", "# farrow-mac:include"}
)

func render(entries []Entry) (string, error) {
	var output strings.Builder
	output.WriteString(linuxMarks.begin)
	output.WriteByte('\n')
	for index, entry := range entries {
		identity, err := openssh.QuoteConfigValue(entry.Identity)
		if err != nil {
			return "", err
		}
		knownHosts, err := openssh.QuoteConfigValue(entry.KnownHosts)
		if err != nil {
			return "", err
		}
		patterns := make([]string, 0, len(entry.Aliases)+1)
		seen := make(map[string]struct{}, len(entry.Aliases)+1)
		for _, pattern := range append([]string{entry.Name + "-" + entry.Node}, entry.Aliases...) {
			if _, exists := seen[pattern]; exists {
				continue
			}
			seen[pattern] = struct{}{}
			patterns = append(patterns, pattern)
		}
		fmt.Fprintf(&output, "Host %s\n  HostName %s\n  User %s\n  Port %d\n  IdentityFile %s\n  UserKnownHostsFile %s\n  IdentitiesOnly yes\n  StrictHostKeyChecking accept-new\n", strings.Join(patterns, " "), entry.Host, entry.User, entry.Port, identity, knownHosts)
		if entry.HostKeyAlias != "" {
			fmt.Fprintf(&output, "  HostKeyAlias %s\n", entry.HostKeyAlias)
		}
		if index != len(entries)-1 {
			output.WriteByte('\n')
		}
	}
	output.WriteString(linuxMarks.end)
	output.WriteByte('\n')
	return output.String(), nil
}

func globalInsertOffset(config string) int {
	offset := 0
	for _, line := range strings.SplitAfter(config, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\n"))
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			fields := strings.Fields(trimmed)
			if len(fields) > 0 && (strings.EqualFold(fields[0], "Host") || strings.EqualFold(fields[0], "Match")) {
				return offset
			}
		}
		offset += len(line)
	}
	return len(config)
}

func installGlobalBlock(config, block string) string {
	without := strings.Replace(config, block, "", 1)
	offset := globalInsertOffset(without)
	prefix, suffix := without[:offset], without[offset:]
	if prefix != "" && !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	return prefix + block + suffix
}

func ensureSSHDir(home string) (string, error) {
	if home == "" || !safeOpenSSHPath(home) {
		return "", errors.New("SSH integration home must be absolute")
	}
	homeInfo, err := os.Lstat(home)
	if err != nil {
		return "", errors.New("SSH integration home is not a real directory")
	}
	homeStat, homeStatOK := homeInfo.Sys().(*syscall.Stat_t)
	if !homeInfo.IsDir() || homeInfo.Mode()&os.ModeSymlink != 0 || !homeStatOK || int(homeStat.Uid) != os.Geteuid() {
		return "", errors.New("SSH integration home is not a real directory")
	}
	directory := filepath.Join(home, ".ssh")
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", errors.New("~/.ssh is missing, unsafe, or writable by other users")
	}
	stat, statOK := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !statOK || int(stat.Uid) != os.Geteuid() {
		return "", errors.New("~/.ssh is missing, unsafe, or writable by other users")
	}
	return directory, nil
}

func readOptionalRegular(pathname string) ([]byte, bool, error) {
	info, err := os.Lstat(pathname)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	stat, statOK := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxConfigBytes || !statOK || int(stat.Uid) != os.Geteuid() || stat.Nlink != 1 {
		return nil, false, errors.New("SSH config target is not a bounded regular non-symlink file")
	}
	handle, err := os.Open(pathname)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		// The existing SSH config is read-only and bounded in memory; atomic
		// publication below handles every target Write, Sync, and Close error.
		_ = handle.Close()
	}()
	data, err := io.ReadAll(io.LimitReader(handle, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		return nil, false, errors.New("SSH config exceeds 1 MiB limit")
	}
	return data, true, nil
}

// managedElsewhere reports a config that is a symlink or hard link, as dotfile
// managers create. Farrow never rewrites such a file.
func managedElsewhere(pathname string) (bool, error) {
	info, err := os.Lstat(pathname)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode()&os.ModeSymlink != 0 || ok && info.Mode().IsRegular() && stat.Nlink > 1, nil
}

// includesFragment reports whether the (possibly linked) config already has an
// Include naming the fragment, as the user was asked to add by hand.
func includesFragment(configPath, fragmentName string) bool {
	handle, err := os.Open(configPath)
	if err != nil {
		return false
	}
	defer func() { _ = handle.Close() }()
	data, err := io.ReadAll(io.LimitReader(handle, maxConfigBytes))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "Include") && strings.Contains(line, fragmentName) {
			return true
		}
	}
	return false
}

// markerOwned reports whether fragment content is exactly one marker-owned
// block; only such fragments may be overwritten or removed.
func markerOwned(data []byte, m marks) bool {
	trimmed := strings.TrimSuffix(string(data), "\n")
	return strings.HasPrefix(trimmed, m.begin+"\n") && strings.HasSuffix(trimmed, "\n"+m.end) && strings.Count(trimmed, m.begin) == 1 && strings.Count(trimmed, m.end) == 1
}

// InstallMany atomically publishes one marker-owned fragment containing every
// node of the deployment and one marker-owned Include in the user's config.
func InstallMany(home string, entries []Entry) (Result, error) {
	if len(entries) == 0 {
		return Result{}, errors.New("SSH config install requires at least one entry")
	}
	entry := entries[0]
	seenNodes := make(map[string]struct{}, len(entries))
	for _, candidate := range entries {
		if err := validateEntry(candidate); err != nil {
			return Result{}, err
		}
		if candidate.Name != entry.Name {
			return Result{}, errors.New("SSH config entries must share one fragment name")
		}
		if _, exists := seenNodes[candidate.Node]; exists {
			return Result{}, fmt.Errorf("duplicate SSH config node %q", candidate.Node)
		}
		seenNodes[candidate.Node] = struct{}{}
	}
	content, err := render(entries)
	if err != nil {
		return Result{}, err
	}
	return install(home, entry.Name, content, linuxMarks, nil)
}

// install publishes content as the named fragment and one Include block.
// owned, when set, must accept an existing fragment before it is replaced.
func install(home, name, content string, m marks, owned func([]byte) error) (Result, error) {
	directory, err := ensureSSHDir(home)
	if err != nil {
		return Result{}, err
	}
	fragment := filepath.Join(directory, name+"_config")
	configPath := filepath.Join(directory, "config")
	existingFragment, fragmentExists, err := readOptionalRegular(fragment)
	if err != nil {
		return Result{}, err
	}
	if fragmentExists && !markerOwned(existingFragment, m) {
		return Result{}, errors.New("will not overwrite the SSH fragment: it lacks Farrow's markers")
	}
	if fragmentExists && owned != nil {
		if err := owned(existingFragment); err != nil {
			return Result{}, err
		}
	}
	quotedFragment, err := openssh.QuoteConfigValue(fragment)
	if err != nil {
		return Result{}, err
	}
	includeLine := "Include " + quotedFragment
	if linked, err := managedElsewhere(configPath); err != nil || linked {
		if err != nil {
			return Result{}, err
		}
		// A dotfile manager owns this config; Farrow only publishes its fragment.
		fragmentChanged := !fragmentExists || string(existingFragment) != content
		if fragmentChanged {
			if err := fsutil.AtomicWrite(fragment, []byte(content), 0o600); err != nil {
				return Result{}, err
			}
		}
		result := Result{Fragment: fragment, Config: configPath, Changed: fragmentChanged, Action: "install"}
		if !includesFragment(configPath, filepath.Base(fragment)) {
			return result, failure.New(failure.Conflict, fmt.Errorf("%s is a link managed outside Farrow; Farrow will not edit it", configPath)).
				Because("ssh_config_linked").Then("add this line near the top of that file: " + includeLine)
		}
		return result, nil
	}
	config, configExists, err := readOptionalRegular(configPath)
	if err != nil {
		return Result{}, err
	}
	configText := string(config)
	block := m.include + "\n" + includeLine + "\n"
	markerCount := strings.Count(configText, m.include)
	blockCount := strings.Count(configText, block)
	if markerCount > 1 || blockCount > 1 || markerCount != blockCount {
		return Result{}, errors.New("will not change ~/.ssh/config: its Farrow Include block is malformed")
	}
	if strings.Contains(configText, includeLine) && blockCount == 0 {
		return Result{}, errors.New("will not adopt an Include of the Farrow fragment that Farrow did not mark")
	}
	fragmentChanged := !fragmentExists || string(existingFragment) != content
	canonicalConfig := installGlobalBlock(configText, block)
	configChanged := canonicalConfig != configText
	if fragmentChanged {
		if err := fsutil.AtomicWrite(fragment, []byte(content), 0o600); err != nil {
			return Result{}, err
		}
	}
	if configChanged || !configExists {
		if err := fsutil.AtomicWrite(configPath, []byte(canonicalConfig), 0o600); err != nil {
			return Result{}, err
		}
	}
	changed := fragmentChanged || configChanged || !configExists
	return Result{Fragment: fragment, Config: configPath, Changed: changed, Action: "install"}, nil
}

// Remove deletes only the exact marker-owned fragment and Include line.
func Remove(home, name string) (Result, error) {
	if !namePattern.MatchString(name) {
		return Result{}, errors.New("SSH remove name is invalid")
	}
	return remove(home, name, linuxMarks, nil)
}

func remove(home, name string, m marks, owned func([]byte) error) (Result, error) {
	directory, err := ensureSSHDir(home)
	if err != nil {
		return Result{}, err
	}
	fragment := filepath.Join(directory, name+"_config")
	configPath := filepath.Join(directory, "config")
	quotedFragment, err := openssh.QuoteConfigValue(fragment)
	if err != nil {
		return Result{}, err
	}
	includeLine := "Include " + quotedFragment
	block := m.include + "\n" + includeLine + "\n"
	var config []byte
	configExists := false
	if linked, err := managedElsewhere(configPath); err != nil {
		return Result{}, err
	} else if !linked {
		// A linked config is never edited; its Include of a missing fragment
		// matches nothing, so removing the fragment alone is enough.
		if config, configExists, err = readOptionalRegular(configPath); err != nil {
			return Result{}, err
		}
	}
	fragmentData, fragmentExists, err := readOptionalRegular(fragment)
	if err != nil {
		return Result{}, err
	}
	if fragmentExists && !markerOwned(fragmentData, m) {
		return Result{}, errors.New("will not remove the SSH fragment: it lacks Farrow's markers")
	}
	if fragmentExists && owned != nil {
		if err := owned(fragmentData); err != nil {
			return Result{}, err
		}
	}
	configText := string(config)
	if strings.Contains(configText, includeLine) && !strings.Contains(configText, block) {
		return Result{}, errors.New("will not remove the fragment while an unmarked Include of it remains in ~/.ssh/config")
	}
	changed := false
	if configExists && strings.Contains(configText, m.include) {
		if strings.Count(configText, m.include) != 1 || strings.Count(configText, block) != 1 {
			return Result{}, errors.New("will not change ~/.ssh/config: its Farrow Include block is malformed")
		}
		updated := strings.Replace(configText, block, "", 1)
		if err := fsutil.AtomicWrite(configPath, []byte(updated), 0o600); err != nil {
			return Result{}, err
		}
		changed = true
	}
	if fragmentExists {
		if err := os.Remove(fragment); err != nil {
			return Result{Fragment: fragment, Config: configPath, Changed: changed, Action: "remove"}, err
		}
		if err := fsutil.SyncDir(directory); err != nil {
			return Result{Fragment: fragment, Config: configPath, Changed: true, Action: "remove"}, err
		}
		changed = true
	}
	return Result{Fragment: fragment, Config: configPath, Changed: changed, Action: "remove"}, nil
}
