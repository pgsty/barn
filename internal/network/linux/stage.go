package linux

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pgsty/barn/internal/fsutil"
)

type StagedTarget struct {
	Source string `json:"source"`
	Owner  string `json:"owner"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

type StagedPlan struct {
	Schema     int                     `json:"schema"`
	StagingDir string                  `json:"staging_dir"`
	Plan       Plan                    `json:"plan"`
	Targets    map[string]StagedTarget `json:"targets"`
}

func PrepareStaging(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return errors.New("staging directory must be a non-root absolute path")
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("staging directory must be a real mode-0700 directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		return errors.New("staging directory must be empty")
	}
	return nil
}

func StageInstallPlan(plan Plan, staging string) (StagedPlan, error) {
	if err := PrepareStaging(staging); err != nil {
		return StagedPlan{}, err
	}
	targets := make(map[string]StagedTarget, len(plan.Files))
	for _, file := range plan.Files {
		relative := strings.TrimPrefix(filepath.Clean(file.Path), "/")
		source := filepath.Join(staging, "rootfs", relative)
		if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
			return StagedPlan{}, err
		}
		modeValue, err := strconv.ParseUint(file.Mode, 8, 32)
		if err != nil {
			return StagedPlan{}, err
		}
		handle, err := os.OpenFile(source, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(modeValue))
		if err != nil {
			return StagedPlan{}, err
		}
		_, writeErr := io.WriteString(handle, file.Content)
		closeErr := handle.Close()
		if writeErr != nil {
			return StagedPlan{}, writeErr
		}
		if closeErr != nil {
			return StagedPlan{}, closeErr
		}
		targets[file.Path] = StagedTarget{Source: source, Owner: file.Owner, Mode: file.Mode, SHA256: fsutil.SHA256Bytes([]byte(file.Content))}
	}
	result := StagedPlan{Schema: 1, StagingDir: staging, Plan: plan, Targets: targets}
	data, err := fsutil.MarshalJSONIndent(result)
	if err != nil {
		return StagedPlan{}, err
	}
	if err := os.WriteFile(filepath.Join(staging, "install-plan.json"), data, 0o600); err != nil {
		return StagedPlan{}, err
	}
	return result, nil
}
