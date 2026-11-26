package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/failure"
)

func writeUpdateRepoArtifact(t *testing.T, repoRoot, relFile, content string) Artifact {
	t.Helper()
	full := filepath.Join(repoRoot, filepath.FromSlash(relFile))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return Artifact{
		File:         relFile,
		SHA256:       hex.EncodeToString(sum[:]),
		Format:       "qcow2",
		ArtifactSize: int64(len(content)),
		VirtualSize:  1 << 30,
		SourceUser:   "dba",
	}
}

func writeCustomCatalog(t *testing.T, repoRoot string, catalog Catalog) {
	t.Helper()
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, CatalogFilename), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestManifestPreviewIsReadOnlyAndEnforcesTrustPolicy(t *testing.T) {
	t.Parallel()
	dataRoot := filepath.Join(t.TempDir(), "fresh-home")
	key := privateKey(t, developmentPrivateRoot1)
	sourceDir := t.TempDir()
	path := signedCatalog(t, sourceDir, EmbeddedManifestVersion+5, key, nil)

	untrusted := ManifestManager{DataRoot: dataRoot}
	if _, _, err := untrusted.Preview(context.Background(), path); err == nil || !strings.Contains(err.Error(), "unknown key ID") {
		t.Fatalf("untrusted preview error = %v", err)
	}
	if _, err := os.Stat(dataRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed preview created data root: %v", err)
	}

	trusted := ManifestManager{DataRoot: dataRoot, Keys: testManifestRoots(t)}
	catalog, state, err := trusted.Preview(context.Background(), path)
	if err != nil || catalog.Version != EmbeddedManifestVersion+5 || state.ActiveVersion != EmbeddedManifestVersion+5 {
		t.Fatalf("preview = %#v %#v %v", catalog, state, err)
	}
	if _, err := os.Stat(dataRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview mutated disk and created data root: %v", err)
	}

	// Activate revision +5, then verify Preview rejects rollback to +4 without mutating state.
	if _, err := trusted.Sync(context.Background(), path, false); err != nil {
		t.Fatal(err)
	}
	older := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion+4, key, nil)
	if _, _, err := trusted.Preview(context.Background(), older); err == nil || !strings.Contains(err.Error(), "below accepted high-water mark") {
		t.Fatalf("rollback preview error = %v", err)
	}
}

