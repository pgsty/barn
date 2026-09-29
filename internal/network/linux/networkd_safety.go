package linux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pgsty/barn/internal/execx"
)

var networkdConfigurationDirectories = []string{
	"/etc/systemd/network",
	"/run/systemd/network",
	"/usr/local/lib/systemd/network",
	"/usr/lib/systemd/network",
}

type ipLinkRecord struct {
	Name             string   `json:"ifname"`
	AlternativeNames []string `json:"altnames"`
	Type             string   `json:"link_type"`
	LinkInfo         struct {
		Kind string `json:"info_kind"`
	} `json:"linkinfo"`
}

func validNetworkdLinkName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}

func boundedReadFile(pathname string) ([]byte, error) {
	info, err := os.Stat(pathname)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, fmt.Errorf("file is not a bounded regular file: %s", pathname)
	}
	data, err := os.ReadFile(pathname)
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("file exceeds safety bound: %s", pathname)
	}
	return data, nil
}

func sysfsLinkType(sysClassNet string, record ipLinkRecord) (string, error) {
	if !validNetworkdLinkName(record.Name) {
		return "", errors.New("ip link inventory contains an invalid interface name")
	}
	linkRoot := filepath.Join(sysClassNet, record.Name)
	ueventPath := filepath.Join(linkRoot, "uevent")
	if data, err := os.ReadFile(ueventPath); err == nil {
		if len(data) > 64<<10 {
			return "", fmt.Errorf("oversized network uevent for %s", record.Name)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if value, found := strings.CutPrefix(line, "DEVTYPE="); found && value != "" {
				return value, nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(linkRoot, "wireless")); err == nil {
		return "wlan", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if record.Type == "" {
		return "", fmt.Errorf("ip link inventory omits the type of %s", record.Name)
	}
	return record.Type, nil
}

func discoverNetworkdLinks(ctx context.Context, runner execx.Runner, sysClassNet string) ([]NetworkdLink, error) {
	result, err := runner.Run(ctx, "/usr/sbin/ip", "-d", "-j", "link", "show")
	if err != nil {
		return nil, fmt.Errorf("inventory links before networkd activation: %w", err)
	}
	if len(result.Stdout) == 0 || len(result.Stdout) > 4<<20 {
		return nil, errors.New("ip link inventory has an invalid size")
	}
	var records []ipLinkRecord
	if err := json.Unmarshal(result.Stdout, &records); err != nil {
		return nil, fmt.Errorf("decode ip link inventory: %w", err)
	}
	if len(records) == 0 {
		return nil, errors.New("ip link inventory is empty")
	}
	links := make([]NetworkdLink, 0, len(records)+1)
	seen := make(map[string]struct{}, len(records)+1)
	for _, record := range records {
		if _, exists := seen[record.Name]; exists {
			return nil, fmt.Errorf("ip link inventory repeats interface %s", record.Name)
		}
		linkType, err := sysfsLinkType(sysClassNet, record)
		if err != nil {
			return nil, err
		}
		alternatives := append([]string(nil), record.AlternativeNames...)
		for _, alternative := range alternatives {
			if !validNetworkdLinkName(alternative) {
				return nil, fmt.Errorf("ip link inventory contains an invalid alternative name for %s", record.Name)
			}
		}
		sort.Strings(alternatives)
		links = append(links, NetworkdLink{Name: record.Name, AlternativeNames: alternatives, Kind: record.LinkInfo.Kind, Type: linkType, BarnOwned: record.Name == BridgeName})
		seen[record.Name] = struct{}{}
	}
	if _, exists := seen[BridgeName]; !exists {
		links = append(links, NetworkdLink{Name: BridgeName, Kind: "bridge", Type: "bridge", BarnOwned: true})
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Name < links[j].Name })
	return links, nil
}

type matchPredicate struct {
	patterns    []string
	unsupported bool
}

func supportedMatchPattern(pattern string) bool {
	if pattern == "" {
		return false
	}
	for _, character := range pattern {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case strings.ContainsRune("_.:@+-*?", character):
		default:
			return false
		}
	}
	_, err := path.Match(pattern, "probe")
	return err == nil
}

func updateMatchPredicate(predicate *matchPredicate, raw string) {
	value := strings.TrimSpace(raw)
	if value == "" {
		predicate.patterns = nil
		predicate.unsupported = false
		return
	}
	words := strings.Fields(value)
	if len(words) == 0 || strings.HasPrefix(value, "!") || strings.ContainsAny(value, "\\\"'") {
		predicate.unsupported = true
		return
	}
	for _, word := range words {
		if strings.HasPrefix(word, "!") || !supportedMatchPattern(word) {
			predicate.unsupported = true
			return
		}
	}
	predicate.patterns = append(predicate.patterns, words...)
}

func parseNetworkdConfiguration(pathname string, data []byte) NetworkdConfiguration {
	predicates := map[string]*matchPredicate{
		"Name": {},
		"Type": {},
	}
	section := ""
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if section != "Match" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if predicate, supported := predicates[strings.TrimSpace(key)]; supported {
			updateMatchPredicate(predicate, value)
		}
	}
	configuration := NetworkdConfiguration{Path: pathname, Kind: "network"}
	if predicate := predicates["Name"]; !predicate.unsupported {
		configuration.MatchNames = append([]string(nil), predicate.patterns...)
	}
	if predicate := predicates["Type"]; !predicate.unsupported {
		configuration.MatchTypes = append([]string(nil), predicate.patterns...)
	}
	return configuration
}

func patternsMatch(patterns, values []string) bool {
	for _, pattern := range patterns {
		for _, value := range values {
			matched, err := path.Match(pattern, value)
			if err == nil && matched {
				return true
			}
		}
	}
	return false
}

