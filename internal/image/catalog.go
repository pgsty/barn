package image

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pgsty/farrow/internal/failure"
)

const (
	ManifestSchema          = 3
	EmbeddedManifestVersion = 2026092001
	MaxManifestSize         = 4 << 20
)

// IsLocalAlias reports an image reference that names an imported local image.
func IsLocalAlias(reference string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(reference)), localAliasPrefix)
}

func reservedLocalNamespace(value string) bool {
	return value == "local" || strings.HasPrefix(value, localAliasPrefix)
}

type CatalogDefaults struct {
	Image   string `json:"image" yaml:"image"`
	Channel string `json:"channel" yaml:"channel"`
	Arch    string `json:"arch" yaml:"arch"`
	Boot    string `json:"boot" yaml:"boot"`
}

type Artifact struct {
	File         string `json:"file" yaml:"file"`
	Upstream     string `json:"upstream,omitempty" yaml:"upstream,omitempty"`
	SHA256       string `json:"sha256" yaml:"sha256"`
	Format       string `json:"format" yaml:"format"`
	ArtifactSize int64  `json:"artifact_size" yaml:"artifact_size"`
	VirtualSize  int64  `json:"virtual_size" yaml:"virtual_size"`
	SourceUser   string `json:"source_user,omitempty" yaml:"source_user,omitempty"`
	Boot         string `json:"boot,omitempty" yaml:"boot,omitempty"`
	Status       string `json:"status,omitempty" yaml:"status,omitempty"`
	Provenance   string `json:"provenance,omitempty" yaml:"provenance,omitempty"`
}

type CatalogVersion struct {
	Status     string              `json:"status,omitempty" yaml:"status,omitempty"`
	Provenance string              `json:"provenance,omitempty" yaml:"provenance,omitempty"`
	Variants   map[string]Artifact `json:"variants" yaml:"variants"`
}

type CatalogImage struct {
	Aliases  []string                  `json:"aliases,omitempty" yaml:"aliases,omitempty"`
	Boot     string                    `json:"boot,omitempty" yaml:"boot,omitempty"`
	Channels map[string]string         `json:"channels" yaml:"channels"`
	Versions map[string]CatalogVersion `json:"versions" yaml:"versions"`
}

type Catalog struct {
	Schema   int                     `json:"schema" yaml:"schema"`
	Version  uint64                  `json:"revision" yaml:"revision"`
	Defaults CatalogDefaults         `json:"defaults" yaml:"defaults"`
	Images   map[string]CatalogImage `json:"images" yaml:"images"`
}

