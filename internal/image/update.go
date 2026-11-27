package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/failure"
)

// ImageUpdateOptions configures a catalog-and-cache update run.
type ImageUpdateOptions struct {
	Selectors []string
	Arch      string
	DryRun    bool
	Prune     bool
	NodeRefs  func(context.Context) ([]Entry, error)
}

// ImageUpdateItem describes one cached or requested image evaluated by UpdateImages.
type ImageUpdateItem struct {
	Alias            string   `json:"alias"`
	Channel          string   `json:"channel,omitempty"`
	Arch             string   `json:"arch,omitempty"`
	State            string   `json:"state"`
	PreviousReleases []string `json:"previous_releases,omitempty"`
	Release          string   `json:"release,omitempty"`
	SHA256           string   `json:"sha256,omitempty"`
	ArtifactSize     int64    `json:"artifact_size,omitempty"`
	Status           string   `json:"status,omitempty"`
	Path             string   `json:"path,omitempty"`
	Message          string   `json:"message,omitempty"`
	Error            string   `json:"error,omitempty"`
}

// ImageUpdateReport summarizes a complete catalog and cached-image update run.
type ImageUpdateReport struct {
	Catalog CatalogUpdate     `json:"catalog"`
	DryRun  bool              `json:"dry_run"`
	Images  []ImageUpdateItem `json:"images"`
	Prune   *PruneReport      `json:"prune,omitempty"`
}

type cachedImageKey struct {
	alias string
	arch  string
}

type updateTarget struct {
	alias          string
	channel        string
	arch           string
	itemName       string
	cachedReleases []string
	entry          *Entry
	skipMessage    string
}

func formatUpdateItemName(alias, arch string) string {
	if arch == "" {
		return alias
	}
	return fmt.Sprintf("%s (%s)", alias, arch)
}

