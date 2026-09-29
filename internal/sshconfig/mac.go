package sshconfig

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/openssh"
)

// MacFragment names the one fragment that holds every Mac machine.
const MacFragment = "barn-mac"

// MacEntry is one macOS guest. Its host key is pinned under the instance
// alias, so the address may change without weakening host verification.
type MacEntry struct {
	Name         string
	User         string
	Host         string
	Identity     string
	KnownHosts   string
	HostKeyAlias string
	// AddressOnly leaves the bare name out when ~/.ssh/config already uses it
	// for another host, so ssh NAME keeps going where the user meant.
	AddressOnly bool
}

const macRootPrefix = "# barn-mac:root "

// ErrMacForeign marks a Mac fragment written for another Barn home. Only an
// explicit install replaces it; lifecycle refreshes leave it alone.
var ErrMacForeign = errors.New("the Mac SSH fragment belongs to another Barn home")

func renderMac(root string, entries []MacEntry) (string, error) {
	if !safeOpenSSHPath(root) {
		return "", errors.New("mac data root is not a safe absolute path")
	}
	var output strings.Builder
	output.WriteString(macMarks.begin + "\n" + macRootPrefix + root + "\n")
	seen := map[string]bool{}
	for index, entry := range entries {
		if !namePattern.MatchString(entry.Name) || !namePattern.MatchString(entry.User) || !aliasPattern.MatchString(entry.HostKeyAlias) ||
			!aliasPattern.MatchString(entry.Host) || !safeOpenSSHPath(entry.Identity) || !safeOpenSSHPath(entry.KnownHosts) {
			return "", fmt.Errorf("mac SSH config entry %q is invalid", entry.Name)
		}
		if seen[entry.Name] || seen[entry.Host] {
			return "", fmt.Errorf("duplicate mac SSH config entry %q", entry.Name)
		}
		seen[entry.Name], seen[entry.Host] = true, true
		identity, err := openssh.QuoteConfigValue(entry.Identity)
		if err != nil {
			return "", err
		}
		knownHosts, err := openssh.QuoteConfigValue(entry.KnownHosts)
		if err != nil {
			return "", err
		}
		patterns := entry.Name + " " + entry.Host
		if entry.AddressOnly {
			patterns = entry.Host
		}
		fmt.Fprintf(&output, "Host %s\n  HostName %s\n  User %s\n  IdentityFile %s\n  IdentitiesOnly yes\n  UserKnownHostsFile %s\n  HostKeyAlias %s\n  StrictHostKeyChecking yes\n  ForwardAgent no\n",
			patterns, entry.Host, entry.User, identity, knownHosts, entry.HostKeyAlias)
		if index != len(entries)-1 {
			output.WriteByte('\n')
		}
	}
	output.WriteString(macMarks.end + "\n")
	return output.String(), nil
}

// RenderMac returns the fragment text without installing it.
func RenderMac(root string, entries []MacEntry) (string, error) { return renderMac(root, entries) }

func macOwner(root string, takeover bool) func([]byte) error {
	return func(data []byte) error {
		if takeover {
			return nil
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, macRootPrefix) && strings.TrimPrefix(line, macRootPrefix) == root {
				return nil
			}
		}
		return failure.New(failure.Conflict, ErrMacForeign).Because("mac_ssh_config_foreign").Then("barn mac ssh-config --install")
	}
}

// InstallMac publishes every Mac machine of root. With no machines it removes
// the fragment. takeover replaces a fragment written for another root. A
// machine whose name ~/.ssh/config already uses as a Host is published by
// address only and listed in Result.Shadowed.
func InstallMac(home, root string, entries []MacEntry, takeover bool) (Result, error) {
	if len(entries) == 0 {
		return RemoveMac(home, root, takeover)
	}
	taken, err := userHostNames(home)
	if err != nil {
		return Result{}, err
	}
	var shadowed []string
	for index := range entries {
		if taken[strings.ToLower(entries[index].Name)] {
			entries[index].AddressOnly = true
			shadowed = append(shadowed, entries[index].Name)
		}
	}
	content, err := renderMac(root, entries)
	if err != nil {
		return Result{}, err
	}
	result, err := install(home, MacFragment, content, macMarks, macOwner(root, takeover))
	result.Shadowed = shadowed
	return result, err
}

// userHostNames lists the literal Host names of ~/.ssh/config itself;
// wildcard and negated patterns match too broadly to count.
func userHostNames(home string) (map[string]bool, error) {
	handle, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	data, err := io.ReadAll(io.LimitReader(handle, maxConfigBytes))
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.Replace(line, "=", " ", 1))
		if len(fields) < 2 || !strings.EqualFold(fields[0], "Host") {
			continue
		}
		for _, pattern := range fields[1:] {
			if strings.HasPrefix(pattern, "#") {
				break
			}
			pattern = strings.Trim(pattern, `"`)
			if pattern != "" && !strings.ContainsAny(pattern, "*?!") {
				names[strings.ToLower(pattern)] = true
			}
		}
	}
	return names, nil
}

// RemoveMac deletes only the marker-owned Mac fragment and its Include block.
func RemoveMac(home, root string, takeover bool) (Result, error) {
	return remove(home, MacFragment, macMarks, macOwner(root, takeover))
}

// MacInstalled reports whether the Mac fragment of root is installed. It
// only reads; a missing ~/.ssh is simply not installed.
func MacInstalled(home, root string) (bool, error) {
	data, exists, err := readOptionalRegular(filepath.Join(home, ".ssh", MacFragment+"_config"))
	if err != nil || !exists {
		return false, err
	}
	return markerOwned(data, macMarks) && macOwner(root, false)(data) == nil, nil
}
