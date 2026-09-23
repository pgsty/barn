package image

import (
	"errors"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pgsty/farrow/internal/failure"
)

const CatalogFilename = "catalog.json"

const (
	GlobalRepositoryURL = "https://repo.pigsty.io/farrow"
	ChinaRepositoryURL  = "https://repo.pigsty.cc/farrow"
)

// DefaultRepositoryURL and MirrorRepositoryURL are variables so tests can
// replace network endpoints without changing the production defaults.
var (
	DefaultRepositoryURL = GlobalRepositoryURL
	MirrorRepositoryURL  = ChinaRepositoryURL
)

// ResolveRepository applies --repo > --mirror > FARROW_REPO > the global
// default. The boolean result records whether the default was overridden,
// which lets the catalog verifier retain the existing custom-repository policy.
func ResolveRepository(repository string, mirror bool) (string, bool, error) {
	repository = strings.TrimSpace(repository)
	explicit := repository != ""
	if repository == "" && mirror {
		repository = MirrorRepositoryURL
		explicit = true
	}
	if repository == "" {
		repository = strings.TrimSpace(os.Getenv("FARROW_REPO"))
		explicit = repository != ""
	}
	if repository == "" {
		repository = DefaultRepositoryURL
	}
	normalized, err := NormalizeRepository(repository)
	return normalized, explicit, err
}

// RepositoryAllowsUnsigned reports whether an explicitly selected repository
// may rely on local ownership or HTTPS rather than a detached trusted
// signature. Both official production repositories remain in the signed trust
// domain even when a user spells either URL explicitly.
func RepositoryAllowsUnsigned(value string) bool {
	normalized, err := NormalizeRepository(value)
	if err != nil || normalized == "" {
		return false
	}
	parsed, _ := url.Parse(normalized)
	if parsed.Scheme == "https" && (parsed.Port() == "" || parsed.Port() == "443") && path.Clean(parsed.Path) == "/farrow" {
		host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
		if host == "repo.pigsty.io" || host == "repo.pigsty.cc" {
			return false
		}
	}
	return parsed.Scheme == "" || parsed.Scheme == "https"
}

func NormalizeRepository(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.Scheme != "" {
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", failure.New(failure.Usage, errors.New("image repository must be an absolute HTTP(S) URL without credentials, query, or fragment, or an absolute directory"))
		}
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
		return parsed.String(), nil
	}
	if !filepath.IsAbs(value) {
		return "", failure.New(failure.Usage, errors.New("image repository must be an absolute HTTP(S) URL or an absolute directory"))
	}
	return filepath.Clean(value), nil
}

func RepositoryCatalogSource(repository string) (string, error) {
	repository, err := NormalizeRepository(repository)
	if err != nil || repository == "" {
		return "", err
	}
	parsed, _ := url.Parse(repository)
	if parsed.Scheme != "" {
		parsed.Path = path.Join(parsed.Path, CatalogFilename)
		return parsed.String(), nil
	}
	return filepath.Join(repository, CatalogFilename), nil
}

func RepositoryArtifactSource(repository, filename string) (string, error) {
	repository, err := NormalizeRepository(repository)
	if err != nil || repository == "" {
		return "", err
	}
	if !safeRepositoryFile(filename) {
		return "", errors.New("image repository artifact path is unsafe")
	}
	parsed, _ := url.Parse(repository)
	if parsed.Scheme != "" {
		parsed.Path = path.Join(parsed.Path, filename)
		return parsed.String(), nil
	}
	return filepath.Join(repository, filepath.FromSlash(filename)), nil
}