func parseCachedImageFilename(family, filename string) (release, arch string, ok bool) {
	prefix := family + "-"
	if !strings.HasPrefix(filename, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(filename, prefix)
	for _, candidateArch := range []string{"amd64", "arm64"} {
		suffix := "-" + candidateArch + ".qcow2"
		if strings.HasSuffix(rest, suffix) {
			release = strings.TrimSuffix(rest, suffix)
			if releaseName.MatchString(release) {
				return release, candidateArch, true
			}
		}
	}
	return "", "", false
}

func compareCachedReleases(left, right string) int {
	leftParts, leftErr := numericVersionParts(left)
	rightParts, rightErr := numericVersionParts(right)
	if leftErr == nil && rightErr == nil {
		if cmp := compareNumericVersions(leftParts, rightParts); cmp != 0 {
			return cmp
		}
	}
	return strings.Compare(left, right)
}

// scanCachedImages discovers locally cached catalog images by directory and
// filename rather than intersecting only against the current catalog. An older
// retired release on disk still identifies a family/architecture the user has
// cached and expects updated.
func scanCachedImages(dataRoot string) (map[cachedImageKey][]string, error) {
	result := make(map[cachedImageKey][]string)
	if dataRoot == "" || !filepath.IsAbs(dataRoot) {
		return nil, errors.New("image data root must be absolute")
	}
	imagesRoot := (Store{DataRoot: dataRoot}).imagesRoot()
	roots, err := os.ReadDir(imagesRoot)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	for _, root := range roots {
		family := root.Name()
		if _, skip := nonImageRoots[family]; skip || family == "local" {
			continue
		}
		rootPath := filepath.Join(imagesRoot, family)
		rootInfo, statErr := os.Lstat(rootPath)
		if statErr != nil {
			return nil, statErr
		}
		if rootInfo.Mode()&os.ModeSymlink != 0 {
			if catalogName.MatchString(family) {
				return nil, fmt.Errorf("image family directory is unsafe: %s", rootPath)
			}
			continue
		}
		if !rootInfo.IsDir() || !catalogName.MatchString(family) {
			continue
		}
		if rootInfo.Mode().Perm()&0o022 != 0 {
			return nil, fmt.Errorf("image family directory is unsafe: %s", rootPath)
		}
		files, readErr := os.ReadDir(rootPath)
		if readErr != nil {
			return nil, readErr
		}
		for _, file := range files {
			name := file.Name()
			if stagingImageName(name) || quarantinedImageName(name) {
				continue
			}
			release, arch, ok := parseCachedImageFilename(family, name)
			if !ok {
				continue
			}
			fullPath := filepath.Join(rootPath, name)
			info, fileStatErr := os.Lstat(fullPath)
			if fileStatErr != nil {
				return nil, fileStatErr
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("managed image cache entry is not a regular non-symlink file: %s", fullPath)
			}
			key := cachedImageKey{alias: family, arch: arch}
			result[key] = append(result[key], release)
		}
	}
	for key, releases := range result {
		sort.Slice(releases, func(i, j int) bool {
			return compareCachedReleases(releases[i], releases[j]) < 0
		})
		result[key] = releases
	}
	return result, nil
}

func (s Service) refreshCatalog(ctx context.Context, dryRun bool) (Catalog, CatalogUpdate, error) {
	manager, err := s.manifestManager()
	if err != nil {
		return Catalog{}, CatalogUpdate{}, err
	}
	if manager.Repository == "" {
		return Catalog{}, CatalogUpdate{}, errors.New("image catalog update requires a configured repository")
	}
	source, err := RepositoryCatalogSource(manager.Repository)
	if err != nil {
		return Catalog{}, CatalogUpdate{}, err
	}
	result := CatalogUpdate{Repository: manager.Repository, Source: source}
	var current ManifestState
	if catalog, currentState, currentErr := manager.Current(); currentErr == nil {
		result.PreviousVersion = catalog.Version
		current = currentState
	}
	var catalog Catalog
	var state ManifestState
	if dryRun {
		catalog, state, err = manager.Preview(ctx, source)
	} else {
		catalog, state, err = manager.syncCatalog(ctx, source, false)
	}
	if err != nil {
		return Catalog{}, result, err
	}
	result.ActiveVersion = state.ActiveVersion
	result.Updated = state.ActiveVersion != current.ActiveVersion || state.ActiveDigest != current.ActiveDigest
	return catalog, result, nil
}

func prevalidateUpdateSelectors(selectors []string) ([]Reference, error) {
	parsed := make([]Reference, 0, len(selectors))
	for _, raw := range selectors {
		trimmed := strings.TrimSpace(raw)
		ref, err := ParseReference(trimmed)
		if err != nil || ref.Image == "" {
			if err == nil {
				err = errors.New("image selector must not be empty")
			}
			return nil, failure.New(failure.Usage, err)
		}
		if ref.Version != "" {
			return nil, failure.New(failure.Usage, fmt.Errorf("image update does not accept pinned version selector %q; use barn image pull", trimmed))
		}
		if IsLocalAlias(ref.Image) {
			return nil, failure.New(failure.Usage, fmt.Errorf("local image %q cannot be updated from a repository", trimmed))
		}
		parsed = append(parsed, ref)
	}
	return parsed, nil
}

func buildUpdateTargets(catalog Catalog, cached map[cachedImageKey][]string, parsedSelectors []Reference, archFilter string) ([]updateTarget, error) {
	if len(parsedSelectors) == 0 {
		keys := make([]cachedImageKey, 0, len(cached))
		for key := range cached {
			if archFilter != "" && key.arch != archFilter {
				continue
			}
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].alias != keys[j].alias {
				return keys[i].alias < keys[j].alias
			}
			return keys[i].arch < keys[j].arch
		})
		targets := make([]updateTarget, 0, len(keys))
		for _, key := range keys {
			target := updateTarget{
				alias:          key.alias,
				channel:        catalog.Defaults.Channel,
				arch:           key.arch,
				itemName:       formatUpdateItemName(key.alias, key.arch),
				cachedReleases: append([]string(nil), cached[key]...),
			}
			entry, err := catalog.Entry(key.alias, key.arch)
			if err != nil {
				if _, exists := catalog.Images[key.alias]; !exists {
					target.skipMessage = "not in the active catalog"
				} else {
					target.skipMessage = fmt.Sprintf("%s build is not in the active catalog", key.arch)
				}
			} else {
				target.channel = entry.Channel
				target.entry = &entry
			}
			targets = append(targets, target)
		}
		return targets, nil
	}

	seen := make(map[string]struct{}, len(parsedSelectors)*2)
	targets := make([]updateTarget, 0, len(parsedSelectors)*2)
	addTarget := func(target updateTarget) {
		dedupKey := target.alias + "\x00" + target.channel + "\x00" + target.arch
		if _, duplicate := seen[dedupKey]; duplicate {
			return
		}
		seen[dedupKey] = struct{}{}
		targets = append(targets, target)
	}

	for _, ref := range parsedSelectors {
		canonical, impliedArch, record, err := catalog.resolveImage(ref.Image)
		if err != nil {
			return nil, err
		}
		channel := ref.Channel
		if channel == "" {
			channel = catalog.Defaults.Channel
		}
		if _, ok := record.Channels[channel]; !ok {
			return nil, unknownSelection(fmt.Errorf("image %s has no channel %q; available: %s", canonical, channel, strings.Join(sortedKeys(record.Channels), ", ")))
		}
		if impliedArch != "" && archFilter != "" && archFilter != impliedArch {
			return nil, failure.New(failure.Usage, fmt.Errorf("image alias %q targets %s, which conflicts with requested architecture %s", ref.Image, impliedArch, archFilter)).Because("arch_conflict")
		}
		explicitArch := impliedArch
		if explicitArch == "" {
			explicitArch = archFilter
		}
		var arches []string
		if explicitArch != "" {
			arches = []string{explicitArch}
		} else {
			for _, arch := range []string{"amd64", "arm64"} {
				if len(cached[cachedImageKey{alias: canonical, arch: arch}]) != 0 {
					arches = append(arches, arch)
				}
			}
			if len(arches) == 0 {
				arches = []string{runtime.GOARCH}
			}
		}
		for _, arch := range arches {
			releases := cached[cachedImageKey{alias: canonical, arch: arch}]
			entry, entryErr := catalog.Entry(canonical+":"+channel, arch)
			if entryErr != nil && len(releases) == 0 {
				return nil, entryErr
			}
			target := updateTarget{
				alias:          canonical,
				channel:        channel,
				arch:           arch,
				itemName:       formatUpdateItemName(canonical, arch),
				cachedReleases: append([]string(nil), releases...),
			}
			if entryErr != nil {
				target.skipMessage = fmt.Sprintf("%s build is not in the active catalog", arch)
			} else {
				target.entry = &entry
			}
			addTarget(target)
		}
	}
	return targets, nil
}

