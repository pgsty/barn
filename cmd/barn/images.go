package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/linuxvm"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

// imageService resolves the deployment data root and returns the image
// façade over it.
func imageService(repository string, mirror bool, progress activity.Reporter) (image.Service, error) {
	dataRoot, err := state.ResolveDataRoot()
	if err != nil {
		return image.Service{}, err
	}
	return image.Service{DataRoot: dataRoot, Repository: repository, Mirror: mirror, Progress: progress}, nil
}

// catalogOrigin names where the active catalog came from in words a user
// recognizes: the built-in copy, a repository host, or a local file.
func catalogOrigin(source string) string {
	switch {
	case source == "embedded", source == "":
		return "built-in"
	case strings.HasPrefix(source, "local:"):
		return "local"
	}
	if parsed, err := url.Parse(source); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return source
}

func printImageStatusWarning(out io.Writer, entry image.Entry) {
	switch {
	case entry.Status == "deprecated" && entry.Alias == "el7":
		warningf(out, "image %s/%s (%s) is deprecated and EOL; use only for isolated compatibility testing", entry.Alias, entry.Arch, entry.Release)
	case entry.Status == "deprecated":
		warningf(out, "image %s/%s (%s) is deprecated and being phased out; prefer a current supported image", entry.Alias, entry.Arch, entry.Release)
	case entry.Status != "" && entry.Status != "supported":
		warningf(out, "image %s/%s (%s) has status %s, not supported; use only with the corresponding test/risk acceptance", entry.Alias, entry.Arch, entry.Release, entry.Status)
	}
}

func resolveImageArch(rawArch string, refs ...string) (string, error) {
	if rawArch == "" {
		return "", nil
	}
	normalized, err := platform.NormalizeArch(rawArch)
	if err != nil {
		return "", newUsageError(fmt.Errorf("--arch: %w", err))
	}
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if resolved, refErr := image.ResolveReference(ref); refErr == nil && resolved.Arch != "" && normalized != resolved.Arch {
			return "", newUsageError(failure.New(failure.Usage, fmt.Errorf("image alias %q targets %s, which conflicts with --arch %s", strings.TrimSpace(ref), resolved.Arch, normalized)).Because("arch_conflict"))
		}
	}
	guestArch, err := platform.GuestArch(normalized, runtime.GOARCH)
	if err != nil {
		return "", newUsageError(fmt.Errorf("--arch: %w", err))
	}
	return guestArch, nil
}

func runUpdateCatalog(parent context.Context, repository string, mirror bool, stderr io.Writer) (commandOutcome, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	var progressItem *progress
	service, err := imageService(repository, mirror, deferredProgressReporter(&progressItem))
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	progressItem = startImageProgress(ctx, stderr, "update", "catalog")
	result, err := service.UpdateCatalog(ctx)
	finishProgressItem(progressItem, "catalog", "active", fmt.Sprintf("catalog revision %d", result.ActiveVersion), err)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
		if result.Updated {
			printItemResult(stdout, "catalog", "active", fmt.Sprintf("updated image catalog from revision %d to %d", result.PreviousVersion, result.ActiveVersion))
		} else {
			printItemResult(stdout, "catalog", "unchanged", fmt.Sprintf("image catalog revision %d is current", result.ActiveVersion))
		}
		textField(stdout, 12, "repository", result.Repository)
		textField(stdout, 12, "source", result.Source)
		return nil
	}}, nil
}

type imageUpdateCLIOptions struct {
	Repository string
	Mirror     bool
	Selectors  []string
	Arch       string
	DryRun     bool
	Prune      bool
}

