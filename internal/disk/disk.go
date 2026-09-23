// Package disk manages qcow2 images through a bounded qemu-img runner.
package disk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/pgsty/farrow/internal/execx"
)

// Info is the security-relevant subset of qemu-img's JSON output.
type Info struct {
	Filename              string          `json:"filename"`
	Format                string          `json:"format"`
	VirtualSize           int64           `json:"virtual-size"`
	BackingFilename       string          `json:"backing-filename"`
	FullBackingFilename   string          `json:"full-backing-filename"`
	BackingFilenameFormat string          `json:"backing-filename-format"`
	DataFile              string          `json:"data-file"`
	Encrypted             bool            `json:"encrypted"`
	FormatSpecific        *FormatSpecific `json:"format-specific"`
}

type FormatSpecific struct {
	Type string             `json:"type"`
	Data FormatSpecificData `json:"data"`
}

type FormatSpecificData struct {
	Corrupt              bool     `json:"corrupt"`
	DataFile             string   `json:"data-file"`
	Encrypted            bool     `json:"encrypted"`
	ExtendedL2           bool     `json:"extended-l2"`
	IncompatibleFeatures []string `json:"incompatible-features"`
}

type CheckInfo struct {
	CheckErrors int64 `json:"check-errors"`
	Corruptions int64 `json:"corruptions"`
}

// Manager deliberately exposes only typed qcow2 operations.
type Manager struct {
	QEMUImg string
	Runner  execx.Runner
}

func (m Manager) validate() error {
	if m.QEMUImg == "" {
		return errors.New("qemu-img path is empty")
	}
	if m.Runner == nil {
		return errors.New("qemu-img runner is nil")
	}
	return nil
}

// Inspect forces qcow2 parsing instead of trusting filename probing.
func (m Manager) Inspect(ctx context.Context, path string) (Info, error) {
	if err := m.validate(); err != nil {
		return Info{}, err
	}
	result, err := m.Runner.Run(ctx, m.QEMUImg, "info", "--output=json", "-f", "qcow2", path)
	if err != nil {
		return Info{}, err
	}
	var info Info
	if err := json.Unmarshal(result.Stdout, &info); err != nil {
		return Info{}, fmt.Errorf("decode qemu-img info JSON: %w", err)
	}
	return info, nil
}

// CheckBase runs qemu-img's full structural check for a repository candidate.
// Runtime cache reads use the cheaper typed metadata inspection plus digest;
// publishing a catalog is the bounded point where a full check is required.
func (m Manager) CheckBase(ctx context.Context, path string) error {
	if err := m.validate(); err != nil {
		return err
	}
	result, err := m.Runner.Run(ctx, m.QEMUImg, "check", "--output=json", "-f", "qcow2", path)
	if err != nil {
		var commandErr *execx.CommandError
		if !errors.As(err, &commandErr) || commandErr.ExitCode != 3 {
			return fmt.Errorf("qemu-img check: %w", err)
		}
	}
	var check CheckInfo
	if err := json.Unmarshal(result.Stdout, &check); err != nil {
		return fmt.Errorf("decode qemu-img check JSON: %w", err)
	}
	if check.CheckErrors != 0 || check.Corruptions != 0 {
		return fmt.Errorf("qemu-img check reported check-errors=%d corruptions=%d", check.CheckErrors, check.Corruptions)
	}
	return nil
}

func (m Manager) inspectChain(ctx context.Context, path string) ([]Info, error) {
	result, err := m.Runner.Run(ctx, m.QEMUImg, "info", "--output=json", "--backing-chain", "-f", "qcow2", path)
	if err != nil {
		return nil, err
	}
	var chain []Info
	if err := json.Unmarshal(result.Stdout, &chain); err != nil {
		return nil, fmt.Errorf("decode qemu-img backing chain JSON: %w", err)
	}
	if len(chain) == 0 {
		return nil, errors.New("qemu-img returned an empty backing chain")
	}
	return chain, nil
}