func TestUpdateImagesDryRunApplyDeprecatedRetiredMultiArchAndPrune(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	d13NewAMD := writeUpdateRepoArtifact(t, repoRoot, "images/d13-20261123.2671.1-amd64.qcow2", "d13-new-amd64-bytes")
	d13NewARM := writeUpdateRepoArtifact(t, repoRoot, "images/d13-20261123.2671.1-arm64.qcow2", "d13-new-arm64-bytes")
	u24SameARM := writeUpdateRepoArtifact(t, repoRoot, "images/u24-20261017.0.1-arm64.qcow2", "u24-current-arm64")
	u22NewARM := writeUpdateRepoArtifact(t, repoRoot, "images/u22-20261109.0.1-arm64.qcow2", "u22-deprecated-new-arm64")

	catalog := Catalog{
		Schema:   ManifestSchema,
		Version:  2026112601,
		Defaults: CatalogDefaults{Image: "u24", Channel: "stable", Arch: "native", Boot: "uefi"},
		Images: map[string]CatalogImage{
			"d13": {
				Aliases:  []string{"debian13", "debian"},
				Boot:     "uefi",
				Channels: map[string]string{"stable": "20261123.2671.1"},
				Versions: map[string]CatalogVersion{
					"20261123.2671.1": {Status: "supported", Variants: map[string]Artifact{"amd64": d13NewAMD, "arm64": d13NewARM}},
				},
			},
			"u24": {
				Aliases:  []string{"ubuntu24"},
				Boot:     "uefi",
				Channels: map[string]string{"stable": "20261017.0.1"},
				Versions: map[string]CatalogVersion{
					"20261017.0.1": {Status: "supported", Variants: map[string]Artifact{"arm64": u24SameARM}},
				},
			},
			"u22": {
				Aliases:  []string{"ubuntu22"},
				Boot:     "uefi",
				Channels: map[string]string{"stable": "20261109.0.1"},
				Versions: map[string]CatalogVersion{
					"20261109.0.1": {Status: "deprecated", Variants: map[string]Artifact{"arm64": u22NewARM}},
				},
			},
		},
	}
	writeCustomCatalog(t, repoRoot, catalog)

	dataRoot := filepath.Join(t.TempDir(), "barn-home")
	store := Store{DataRoot: dataRoot, QEMUImg: "/fake/qemu-img", Runner: pruneRunner{virtualSize: 1 << 30}}

	// Populate local cache:
	// - d13 arm64 has two retired versions (not in new catalog)
	// - d13 amd64 has one older version
	// - u24 arm64 already matches target bytes
	// - u22 arm64 is deprecated and has an older version
	// - legacy arm64 is not in the catalog at all (must be skipped, not abort)
	// - local/ imported file, .partial staging file, and .corrupt file must not be treated as catalog families
	retiredD13ARM, retiredD13ARMPath := writeCachedFixture(t, store, "d13", "d13-20261026.2643.1-arm64.qcow2", "d13-very-old-arm64")
	_, olderD13ARMPath := writeCachedFixture(t, store, "d13", "d13-20261103.2651.1-arm64.qcow2", "d13-old-arm64")
	_, olderD13AMDPath := writeCachedFixture(t, store, "d13", "d13-20261103.2651.1-amd64.qcow2", "d13-old-amd64")
	_, _ = writeCachedFixture(t, store, "u24", "u24-20261017.0.1-arm64.qcow2", "u24-current-arm64")
	_, olderU22ARMPath := writeCachedFixture(t, store, "u22", "u22-20261020.0.1-arm64.qcow2", "u22-old-arm64")
	_, legacyPath := writeCachedFixture(t, store, "legacy", "legacy-1.0-arm64.qcow2", "legacy-bytes")
	_, _ = writeCachedFixture(t, store, "local", strings.Repeat("a", 64)+".qcow2", "local-import")
	stalePartial := filepath.Join(dataRoot, "images", "d13", ".download-stale.partial")
	if err := os.WriteFile(stalePartial, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(dataRoot, "images", "d13", "d13-20261001.1-arm64.qcow2.corrupt-123")
	if err := os.WriteFile(quarantine, []byte("corrupt"), 0o444); err != nil {
		t.Fatal(err)
	}

	service := Service{
		DataRoot:   dataRoot,
		Repository: repoRoot,
		QEMUImg:    "/fake/qemu-img",
		Runner:     pruneRunner{virtualSize: 1 << 30},
	}

	// 1. Dry-run with --prune must be strictly read-only: no manifest state written, no downloads, no deletions.
	dryReport, err := service.UpdateImages(context.Background(), ImageUpdateOptions{
		DryRun: true,
		Prune:  true,
		NodeRefs: func(context.Context) (map[string]struct{}, error) {
			return map[string]struct{}{retiredD13ARM.SHA256: {}}, nil
		},
	})
	if err != nil {
		t.Fatalf("dry-run UpdateImages failed: %v", err)
	}
	if !dryReport.DryRun || !dryReport.Catalog.Updated || dryReport.Catalog.ActiveVersion != 2026112601 {
		t.Fatalf("dry-run catalog = %#v", dryReport.Catalog)
	}
	if _, statErr := os.Stat(filepath.Join(dataRoot, "images", "manifests", "state.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("dry-run wrote manifest state.json: %v", statErr)
	}
	byKey := make(map[string]ImageUpdateItem, len(dryReport.Images))
	for _, item := range dryReport.Images {
		byKey[item.Alias+"/"+item.Arch] = item
	}
	if len(byKey) != 5 {
		t.Fatalf("dry-run images = %#v", dryReport.Images)
	}
	if byKey["d13/amd64"].State != "planned" || byKey["d13/arm64"].State != "planned" || byKey["u22/arm64"].State != "planned" || byKey["u22/arm64"].Status != "deprecated" || byKey["u24/arm64"].State != "unchanged" || byKey["legacy/arm64"].State != "skipped" {
		t.Fatalf("unexpected dry-run states: %#v", byKey)
	}
	if gotPrev := byKey["d13/arm64"].PreviousReleases; len(gotPrev) != 2 || gotPrev[1] != "20261103.2651.1" {
		t.Fatalf("d13/arm64 previous releases = %v", gotPrev)
	}
	if dryReport.Prune == nil || dryReport.Prune.Apply || len(dryReport.Prune.Items) == 0 {
		t.Fatalf("dry-run prune preview = %#v", dryReport.Prune)
	}
	// Verify retired node-referenced file was protected even in preview, and nothing was deleted.
	for _, item := range dryReport.Prune.Items {
		if item.ImagePath == retiredD13ARMPath {
			t.Fatalf("dry-run prune included node-referenced image %s", retiredD13ARMPath)
		}
		if item.Applied {
			t.Fatalf("dry-run prune applied deletion: %#v", item)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dataRoot, "images", "d13", "d13-20261123.2671.1-arm64.qcow2")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("dry-run downloaded image: %v", statErr)
	}

	// 2. Apply without --prune: activates catalog, downloads updates (including deprecated u22), preserves all old files.
	applied, err := service.UpdateImages(context.Background(), ImageUpdateOptions{})
	if err != nil {
		t.Fatalf("apply UpdateImages failed: %v", err)
	}
	if !applied.Catalog.Updated || applied.Prune != nil {
		t.Fatalf("apply report = %#v", applied)
	}
	appliedByKey := make(map[string]ImageUpdateItem, len(applied.Images))
	for _, item := range applied.Images {
		appliedByKey[item.Alias+"/"+item.Arch] = item
	}
	if appliedByKey["d13/amd64"].State != "updated" || appliedByKey["d13/arm64"].State != "updated" || appliedByKey["u22/arm64"].State != "updated" || appliedByKey["u22/arm64"].Status != "deprecated" || appliedByKey["u24/arm64"].State != "unchanged" || appliedByKey["legacy/arm64"].State != "skipped" {
		t.Fatalf("unexpected applied states: %#v", appliedByKey)
	}
	for _, preserved := range []string{retiredD13ARMPath, olderD13ARMPath, olderD13AMDPath, olderU22ARMPath, legacyPath, stalePartial} {
		if _, statErr := os.Stat(preserved); statErr != nil {
			t.Fatalf("update without --prune removed %s: %v", preserved, statErr)
		}
	}

	// 3. Repeat with --prune: catalog is current, all catalog images unchanged, unreferenced old images & staging removed while node-referenced old image stays.
	pruned, err := service.UpdateImages(context.Background(), ImageUpdateOptions{
		Prune: true,
		NodeRefs: func(context.Context) (map[string]struct{}, error) {
			return map[string]struct{}{retiredD13ARM.SHA256: {}}, nil
		},
	})
	if err != nil {
		t.Fatalf("pruned UpdateImages failed: %v", err)
	}
	if pruned.Catalog.Updated || pruned.Prune == nil || !pruned.Prune.Apply {
		t.Fatalf("pruned report = %#v", pruned)
	}
	if _, statErr := os.Stat(retiredD13ARMPath); statErr != nil {
		t.Fatalf("prune removed node-referenced old image: %v", statErr)
	}
	for _, removed := range []string{olderD13ARMPath, olderD13AMDPath, olderU22ARMPath, legacyPath, stalePartial} {
		if _, statErr := os.Stat(removed); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("prune left unreferenced file %s: %v", removed, statErr)
		}
	}
	for _, kept := range []string{
		filepath.Join(dataRoot, "images", "d13", "d13-20261123.2671.1-arm64.qcow2"),
		filepath.Join(dataRoot, "images", "d13", "d13-20261123.2671.1-amd64.qcow2"),
		filepath.Join(dataRoot, "images", "u22", "u22-20261109.0.1-arm64.qcow2"),
		filepath.Join(dataRoot, "images", "u24", "u24-20261017.0.1-arm64.qcow2"),
	} {
		if _, statErr := os.Stat(kept); statErr != nil {
			t.Fatalf("prune removed active updated image %s: %v", kept, statErr)
		}
	}
}

func TestUpdateImagesSelectorsArchFilterAndValidation(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	d13AMD := writeUpdateRepoArtifact(t, repoRoot, "images/d13-20261123.2671.1-amd64.qcow2", "d13-new-amd")
	d13ARM := writeUpdateRepoArtifact(t, repoRoot, "images/d13-20261123.2671.1-arm64.qcow2", "d13-new-arm")
	el9ARM := writeUpdateRepoArtifact(t, repoRoot, "images/el9-9.9.20261112.1-arm64.qcow2", "el9-new-arm")
	el9AMD := writeUpdateRepoArtifact(t, repoRoot, "images/el9-9.9.20261112.1-amd64.qcow2", "el9-new-amd")
	catalog := Catalog{
		Schema:   ManifestSchema,
		Version:  2026112602,
		Defaults: CatalogDefaults{Image: "d13", Channel: "stable", Arch: "native", Boot: "uefi"},
		Images: map[string]CatalogImage{
			"d13": {
				Aliases:  []string{"debian13"},
				Boot:     "uefi",
				Channels: map[string]string{"stable": "20261123.2671.1"},
				Versions: map[string]CatalogVersion{"20261123.2671.1": {Status: "supported", Variants: map[string]Artifact{"amd64": d13AMD, "arm64": d13ARM}}},
			},
			"el9": {
				Aliases:  []string{"rocky9"},
				Boot:     "uefi",
				Channels: map[string]string{"stable": "9.9.20261112.1"},
				Versions: map[string]CatalogVersion{"9.9.20261112.1": {Status: "supported", Variants: map[string]Artifact{"amd64": el9AMD, "arm64": el9ARM}}},
			},
		},
	}
	writeCustomCatalog(t, repoRoot, catalog)

	dataRoot := filepath.Join(t.TempDir(), "barn-home")
	store := Store{DataRoot: dataRoot, QEMUImg: "/fake/qemu-img", Runner: pruneRunner{virtualSize: 1 << 30}}
	_, _ = writeCachedFixture(t, store, "d13", "d13-20261103.2651.1-amd64.qcow2", "d13-old-amd")
	_, _ = writeCachedFixture(t, store, "d13", "d13-20261103.2651.1-arm64.qcow2", "d13-old-arm")
	service := Service{DataRoot: dataRoot, Repository: repoRoot, QEMUImg: "/fake/qemu-img", Runner: pruneRunner{virtualSize: 1 << 30}}

	// A bare family with an empty cache uses the same architecture as image pull.
	report, err := service.UpdateImages(context.Background(), ImageUpdateOptions{Selectors: []string{"d13a", "el9"}})
	if err != nil {
		t.Fatalf("selector update failed: %v", err)
	}
	if len(report.Images) != 2 || report.Images[0].Alias != "d13" || report.Images[0].Arch != "arm64" || report.Images[0].State != "updated" || report.Images[1].Alias != "el9" || report.Images[1].Arch != runtime.GOARCH || report.Images[1].State != "updated" {
		t.Fatalf("selector report = %#v", report.Images)
	}
	if _, statErr := os.Stat(filepath.Join(dataRoot, "images", "d13", "d13-20261123.2671.1-amd64.qcow2")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("d13a selector unexpectedly downloaded amd64: %v", statErr)
	}

	// Deduplicated aliases + --arch filter updates only amd64.
	amdReport, err := service.UpdateImages(context.Background(), ImageUpdateOptions{Selectors: []string{"d13", "debian13"}, Arch: "amd64"})
	if err != nil {
		t.Fatalf("arch-filtered selector update failed: %v", err)
	}
	if len(amdReport.Images) != 1 || amdReport.Images[0].Arch != "amd64" || amdReport.Images[0].State != "updated" {
		t.Fatalf("amd64 report = %#v", amdReport.Images)
	}

	// Invalid selectors fail with usage errors.
	for _, tc := range []struct {
		opts   ImageUpdateOptions
		reason string
	}{
		{opts: ImageUpdateOptions{Selectors: []string{"d13@20261123.2671.1"}}},
		{opts: ImageUpdateOptions{Selectors: []string{"local-custom"}}},
		{opts: ImageUpdateOptions{Selectors: []string{"bogus"}}, reason: "unknown_image"},
		{opts: ImageUpdateOptions{Selectors: []string{"d13a"}, Arch: "amd64"}, reason: "arch_conflict"},
	} {
		_, err := service.UpdateImages(context.Background(), tc.opts)
		class, reason, _ := failure.Classify(err)
		if err == nil || class != failure.Usage || (tc.reason != "" && reason != tc.reason) {
			t.Errorf("opts %#v -> err=%v class=%q reason=%q", tc.opts, err, class, reason)
		}
	}
}

func TestUpdateImagesContinuesOnPartialFailure(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	d13ARM := writeUpdateRepoArtifact(t, repoRoot, "images/d13-20261123.2671.1-arm64.qcow2", "d13-good-bytes")
	el9ARM := writeUpdateRepoArtifact(t, repoRoot, "images/el9-9.9.20261112.1-arm64.qcow2", "el9-will-be-removed")
	// Remove el9 artifact from the repo so d13 succeeds while el9 fails.
	if err := os.Remove(filepath.Join(repoRoot, filepath.FromSlash(el9ARM.File))); err != nil {
		t.Fatal(err)
	}
	catalog := Catalog{
		Schema:   ManifestSchema,
		Version:  2026112603,
		Defaults: CatalogDefaults{Image: "d13", Channel: "stable", Arch: "native", Boot: "uefi"},
		Images: map[string]CatalogImage{
			"d13": {
				Boot:     "uefi",
				Channels: map[string]string{"stable": "20261123.2671.1"},
				Versions: map[string]CatalogVersion{"20261123.2671.1": {Status: "supported", Variants: map[string]Artifact{"arm64": d13ARM}}},
			},
			"el9": {
				Boot:     "uefi",
				Channels: map[string]string{"stable": "9.9.20261112.1"},
				Versions: map[string]CatalogVersion{"9.9.20261112.1": {Status: "supported", Variants: map[string]Artifact{"arm64": el9ARM}}},
			},
		},
	}
	writeCustomCatalog(t, repoRoot, catalog)

	dataRoot := filepath.Join(t.TempDir(), "barn-home")
	store := Store{DataRoot: dataRoot, QEMUImg: "/fake/qemu-img", Runner: pruneRunner{virtualSize: 1 << 30}}
	_, _ = writeCachedFixture(t, store, "d13", "d13-20261103.2651.1-arm64.qcow2", "d13-old")
	_, _ = writeCachedFixture(t, store, "el9", "el9-9.8.20260525.2-arm64.qcow2", "el9-old")
	service := Service{DataRoot: dataRoot, Repository: repoRoot, QEMUImg: "/fake/qemu-img", Runner: pruneRunner{virtualSize: 1 << 30}}

	report, err := service.UpdateImages(context.Background(), ImageUpdateOptions{Prune: true})
	class, reason, _ := failure.Classify(err)
	if class != failure.Partial || reason != "partial_image_update" {
		t.Fatalf("partial failure err=%v class=%q reason=%q", err, class, reason)
	}
	if len(report.Images) != 2 || report.Images[0].State != "updated" || report.Images[1].State != "failed" || report.Prune != nil {
		t.Fatalf("partial report = %#v", report)
	}
}
