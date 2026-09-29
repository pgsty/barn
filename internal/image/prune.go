package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/disk"
	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/lock"
)

type PruneItem struct {
	Kind      string `json:"kind"`
	Digest    string `json:"digest,omitempty"`
	ImagePath string `json:"image_path"`
	Bytes     int64  `json:"bytes"`
	Applied   bool   `json:"applied"`
}

type PruneReport struct {
	Apply      bool        `json:"apply"`
	Referenced []string    `json:"referenced"`
	Items      []PruneItem `json:"items"`
}

// manifests is the one non-family directory inside images/.
var nonImageRoots = map[string]struct{}{
	"manifests": {},
}

func stagingImageName(name string) bool {
	if !strings.HasSuffix(name, ".partial") {
		return false
	}
	return strings.HasPrefix(name, ".download-") || strings.HasPrefix(name, ".import-") || strings.HasPrefix(name, ".repository-")
}

type pruneCandidate struct {
	path string
	kind string
}

func (s Store) scanPrune(ctx context.Context, referenced map[string]struct{}, apply bool) (PruneReport, error) {
	report := PruneReport{Apply: apply, Referenced: make([]string, 0, len(referenced)), Items: []PruneItem{}}
	for digest := range referenced {
		report.Referenced = append(report.Referenced, digest)
	}
	sort.Strings(report.Referenced)
	roots, err := os.ReadDir(s.imagesRoot())
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	candidates := make([]pruneCandidate, 0)
	for _, root := range roots {
		if _, skip := nonImageRoots[root.Name()]; skip {
			continue
		}
		if !catalogName.MatchString(root.Name()) {
			continue
		}
		rootPath := filepath.Join(s.imagesRoot(), root.Name())
		rootInfo, statErr := os.Lstat(rootPath)
		if statErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm()&0o022 != 0 {
			return report, fmt.Errorf("image family directory is unsafe: %s", rootPath)
		}
		files, readErr := os.ReadDir(rootPath)
		if readErr != nil {
			return report, readErr
		}
		for _, file := range files {
			extension := strings.ToLower(filepath.Ext(file.Name()))
			kind := ""
			switch {
			case extension == ".qcow2" || extension == ".img":
				kind = "image"
			case stagingImageName(file.Name()):
				kind = "staging"
			}
			if kind != "" {
				candidates = append(candidates, pruneCandidate{path: filepath.Join(rootPath, file.Name()), kind: kind})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].path < candidates[j].path })
	for _, candidate := range candidates {
		info, statErr := os.Lstat(candidate.path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return report, fmt.Errorf("managed image cache entry is not a regular non-symlink file: %s", candidate.path)
		}
		if candidate.kind == "staging" {
			if info.Mode().Perm()&0o077 != 0 {
				return report, fmt.Errorf("managed image staging file has unsafe permissions: %s", candidate.path)
			}
			item := PruneItem{Kind: candidate.kind, ImagePath: candidate.path, Bytes: info.Size()}
			if apply {
				if removeErr := os.Remove(candidate.path); removeErr != nil {
					return report, removeErr
				}
				if syncErr := fsutil.SyncDir(filepath.Dir(candidate.path)); syncErr != nil {
					return report, syncErr
				}
				item.Applied = true
			}
			report.Items = append(report.Items, item)
			continue
		}
		if info.Mode().Perm()&0o222 != 0 {
			return report, fmt.Errorf("managed image is not immutable: %s", candidate.path)
		}
		digest, size, digestErr := digestFile(candidate.path)
		if digestErr != nil {
			return report, digestErr
		}
		imageInfo, inspectErr := s.manager().Inspect(ctx, candidate.path)
		if inspectErr != nil {
			return report, inspectErr
		}
		if validateErr := disk.ValidateBase(imageInfo); validateErr != nil {
			return report, validateErr
		}
		if _, keep := referenced[digest]; keep {
			continue
		}
		item := PruneItem{Kind: candidate.kind, Digest: digest, ImagePath: candidate.path, Bytes: size}
		if apply {
			if removeErr := os.Remove(candidate.path); removeErr != nil {
				return report, removeErr
			}
			if syncErr := fsutil.SyncDir(filepath.Dir(candidate.path)); syncErr != nil {
				return report, syncErr
			}
			item.Applied = true
		}
		report.Items = append(report.Items, item)
	}
	return report, nil
}

func (s Store) Prune(ctx context.Context, apply bool, resolveReferences func() (map[string]struct{}, error)) (_ PruneReport, returnErr error) {
	if s.DataRoot == "" || !filepath.IsAbs(s.DataRoot) || s.QEMUImg == "" || s.Runner == nil {
		return PruneReport{}, errors.New("image prune store is incomplete")
	}
	if resolveReferences == nil {
		return PruneReport{}, errors.New("image prune reference resolver is required")
	}
	rootInfo, err := os.Lstat(s.DataRoot)
	if errors.Is(err, os.ErrNotExist) {
		return PruneReport{Apply: apply, Items: []PruneItem{}}, nil
	}
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm()&0o022 != 0 {
		return PruneReport{}, errors.New("barn home directory is unsafe")
	}
	if !apply {
		referenced, resolveErr := resolveReferences()
		if resolveErr != nil {
			return PruneReport{}, resolveErr
		}
		return s.scanPrune(ctx, referenced, false)
	}
	lockContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	imageLock, err := lock.Acquire(lockContext, s.lockPath(), false)
	if err != nil {
		return PruneReport{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, imageLock, "image prune lock")
	}()
	referenced, err := resolveReferences()
	if err != nil {
		return PruneReport{}, err
	}
	return s.scanPrune(ctx, referenced, true)
}