// ValidateBase rejects any feature that makes the managed cache depend on an
// external file or an unsupported encryption/incompatible feature path.
func ValidateBase(info Info) error {
	if info.Format != "qcow2" {
		return fmt.Errorf("managed base format is %q, want qcow2", info.Format)
	}
	if info.VirtualSize <= 0 {
		return fmt.Errorf("managed base virtual size is invalid: %d", info.VirtualSize)
	}
	if info.BackingFilename != "" || info.FullBackingFilename != "" {
		return errors.New("managed base has a backing file")
	}
	if info.DataFile != "" || info.Encrypted {
		return errors.New("managed base uses a data file or encryption")
	}
	if info.FormatSpecific != nil {
		data := info.FormatSpecific.Data
		if data.Corrupt {
			return errors.New("managed base is marked corrupt")
		}
		if data.DataFile != "" || data.Encrypted || data.ExtendedL2 || len(data.IncompatibleFeatures) > 0 {
			return errors.New("managed base uses unsupported data-file, encryption, extended L2, or incompatible features")
		}
	}
	return nil
}

func ValidateRuntime(info Info, allowBacking bool) error {
	if info.Format != "qcow2" || info.VirtualSize <= 0 {
		return fmt.Errorf("runtime disk format/size is invalid: format=%q size=%d", info.Format, info.VirtualSize)
	}
	if !allowBacking && (info.BackingFilename != "" || info.FullBackingFilename != "") {
		return errors.New("standalone runtime disk unexpectedly has a backing file")
	}
	if allowBacking && info.BackingFilename != "" && info.BackingFilenameFormat != "qcow2" {
		return fmt.Errorf("runtime overlay backing format is %q, want qcow2", info.BackingFilenameFormat)
	}
	if info.DataFile != "" || info.Encrypted {
		return errors.New("runtime disk uses a data file or encryption")
	}
	if info.FormatSpecific != nil {
		data := info.FormatSpecific.Data
		if data.Corrupt || data.DataFile != "" || data.Encrypted || data.ExtendedL2 || len(data.IncompatibleFeatures) > 0 {
			return errors.New("runtime disk uses corrupt or unsupported qcow2 features")
		}
	}
	return nil
}

// Grow performs an idempotent offline qcow2 virtual-size increase. It never
// shrinks and verifies the resulting metadata before syncing the disk.
func (m Manager) Grow(ctx context.Context, path string, targetSize int64, allowBacking bool) (Info, bool, error) {
	if err := m.validate(); err != nil {
		return Info{}, false, err
	}
	if targetSize <= 0 || path == "" || !filepath.IsAbs(path) {
		return Info{}, false, errors.New("grow requires an absolute path and positive target size")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Info{}, false, fmt.Errorf("runtime disk is not a regular non-symlink file: %s", path)
	}
	before, err := m.Inspect(ctx, path)
	if err != nil {
		return Info{}, false, err
	}
	if err := ValidateRuntime(before, allowBacking); err != nil {
		return Info{}, false, err
	}
	if before.VirtualSize > targetSize {
		return Info{}, false, fmt.Errorf("will not shrink a disk from %d to %d bytes", before.VirtualSize, targetSize)
	}
	if before.VirtualSize == targetSize {
		return before, false, nil
	}
	if _, err := m.Runner.Run(ctx, m.QEMUImg, "resize", "-f", "qcow2", path, strconv.FormatInt(targetSize, 10)); err != nil {
		return Info{}, false, fmt.Errorf("grow runtime qcow2 disk: %w", err)
	}
	after, err := m.Inspect(ctx, path)
	if err != nil {
		return Info{}, true, fmt.Errorf("inspect grown runtime disk: %w", err)
	}
	if err := ValidateRuntime(after, allowBacking); err != nil {
		return Info{}, true, err
	}
	if after.VirtualSize != targetSize {
		return Info{}, true, fmt.Errorf("grown runtime disk size %d does not match target %d", after.VirtualSize, targetSize)
	}
	handle, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return Info{}, true, err
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return Info{}, true, err
	}
	if err := handle.Close(); err != nil {
		return Info{}, true, err
	}
	return after, true, nil
}

