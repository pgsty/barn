package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pgsty/barn/internal/activity"
)

// CatalogSession pins one catalog revision and repository selection for a
// complete command, so every lookup and pull agrees on aliases, boot
// metadata, digests, and artifact locations. Opening a session never touches
// the network; only explicit update or sync operations activate new bytes.
type CatalogSession struct {
	service    Service
	catalog    Catalog
	manifest   ManifestState
	repository string
}

// OpenCatalog loads the active local catalog for the configured repository.
func (s Service) OpenCatalog() (*CatalogSession, error) {
	manager, err := s.manifestManager()
	if err != nil {
		return nil, err
	}
	catalog, manifest, err := manager.Current()
	if err != nil {
		return nil, err
	}
	return &CatalogSession{service: s, catalog: catalog, manifest: manifest, repository: manager.Repository}, nil
}

func (session *CatalogSession) Manifest() ManifestState { return session.manifest }

func (session *CatalogSession) Entries() []Entry { return session.catalog.Entries() }

func validateCatalogArch(arch string) error {
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("unsupported image architecture %q", arch)
	}
	return nil
}

func (session *CatalogSession) store() (Store, error) {
	return session.service.store(session.repository)
}

// located is one alias resolved either from the pinned catalog or from a
// registered local image. Path and Metadata are set only for local images.
type located struct {
	Entry    Entry
	Local    bool
	Path     string
	Metadata Metadata
}

func (l located) info(manifest ManifestState) Info {
	return Info{Entry: l.Entry, Manifest: manifest, Cached: true, Path: l.Path, Metadata: &l.Metadata}
}

// locate answers from the catalog first and falls back to a registered local
// alias. A miss reports the catalog error; a registered local image retains
// its own validation error instead of looking like an unknown alias.
func (session *CatalogSession) locate(ctx context.Context, alias, arch string) (located, error) {
	if err := validateCatalogArch(arch); err != nil {
		return located{}, err
	}
	entry, entryErr := session.catalog.Entry(alias, arch)
	if entryErr == nil {
		return located{Entry: entry}, nil
	}
	name := strings.ToLower(strings.TrimSpace(alias))
	if !strings.HasPrefix(name, localAliasPrefix) {
		return located{}, entryErr
	}
	registry, err := (Store{DataRoot: session.service.DataRoot}).readLocalAliases()
	if err != nil {
		return located{}, fmt.Errorf("read local image registry: %w", err)
	}
	registered, ok := registry.Aliases[name]
	if !ok {
		return located{}, entryErr
	}
	if registered.Arch != arch {
		return located{}, fmt.Errorf("local image %q is for %s, requested %s", name, registered.Arch, arch)
	}
	store, err := session.store()
	if err != nil {
		return located{}, err
	}
	localEntry, path, metadata, localErr := store.ResolveLocalAlias(ctx, alias, arch)
	if localErr != nil {
		return located{}, fmt.Errorf("local image %q: %w", name, localErr)
	}
	return located{Entry: localEntry, Local: true, Path: path, Metadata: metadata}, nil
}

// LookupArch resolves catalog metadata without downloading anything.
func (session *CatalogSession) LookupArch(ctx context.Context, alias, arch string) (Entry, error) {
	found, err := session.locate(ctx, alias, arch)
	return found.Entry, err
}

// InfoArch reports one alias and its cache state without downloading it.
func (session *CatalogSession) InfoArch(ctx context.Context, alias, arch string) (Info, error) {
	found, err := session.locate(ctx, alias, arch)
	if err != nil {
		return Info{}, err
	}
	if found.Local {
		return found.info(session.manifest), nil
	}
	// An absent cache is a metadata-only lookup. QEMU is needed only when
	// there are image bytes to inspect; do not install tools just to browse.
	cachePath, err := (Store{DataRoot: session.service.DataRoot}).Path(found.Entry)
	if err != nil {
		return Info{}, err
	}
	if _, err := os.Lstat(cachePath); errors.Is(err, os.ErrNotExist) {
		return Info{Entry: found.Entry, Manifest: session.manifest}, nil
	} else if err != nil {
		return Info{}, err
	}
	store, err := session.store()
	if err != nil {
		return Info{}, err
	}
	path, metadata, cacheErr := store.ValidateCached(ctx, found.Entry)
	if errors.Is(cacheErr, os.ErrNotExist) {
		return Info{Entry: found.Entry, Manifest: session.manifest}, nil
	}
	if cacheErr != nil {
		return Info{}, cacheErr
	}
	return Info{Entry: found.Entry, Manifest: session.manifest, Cached: true, Path: path, Metadata: &metadata}, nil
}

// PullArch downloads and verifies one alias into the store.
func (session *CatalogSession) PullArch(ctx context.Context, alias, arch string) (Info, error) {
	found, err := session.locate(ctx, alias, arch)
	if err != nil {
		return Info{}, err
	}
	if found.Local {
		return found.info(session.manifest), nil
	}
	store, err := session.store()
	if err != nil {
		return Info{}, err
	}
	path, metadata, err := store.Pull(ctx, found.Entry)
	if err != nil {
		return Info{}, err
	}
	return Info{Entry: found.Entry, Manifest: session.manifest, Cached: true, Path: path, Metadata: &metadata}, nil
}

// ResolveArch is the lifecycle seam for an explicit guest architecture. It
// does not infer acceleration or fall back to another artifact architecture.
func (session *CatalogSession) ResolveArch(ctx context.Context, alias, arch string) (Entry, string, Metadata, error) {
	session.service.report("image-resolve", fmt.Sprintf("Resolving image %s for %s", alias, arch))
	found, err := session.locate(ctx, alias, arch)
	if err != nil {
		return Entry{}, "", Metadata{}, err
	}
	if found.Local {
		session.service.Progress.Report(activity.Event{Phase: "image-ready", Message: fmt.Sprintf("Using local image %s (%s)", found.Entry.Alias, arch), Done: true})
		return found.Entry, found.Path, found.Metadata, nil
	}
	store, err := session.store()
	if err != nil {
		return Entry{}, "", Metadata{}, err
	}
	path, metadata, err := store.Pull(ctx, found.Entry)
	return found.Entry, path, metadata, err
}