func runImageUpdate(parent context.Context, options imageUpdateCLIOptions, stderr io.Writer) (commandOutcome, error) {
	arch, err := resolveImageArch(options.Arch, options.Selectors...)
	if err != nil {
		return commandOutcome{}, err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var progressItem *progress
	service, err := imageService(options.Repository, options.Mirror, deferredProgressReporter(&progressItem))
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	operation := "image update"
	if options.DryRun {
		operation += " plan"
	}
	progressItem = startOperationProgress(ctx, stderr, "Updating image catalog and cached images", operation)
	report, err := service.UpdateImages(ctx, image.ImageUpdateOptions{
		Selectors: options.Selectors,
		Arch:      arch,
		DryRun:    options.DryRun,
		Prune:     options.Prune,
		NodeRefs: func(context.Context) (map[string]struct{}, error) {
			return linuxvm.NodeImageReferences(service.DataRoot)
		},
	})
	progressItem.Stop(err)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	for _, item := range report.Images {
		if item.State == "updated" || item.State == "planned" {
			printImageStatusWarning(stderr, image.Entry{Alias: item.Alias, Arch: item.Arch, Release: item.Release, Status: item.Status})
		}
	}
	renderText := func(stdout, _ io.Writer) error {
		renderImageUpdateText(stdout, report)
		return nil
	}
	if err != nil {
		if len(report.Images) != 0 || report.Prune != nil {
			return commandOutcome{}, newCommandError(exitRuntime, err).withPayload(report).withText(renderText)
		}
		return commandOutcome{}, newCommandError(exitRuntime, err)
	}
	return commandOutcome{payload: report, text: renderText}, nil
}

func runImageList(parent context.Context, repository string) (commandOutcome, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	service, err := imageService(repository, false, nil)
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	entries, manifestState, err := service.List(ctx)
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	payload := struct {
		Manifest image.ManifestState `json:"manifest"`
		Images   []image.Entry       `json:"images"`
	}{manifestState, entries}
	return commandOutcome{payload: payload, text: func(stdout, _ io.Writer) error {
		textField(stdout, 9, "catalog", fmt.Sprintf("revision %d (%s)", manifestState.ActiveVersion, catalogOrigin(manifestState.Source)))
		rows := make([][]string, 0, len(entries))
		for _, entry := range entries {
			rows = append(rows, []string{entry.Alias, entry.Status, entry.Release, entry.Arch, strings.Join(entry.Channels, ",")})
		}
		printTable(stdout, []string{"IMAGE", "STATE", "RELEASE", "ARCH", "CHANNELS"}, rows, 1)
		return nil
	}}, nil
}

func runImageInfo(parent context.Context, alias, rawArch, repository string, stderr io.Writer) (commandOutcome, error) {
	arch, err := resolveImageArch(rawArch, alias)
	if err != nil {
		return commandOutcome{}, err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	service, err := imageService(repository, false, nil)
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	var info image.Info
	if arch == "" {
		info, err = service.Info(ctx, alias)
	} else {
		info, err = service.InfoArch(ctx, alias, arch)
	}
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	return imageInfoOutcome(info, stderr), nil
}

func runImagePull(parent context.Context, alias, rawArch, repository string, mirror bool, stderr io.Writer) (commandOutcome, error) {
	arch, err := resolveImageArch(rawArch, alias)
	if err != nil {
		return commandOutcome{}, err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var progressItem *progress
	service, err := imageService(repository, mirror, deferredProgressReporter(&progressItem))
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	debugf(stderr, "image pull alias=%s (inactivity timeout; resumable)", alias)
	progressItem = startImageProgress(ctx, stderr, "pull", alias)
	var info image.Info
	if arch == "" {
		info, err = service.PullAlias(ctx, alias)
	} else {
		info, err = service.PullArch(ctx, alias, arch)
	}
	finishProgressItem(progressItem, alias, "cached", info.Entry.Release+", "+info.Entry.Arch, err)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	return imageInfoOutcome(info, stderr), nil
}

func imageInfoOutcome(info image.Info, stderr io.Writer) commandOutcome {
	printImageStatusWarning(stderr, info.Entry)
	return commandOutcome{payload: info, text: func(stdout, _ io.Writer) error {
		if !lifecycleProgressFrozen(stdout) {
			state := "available"
			if info.Cached {
				state = "cached"
			}
			printTable(stdout, []string{"IMAGE", "STATE", "ARCH", "RELEASE"}, [][]string{{info.Entry.Alias, state, info.Entry.Arch, info.Entry.Release}}, 1)
		}
		textField(stdout, 10, "sha256", info.Entry.SHA256)
		if info.Path != "" {
			bestEffortf(stdout, "path: %s\n", info.Path)
		}
		return nil
	}}
}

func runImagePrune(parent context.Context, dryRun, apply bool, repository string, stderr io.Writer) (commandOutcome, error) {
	if dryRun && apply {
		return commandOutcome{}, newUsageError(errors.New("--dry-run and --yes are mutually exclusive"))
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	var progressItem *progress
	service, err := imageService(repository, false, deferredProgressReporter(&progressItem))
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	operation := "image prune"
	if !apply {
		operation += " plan"
	}
	progressItem = startOperationProgress(ctx, stderr, "Scanning unreferenced image cache entries", operation)
	report, err := service.PruneAll(ctx, apply, func(context.Context) (map[string]struct{}, error) {
		return linuxvm.NodeImageReferences(service.DataRoot)
	})
	progressItem.Stop(err)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	if err != nil {
		return commandOutcome{}, newCommandError(exitRuntime, err)
	}
	return commandOutcome{payload: report, text: func(stdout, _ io.Writer) error {
		if len(report.Items) == 0 {
			printItemResult(stdout, "cache", "unchanged", "no unreferenced cache images")
			return nil
		}
		if !lifecycleProgressFrozen(stdout) {
			rows := make([][]string, 0, len(report.Items))
			for _, item := range report.Items {
				state := "planned"
				if item.Applied {
					state = "removed"
				}
				rows = append(rows, []string{item.ImagePath, state, item.Kind, progressBytes(item.Bytes)})
			}
			printTable(stdout, []string{"IMAGE", "STATE", "KIND", "SIZE"}, rows, 1)
		}
		return nil
	}}, nil
}

func runImageSync(parent context.Context, source string, allowDowngrade bool, repository string, stderr io.Writer) (commandOutcome, error) {
	if source == "" {
		return commandOutcome{}, newUsageError(errors.New("image sync requires a URL or path"))
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	debugf(stderr, "image catalog sync source=%s allow_downgrade=%t", progressSource(source), allowDowngrade)
	var progressItem *progress
	service, err := imageService(repository, false, deferredProgressReporter(&progressItem))
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	progressItem = startImageProgress(ctx, stderr, "sync", "catalog")
	manifestState, err := service.SyncManifest(ctx, source, allowDowngrade)
	finishProgressItem(progressItem, "catalog", "active", fmt.Sprintf("catalog revision %d", manifestState.ActiveVersion), err)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	return commandOutcome{payload: manifestState, text: func(stdout, _ io.Writer) error {
		printItemResult(stdout, "catalog", "active", fmt.Sprintf("activated image catalog revision %d", manifestState.ActiveVersion))
		textField(stdout, 10, "sha256", manifestState.ActiveDigest)
		textField(stdout, 10, "key", manifestState.KeyID)
		return nil
	}}, nil
}

func runImageReset(parent context.Context, repository string) (commandOutcome, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	service, err := imageService(repository, false, nil)
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	manifestState, err := service.ResetManifest(ctx)
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	return commandOutcome{payload: manifestState, text: func(stdout, _ io.Writer) error {
		printItemResult(stdout, "catalog", "active", fmt.Sprintf("image catalog reset to the built-in revision %d; downgrade protection still at %d", manifestState.ActiveVersion, manifestState.HighestVersion))
		return nil
	}}, nil
}

type imageImportCLIOptions struct {
	Path           string
	ExpectedSHA256 string
	Name           string
	Arch           string
	Boot           string
	SourceUser     string
}

func runImageImport(parent context.Context, options imageImportCLIOptions, stderr io.Writer) (commandOutcome, error) {
	arch, err := resolveImageArch(options.Arch)
	if err != nil {
		return commandOutcome{}, err
	}
	if options.Path == "" {
		return commandOutcome{}, newUsageError(errors.New("image import requires a path"))
	}
	if arch != "" && options.Name == "" {
		return commandOutcome{}, newUsageError(errors.New("--arch requires --name so the imported image's architecture can be recorded"))
	}
	invalidAliasMetadata := (options.Name == "" && (options.Boot != "" || options.SourceUser != "")) || (options.Name != "" && (options.SourceUser == "" || (options.Boot != "bios" && options.Boot != "uefi")))
	if invalidAliasMetadata {
		return commandOutcome{}, newUsageError(errors.New("--name requires --boot bios|uefi and --source-user; alias metadata is immutable"))
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	var progressItem *progress
	service, err := imageService("", false, deferredProgressReporter(&progressItem))
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	debugf(stderr, "image import source=%s expected_sha256=%s alias=%s", options.Path, options.ExpectedSHA256, options.Name)
	name := options.Name
	if name == "" {
		name = filepath.Base(options.Path)
	}
	progressItem = startImageProgress(ctx, stderr, "import", name)
	var path string
	var metadata image.Metadata
	var localEntry *image.Entry
	if options.Name == "" {
		path, metadata, err = service.ImportFile(ctx, options.Path, options.ExpectedSHA256)
	} else {
		entry, importedPath, importedMetadata, importErr := service.ImportNamed(ctx, options.Path, options.ExpectedSHA256, options.Name, arch, options.Boot, options.SourceUser)
		path, metadata, err = importedPath, importedMetadata, importErr
		if importErr == nil {
			localEntry = &entry
		}
	}
	finishProgressItem(progressItem, name, "imported", progressBytes(metadata.ArtifactSize)+", verified", err)
	if errors.Is(parent.Err(), context.Canceled) {
		return commandOutcome{}, ErrCancelled
	}
	if err != nil {
		return commandOutcome{}, newRuntimeError(err)
	}
	result := struct {
		Path     string         `json:"path"`
		Metadata image.Metadata `json:"metadata"`
		Entry    *image.Entry   `json:"entry,omitempty"`
	}{Path: path, Metadata: metadata, Entry: localEntry}
	return commandOutcome{payload: result, text: func(stdout, _ io.Writer) error {
		if path != "" {
			printItemResult(stdout, name, "imported", progressBytes(metadata.ArtifactSize)+", verified")
			textField(stdout, 10, "path", path)
			textField(stdout, 10, "sha256", metadata.Digest)
		}
		return nil
	}}, nil
}

// checkInventoryImages resolves each node's image against the repository's
// active local catalog without touching the network, so validate reports an
// unknown image, channel, or version with the choices instead of leaving it to
// plan or up. Only a definite miss fails; a catalog that cannot be read is a
// warning, and local aliases are left to up, which verifies their bytes.
func checkInventoryImages(ctx context.Context, resolved spec.Resolved, repository string) ([]string, error) {
	service, err := imageService(repository, false, nil)
	if err == nil {
		var session *image.CatalogSession
		if session, err = service.OpenCatalog(); err == nil {
			return nil, lookupInventoryImages(ctx, session, resolved)
		}
	}
	return []string{"images were not checked: " + err.Error()}, nil
}

func lookupInventoryImages(ctx context.Context, session *image.CatalogSession, resolved spec.Resolved) error {
	arch := lifecycleImageArch(resolved)
	checked := make(map[string]image.Entry)
	for _, node := range resolved.Nodes {
		reference := node.Image
		if reference == "" {
			reference = resolved.Image
		}
		if image.IsLocalAlias(reference) {
			continue
		}
		entry, ok := checked[reference]
		if !ok {
			var err error
			entry, err = session.LookupArch(ctx, reference, arch)
			if err != nil {
				return fmt.Errorf("host %s (%s): %w", node.Address, node.Name, err)
			}
			checked[reference] = entry
		}
		if node.RootDisk < entry.VirtualSize {
			return fmt.Errorf("host %s (%s): root disk %s is smaller than image virtual size %s", node.Address, node.Name, planSize(node.RootDisk), planSize(entry.VirtualSize))
		}
	}
	return nil
}

func renderImageUpdateText(stdout io.Writer, report image.ImageUpdateReport) {
	frozen := lifecycleProgressFrozen(stdout)
	if !frozen {
		switch {
		case report.Catalog.Updated && report.DryRun:
			printItemResult(stdout, "catalog", "planned", fmt.Sprintf("would update image catalog from revision %d to %d", report.Catalog.PreviousVersion, report.Catalog.ActiveVersion))
		case report.Catalog.Updated:
			printItemResult(stdout, "catalog", "active", fmt.Sprintf("updated image catalog from revision %d to %d", report.Catalog.PreviousVersion, report.Catalog.ActiveVersion))
		default:
			printItemResult(stdout, "catalog", "unchanged", fmt.Sprintf("image catalog revision %d is current", report.Catalog.ActiveVersion))
		}
		textField(stdout, 12, "repository", report.Catalog.Repository)
	}

	if len(report.Images) == 0 {
		printResultSummary(stdout, "images", "unchanged", "no cached catalog images to update")
	} else {
		var updatedCount, plannedCount, unchangedCount, skippedCount, failedCount int
		var transferBytes int64
		showMessage := false
		for _, item := range report.Images {
			switch item.State {
			case "updated":
				updatedCount++
				transferBytes += item.ArtifactSize
			case "planned":
				plannedCount++
				transferBytes += item.ArtifactSize
			case "unchanged":
				unchangedCount++
			case "skipped":
				skippedCount++
				showMessage = true
			case "failed":
				failedCount++
				showMessage = true
			}
		}
		if !frozen {
			header := []string{"IMAGE", "ARCH", "STATE", "RELEASE", "SIZE"}
			if showMessage {
				header = append(header, "MESSAGE")
			}
			rows := make([][]string, 0, len(report.Images))
			for _, item := range report.Images {
				archCell := item.Arch
				if archCell == "" {
					archCell = "-"
				}
				releaseCell := "-"
				switch {
				case (item.State == "updated" || item.State == "planned") && len(item.PreviousReleases) > 0:
					releaseCell = item.PreviousReleases[len(item.PreviousReleases)-1] + " -> " + item.Release
				case item.Release != "":
					releaseCell = item.Release
				case len(item.PreviousReleases) > 0:
					releaseCell = item.PreviousReleases[len(item.PreviousReleases)-1]
				}
				sizeCell := "-"
				if (item.State == "updated" || item.State == "planned") && item.ArtifactSize > 0 {
					sizeCell = progressBytes(item.ArtifactSize)
				}
				row := []string{item.Alias, archCell, item.State, releaseCell, sizeCell}
				if showMessage {
					message := ""
					if item.State == "skipped" || item.State == "failed" {
						message = item.Message
						if message == "" {
							message = item.Error
						}
					}
					row = append(row, message)
				}
				rows = append(rows, row)
			}
			bestEffortln(stdout)
			printTable(stdout, header, rows, 2)
		}
		parts := make([]string, 0, 4)
		if updatedCount > 0 {
			parts = append(parts, fmt.Sprintf("%d updated (%s)", updatedCount, progressBytes(transferBytes)))
		}
		if plannedCount > 0 {
			parts = append(parts, fmt.Sprintf("%d planned (%s)", plannedCount, progressBytes(transferBytes)))
		}
		if unchangedCount > 0 {
			parts = append(parts, fmt.Sprintf("%d unchanged", unchangedCount))
		}
		if skippedCount > 0 {
			parts = append(parts, fmt.Sprintf("%d skipped", skippedCount))
		}
		if failedCount > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", failedCount))
		}
		summaryState := "unchanged"
		switch {
		case failedCount > 0 && updatedCount+unchangedCount > 0:
			summaryState = "partial"
		case failedCount > 0:
			summaryState = "failed"
		case updatedCount > 0:
			summaryState = "updated"
		case plannedCount > 0:
			summaryState = "planned"
		case skippedCount > 0:
			summaryState = "skipped"
		}
		printResultSummary(stdout, "images", summaryState, strings.Join(parts, ", "))
	}

	if report.Prune != nil {
		if len(report.Prune.Items) == 0 {
			printResultSummary(stdout, "prune", "unchanged", "no unreferenced cache images")
			return
		}
		var pruneBytes int64
		pruneRows := make([][]string, 0, len(report.Prune.Items))
		for _, item := range report.Prune.Items {
			pruneBytes += item.Bytes
			state := "planned"
			if item.Applied {
				state = "removed"
			}
			pruneRows = append(pruneRows, []string{item.ImagePath, state, item.Kind, progressBytes(item.Bytes)})
		}
		if !frozen {
			bestEffortln(stdout)
			printTable(stdout, []string{"IMAGE", "STATE", "KIND", "SIZE"}, pruneRows, 1)
		}
		state, verb := "planned", "would remove"
		if report.Prune.Apply {
			state, verb = "removed", "removed"
		}
		printResultSummary(stdout, "prune", state, fmt.Sprintf("%s %d file(s) (%s)", verb, len(report.Prune.Items), progressBytes(pruneBytes)))
	}
}
