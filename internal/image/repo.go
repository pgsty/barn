package image

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pgsty/barn/internal/disk"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/fsutil"
	"go.yaml.in/yaml/v3"
)

const (
	RepoSchema      = 1
	RepoFilename    = "repo.yaml"
	RepoImagesDir   = "images"
	MaxRepoSpecSize = 4 << 20
)

type RepoVariant struct {
	File     string `yaml:"file,omitempty"`
	Upstream string `yaml:"upstream,omitempty"`
	// SourceUser is the distribution image's original guest login identity.
	// It records normalization/import provenance; Barn provisions and uses
	// the deployment SSH user separately.
	SourceUser string `yaml:"source_user,omitempty"`
	Boot       string `yaml:"boot,omitempty"`
	Status     string `yaml:"status,omitempty"`
	Provenance string `yaml:"provenance,omitempty"`
}

type RepoVersion struct {
	Status     string                 `yaml:"status,omitempty"`
	Provenance string                 `yaml:"provenance,omitempty"`
	Variants   map[string]RepoVariant `yaml:"variants"`
}

type RepoImage struct {
	Aliases  []string               `yaml:"aliases,omitempty"`
	Boot     string                 `yaml:"boot,omitempty"`
	Channels map[string]string      `yaml:"channels"`
	Versions map[string]RepoVersion `yaml:"versions"`
}

type RepoSpec struct {
	Schema   int                  `yaml:"schema"`
	Revision uint64               `yaml:"revision"`
	Defaults CatalogDefaults      `yaml:"defaults"`
	Images   map[string]RepoImage `yaml:"images"`
}

type RepoScanReport struct {
	Root      string   `json:"root"`
	Tracked   []string `json:"tracked"`
	Missing   []string `json:"missing"`
	Untracked []string `json:"untracked"`
	Unsafe    []string `json:"unsafe"`
}

type RepoBuildResult struct {
	Catalog Catalog        `json:"catalog"`
	Path    string         `json:"path"`
	Bytes   int            `json:"bytes"`
	Scan    RepoScanReport `json:"scan"`
}

type RepoBuilder struct {
	QEMUImg string
	Runner  execx.Runner
}

func canonicalArtifactName(image, version, arch string) string {
	return image + "-" + version + "-" + arch + ".qcow2"
}

func repoVariantFilename(image, version, arch string, variant RepoVariant) (string, error) {
	filename := strings.TrimSpace(variant.File)
	if filename == "" {
		filename = canonicalArtifactName(image, version, arch)
	}
	if filepath.Base(filename) != filename || !repositoryFilename.MatchString(filename) {
		return "", fmt.Errorf("variant file %q must be a safe qcow2 basename", filename)
	}
	return filename, nil
}

