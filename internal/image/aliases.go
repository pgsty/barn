package image

import (
	"sort"
	"strings"
)

// AliasTarget maps a built-in shorthand or friendly image name to its
// canonical catalog family and optional pinned guest architecture.
type AliasTarget struct {
	Image string
	Arch  string
}

var builtinAliases = map[string]AliasTarget{
	// CentOS Linux 7 (amd64-only)
	"c7":       {Image: "el7"},
	"centos7":  {Image: "el7"},
	"centos79": {Image: "el7"},
	"el7x":     {Image: "el7", Arch: "amd64"},
	"el7i":     {Image: "el7", Arch: "amd64"},
	"c7x":      {Image: "el7", Arch: "amd64"},
	"c7i":      {Image: "el7", Arch: "amd64"},

	// Rocky Linux 8
	"rocky8": {Image: "el8"},
	"el8a":   {Image: "el8", Arch: "arm64"},
	"el8x":   {Image: "el8", Arch: "amd64"},
	"el8i":   {Image: "el8", Arch: "amd64"},

	// Rocky Linux 9
	"rocky9": {Image: "el9"},
	"el9a":   {Image: "el9", Arch: "arm64"},
	"el9x":   {Image: "el9", Arch: "amd64"},
	"el9i":   {Image: "el9", Arch: "amd64"},

	// Rocky Linux 10 (current EL family target for e / el / rocky)
	"e":       {Image: "el10"},
	"el":      {Image: "el10"},
	"rocky":   {Image: "el10"},
	"rocky10": {Image: "el10"},
	"el10a":   {Image: "el10", Arch: "arm64"},
	"el10x":   {Image: "el10", Arch: "amd64"},
	"el10i":   {Image: "el10", Arch: "amd64"},
	"ea":      {Image: "el10", Arch: "arm64"},
	"ex":      {Image: "el10", Arch: "amd64"},
	"ei":      {Image: "el10", Arch: "amd64"},
	"ela":     {Image: "el10", Arch: "arm64"},
	"elx":     {Image: "el10", Arch: "amd64"},
	"eli":     {Image: "el10", Arch: "amd64"},

	// Debian 12
	"debian12": {Image: "d12"},
	"bookworm": {Image: "d12"},
	"d12a":     {Image: "d12", Arch: "arm64"},
	"d12x":     {Image: "d12", Arch: "amd64"},
	"d12i":     {Image: "d12", Arch: "amd64"},

	// Debian 13 (current Debian family target for d / debian)
	"d":        {Image: "d13"},
	"debian":   {Image: "d13"},
	"debian13": {Image: "d13"},
	"trixie":   {Image: "d13"},
	"d13a":     {Image: "d13", Arch: "arm64"},
	"d13x":     {Image: "d13", Arch: "amd64"},
	"d13i":     {Image: "d13", Arch: "amd64"},
	"da":       {Image: "d13", Arch: "arm64"},
	"dx":       {Image: "d13", Arch: "amd64"},
	"di":       {Image: "d13", Arch: "amd64"},

	// Ubuntu 22.04 LTS
	"ubuntu22":   {Image: "u22"},
	"ubuntu2204": {Image: "u22"},
	"jammy":      {Image: "u22"},
	"u22a":       {Image: "u22", Arch: "arm64"},
	"u22x":       {Image: "u22", Arch: "amd64"},
	"u22i":       {Image: "u22", Arch: "amd64"},

	// Ubuntu 24.04 LTS
	"ubuntu24":   {Image: "u24"},
	"ubuntu2404": {Image: "u24"},
	"noble":      {Image: "u24"},
	"u24a":       {Image: "u24", Arch: "arm64"},
	"u24x":       {Image: "u24", Arch: "amd64"},
	"u24i":       {Image: "u24", Arch: "amd64"},

	// Ubuntu 26.04 LTS (current Ubuntu family target for u / ubuntu)
	"u":          {Image: "u26"},
	"ubuntu":     {Image: "u26"},
	"ubuntu26":   {Image: "u26"},
	"ubuntu2604": {Image: "u26"},
	"resolute":   {Image: "u26"},
	"u26a":       {Image: "u26", Arch: "arm64"},
	"u26x":       {Image: "u26", Arch: "amd64"},
	"u26i":       {Image: "u26", Arch: "amd64"},
	"ua":         {Image: "u26", Arch: "arm64"},
	"ux":         {Image: "u26", Arch: "amd64"},
	"ui":         {Image: "u26", Arch: "amd64"},
}

// floatingShortArchAliases binds short family + arch shorthands (ea, ela, da,
// ua, etc.) to their base family alias so a future signed catalog that moves
// e/el/d/u to a newer family automatically moves ea/da/ua with it.
var floatingShortArchAliases = map[string]AliasTarget{
	"ea":  {Image: "e", Arch: "arm64"},
	"ex":  {Image: "e", Arch: "amd64"},
	"ei":  {Image: "e", Arch: "amd64"},
	"ela": {Image: "el", Arch: "arm64"},
	"elx": {Image: "el", Arch: "amd64"},
	"eli": {Image: "el", Arch: "amd64"},
	"da":  {Image: "d", Arch: "arm64"},
	"dx":  {Image: "d", Arch: "amd64"},
	"di":  {Image: "d", Arch: "amd64"},
	"ua":  {Image: "u", Arch: "arm64"},
	"ux":  {Image: "u", Arch: "amd64"},
	"ui":  {Image: "u", Arch: "amd64"},
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
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func recordHasArch(record CatalogImage, arch string) bool {
	for _, version := range record.Versions {
		if _, ok := version.Variants[arch]; ok {
			return true
		}
	}
	return false
}