var (
	catalogName        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	channelName        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	releaseName        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	numericVersion     = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*$`)
	repositoryFilename = regexp.MustCompile(`^[a-z0-9][A-Za-z0-9._+-]{0,191}\.qcow2$`)
	sourceUserName     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

const localAliasPrefix = "local-"

// sortedKeys walks a map in a stable order. Catalog validation, ranking, and
// listing all use it so identical input always yields identical output.
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("image catalog contains trailing JSON data")
	}
	return nil
}

func strictCatalog(data []byte) (Catalog, error) {
	if len(data) == 0 || len(data) > MaxManifestSize {
		return Catalog{}, fmt.Errorf("catalog size %d is outside 1..%d", len(data), MaxManifestSize)
	}
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return Catalog{}, fmt.Errorf("decode image catalog header: %w", err)
	}
	if header.Schema != ManifestSchema {
		return Catalog{}, fmt.Errorf("unsupported image catalog schema %d; this Farrow reads schema %d", header.Schema, ManifestSchema)
	}
	var catalog Catalog
	if err := strictDecode(data, &catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode strict image catalog schema %d: %w", ManifestSchema, err)
	}
	if err := catalog.Validate(); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

// Status is advisory rather than an activation switch: supported suppresses
// risk warnings; testing and unknown remain runnable but warn; deprecated gets
// the stronger EOL warning.
func validStatus(value string) bool {
	return value == "supported" || value == "testing" || value == "deprecated" || value == "unknown"
}

func validBoot(value string) bool { return value == "bios" || value == "uefi" }

func safeRepositoryFile(filename string) bool {
	if filename == "" || path.Clean(filename) != filename || strings.HasPrefix(filename, "/") || strings.Contains(filename, "\\") {
		return false
	}
	parts := strings.Split(filename, "/")
	if len(parts) != 2 || !repositoryFilename.MatchString(parts[1]) {
		return false
	}
	return parts[0] == "images" || catalogName.MatchString(parts[0])
}

func validateUpstream(value string) error {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || hasMovingReleasePath(parsed.Path) {
		return errors.New("upstream must be empty or an immutable absolute HTTPS URL")
	}
	return nil
}

func (c Catalog) Validate() error {
	if c.Schema != ManifestSchema || c.Version == 0 || len(c.Images) == 0 {
		return errors.New("image catalog schema/revision/images are invalid")
	}
	if !catalogName.MatchString(c.Defaults.Image) || !channelName.MatchString(c.Defaults.Channel) || c.Defaults.Arch != "native" || !validBoot(c.Defaults.Boot) {
		return errors.New("image catalog defaults must name a valid image, channel, native architecture, and boot mode")
	}
	if _, ok := c.Images[c.Defaults.Image]; !ok {
		return fmt.Errorf("default image %q does not exist", c.Defaults.Image)
	}
	// Validation walks the catalog in sorted order so one invalid catalog always
	// produces the same first error instead of a map-order lottery.
	imageNames := make([]string, 0, len(c.Images))
	for name := range c.Images {
		imageNames = append(imageNames, name)
	}
	sort.Strings(imageNames)
	allNames := make(map[string]struct{})
	for _, name := range imageNames {
		if !catalogName.MatchString(name) || reservedLocalNamespace(name) {
			return fmt.Errorf("invalid or reserved image name %q", name)
		}
		allNames[name] = struct{}{}
	}
	for _, name := range imageNames {
		imageRecord := c.Images[name]
		if len(imageRecord.Channels) == 0 || len(imageRecord.Versions) == 0 {
			return fmt.Errorf("image %s has no channels or versions", name)
		}
		if imageRecord.Boot != "" && !validBoot(imageRecord.Boot) {
			return fmt.Errorf("image %s has invalid boot mode %q", name, imageRecord.Boot)
		}
		for _, alias := range imageRecord.Aliases {
			alias = strings.ToLower(strings.TrimSpace(alias))
			if !catalogName.MatchString(alias) || reservedLocalNamespace(alias) {
				return fmt.Errorf("invalid or reserved image alias %q", alias)
			}
			if _, exists := allNames[alias]; exists {
				return fmt.Errorf("duplicate image name/alias %q", alias)
			}
			allNames[alias] = struct{}{}
		}
		for _, channel := range sortedKeys(imageRecord.Channels) {
			release := imageRecord.Channels[channel]
			if !channelName.MatchString(channel) || !releaseName.MatchString(release) {
				return fmt.Errorf("image %s has invalid channel mapping %q -> %q", name, channel, release)
			}
			if _, ok := imageRecord.Versions[release]; !ok {
				return fmt.Errorf("image %s channel %s targets missing version %s", name, channel, release)
			}
		}
		if _, ok := imageRecord.Channels[c.Defaults.Channel]; !ok {
			return fmt.Errorf("image %s has no default channel %s", name, c.Defaults.Channel)
		}
		// Two version keys that differ textually but rank identically would make
		// numeric prefix selection ambiguous at resolve time. Reject them at the
		// catalog boundary so `vm_version: 9` can never become a runtime coin flip.
		rankedVersions := make(map[string]string, len(imageRecord.Versions))
		for _, release := range sortedKeys(imageRecord.Versions) {
			parts, err := numericVersionParts(release)
			if err != nil {
				// Non-numeric keys are only ever matched exactly, never ranked.
				continue
			}
			key := canonicalNumericKey(parts)
			if previous, clash := rankedVersions[key]; clash {
				return fmt.Errorf("image %s versions %q and %q rank identically; numeric version selection would be ambiguous", name, previous, release)
			}
			rankedVersions[key] = release
		}
		for _, release := range sortedKeys(imageRecord.Versions) {
			version := imageRecord.Versions[release]
			if !releaseName.MatchString(release) || len(version.Variants) == 0 {
				return fmt.Errorf("image %s has invalid version %q", name, release)
			}
			if version.Status != "" && !validStatus(version.Status) {
				return fmt.Errorf("image %s version %s has invalid status %q", name, release, version.Status)
			}
			for _, arch := range sortedKeys(version.Variants) {
				artifact := version.Variants[arch]
				if arch != "arm64" && arch != "amd64" {
					return fmt.Errorf("image %s version %s has unsupported arch %s", name, release, arch)
				}
				boot := artifact.Boot
				if boot == "" {
					boot = imageRecord.Boot
				}
				if boot == "" {
					boot = c.Defaults.Boot
				}
				status := artifact.Status
				if status == "" {
					status = version.Status
				}
				if !digestPattern.MatchString(artifact.SHA256) || artifact.Format != "qcow2" || artifact.ArtifactSize <= 0 || artifact.ArtifactSize > MaxArtifactSize || artifact.VirtualSize <= 0 || !validBoot(boot) || !validStatus(status) || !safeRepositoryFile(artifact.File) {
					return fmt.Errorf("image %s version %s/%s has invalid artifact fields", name, release, arch)
				}
				if artifact.SourceUser != "" && !sourceUserName.MatchString(artifact.SourceUser) {
					return fmt.Errorf("image %s version %s/%s has invalid source user %q", name, release, arch, artifact.SourceUser)
				}
				if err := validateUpstream(artifact.Upstream); err != nil {
					return fmt.Errorf("image %s version %s/%s %w", name, release, arch, err)
				}
			}
		}
	}
	return nil
}

func hasMovingReleasePath(pathname string) bool {
	for _, segment := range strings.Split(strings.ToLower(pathname), "/") {
		if segment == "latest" || segment == "current" || segment == "release" || strings.Contains(segment, ".latest.") {
			return true
		}
	}
	return false
}

func (c Catalog) canonicalImage(value string) (string, CatalogImage, error) {
	if value == "" {
		value = c.Defaults.Image
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if record, ok := c.Images[value]; ok {
		return value, record, nil
	}
	for name, record := range c.Images {
		for _, alias := range record.Aliases {
			if value == alias {
				return name, record, nil
			}
		}
	}
	return "", CatalogImage{}, unknownSelection(fmt.Errorf("unknown image %q; available: %s", value, strings.Join(sortedKeys(c.Images), ", ")))
}

// unknownSelection marks a name the user chose that the catalog does not
// offer; the message lists what it does offer.
func unknownSelection(err error) error {
	return failure.New(failure.Usage, err).Because("unknown_image")
}

func cacheFile(alias, release, arch string) string {
	return fmt.Sprintf("%s/%s-%s-%s.qcow2", alias, alias, release, arch)
}

func numericVersionParts(value string) ([]uint64, error) {
	if !numericVersion.MatchString(value) {
		return nil, fmt.Errorf("version selector %q must contain only dot-separated non-negative integers", value)
	}
	parts := strings.Split(value, ".")
	result := make([]uint64, len(parts))
	for index, part := range parts {
		parsed, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("version component %q is outside the supported integer range", part)
		}
		result[index] = parsed
	}
	return result, nil
}

func compareNumericVersions(first, second []uint64) int {
	length := len(first)
	if len(second) > length {
		length = len(second)
	}
	for index := 0; index < length; index++ {
		var left, right uint64
		if index < len(first) {
			left = first[index]
		}
		if index < len(second) {
			right = second[index]
		}
		switch {
		case left < right:
			return -1
		case left > right:
			return 1
		}
	}
	return 0
}

// resolveVersion gives an exact catalog key priority. Otherwise a numeric
// selector matches only on a dot-component boundary and resolves to the
// numerically newest matching release (9.10 sorts after 9.9).
func resolveVersion(versions map[string]CatalogVersion, selector string) (string, error) {
	if _, exact := versions[selector]; exact {
		return selector, nil
	}
	if _, err := numericVersionParts(selector); err != nil {
		return "", err
	}
	type candidate struct {
		name  string
		parts []uint64
	}
	matches := make([]candidate, 0)
	for version := range versions {
		if !strings.HasPrefix(version, selector+".") {
			continue
		}
		parts, err := numericVersionParts(version)
		if err != nil {
			return "", fmt.Errorf("matching catalog version %q cannot be ranked: %w", version, err)
		}
		matches = append(matches, candidate{name: version, parts: parts})
	}
	if len(matches) == 0 {
		return "", unknownSelection(fmt.Errorf("no release matches version %q; available: %s", selector, strings.Join(sortedKeys(versions), ", ")))
	}
	// Map iteration order must never reach the result. Rank by numeric value and
	// break every remaining tie on the catalog key itself, so one catalog and one
	// selector always resolve to the same release, or always to the same error.
	sort.Slice(matches, func(first, second int) bool {
		if comparison := compareNumericVersions(matches[first].parts, matches[second].parts); comparison != 0 {
			return comparison > 0
		}
		return matches[first].name < matches[second].name
	})
	if len(matches) > 1 && compareNumericVersions(matches[0].parts, matches[1].parts) == 0 {
		return "", fmt.Errorf("numeric prefix %q is ambiguous between semantically equal versions %q and %q", selector, matches[0].name, matches[1].name)
	}
	return matches[0].name, nil
}

// canonicalNumericKey renders a numeric version without its trailing zero
// components, so 9.7 and 9.7.0 collapse onto one key.
func canonicalNumericKey(parts []uint64) string {
	end := len(parts)
	for end > 1 && parts[end-1] == 0 {
		end--
	}
	rendered := make([]string, end)
	for index := 0; index < end; index++ {
		rendered[index] = strconv.FormatUint(parts[index], 10)
	}
	return strings.Join(rendered, ".")
}

func (c Catalog) Entry(reference, arch string) (Entry, error) {
	ref, err := ParseReference(reference)
	if err != nil {
		return Entry{}, failure.New(failure.Usage, err)
	}
	canonical, imageRecord, err := c.canonicalImage(ref.Image)
	if err != nil {
		return Entry{}, err
	}
	if arch == "" || arch == "native" {
		return Entry{}, errors.New("catalog entry resolution requires a concrete architecture")
	}
	release := ref.Version
	channel := ref.Channel
	if release != "" {
		release, err = resolveVersion(imageRecord.Versions, release)
		if err != nil {
			return Entry{}, fmt.Errorf("image %s: %w", canonical, err)
		}
	} else {
		if channel == "" {
			channel = c.Defaults.Channel
		}
		var ok bool
		release, ok = imageRecord.Channels[channel]
		if !ok {
			return Entry{}, unknownSelection(fmt.Errorf("image %s has no channel %q; available: %s", canonical, channel, strings.Join(sortedKeys(imageRecord.Channels), ", ")))
		}
	}
	version, ok := imageRecord.Versions[release]
	if !ok {
		return Entry{}, fmt.Errorf("image %s has no version %q", canonical, release)
	}
	artifact, ok := version.Variants[arch]
	if !ok {
		return Entry{}, unknownSelection(fmt.Errorf("image %s %s has no %s build; available: %s", canonical, release, arch, strings.Join(sortedKeys(version.Variants), ", ")))
	}
	boot := artifact.Boot
	if boot == "" {
		boot = imageRecord.Boot
	}
	if boot == "" {
		boot = c.Defaults.Boot
	}
	status := artifact.Status
	if status == "" {
		status = version.Status
	}
	provenance := artifact.Provenance
	if provenance == "" {
		provenance = version.Provenance
	}
	return Entry{
		Alias: canonical, Channel: channel, Release: release, Arch: arch,
		File: artifact.File, CacheFile: cacheFile(canonical, release, arch), Upstream: artifact.Upstream,
		SHA256: artifact.SHA256, Format: artifact.Format, ArtifactSize: artifact.ArtifactSize, VirtualSize: artifact.VirtualSize,
		SourceUser: artifact.SourceUser, Boot: boot, Status: status, Provenance: provenance,
	}, nil
}

func (c Catalog) Entries() []Entry {
	result := make([]Entry, 0)
	for _, name := range sortedKeys(c.Images) {
		record := c.Images[name]
		for _, release := range sortedKeys(record.Versions) {
			for _, arch := range sortedKeys(record.Versions[release].Variants) {
				entry, err := c.Entry(name+"@"+release, arch)
				if err != nil {
					continue
				}
				for _, channel := range sortedKeys(record.Channels) {
					if record.Channels[channel] == release {
						entry.Channels = append(entry.Channels, channel)
					}
				}
				result = append(result, entry)
			}
		}
	}
	return result
}
