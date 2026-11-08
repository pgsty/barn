package image

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/disk"
	"github.com/pgsty/barn/internal/execx"
)

type repoRunner struct{}

func (repoRunner) Run(_ context.Context, _ string, arguments ...string) (execx.Result, error) {
	switch arguments[0] {
	case "info":
		data, _ := json.Marshal(disk.Info{Filename: arguments[len(arguments)-1], Format: "qcow2", VirtualSize: 1 << 30})
		return execx.Result{Stdout: data}, nil
	case "check":
		return execx.Result{Stdout: []byte(`{}`)}, nil
	default:
		return execx.Result{}, nil
	}
}

func writeRepoFixture(t *testing.T, root string, revision int, aliases string) []byte {
	t.Helper()
	data := []byte("schema: 1\nrevision: " + strconv.Itoa(revision) + "\ndefaults:\n  image: d13\n  channel: stable\n  arch: native\n  boot: uefi\nimages:\n  d13:\n    aliases: [" + aliases + "]\n    channels:\n      stable: \"1\"\n    versions:\n      \"1\":\n        variants:\n          arm64: {}\n")
	if err := os.WriteFile(filepath.Join(root, RepoFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRepositoryBuildSeparatesAuthoringCatalogAndCachePaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	images := filepath.Join(root, RepoImagesDir)
	if err := os.Mkdir(images, 0o700); err != nil {
		t.Fatal(err)
	}
	source := writeRepoFixture(t, root, 1, "debian13")
	artifactName := "d13-1-arm64.qcow2"
	if err := os.WriteFile(filepath.Join(images, artifactName), []byte("fixture-qcow"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(images, "untracked.qcow2"), []byte("untracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	builder := RepoBuilder{QEMUImg: "/fake/qemu-img", Runner: repoRunner{}}
	var events []activity.Event
	builder.Progress = func(event activity.Event) { events = append(events, event) }
	result, err := builder.Build(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Scan.Tracked) != 1 || len(result.Scan.Untracked) != 1 || result.Scan.Tracked[0] != artifactName {
		t.Fatalf("scan = %#v", result.Scan)
	}
	var queued, bytes, verified bool
	for _, event := range events {
		queued = queued || event.Item == artifactName && event.State == "queued"
		bytes = bytes || event.TotalBytes == int64(len("fixture-qcow")) && event.CurrentBytes == event.TotalBytes
		verified = verified || event.Item == artifactName && event.State == "verified" && event.Done
	}
	if !queued || !bytes || !verified {
		t.Fatalf("missing artifact progress: %+v", events)
	}
	entry, err := result.Catalog.Entry("debian13", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("fixture-qcow")))
	if entry.File != "images/"+artifactName || entry.CacheFile != "d13/d13-1-arm64.qcow2" || entry.Status != "unknown" || entry.Boot != "uefi" || entry.SHA256 != wantDigest || entry.ArtifactSize != int64(len("fixture-qcow")) || entry.VirtualSize != 1<<30 {
		t.Fatalf("materialized entry = %#v", entry)
	}
	after, err := os.ReadFile(filepath.Join(root, RepoFilename))
	if err != nil || string(after) != string(source) {
		t.Fatalf("build changed repo.yaml: %v", err)
	}
	if _, err := builder.Verify(context.Background(), root); err != nil {
		t.Fatal(err)
	}
}

type failedRepoRunner struct{ repoRunner }

func (r failedRepoRunner) Run(ctx context.Context, binary string, args ...string) (execx.Result, error) {
	if args[0] == "check" {
		return execx.Result{}, errors.New("broken qcow2")
	}
	return r.repoRunner.Run(ctx, binary, args...)
}

func TestRepositoryProgressMarksFailedArtifactAndDoesNotPublishCatalog(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, RepoImagesDir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRepoFixture(t, root, 1, "debian13")
	name := "d13-1-arm64.qcow2"
	if err := os.WriteFile(filepath.Join(root, RepoImagesDir, name), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	var events []activity.Event
	builder := RepoBuilder{QEMUImg: "/fake/qemu-img", Runner: failedRepoRunner{}, Progress: func(event activity.Event) { events = append(events, event) }}
	if _, err := builder.Build(context.Background(), root); err == nil {
		t.Fatal("invalid artifact passed")
	}
	last := events[len(events)-1]
	if last.Item != name || last.State != "failed" || !last.Done || !strings.Contains(last.Message, "broken qcow2") {
		t.Fatalf("failure = %+v", last)
	}
	if _, err := os.Stat(filepath.Join(root, CatalogFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published failed catalog: %v", err)
	}
}

func TestRepositoryBuildRequiresRevisionBumpForDifferentBytes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	images := filepath.Join(root, RepoImagesDir)
	if err := os.Mkdir(images, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRepoFixture(t, root, 1, "debian13")
	if err := os.WriteFile(filepath.Join(images, "d13-1-arm64.qcow2"), []byte("fixture-qcow"), 0o600); err != nil {
		t.Fatal(err)
	}
	builder := RepoBuilder{QEMUImg: "/fake/qemu-img", Runner: repoRunner{}}
	if _, err := builder.Build(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	writeRepoFixture(t, root, 1, "debian13, trixie")
	if _, err := builder.Build(context.Background(), root); err == nil || !strings.Contains(err.Error(), "bump revision") {
		t.Fatalf("same-revision rebuild error = %v", err)
	}
}

func TestOfficialRepoSourceMatchesEmbeddedCatalogIntent(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile(filepath.Join("..", "..", "packaging", "image-repository", RepoFilename))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, RepoFilename), source, 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := readRepoSpec(root)
	if err != nil {
		t.Fatal(err)
	}
	catalog := EmbeddedCatalog()
	if spec.Revision != catalog.Version || spec.Defaults != catalog.Defaults || len(spec.Images) != len(catalog.Images) {
		t.Fatalf("official repo/catalog envelope mismatch: spec=%#v catalog=%#v", spec, catalog)
	}
	for name, catalogImage := range catalog.Images {
		repoImage, ok := spec.Images[name]
		if !ok || !reflect.DeepEqual(repoImage.Aliases, catalogImage.Aliases) || repoImage.Boot != catalogImage.Boot || !reflect.DeepEqual(repoImage.Channels, catalogImage.Channels) || len(repoImage.Versions) != len(catalogImage.Versions) {
			t.Errorf("official image %s metadata mismatch", name)
			continue
		}
		for versionName, catalogVersion := range catalogImage.Versions {
			repoVersion, ok := repoImage.Versions[versionName]
			if !ok || repoVersion.Status != catalogVersion.Status || repoVersion.Provenance != catalogVersion.Provenance || len(repoVersion.Variants) != len(catalogVersion.Variants) {
				t.Errorf("official image %s version %s metadata mismatch", name, versionName)
				continue
			}
			for arch, catalogArtifact := range catalogVersion.Variants {
				repoVariant, ok := repoVersion.Variants[arch]
				if !ok || repoVariant.Upstream != catalogArtifact.Upstream || repoVariant.SourceUser != catalogArtifact.SourceUser || repoVariant.Boot != catalogArtifact.Boot || repoVariant.Status != catalogArtifact.Status || repoVariant.Provenance != catalogArtifact.Provenance {
					t.Errorf("official image %s version %s/%s variant mismatch", name, versionName, arch)
				}
			}
		}
	}
}
