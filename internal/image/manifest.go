// Package image owns Barn image references, signed catalogs, repository
// authoring, and the verified local image store.
package image

import (
	_ "embed"
	"fmt"
	"strings"
	"time"
)

type Entry struct {
	Alias        string   `json:"alias"`
	Channel      string   `json:"channel,omitempty"`
	Channels     []string `json:"channels,omitempty"`
	Release      string   `json:"release"`
	Arch         string   `json:"arch"`
	File         string   `json:"file"`
	CacheFile    string   `json:"-"`
	Upstream     string   `json:"upstream,omitempty"`
	SHA256       string   `json:"sha256"`
	Format       string   `json:"format"`
	ArtifactSize int64    `json:"artifact_size"`
	VirtualSize  int64    `json:"virtual_size"`
	SourceUser   string   `json:"source_user,omitempty"`
	Boot         string   `json:"boot"`
	Status       string   `json:"status"`
	Provenance   string   `json:"provenance,omitempty"`
}

//go:embed default-catalog.json
var embeddedCatalogBytes []byte

var embeddedManifestGeneratedAt = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

var formalAliases = []string{"el7", "el8", "el9", "el10", "d12", "d13", "u22", "u24", "u26"}

var formalMatrix = []string{
	"el7/amd64",
	"el8/amd64", "el8/arm64",
	"el9/amd64", "el9/arm64",
	"el10/amd64", "el10/arm64",
	"d12/amd64", "d12/arm64",
	"d13/amd64", "d13/arm64",
	"u22/amd64", "u22/arm64",
	"u24/amd64", "u24/arm64",
	"u26/amd64", "u26/arm64",
}

func EmbeddedCatalog() Catalog {
	catalog, err := strictCatalog(embeddedCatalogBytes)
	if err != nil {
		panic(fmt.Sprintf("invalid embedded image catalog: %v", err))
	}
	return catalog
}

func EmbeddedCatalogBytes() ([]byte, error) {
	return append([]byte(nil), embeddedCatalogBytes...), nil
}

func CanonicalAlias(alias string) string {
	alias = strings.ToLower(strings.TrimSpace(alias))
	canonical, _, err := EmbeddedCatalog().canonicalImage(alias)
	if err == nil {
		return canonical
	}
	return alias
}

// CanonicalReference normalizes a built-in alias while preserving its stable
// channel or explicit version selector. Unknown custom-repository names remain
// intact for runtime catalog resolution.
func CanonicalReference(value string) (string, error) {
	ref, err := ParseReference(value)
	if err != nil {
		return "", err
	}
	if ref.Image == "" {
		ref.Image = EmbeddedCatalog().Defaults.Image
	}
	ref.Image = CanonicalAlias(ref.Image)
	return ref.String(), nil
}
