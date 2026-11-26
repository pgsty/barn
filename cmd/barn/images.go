package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/image"
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

func validImageDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == fmt.Sprintf("%x", decoded)
}

// nodeImageReferences collects every image digest a deployment node still
// references, so prune never deletes a base image in use.
func nodeImageReferences(dataRoot string) (map[string]struct{}, error) {
	references := make(map[string]struct{})
	store := state.Store{Root: dataRoot}
	entries, readErr := os.ReadDir(filepath.Join(dataRoot, "nodes"))
	if errors.Is(readErr, os.ErrNotExist) {
		return references, nil
	}
	if readErr != nil {
		return nil, readErr
	}
	for _, entry := range entries {
		if transaction, transactionErr := store.ReadTransaction(entry.Name()); transactionErr == nil {
			return nil, fmt.Errorf("node %s has pending transaction %s", entry.Name(), transaction.OperationID)
		} else if !errors.Is(transactionErr, os.ErrNotExist) {
			return nil, transactionErr
		}
		node, nodeErr := store.ReadNode(entry.Name())
		if nodeErr != nil {
			return nil, fmt.Errorf("read node %s before image prune: %w", entry.Name(), nodeErr)
		}
		if !validImageDigest(node.Image.Digest) {
			return nil, fmt.Errorf("node %s has an invalid image digest", entry.Name())
		}
		references[node.Image.Digest] = struct{}{}
	}
	return references, nil
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
