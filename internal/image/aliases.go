package image

import (
	"maps"
	"slices"
	"strings"
)

// AliasTarget maps a built-in shorthand or friendly image name to its
// canonical catalog family and optional pinned guest architecture.
type AliasTarget struct {
	Image string
	Arch  string
}

type aliasFamily struct {
	canonical string
	bare      []string
	archBases []string // suffixes x/i -> amd64, plus a -> arm64 unless amd64Only
	floating  []string // short family bases that follow future catalog family moves
	amd64Only bool
}

var aliasFamilies = []aliasFamily{
	{canonical: "el7", bare: []string{"c7", "centos7", "centos79"}, archBases: []string{"el7", "c7"}, amd64Only: true},
	{canonical: "el8", bare: []string{"rocky8"}, archBases: []string{"el8"}},
	{canonical: "el9", bare: []string{"rocky9"}, archBases: []string{"el9"}},
	{canonical: "el10", bare: []string{"e", "el", "rocky", "rocky10"}, archBases: []string{"el10", "e", "el"}, floating: []string{"e", "el"}},
	{canonical: "d12", bare: []string{"debian12", "bookworm"}, archBases: []string{"d12"}},
	{canonical: "d13", bare: []string{"d", "debian", "debian13", "trixie"}, archBases: []string{"d13", "d"}, floating: []string{"d"}},
	{canonical: "u22", bare: []string{"ubuntu22", "ubuntu2204", "jammy"}, archBases: []string{"u22"}},
	{canonical: "u24", bare: []string{"ubuntu24", "ubuntu2404", "noble"}, archBases: []string{"u24"}},
	{canonical: "u26", bare: []string{"u", "ubuntu", "ubuntu26", "ubuntu2604", "resolute"}, archBases: []string{"u26", "u"}, floating: []string{"u"}},
}

var (
	builtinAliases = make(map[string]AliasTarget, 72)
	// floatingShortArchAliases binds short family + arch shorthands (ea, ela, da,
	// ua, etc.) to their base family alias so a future signed catalog that moves
	// e/el/d/u to a newer family automatically moves ea/da/ua with it.
	floatingShortArchAliases = make(map[string]AliasTarget, 16)
)

func init() {
	archSuffixes := []struct{ suffix, arch string }{{"a", "arm64"}, {"x", "amd64"}, {"i", "amd64"}}
	for _, family := range aliasFamilies {
		for _, name := range family.bare {
			builtinAliases[name] = AliasTarget{Image: family.canonical}
		}
		for _, base := range family.archBases {
			for _, item := range archSuffixes {
				if family.amd64Only && item.arch != "amd64" {
					continue
				}
				builtinAliases[base+item.suffix] = AliasTarget{Image: family.canonical, Arch: item.arch}
			}
		}
		for _, base := range family.floating {
			for _, item := range archSuffixes {
				floatingShortArchAliases[base+item.suffix] = AliasTarget{Image: base, Arch: item.arch}
			}
		}
	}
}

func (c Catalog) isSupersededCatalogAlias(imageName, alias string) bool {
	switch {
	case imageName == "el9" && alias == "rocky":
		_, ok := c.Images["el10"]
		return ok
	case imageName == "u24" && alias == "ubuntu":
		_, ok := c.Images["u26"]
		return ok
	default:
		return false
	}
}

// ResolveAlias normalizes a bare image name against built-in and embedded
// catalog aliases, returning the canonical family name and any architecture
// implied by an architecture-specific alias ("" when architecture-agnostic).
func ResolveAlias(alias string) (canonical, arch string) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" {
		return "", ""
	}
	if target, ok := builtinAliases[alias]; ok {
		return target.Image, target.Arch
	}
	resolved, impliedArch, _, err := EmbeddedCatalog().resolveImage(alias)
	if err == nil {
		return resolved, impliedArch
	}
	return alias, ""
}

// CompletionNames returns the sorted, deduplicated image names, aliases,
// channel selectors, and version selectors offered by c.
func (c Catalog) CompletionNames() []string {
	seen := make(map[string]struct{}, len(c.Images)*6+len(builtinAliases))
	add := func(value string) {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	for name, record := range c.Images {
		add(name)
		for _, alias := range record.Aliases {
			add(alias)
		}
		for channel := range record.Channels {
			add(name + ":" + channel)
		}
		for version := range record.Versions {
			add(name + "@" + version)
		}
	}
	for alias, target := range builtinAliases {
		record, exists := c.Images[target.Image]
		if !exists {
			continue
		}
		if target.Arch != "" && !recordHasArch(record, target.Arch) {
			continue
		}
		add(alias)
	}
	return slices.Sorted(maps.Keys(seen))
}

func recordHasArch(record CatalogImage, arch string) bool {
	for _, version := range record.Versions {
		if _, ok := version.Variants[arch]; ok {
			return true
		}
	}
	return false
}
