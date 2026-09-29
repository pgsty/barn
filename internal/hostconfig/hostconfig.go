// Package hostconfig owns the narrowly scoped, marker-delimited /etc/hosts
// integration for the deployment.
package hostconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/fsutil"
)

const (
	maxHostsBytes       = 1 << 20
	ActionInstall       = "install"
	ActionUninstall     = "uninstall"
	InstalledHelperPath = "/opt/barn/libexec/barn-hosts-helper"
)

// ExpectedHelperSHA256 is injected into packaged/main binaries after the
// companion helper is built. Source-tree `go run` leaves it empty and still
// requires an immutable root-owned helper, while release builds additionally
// require exact archive pairing.
var ExpectedHelperSHA256 string

var (
	markerPattern = regexp.MustCompile(`^# barn:(begin|end)$`)
	hashPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hostPattern   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,251}[A-Za-z0-9])?$`)
)

type Entry struct {
	Address string   `json:"address"`
	Names   []string `json:"names"`
}

type Plan struct {
	Schema       int      `json:"schema"`
	Action       string   `json:"action"`
	Target       string   `json:"target"`
	Changed      bool     `json:"changed"`
	BeforeSHA256 string   `json:"before_sha256"`
	AfterSHA256  string   `json:"after_sha256"`
	Lines        []string `json:"lines,omitempty"`
	HelperPath   string   `json:"helper_path"`
	HelperSHA256 string   `json:"helper_sha256,omitempty"`
}

type Report struct {
	Plan    Plan `json:"plan"`
	Applied bool `json:"applied"`
}

type Executor struct {
	Root   execx.Runner
	Target string
	// Reviewed, when set, is the plan the user confirmed; apply stops if the
	// hosts file no longer yields exactly that plan.
	Reviewed *Plan
}

type ownedBlock struct {
	start int
	end   int
}

func NativePath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return "/private/etc/hosts", nil
	case "linux":
		return "/etc/hosts", nil
	default:
		return "", fmt.Errorf("hosts integration is unsupported on %s", runtime.GOOS)
	}
}

func digest(data []byte) string {
	value := sha256.Sum256(data)
	return hex.EncodeToString(value[:])
}

func markers() (string, string) {
	return "# barn:begin", "# barn:end"
}

func parseBlocks(data []byte) ([]ownedBlock, error) {
	if len(data) > maxHostsBytes {
		return nil, errors.New("hosts file exceeds 1 MiB limit")
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("hosts file contains a NUL byte")
	}
	blocks := make([]ownedBlock, 0)
	var active *ownedBlock
	for offset := 0; offset < len(data); {
		lineStart := offset
		newline := bytes.IndexByte(data[offset:], '\n')
		lineEnd := len(data)
		if newline >= 0 {
			lineEnd = offset + newline
			offset = lineEnd + 1
		} else {
			offset = len(data)
		}
		line := string(data[lineStart:lineEnd])
		if !strings.Contains(line, "# barn:") {
			continue
		}
		match := markerPattern.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("malformed Barn hosts marker at byte %d; repair the block manually", lineStart)
		}
		kind := match[1]
		if kind == "begin" {
			if active != nil {
				return nil, errors.New("nested Barn hosts marker blocks are unsafe")
			}
			if len(blocks) != 0 {
				return nil, errors.New("duplicate Barn hosts block")
			}
			active = &ownedBlock{start: lineStart}
			continue
		}
		if active == nil {
			return nil, errors.New("unmatched Barn hosts end marker")
		}
		active.end = offset
		blocks = append(blocks, *active)
		active = nil
	}
	if active != nil {
		return nil, errors.New("unterminated Barn hosts block")
	}
	return blocks, nil
}

func validHostName(name string) bool {
	if !hostPattern.MatchString(name) || strings.Contains(name, "..") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' || label[0] == '_' || label[len(label)-1] == '_' {
			return false
		}
	}
	return true
}

func validateEntries(entries []Entry) error {
	if len(entries) == 0 {
		return errors.New("hosts install requires at least one node entry")
	}
	addresses := make(map[string]struct{}, len(entries))
	names := make(map[string]string)
	for _, entry := range entries {
		ip := net.ParseIP(entry.Address).To4()
		rfc1918 := ip != nil && (ip[0] == 10 || (ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) || (ip[0] == 192 && ip[1] == 168))
		if ip == nil || ip.String() != entry.Address || !rfc1918 || ip[3] < 9 || ip[3] == 255 || len(entry.Names) == 0 {
			return fmt.Errorf("hosts entry address or names are invalid: %q", entry.Address)
		}
		if _, exists := addresses[entry.Address]; exists {
			return fmt.Errorf("duplicate hosts address %s", entry.Address)
		}
		addresses[entry.Address] = struct{}{}
		seenInEntry := make(map[string]struct{}, len(entry.Names))
		for _, name := range entry.Names {
			if !validHostName(name) {
				return fmt.Errorf("invalid hosts name %q", name)
			}
			if _, exists := seenInEntry[name]; exists {
				return fmt.Errorf("duplicate hosts name %q", name)
			}
			seenInEntry[name] = struct{}{}
			if previous, exists := names[name]; exists && previous != entry.Address {
				return fmt.Errorf("hosts name %q maps to both %s and %s", name, previous, entry.Address)
			}
			names[name] = entry.Address
		}
	}
	return nil
}

func renderBlock(entries []Entry) ([]byte, []string, error) {
	if err := validateEntries(entries); err != nil {
		return nil, nil, err
	}
	begin, end := markers()
	lines := make([]string, 0, len(entries))
	var output strings.Builder
	output.WriteString(begin)
	output.WriteByte('\n')
	for _, entry := range entries {
		line := entry.Address + "\t" + strings.Join(entry.Names, " ")
		lines = append(lines, line)
		output.WriteString(line)
		output.WriteByte('\n')
	}
	output.WriteString(end)
	output.WriteByte('\n')
	return []byte(output.String()), lines, nil
}

func validateNoHostConflicts(before []byte, entries []Entry) error {
	base, _, err := stripOwnedBlock(before)
	if err != nil {
		return err
	}
	mappings := make(map[string]map[string]struct{})
	for _, rawLine := range strings.Split(string(base), "\n") {
		line := rawLine
		if comment := strings.IndexByte(line, '#'); comment >= 0 {
			line = line[:comment]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || net.ParseIP(fields[0]) == nil {
			continue
		}
		for _, name := range fields[1:] {
			if mappings[name] == nil {
				mappings[name] = make(map[string]struct{})
			}
			mappings[name][fields[0]] = struct{}{}
		}
	}
	for _, entry := range entries {
		for _, name := range entry.Names {
			for address := range mappings[name] {
				if address != entry.Address {
					return failure.New(failure.Conflict, fmt.Errorf("/etc/hosts already maps %s to %s outside the Barn block", name, address)).Because("hosts_name_taken").Then(fmt.Sprintf("remove or rename the %s %s line in /etc/hosts, then retry", address, name))
				}
			}
		}
	}
	return nil
}

func findBlock(blocks []ownedBlock) (ownedBlock, bool) {
	if len(blocks) == 0 {
		return ownedBlock{}, false
	}
	return blocks[0], true
}

// ReconcileContent changes only the exact marker-owned barn block. Bytes
// outside that block are preserved verbatim.
func ReconcileContent(before []byte, action string, entries []Entry) ([]byte, []string, bool, error) {
	blocks, err := parseBlocks(before)
	if err != nil {
		return nil, nil, false, err
	}
	current, exists := findBlock(blocks)
	switch action {
	case ActionInstall:
		block, lines, err := renderBlock(entries)
		if err != nil {
			return nil, nil, false, err
		}
		if err := validateNoHostConflicts(before, entries); err != nil {
			return nil, nil, false, err
		}
		var after []byte
		if exists {
			after = make([]byte, 0, len(before)-current.end+current.start+len(block))
			after = append(after, before[:current.start]...)
			after = append(after, block...)
			after = append(after, before[current.end:]...)
		} else {
			after = append([]byte(nil), before...)
			if len(after) > 0 && after[len(after)-1] != '\n' {
				after = append(after, '\n')
			}
			after = append(after, block...)
		}
		return after, lines, !bytes.Equal(before, after), nil
	case ActionUninstall:
		if len(entries) != 0 {
			return nil, nil, false, errors.New("hosts uninstall does not accept entries")
		}
		if !exists {
			return append([]byte(nil), before...), nil, false, nil
		}
		after := make([]byte, 0, len(before)-(current.end-current.start))
		after = append(after, before[:current.start]...)
		after = append(after, before[current.end:]...)
		return after, nil, true, nil
	default:
		return nil, nil, false, fmt.Errorf("unsupported hosts action %q", action)
	}
}

func secureRead(path string, limit int64) ([]byte, os.FileInfo, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, nil, errors.New("hosts path must be absolute")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() > limit {
		return nil, nil, fmt.Errorf("hosts path is not a bounded regular non-symlink file: %s", path)
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		// This identity-checked hosts handle is read-only; the reviewed write
		// transaction handles every target Write, Sync, and Close error.
		_ = handle.Close()
	}()
	opened, err := handle.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return nil, nil, errors.New("hosts file identity changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(handle, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, nil, errors.New("hosts file exceeds bounded read limit")
	}
	return data, opened, nil
}

func prepare(target, action string, entries []Entry) (Plan, []byte, error) {
	before, _, err := secureRead(target, maxHostsBytes)
	if err != nil {
		return Plan{}, nil, err
	}
	after, lines, changed, err := ReconcileContent(before, action, entries)
	if err != nil {
		return Plan{}, nil, err
	}
	return Plan{Schema: 1, Action: action, Target: target, Changed: changed, BeforeSHA256: digest(before), AfterSHA256: digest(after), Lines: lines}, after, nil
}

func (e Executor) Execute(ctx context.Context, action string, entries []Entry, apply bool) (Report, error) {
	target := e.Target
	if target == "" {
		var err error
		target, err = NativePath()
		if err != nil {
			return Report{}, err
		}
	}
	plan, desired, err := prepare(target, action, entries)
	if err != nil {
		return Report{}, err
	}
	plan.HelperPath = InstalledHelperPath
	helperDigest, helperErr := installedHelperDigest(InstalledHelperPath)
	if helperErr == nil {
		plan.HelperSHA256 = helperDigest
	}
	report := Report{Plan: plan}
	if !apply || !plan.Changed {
		return report, nil
	}
	if e.Reviewed != nil && (e.Reviewed.BeforeSHA256 != plan.BeforeSHA256 || e.Reviewed.AfterSHA256 != plan.AfterSHA256) {
		return Report{}, errors.New("the hosts file changed after the plan was made; rerun the command to review a fresh plan")
	}
	if e.Root == nil {
		return Report{}, errors.New("hosts apply requires a privileged runner")
	}
	helper := InstalledHelperPath
	if helperErr != nil {
		return Report{}, helperErr
	}
	staging, err := os.CreateTemp("", "barn-hosts-stage-")
	if err != nil {
		return Report{}, err
	}
	stagingPath := staging.Name()
	defer func() { _ = os.Remove(stagingPath) }()
	if err := staging.Chmod(0o600); err != nil {
		_ = staging.Close()
		return Report{}, err
	}
	if _, err := staging.Write(desired); err != nil {
		_ = staging.Close()
		return Report{}, err
	}
	if err := staging.Sync(); err != nil {
		_ = staging.Close()
		return Report{}, err
	}
	if err := staging.Close(); err != nil {
		return Report{}, err
	}
	if _, err := e.Root.Run(ctx, helper, "--target", target, "--staging", stagingPath, "--action", action, "--before-sha256", plan.BeforeSHA256, "--after-sha256", plan.AfterSHA256); err != nil {
		return Report{}, fmt.Errorf("update %s: %w", target, err)
	}
	actual, _, err := secureRead(target, maxHostsBytes)
	if err != nil {
		return Report{}, err
	}
	if digest(actual) != plan.AfterSHA256 {
		return Report{}, failure.New(failure.Integrity, errors.New("hosts apply returned success but target digest does not match the reviewed plan"))
	}
	report.Applied = true
	return report, nil
}

func validateInstalledHelper(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("privileged hosts helper path must be canonical and absolute")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("privileged hosts helper is missing or unsafe: %s", path)
	}
	uid, gid, links, err := statOwnership(info)
	if err != nil {
		return err
	}
	if uid != 0 || gid != 0 || links != 1 || (info.Mode().Perm() != 0o755 && info.Mode().Perm() != 0o555) || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New("privileged hosts helper must be root-owned, singly linked, executable, and not writable by group or other")
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		parentInfo, err := os.Lstat(parent)
		if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("privileged hosts helper parent is missing or unsafe: %s", parent)
		}
		parentUID, parentGID, _, err := statOwnership(parentInfo)
		if err != nil || parentUID != 0 || parentGID != 0 || parentInfo.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("privileged hosts helper parent is not root-owned and non-writable: %s", parent)
		}
		if parent == string(filepath.Separator) {
			break
		}
	}
	return nil
}

func installedHelperDigest(path string) (string, error) {
	actual, err := RootOwnedHelperDigest(path)
	if err != nil {
		return "", err
	}
	if ExpectedHelperSHA256 != "" {
		if !hashPattern.MatchString(ExpectedHelperSHA256) {
			return "", errors.New("packaged hosts helper digest is invalid")
		}
		if actual != ExpectedHelperSHA256 {
			return "", failure.New(failure.Integrity, errors.New("root-owned hosts helper digest differs from the packaged CLI companion"))
		}
	}
	return actual, nil
}

// RootOwnedHelperDigest verifies that path is a bounded, immutable-by-users
// executable beneath root-owned non-writable parents, then returns its digest.
// Setup uses this only for a newly staged helper before atomically publishing
// it; runtime callers must use InstalledHelperDigest so CLI pairing is also
// enforced.
func RootOwnedHelperDigest(path string) (string, error) {
	if err := validateInstalledHelper(path); err != nil {
		return "", err
	}
	data, _, err := secureRead(path, 64<<20)
	if err != nil {
		return "", err
	}
	return digest(data), nil
}

// CompanionHelperDigest verifies an unprivileged archive/formula companion
// before setup copies it into the fixed root-owned location. Release and Make
// builds always inject the expected digest; an unpaired `go run` binary is not
// allowed to provision privileged executable bytes.
func CompanionHelperDigest(path string) (string, error) {
	if ExpectedHelperSHA256 == "" || !hashPattern.MatchString(ExpectedHelperSHA256) {
		return "", errors.New("barn binary has no valid packaged hosts-helper digest")
	}
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("companion hosts helper path must be canonical and absolute")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("companion hosts helper is missing or unsafe: %s", path)
	}
	data, _, err := secureRead(path, 64<<20)
	if err != nil {
		return "", err
	}
	actual := digest(data)
	if actual != ExpectedHelperSHA256 {
		return "", failure.New(failure.Integrity, errors.New("companion hosts helper digest differs from the packaged CLI"))
	}
	return actual, nil
}

// InstalledHelperDigest verifies the complete privileged path/ownership and
// the CLI/helper digest pairing.
func InstalledHelperDigest() (string, error) {
	return installedHelperDigest(InstalledHelperPath)
}

func stripOwnedBlock(data []byte) ([]byte, bool, error) {
	blocks, err := parseBlocks(data)
	if err != nil {
		return nil, false, err
	}
	block, exists := findBlock(blocks)
	if !exists {
		return append([]byte(nil), data...), false, nil
	}
	result := make([]byte, 0, len(data)-(block.end-block.start))
	result = append(result, data[:block.start]...)
	result = append(result, data[block.end:]...)
	return result, true, nil
}

func statOwnership(info os.FileInfo) (int, int, uint64, error) {
	value, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, errors.New("hosts file has no Unix stat ownership")
	}
	return int(value.Uid), int(value.Gid), uint64(value.Nlink), nil
}

func atomicReplace(target string, data []byte, mode os.FileMode, uid, gid int, expectedBefore string) error {
	parent := filepath.Dir(target)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("hosts target parent is not a real directory")
	}
	if _, err := os.Lstat(target); err != nil {
		return err
	}
	temp, err := os.CreateTemp(parent, ".barn-hosts-apply-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	tempInfo, err := temp.Stat()
	if err != nil {
		return err
	}
	tempUID, tempGID, _, err := statOwnership(tempInfo)
	if err != nil {
		return err
	}
	if tempUID != uid || tempGID != gid {
		if err := temp.Chown(uid, gid); err != nil {
			return err
		}
	}
	if tempInfo.Mode().Perm() != mode.Perm() {
		if err := temp.Chmod(mode.Perm()); err != nil {
			return err
		}
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	current, _, err := secureRead(target, maxHostsBytes)
	if err != nil {
		return err
	}
	if digest(current) != expectedBefore {
		return errors.New("the hosts file changed after the plan was made; rerun the command to review a fresh plan")
	}
	if err := os.Rename(tempPath, target); err != nil {
		return err
	}
	keep = true
	return fsutil.SyncDir(parent)
}

// ApplyHelper is the privileged, digest-bound half of Execute. The caller is
// responsible for restricting target to the native hosts path.
func ApplyHelper(target, staging, action, beforeSHA256, afterSHA256 string, requireRootTarget bool) error {
	if !hashPattern.MatchString(beforeSHA256) || !hashPattern.MatchString(afterSHA256) {
		return errors.New("hosts apply digests are invalid")
	}
	if action != ActionInstall && action != ActionUninstall {
		return fmt.Errorf("unsupported hosts action %q", action)
	}
	before, info, err := secureRead(target, maxHostsBytes)
	if err != nil {
		return err
	}
	uid, gid, links, err := statOwnership(info)
	if err != nil {
		return err
	}
	if links != 1 || info.Mode().Perm()&0o022 != 0 || (requireRootTarget && (os.Geteuid() != 0 || uid != 0 || gid != 0)) {
		return errors.New("hosts target ownership, links, mode, or privilege is unsafe")
	}
	if digest(before) != beforeSHA256 {
		return errors.New("the hosts file changed after the plan was made; rerun the command to review a fresh plan")
	}
	after, stagingInfo, err := secureRead(staging, maxHostsBytes)
	if err != nil {
		return err
	}
	stagingUID, _, stagingLinks, err := statOwnership(stagingInfo)
	if err != nil {
		return err
	}
	expectedStagingUID := stagingUID
	if requireRootTarget {
		rawUID := os.Getenv("SUDO_UID")
		parsedUID, parseErr := strconv.Atoi(rawUID)
		if parseErr != nil || parsedUID < 0 {
			return errors.New("privileged hosts helper requires a valid SUDO_UID")
		}
		expectedStagingUID = parsedUID
	}
	if stagingUID != expectedStagingUID || stagingLinks != 1 || stagingInfo.Mode().Perm() != 0o600 || digest(after) != afterSHA256 {
		return errors.New("hosts staging file mode, links, or digest is unsafe")
	}
	if err := atomicReplace(target, after, info.Mode().Perm(), uid, gid, beforeSHA256); err != nil {
		return err
	}
	final, finalInfo, err := secureRead(target, maxHostsBytes)
	if err != nil {
		return err
	}
	finalUID, finalGID, finalLinks, err := statOwnership(finalInfo)
	if err != nil {
		return err
	}
	if digest(final) != afterSHA256 || finalUID != uid || finalGID != gid || finalLinks != 1 || finalInfo.Mode().Perm() != info.Mode().Perm() {
		return errors.New("hosts target metadata or digest is wrong after atomic apply")
	}
	return nil
}