func readRepoSpec(root string) (RepoSpec, error) {
	if root == "" || !filepath.IsAbs(root) {
		return RepoSpec{}, errors.New("repository root must be an absolute directory")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm()&0o022 != 0 {
		return RepoSpec{}, fmt.Errorf("repository root is missing, symlinked, or writable by group/other: %s", root)
	}
	pathname := filepath.Join(root, RepoFilename)
	info, err := os.Lstat(pathname)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > MaxRepoSpecSize {
		return RepoSpec{}, fmt.Errorf("%s must be a regular non-symlink file no larger than %d bytes", pathname, MaxRepoSpecSize)
	}
	data, err := os.ReadFile(pathname)
	if err != nil {
		return RepoSpec{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var spec RepoSpec
	if err := decoder.Decode(&spec); err != nil {
		return RepoSpec{}, fmt.Errorf("decode strict %s: %w", RepoFilename, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RepoSpec{}, errors.New("repo.yaml contains multiple YAML documents or trailing data")
	}
	if err := spec.Validate(); err != nil {
		return RepoSpec{}, err
	}
	return spec, nil
}

func (s RepoSpec) Validate() error {
	if s.Schema != RepoSchema || s.Revision == 0 || len(s.Images) == 0 {
		return errors.New("repo.yaml schema/revision/images are invalid")
	}
	if !catalogName.MatchString(s.Defaults.Image) || !channelName.MatchString(s.Defaults.Channel) || s.Defaults.Arch != "native" || !validBoot(s.Defaults.Boot) {
		return errors.New("repo.yaml defaults must specify image, channel, native arch, and bios|uefi boot")
	}
	if _, ok := s.Images[s.Defaults.Image]; !ok {
		return fmt.Errorf("repo.yaml default image %q does not exist", s.Defaults.Image)
	}
	names := make(map[string]struct{})
	for name := range s.Images {
		if !catalogName.MatchString(name) || reservedLocalNamespace(name) {
			return fmt.Errorf("invalid or reserved repository image %q", name)
		}
		names[name] = struct{}{}
	}
	for name, imageRecord := range s.Images {
		if imageRecord.Boot != "" && !validBoot(imageRecord.Boot) {
			return fmt.Errorf("image %s has invalid boot mode %q", name, imageRecord.Boot)
		}
		if len(imageRecord.Channels) == 0 || len(imageRecord.Versions) == 0 {
			return fmt.Errorf("image %s requires channels and versions", name)
		}
		for _, alias := range imageRecord.Aliases {
			if !catalogName.MatchString(alias) || reservedLocalNamespace(alias) {
				return fmt.Errorf("image %s has invalid or reserved alias %q", name, alias)
			}
			if _, duplicate := names[alias]; duplicate {
				return fmt.Errorf("duplicate repository image name/alias %q", alias)
			}
			names[alias] = struct{}{}
		}
		for channel, version := range imageRecord.Channels {
			if !channelName.MatchString(channel) || !releaseName.MatchString(version) {
				return fmt.Errorf("image %s has invalid channel mapping %q -> %q", name, channel, version)
			}
			if _, ok := imageRecord.Versions[version]; !ok {
				return fmt.Errorf("image %s channel %s targets missing version %s", name, channel, version)
			}
		}
		if _, ok := imageRecord.Channels[s.Defaults.Channel]; !ok {
			return fmt.Errorf("image %s has no default channel %s", name, s.Defaults.Channel)
		}
		for versionName, version := range imageRecord.Versions {
			if !releaseName.MatchString(versionName) || len(version.Variants) == 0 {
				return fmt.Errorf("image %s has invalid version %q", name, versionName)
			}
			if version.Status != "" && !validStatus(version.Status) {
				return fmt.Errorf("image %s version %s has invalid status %q", name, versionName, version.Status)
			}
			for arch, variant := range version.Variants {
				if arch != "amd64" && arch != "arm64" {
					return fmt.Errorf("image %s version %s has invalid architecture %s", name, versionName, arch)
				}
				if _, err := repoVariantFilename(name, versionName, arch, variant); err != nil {
					return err
				}
				if variant.Boot != "" && !validBoot(variant.Boot) {
					return fmt.Errorf("image %s version %s/%s has invalid boot %q", name, versionName, arch, variant.Boot)
				}
				if variant.Status != "" && !validStatus(variant.Status) {
					return fmt.Errorf("image %s version %s/%s has invalid status %q", name, versionName, arch, variant.Status)
				}
				if variant.SourceUser != "" && !sourceUserName.MatchString(variant.SourceUser) {
					return fmt.Errorf("image %s version %s/%s has invalid source user %q", name, versionName, arch, variant.SourceUser)
				}
				if err := validateUpstream(variant.Upstream); err != nil {
					return fmt.Errorf("image %s version %s/%s %w", name, versionName, arch, err)
				}
			}
		}
	}
	return nil
}

func expectedRepoFiles(spec RepoSpec) (map[string]string, error) {
	result := make(map[string]string)
	for imageName, imageRecord := range spec.Images {
		for versionName, version := range imageRecord.Versions {
			for arch, variant := range version.Variants {
				filename, err := repoVariantFilename(imageName, versionName, arch, variant)
				if err != nil {
					return nil, err
				}
				if prior, duplicate := result[filename]; duplicate {
					return nil, fmt.Errorf("artifact %s is referenced by both %s and %s/%s/%s", filename, prior, imageName, versionName, arch)
				}
				result[filename] = imageName + "/" + versionName + "/" + arch
			}
		}
	}
	return result, nil
}

func ScanRepository(root string) (RepoScanReport, error) {
	spec, err := readRepoSpec(root)
	if err != nil {
		return RepoScanReport{}, err
	}
	expected, err := expectedRepoFiles(spec)
	if err != nil {
		return RepoScanReport{}, err
	}
	report := RepoScanReport{Root: root, Tracked: []string{}, Missing: []string{}, Untracked: []string{}, Unsafe: []string{}}
	imagesRoot := filepath.Join(root, RepoImagesDir)
	imagesInfo, err := os.Lstat(imagesRoot)
	if err != nil || !imagesInfo.IsDir() || imagesInfo.Mode()&os.ModeSymlink != 0 || imagesInfo.Mode().Perm()&0o022 != 0 {
		return RepoScanReport{}, fmt.Errorf("repository images directory is missing, symlinked, or writable by group/other: %s", imagesRoot)
	}
	entries, err := os.ReadDir(imagesRoot)
	if err != nil {
		return RepoScanReport{}, fmt.Errorf("read repository images: %w", err)
	}
	seen := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		pathname := filepath.Join(imagesRoot, name)
		info, statErr := os.Lstat(pathname)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !repositoryFilename.MatchString(name) {
			report.Unsafe = append(report.Unsafe, name)
			continue
		}
		if _, ok := expected[name]; ok {
			report.Tracked = append(report.Tracked, name)
			seen[name] = struct{}{}
		} else {
			report.Untracked = append(report.Untracked, name)
		}
	}
	for name := range expected {
		if _, ok := seen[name]; !ok {
			report.Missing = append(report.Missing, name)
		}
	}
	sort.Strings(report.Tracked)
	sort.Strings(report.Missing)
	sort.Strings(report.Untracked)
	sort.Strings(report.Unsafe)
	return report, nil
}

func (b RepoBuilder) validate() error {
	if b.QEMUImg == "" || b.Runner == nil {
		return errors.New("repository builder requires qemu-img and a bounded runner")
	}
	return nil
}

func (b RepoBuilder) materialize(ctx context.Context, root string) (Catalog, []byte, RepoScanReport, error) {
	if err := b.validate(); err != nil {
		return Catalog{}, nil, RepoScanReport{}, err
	}
	spec, err := readRepoSpec(root)
	if err != nil {
		return Catalog{}, nil, RepoScanReport{}, err
	}
	scan, err := ScanRepository(root)
	if err != nil {
		return Catalog{}, nil, RepoScanReport{}, err
	}
	if len(scan.Missing) != 0 || len(scan.Unsafe) != 0 {
		return Catalog{}, nil, scan, fmt.Errorf("repository has %d missing and %d unsafe artifacts", len(scan.Missing), len(scan.Unsafe))
	}
	catalog := Catalog{Schema: ManifestSchema, Version: spec.Revision, Defaults: spec.Defaults, Images: make(map[string]CatalogImage, len(spec.Images))}
	manager := disk.Manager{QEMUImg: b.QEMUImg, Runner: b.Runner}
	for imageName, sourceImage := range spec.Images {
		imageRecord := CatalogImage{Aliases: append([]string(nil), sourceImage.Aliases...), Boot: sourceImage.Boot, Channels: make(map[string]string, len(sourceImage.Channels)), Versions: make(map[string]CatalogVersion, len(sourceImage.Versions))}
		for channel, version := range sourceImage.Channels {
			imageRecord.Channels[channel] = version
		}
		for versionName, sourceVersion := range sourceImage.Versions {
			version := CatalogVersion{Status: sourceVersion.Status, Provenance: sourceVersion.Provenance, Variants: make(map[string]Artifact, len(sourceVersion.Variants))}
			if version.Status == "" {
				version.Status = "unknown"
			}
			for arch, sourceVariant := range sourceVersion.Variants {
				filename, _ := repoVariantFilename(imageName, versionName, arch, sourceVariant)
				pathname := filepath.Join(root, RepoImagesDir, filename)
				before, err := os.Lstat(pathname)
				if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() <= 0 || before.Size() > MaxArtifactSize {
					return Catalog{}, nil, scan, fmt.Errorf("unsafe repository artifact %s", pathname)
				}
				digest, size, err := digestFile(pathname)
				if err != nil {
					return Catalog{}, nil, scan, err
				}
				info, err := manager.Inspect(ctx, pathname)
				if err != nil {
					return Catalog{}, nil, scan, fmt.Errorf("inspect repository artifact %s: %w", filename, err)
				}
				if err := disk.ValidateBase(info); err != nil {
					return Catalog{}, nil, scan, fmt.Errorf("validate repository artifact %s: %w", filename, err)
				}
				if err := manager.CheckBase(ctx, pathname); err != nil {
					return Catalog{}, nil, scan, fmt.Errorf("check repository artifact %s: %w", filename, err)
				}
				after, err := os.Lstat(pathname)
				if err != nil || before.Size() != after.Size() || before.ModTime() != after.ModTime() || size != before.Size() {
					return Catalog{}, nil, scan, fmt.Errorf("repository artifact changed while scanning: %s", filename)
				}
				version.Variants[arch] = Artifact{
					File: "images/" + filename, Upstream: sourceVariant.Upstream,
					SHA256: digest, Format: "qcow2", ArtifactSize: size, VirtualSize: info.VirtualSize,
					SourceUser: sourceVariant.SourceUser, Boot: sourceVariant.Boot, Status: sourceVariant.Status, Provenance: sourceVariant.Provenance,
				}
			}
			imageRecord.Versions[versionName] = version
		}
		catalog.Images[imageName] = imageRecord
	}
	if err := catalog.Validate(); err != nil {
		return Catalog{}, nil, scan, err
	}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return Catalog{}, nil, scan, err
	}
	return catalog, append(data, '\n'), scan, nil
}

func (b RepoBuilder) Build(ctx context.Context, root string) (RepoBuildResult, error) {
	catalog, data, scan, err := b.materialize(ctx, root)
	if err != nil {
		return RepoBuildResult{Scan: scan}, err
	}
	target := filepath.Join(root, CatalogFilename)
	if existing, readErr := os.ReadFile(target); readErr == nil {
		current, parseErr := strictCatalog(existing)
		if parseErr != nil {
			return RepoBuildResult{}, fmt.Errorf("existing catalog is invalid: %w", parseErr)
		}
		if current.Version > catalog.Version {
			return RepoBuildResult{}, fmt.Errorf("repo revision %d is below existing catalog revision %d", catalog.Version, current.Version)
		}
		if current.Version == catalog.Version && !bytes.Equal(existing, data) {
			return RepoBuildResult{}, fmt.Errorf("repo revision %d would produce different catalog bytes; bump revision first", catalog.Version)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return RepoBuildResult{}, readErr
	}
	if err := fsutil.AtomicWrite(target, data, 0o644); err != nil {
		return RepoBuildResult{}, err
	}
	return RepoBuildResult{Catalog: catalog, Path: target, Bytes: len(data), Scan: scan}, nil
}

func (b RepoBuilder) Verify(ctx context.Context, root string) (RepoBuildResult, error) {
	catalog, data, scan, err := b.materialize(ctx, root)
	if err != nil {
		return RepoBuildResult{Scan: scan}, err
	}
	target := filepath.Join(root, CatalogFilename)
	existing, err := os.ReadFile(target)
	if err != nil {
		return RepoBuildResult{}, err
	}
	if !bytes.Equal(existing, data) {
		return RepoBuildResult{}, errors.New("catalog.json is stale or differs from repo.yaml and images")
	}
	return RepoBuildResult{Catalog: catalog, Path: target, Bytes: len(data), Scan: scan}, nil
}