// CreateBlank creates and atomically publishes a sparse standalone qcow2 data
// disk. Existing targets are never overwritten.
func (m Manager) CreateBlank(ctx context.Context, targetPath string, size int64) (Info, error) {
	if err := m.validate(); err != nil {
		return Info{}, err
	}
	if size <= 0 {
		return Info{}, fmt.Errorf("blank disk size must be positive: %d", size)
	}
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return Info{}, fmt.Errorf("resolve blank disk target: %w", err)
	}
	if _, err := os.Lstat(absTarget); err == nil {
		return Info{}, fmt.Errorf("blank disk target already exists: %s", absTarget)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Info{}, fmt.Errorf("stat blank disk target: %w", err)
	}
	parent := filepath.Dir(absTarget)
	parentStat, err := os.Stat(parent)
	if err != nil || !parentStat.IsDir() {
		return Info{}, fmt.Errorf("blank disk parent is not an existing directory: %s", parent)
	}
	placeholder, err := os.CreateTemp(parent, "."+filepath.Base(absTarget)+".partial-")
	if err != nil {
		return Info{}, fmt.Errorf("reserve blank disk temporary path: %w", err)
	}
	tempPath := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		_ = os.Remove(tempPath)
		return Info{}, fmt.Errorf("close blank disk placeholder: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return Info{}, fmt.Errorf("release blank disk placeholder: %w", err)
	}
	defer func() { _ = os.Remove(tempPath) }()
	if _, err := m.Runner.Run(ctx, m.QEMUImg, "create", "-f", "qcow2", tempPath, strconv.FormatInt(size, 10)); err != nil {
		return Info{}, fmt.Errorf("create blank qcow2 disk: %w", err)
	}
	info, err := m.Inspect(ctx, tempPath)
	if err != nil {
		return Info{}, fmt.Errorf("inspect blank qcow2 disk: %w", err)
	}
	if err := ValidateBase(info); err != nil {
		return Info{}, fmt.Errorf("validate blank qcow2 disk: %w", err)
	}
	if info.VirtualSize != size {
		return Info{}, fmt.Errorf("blank disk virtual size %d does not match requested %d", info.VirtualSize, size)
	}
	file, err := os.OpenFile(tempPath, os.O_RDWR, 0)
	if err != nil {
		return Info{}, fmt.Errorf("open blank disk for sync: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return Info{}, fmt.Errorf("set blank disk permissions: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return Info{}, fmt.Errorf("sync blank disk: %w", err)
	}
	if err := file.Close(); err != nil {
		return Info{}, fmt.Errorf("close blank disk: %w", err)
	}
	if err := os.Rename(tempPath, absTarget); err != nil {
		return Info{}, fmt.Errorf("publish blank disk: %w", err)
	}
	directory, err := os.Open(parent)
	if err != nil {
		return Info{}, fmt.Errorf("open blank disk parent for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return Info{}, fmt.Errorf("sync blank disk parent: %w", err)
	}
	if err := directory.Close(); err != nil {
		return Info{}, fmt.Errorf("close blank disk parent: %w", err)
	}
	return info, nil
}

// CreateOverlay creates, optionally grows, verifies, fsyncs, and atomically
// publishes a root overlay. Existing targets and symlink bases are rejected.
func (m Manager) CreateOverlay(ctx context.Context, basePath, targetPath string, targetSize int64) (Info, error) {
	if err := m.validate(); err != nil {
		return Info{}, err
	}
	absBase, err := filepath.Abs(basePath)
	if err != nil {
		return Info{}, fmt.Errorf("resolve base path: %w", err)
	}
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return Info{}, fmt.Errorf("resolve target path: %w", err)
	}
	baseStat, err := os.Lstat(absBase)
	if err != nil {
		return Info{}, fmt.Errorf("stat managed base: %w", err)
	}
	if !baseStat.Mode().IsRegular() {
		return Info{}, fmt.Errorf("managed base is not a regular file: %s", absBase)
	}
	if _, err := os.Lstat(absTarget); err == nil {
		return Info{}, fmt.Errorf("overlay target already exists: %s", absTarget)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Info{}, fmt.Errorf("stat overlay target: %w", err)
	}

	baseInfo, err := m.Inspect(ctx, absBase)
	if err != nil {
		return Info{}, fmt.Errorf("inspect managed base: %w", err)
	}
	if err := ValidateBase(baseInfo); err != nil {
		return Info{}, err
	}
	if targetSize == 0 {
		targetSize = baseInfo.VirtualSize
	}
	if targetSize < baseInfo.VirtualSize {
		return Info{}, fmt.Errorf("root target size %d is smaller than base virtual size %d", targetSize, baseInfo.VirtualSize)
	}

	parent := filepath.Dir(absTarget)
	parentStat, err := os.Stat(parent)
	if err != nil {
		return Info{}, fmt.Errorf("stat overlay parent: %w", err)
	}
	if !parentStat.IsDir() {
		return Info{}, fmt.Errorf("overlay parent is not a directory: %s", parent)
	}
	placeholder, err := os.CreateTemp(parent, "."+filepath.Base(absTarget)+".partial-")
	if err != nil {
		return Info{}, fmt.Errorf("reserve overlay temporary path: %w", err)
	}
	tempPath := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		_ = os.Remove(tempPath)
		return Info{}, fmt.Errorf("close overlay placeholder: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return Info{}, fmt.Errorf("release overlay placeholder: %w", err)
	}
	defer func() { _ = os.Remove(tempPath) }()

	if _, err := m.Runner.Run(ctx, m.QEMUImg, "create", "-f", "qcow2", "-F", "qcow2", "-b", absBase, tempPath); err != nil {
		return Info{}, fmt.Errorf("create root overlay: %w", err)
	}
	if targetSize > baseInfo.VirtualSize {
		if _, err := m.Runner.Run(ctx, m.QEMUImg, "resize", "-f", "qcow2", tempPath, strconv.FormatInt(targetSize, 10)); err != nil {
			return Info{}, fmt.Errorf("resize root overlay: %w", err)
		}
	}

	chain, err := m.inspectChain(ctx, tempPath)
	if err != nil {
		return Info{}, fmt.Errorf("verify root overlay chain: %w", err)
	}
	overlay := chain[0]
	if overlay.Format != "qcow2" || overlay.VirtualSize != targetSize {
		return Info{}, fmt.Errorf("root overlay verification mismatch: format=%q size=%d", overlay.Format, overlay.VirtualSize)
	}
	if overlay.FullBackingFilename != absBase || overlay.BackingFilenameFormat != "qcow2" {
		return Info{}, fmt.Errorf("root overlay backing mismatch: path=%q format=%q", overlay.FullBackingFilename, overlay.BackingFilenameFormat)
	}

	tempFile, err := os.OpenFile(tempPath, os.O_RDWR, 0)
	if err != nil {
		return Info{}, fmt.Errorf("open verified overlay for sync: %w", err)
	}
	if err := tempFile.Chmod(0o600); err != nil {
		_ = tempFile.Close()
		return Info{}, fmt.Errorf("set overlay permissions: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return Info{}, fmt.Errorf("sync verified overlay: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return Info{}, fmt.Errorf("close verified overlay: %w", err)
	}
	if err := os.Rename(tempPath, absTarget); err != nil {
		return Info{}, fmt.Errorf("publish verified overlay: %w", err)
	}
	parentFile, err := os.Open(parent)
	if err != nil {
		return Info{}, fmt.Errorf("open overlay parent for sync: %w", err)
	}
	if err := parentFile.Sync(); err != nil {
		_ = parentFile.Close()
		return Info{}, fmt.Errorf("sync overlay parent: %w", err)
	}
	if err := parentFile.Close(); err != nil {
		return Info{}, fmt.Errorf("close overlay parent: %w", err)
	}
	return overlay, nil
}