func (s Store) dryRunTargetCached(entry Entry) (string, bool, error) {
	cachePath, err := s.Path(entry)
	if err != nil {
		return "", false, err
	}
	info, err := os.Lstat(cachePath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 {
		return "", false, nil
	}
	if entry.ArtifactSize > 0 && info.Size() != entry.ArtifactSize {
		return "", false, nil
	}
	digest, _, err := digestFileWithProgress(cachePath, s.Progress, activity.Event{
		Phase: "image-verify", Message: fmt.Sprintf("Verifying cached image %s %s (%s)", entry.Alias, entry.Release, entry.Arch),
	})
	return cachePath, digest == entry.SHA256, err
}

func previousReleasesExcluding(releases []string, current string) []string {
	filtered := make([]string, 0, len(releases))
	for _, release := range releases {
		if release != current {
			filtered = append(filtered, release)
		}
	}
	return filtered
}

// UpdateImages refreshes the catalog and fetches the requested images.
// Without selectors it updates only previously cached families and architectures.
func (s Service) UpdateImages(ctx context.Context, options ImageUpdateOptions) (ImageUpdateReport, error) {
	report := ImageUpdateReport{DryRun: options.DryRun, Images: []ImageUpdateItem{}}
	if options.Arch != "" {
		if err := validateCatalogArch(options.Arch); err != nil {
			return report, failure.New(failure.Usage, err)
		}
	}
	parsedSelectors, err := prevalidateUpdateSelectors(options.Selectors)
	if err != nil {
		return report, err
	}

	s.Progress.Report(activity.Event{Phase: "image-resolve", Item: "catalog", State: "checking", Message: "checking image catalog"})
	catalogCtx, cancelCatalog := context.WithTimeout(ctx, 2*time.Minute)
	catalog, update, err := s.refreshCatalog(catalogCtx, options.DryRun)
	cancelCatalog()
	report.Catalog = update
	if err != nil {
		state := "failed"
		if errors.Is(err, context.Canceled) {
			state = "cancelled"
		}
		s.Progress.Report(activity.Event{Phase: "image-resolve", Item: "catalog", State: state, Message: err.Error(), Done: true})
		return report, err
	}
	catalogState, catalogMessage := "unchanged", fmt.Sprintf("revision %d is current", report.Catalog.ActiveVersion)
	if report.Catalog.Updated {
		if options.DryRun {
			catalogState = "planned"
			catalogMessage = fmt.Sprintf("would update revision %d -> %d", report.Catalog.PreviousVersion, report.Catalog.ActiveVersion)
		} else {
			catalogState = "active"
			catalogMessage = fmt.Sprintf("updated revision %d -> %d", report.Catalog.PreviousVersion, report.Catalog.ActiveVersion)
		}
	}
	s.Progress.Report(activity.Event{Phase: "image-resolve", Item: "catalog", State: catalogState, Message: catalogMessage, Done: true})

	cached, err := scanCachedImages(s.DataRoot)
	if err != nil {
		return report, err
	}
	targets, err := buildUpdateTargets(catalog, cached, parsedSelectors, options.Arch)
	if err != nil {
		return report, err
	}

	needsStore := false
	for _, target := range targets {
		s.Progress.Report(activity.Event{Phase: "image-update", Item: target.itemName, State: "queued", Message: "waiting to check image"})
		if target.entry != nil {
			needsStore = true
		}
	}

	store := Store{DataRoot: s.DataRoot, Progress: s.Progress}
	if !options.DryRun && needsStore {
		store, err = s.store(update.Repository)
		if err != nil {
			return report, err
		}
	}

	failedCount := 0
	succeededCount := 0
	var firstFailure error
	for _, target := range targets {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if target.entry == nil {
			item := ImageUpdateItem{
				Alias:            target.alias,
				Channel:          target.channel,
				Arch:             target.arch,
				State:            "skipped",
				PreviousReleases: target.cachedReleases,
				Message:          target.skipMessage,
			}
			report.Images = append(report.Images, item)
			s.Progress.Report(activity.Event{Phase: "image-update", Item: target.itemName, State: "skipped", Message: target.skipMessage, Done: true})
			continue
		}

		entry := *target.entry
		item := ImageUpdateItem{
			Alias:            entry.Alias,
			Channel:          entry.Channel,
			Arch:             entry.Arch,
			PreviousReleases: previousReleasesExcluding(target.cachedReleases, entry.Release),
			Release:          entry.Release,
			SHA256:           entry.SHA256,
			ArtifactSize:     entry.ArtifactSize,
			Status:           entry.Status,
		}
		if options.DryRun {
			path, cached, err := store.dryRunTargetCached(entry)
			if err != nil {
				return report, err
			}
			if cached {
				item.State = "unchanged"
				item.Path = path
				item.Message = "already up to date"
			} else {
				item.State = "planned"
				if len(item.PreviousReleases) > 0 {
					item.Message = fmt.Sprintf("would update %s -> %s", item.PreviousReleases[len(item.PreviousReleases)-1], entry.Release)
				} else {
					item.Message = fmt.Sprintf("would update to %s", entry.Release)
				}
			}
			succeededCount++
			report.Images = append(report.Images, item)
			s.Progress.Report(activity.Event{Phase: "image-update", Item: target.itemName, State: item.State, Message: item.Message, Done: true})
			continue
		}

		s.Progress.Report(activity.Event{Phase: "image-update", Item: target.itemName, State: "checking", Message: fmt.Sprintf("checking %s %s (%s)", entry.Alias, entry.Release, entry.Arch)})
		path, metadata, pullErr := store.Pull(ctx, entry)
		if pullErr != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				s.Progress.Report(activity.Event{Phase: "image-update", Item: target.itemName, State: "cancelled", Message: "cancelled", Done: true})
				return report, ctx.Err()
			}
			item.State = "failed"
			item.Error = pullErr.Error()
			item.Message = pullErr.Error()
			report.Images = append(report.Images, item)
			failedCount++
			if firstFailure == nil {
				firstFailure = pullErr
			}
			s.Progress.Report(activity.Event{Phase: "image-update", Item: target.itemName, State: "failed", Message: pullErr.Error(), Done: true})
			continue
		}

		item.Path = path
		if strings.HasPrefix(metadata.Source, "local:") {
			item.State = "unchanged"
			item.Message = "already up to date"
		} else {
			item.State = "updated"
			if len(item.PreviousReleases) > 0 {
				item.Message = fmt.Sprintf("%s -> %s", item.PreviousReleases[len(item.PreviousReleases)-1], entry.Release)
			} else {
				item.Message = fmt.Sprintf("updated to %s", entry.Release)
			}
		}
		succeededCount++
		report.Images = append(report.Images, item)
		s.Progress.Report(activity.Event{Phase: "image-update", Item: target.itemName, State: item.State, Message: item.Message, Done: true})
	}

	if options.Prune && failedCount == 0 {
		pruneReport, pruneErr := s.pruneAllWithCatalog(ctx, !options.DryRun, &catalog, options.NodeRefs)
		if pruneErr != nil {
			return report, pruneErr
		}
		report.Prune = &pruneReport
	}

	if failedCount > 0 {
		actionable := succeededCount + failedCount
		if succeededCount > 0 {
			return report, failure.New(failure.Partial, fmt.Errorf("failed to update %d of %d image(s)", failedCount, actionable)).Because("partial_image_update")
		}
		if failedCount == 1 && firstFailure != nil {
			return report, firstFailure
		}
		return report, fmt.Errorf("failed to update %d image(s)", failedCount)
	}
	return report, nil
}