func configurationCouldMatch(configuration NetworkdConfiguration, link NetworkdLink) bool {
	if len(configuration.MatchNames) != 0 {
		names := append([]string{link.Name}, link.AlternativeNames...)
		if !patternsMatch(configuration.MatchNames, names) {
			return false
		}
	}
	if len(configuration.MatchTypes) != 0 && !patternsMatch(configuration.MatchTypes, []string{link.Type}) {
		return false
	}
	return true
}

func trustedNetworkdDirectory(directory string) bool {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	uid, _, _, err := hostStatIdentity(info)
	return err == nil && uid == 0 && safeRootParents(filepath.Join(directory, ".barn-safety-probe"))
}

func readNetworkdFile(pathname string, requireTrusted bool) ([]byte, bool, string) {
	linkInfo, err := os.Lstat(pathname)
	if err != nil {
		return nil, false, err.Error()
	}
	resolved := pathname
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		resolved, err = filepath.EvalSymlinks(pathname)
		if err != nil {
			return nil, false, err.Error()
		}
		if resolved == "/dev/null" {
			return nil, true, ""
		}
	}
	info, err := os.Stat(pathname)
	if err != nil {
		return nil, false, err.Error()
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, false, "configuration is not a bounded regular file"
	}
	if requireTrusted {
		uid, _, _, identityErr := hostStatIdentity(info)
		if identityErr != nil || uid != 0 || info.Mode().Perm()&0o022 != 0 || !safeRootParents(resolved) {
			return nil, false, "configuration is not immutable below safe root-owned parents"
		}
	}
	if info.Size() == 0 {
		return nil, true, ""
	}
	data, err := boundedReadFile(pathname)
	if err != nil {
		return nil, false, err.Error()
	}
	return data, false, ""
}

func networkdDropInConflict(name string, directories []string) string {
	for _, directory := range directories {
		dropInDirectory := filepath.Join(directory, name+".d")
		entries, err := os.ReadDir(dropInDirectory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return dropInDirectory
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".conf") {
				return filepath.Join(dropInDirectory, entry.Name())
			}
		}
	}
	return ""
}

func inspectNetworkdActivation(directories []string, links []NetworkdLink, ownedDigests map[string]string, requireTrusted bool) NetworkdActivationSafety {
	safety := NetworkdActivationSafety{Checked: true, Links: append([]NetworkdLink(nil), links...)}
	seen := make(map[string]struct{})
	for _, directory := range directories {
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			safety.Conflicts = append(safety.Conflicts, NetworkdActivationConflict{Path: directory, Reason: "cannot inventory networkd configuration directory"})
			continue
		}
		if requireTrusted && !trustedNetworkdDirectory(directory) {
			safety.Conflicts = append(safety.Conflicts, NetworkdActivationConflict{Path: directory, Reason: "networkd configuration directory is not a safe root-owned directory"})
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".network") && !strings.HasSuffix(name, ".netdev") {
				continue
			}
			if _, alreadySeen := seen[name]; alreadySeen {
				continue
			}
			seen[name] = struct{}{}
			pathname := filepath.Join(directory, name)
			data, masked, reason := readNetworkdFile(pathname, requireTrusted)
			if reason != "" {
				safety.Conflicts = append(safety.Conflicts, NetworkdActivationConflict{Path: pathname, Reason: reason})
				continue
			}
			if masked {
				continue
			}
			if digest, owned := ownedDigests[pathname]; owned {
				if fileDigest(string(data)) != digest {
					safety.Conflicts = append(safety.Conflicts, NetworkdActivationConflict{Path: pathname, Reason: "Barn-owned networkd configuration changed"})
				}
				continue
			}
			if dropIn := networkdDropInConflict(name, directories); dropIn != "" {
				safety.Conflicts = append(safety.Conflicts, NetworkdActivationConflict{Path: dropIn, Reason: "networkd drop-in matching is outside the activation safety proof"})
				continue
			}
			if strings.HasSuffix(name, ".netdev") {
				safety.Configurations = append(safety.Configurations, NetworkdConfiguration{Path: pathname, Kind: "netdev"})
				safety.Conflicts = append(safety.Conflicts, NetworkdActivationConflict{Path: pathname, Reason: "non-Barn .netdev may create or change a virtual link"})
				continue
			}
			configuration := parseNetworkdConfiguration(pathname, data)
			safety.Configurations = append(safety.Configurations, configuration)
			for _, link := range links {
				if configurationCouldMatch(configuration, link) {
					reason := "effective .network file is not provably disjoint from the link"
					if link.BarnOwned {
						reason = "unowned .network file could take precedence for the Barn bridge"
					}
					safety.Conflicts = append(safety.Conflicts, NetworkdActivationConflict{Path: pathname, Link: link.Name, Reason: reason})
				}
			}
		}
	}
	sort.Slice(safety.Configurations, func(i, j int) bool { return safety.Configurations[i].Path < safety.Configurations[j].Path })
	sort.Slice(safety.Conflicts, func(i, j int) bool {
		if safety.Conflicts[i].Path != safety.Conflicts[j].Path {
			return safety.Conflicts[i].Path < safety.Conflicts[j].Path
		}
		return safety.Conflicts[i].Link < safety.Conflicts[j].Link
	})
	return safety
}

func ownedNetworkdDigests(manifest *Manifest) map[string]string {
	if manifest == nil {
		return nil
	}
	return map[string]string{
		NetDevPath:  manifest.Files[NetDevPath],
		NetworkPath: manifest.Files[NetworkPath],
	}
}
