package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aead.dev/minisign"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/lock"
	"github.com/pgsty/barn/internal/webclient"
)

const (
	ManifestStateSchema    = 1
	ManifestRegistrySchema = 2
	MaxSignatureSize       = 64 << 10
)

type ManifestState struct {
	Schema         int       `json:"schema"`
	HighestVersion uint64    `json:"highest_version"`
	Active         string    `json:"active"`
	ActiveVersion  uint64    `json:"active_version"`
	ActiveDigest   string    `json:"active_digest"`
	KeyID          string    `json:"key_id,omitempty"`
	Source         string    `json:"source"`
	AcceptedAt     time.Time `json:"accepted_at"`
	Downgrade      bool      `json:"downgrade"`
}

type ManifestManager struct {
	DataRoot      string
	Repository    string
	AllowUnsigned bool
	Keys          []minisign.PublicKey
	HTTPClient    *http.Client
}

var manifestFilename = regexp.MustCompile(`^v[0-9]+-[0-9a-f]{64}\.json$`)
var manifestRepositoryKey = regexp.MustCompile(`^(?:default|repo-[0-9a-f]{64})$`)

type ManifestRegistry struct {
	Schema       int                      `json:"schema"`
	Repositories map[string]ManifestState `json:"repositories"`
}

func (m ManifestManager) keys() ([]minisign.PublicKey, error) {
	keys := m.Keys
	if len(keys) == 0 {
		keys = productionManifestKeys
	}
	if len(keys) < 2 {
		return nil, errors.New("manifest verifier has no active and standby production public keys")
	}
	return append([]minisign.PublicKey(nil), keys...), nil
}

func (m ManifestManager) root() string      { return filepath.Join(m.DataRoot, "images", "manifests") }
func (m ManifestManager) versions() string  { return filepath.Join(m.root(), "versions") }
func (m ManifestManager) statePath() string { return filepath.Join(m.root(), "state.json") }
func (m ManifestManager) lockPath() string {
	return filepath.Join(m.DataRoot, "locks", "manifest.lock")
}

func (m ManifestManager) validate() error {
	if m.DataRoot == "" || !filepath.IsAbs(m.DataRoot) {
		return errors.New("manifest data root must be absolute")
	}
	for _, directory := range []string{m.DataRoot, m.root(), m.versions(), filepath.Dir(m.lockPath())} {
		if err := ensurePrivateDir(directory); err != nil {
			return err
		}
	}
	return nil
}

func manifestDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func versionSignaturePath(manifestPath, keyID string) string {
	return manifestPath + "." + strings.ToLower(keyID) + ".minisig"
}

func verifyManifest(keys []minisign.PublicKey, data, signatureText []byte) (uint64, error) {
	if len(signatureText) == 0 || len(signatureText) > MaxSignatureSize {
		return 0, errors.New("manifest signature size is invalid")
	}
	var signature minisign.Signature
	if err := signature.UnmarshalText(signatureText); err != nil {
		return 0, fmt.Errorf("decode minisign signature: %w", err)
	}
	for _, key := range keys {
		if key.ID() != signature.KeyID {
			continue
		}
		if !minisign.Verify(key, data, signatureText) {
			return 0, errors.New("minisign signature or trusted comment verification failed")
		}
		return key.ID(), nil
	}
	return 0, fmt.Errorf("manifest signature uses unknown key ID %016X", signature.KeyID)
}

func strictManifestState(data []byte) (ManifestState, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state ManifestState
	if err := decoder.Decode(&state); err != nil {
		return ManifestState{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ManifestState{}, errors.New("manifest state has trailing JSON")
	}
	if err := validateManifestState(state); err != nil {
		return ManifestState{}, err
	}
	return state, nil
}

func validateManifestState(state ManifestState) error {
	if state.Schema != ManifestStateSchema || state.HighestVersion == 0 || state.ActiveVersion == 0 || (state.Active != "embedded" && state.HighestVersion < state.ActiveVersion) || !digestPattern.MatchString(state.ActiveDigest) || state.Source == "" || state.AcceptedAt.IsZero() {
		return errors.New("manifest state fields are invalid")
	}
	if state.Active != "embedded" && !manifestFilename.MatchString(state.Active) {
		return errors.New("manifest state active filename is unsafe")
	}
	return nil
}

func (m ManifestManager) stateKey() (string, error) {
	repository := strings.TrimSpace(m.Repository)
	if repository == "" {
		return "default", nil
	}
	normalized, err := NormalizeRepository(repository)
	if err != nil {
		return "", err
	}
	if DefaultRepositoryURL != "" {
		defaultRepository, defaultErr := NormalizeRepository(DefaultRepositoryURL)
		if defaultErr == nil && normalized == defaultRepository {
			return "default", nil
		}
	}
	digest := sha256.Sum256([]byte(normalized))
	return "repo-" + hex.EncodeToString(digest[:]), nil
}

func (m ManifestManager) unsignedAllowed() bool {
	return m.AllowUnsigned && RepositoryAllowsUnsigned(m.Repository)
}

func (m ManifestManager) unsignedSourceMatchesRepository(provenance string) bool {
	expected, err := RepositoryCatalogSource(m.Repository)
	if err != nil || expected == "" {
		return false
	}
	if strings.HasPrefix(provenance, "local:") {
		return expected == strings.TrimPrefix(provenance, "local:")
	}
	return expected == provenance
}

func (m ManifestManager) readRegistry() (ManifestRegistry, error) {
	info, err := os.Lstat(m.statePath())
	if err != nil {
		return ManifestRegistry{}, err
	}
	if !info.Mode().IsRegular() {
		return ManifestRegistry{}, errors.New("manifest state must be a regular non-symlink file")
	}
	data, err := os.ReadFile(m.statePath())
	if err != nil {
		return ManifestRegistry{}, err
	}
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return ManifestRegistry{}, err
	}
	// Compatibility expiry: manifest-state-v1 in CONTRIBUTING.md#compatibility-expiry.
	if header.Schema == ManifestStateSchema {
		legacy, err := strictManifestState(data)
		if err != nil {
			return ManifestRegistry{}, err
		}
		return ManifestRegistry{Schema: ManifestRegistrySchema, Repositories: map[string]ManifestState{"default": legacy}}, nil
	}
	if header.Schema != ManifestRegistrySchema {
		return ManifestRegistry{}, fmt.Errorf("unsupported manifest registry schema %d", header.Schema)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var registry ManifestRegistry
	if err := decoder.Decode(&registry); err != nil {
		return ManifestRegistry{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ManifestRegistry{}, errors.New("manifest registry has trailing JSON")
	}
	if registry.Repositories == nil {
		return ManifestRegistry{}, errors.New("manifest registry repositories are missing")
	}
	for key, state := range registry.Repositories {
		if !manifestRepositoryKey.MatchString(key) {
			return ManifestRegistry{}, fmt.Errorf("manifest repository key %q is invalid", key)
		}
		if err := validateManifestState(state); err != nil {
			return ManifestRegistry{}, fmt.Errorf("manifest repository %s: %w", key, err)
		}
	}
	return registry, nil
}

func (m ManifestManager) readState() (ManifestState, error) {
	registry, err := m.readRegistry()
	if err != nil {
		return ManifestState{}, err
	}
	key, err := m.stateKey()
	if err != nil {
		return ManifestState{}, err
	}
	state, ok := registry.Repositories[key]
	if !ok {
		return ManifestState{}, os.ErrNotExist
	}
	return state, nil
}

func (m ManifestManager) writeState(state ManifestState) error {
	registry, err := m.readRegistry()
	if errors.Is(err, os.ErrNotExist) {
		registry = ManifestRegistry{Schema: ManifestRegistrySchema, Repositories: make(map[string]ManifestState)}
	} else if err != nil {
		return err
	}
	key, err := m.stateKey()
	if err != nil {
		return err
	}
	registry.Repositories[key] = state
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(m.statePath(), append(data, '\n'), 0o600)
}

func embeddedState() (ManifestState, error) {
	data, err := EmbeddedCatalogBytes()
	if err != nil {
		return ManifestState{}, err
	}
	return ManifestState{Schema: ManifestStateSchema, HighestVersion: EmbeddedManifestVersion, Active: "embedded", ActiveVersion: EmbeddedManifestVersion, ActiveDigest: manifestDigest(data), Source: "embedded", AcceptedAt: embeddedManifestGeneratedAt}, nil
}

// Compatibility expiry: manifest-state-v1 in CONTRIBUTING.md#compatibility-expiry.
// currentBaselineState advances any state whose accepted high-water mark
// predates this binary. The compiled catalog is trusted, strictly newer data;
// older signed files remain on disk and can be selected again only through an
// explicit, high-water-aware sync.
func currentBaselineState(state ManifestState) (ManifestState, error) {
	if state.HighestVersion < EmbeddedManifestVersion {
		return embeddedState()
	}
	if state.Active != "embedded" {
		if state.ActiveVersion == EmbeddedManifestVersion {
			data, err := EmbeddedCatalogBytes()
			if err != nil {
				return ManifestState{}, err
			}
			if state.ActiveDigest != manifestDigest(data) {
				return ManifestState{}, errors.New("signed manifest equivocation with embedded baseline")
			}
		}
		return state, nil
	}
	data, err := EmbeddedCatalogBytes()
	if err != nil {
		return ManifestState{}, err
	}
	if state.ActiveVersion != EmbeddedManifestVersion || state.ActiveDigest != manifestDigest(data) {
		return ManifestState{}, errors.New("embedded manifest state does not match binary baseline")
	}
	return state, nil
}

func (m ManifestManager) Current() (Catalog, ManifestState, error) {
	if m.DataRoot == "" || !filepath.IsAbs(m.DataRoot) {
		return Catalog{}, ManifestState{}, errors.New("manifest data root must be absolute")
	}
	state, err := m.readState()
	if errors.Is(err, os.ErrNotExist) {
		state, err = embeddedState()
		if err != nil {
			return Catalog{}, ManifestState{}, err
		}
		return EmbeddedCatalog(), state, nil
	}
	if err != nil {
		return Catalog{}, ManifestState{}, err
	}
	key, err := m.stateKey()
	if err != nil {
		return Catalog{}, ManifestState{}, err
	}
	if key == "default" {
		state, err = currentBaselineState(state)
		if err != nil {
			return Catalog{}, ManifestState{}, err
		}
	}
	if state.Active == "embedded" {
		return EmbeddedCatalog(), state, nil
	}
	if state.KeyID == "" && !m.unsignedAllowed() {
		return Catalog{}, ManifestState{}, errors.New("active repository catalog is unsigned but this repository requires a trusted signature")
	}
	manifestPath := filepath.Join(m.versions(), state.Active)
	data, err := readLimitedFile(manifestPath, MaxManifestSize)
	if err != nil {
		return Catalog{}, ManifestState{}, err
	}
	keyID := uint64(0)
	if state.KeyID != "" {
		keys, keyErr := m.keys()
		if keyErr != nil {
			return Catalog{}, ManifestState{}, keyErr
		}
		signatureText, signatureErr := readLimitedFile(versionSignaturePath(manifestPath, state.KeyID), MaxSignatureSize)
		if errors.Is(signatureErr, os.ErrNotExist) {
			// Schema-1 state stored one unqualified signature beside each
			// catalog. Preserve that read path during migration.
			signatureText, signatureErr = readLimitedFile(manifestPath+".minisig", MaxSignatureSize)
		}
		if signatureErr != nil {
			return Catalog{}, ManifestState{}, signatureErr
		}
		keyID, err = verifyManifest(keys, data, signatureText)
		if err != nil {
			return Catalog{}, ManifestState{}, err
		}
	}
	catalog, err := strictCatalog(data)
	if err != nil {
		return Catalog{}, ManifestState{}, err
	}
	if catalog.Version != state.ActiveVersion || manifestDigest(data) != state.ActiveDigest || (state.KeyID != "" && strings.ToUpper(strconv.FormatUint(keyID, 16)) != state.KeyID) {
		return Catalog{}, ManifestState{}, errors.New("active manifest bytes/signature do not match state pointer")
	}
	return catalog, state, nil
}

func readLimitedFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect source %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("source must be a regular file no larger than %d bytes: %s", limit, path)
	}
	return os.ReadFile(path)
}

func (m ManifestManager) httpClient() *http.Client {
	client := *webclient.New(30 * time.Second)
	if m.HTTPClient != nil {
		client = *m.HTTPClient
		if client.Timeout == 0 {
			client.Timeout = 30 * time.Second
		}
	}
	previous := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 5 || (request.URL.Scheme != "http" && request.URL.Scheme != "https") {
			return errors.New("catalog redirect scheme is invalid or exceeds five hops")
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && request.URL.Scheme != "https" {
			return errors.New("catalog redirect attempts an HTTPS downgrade")
		}
		if previous != nil {
			return previous(request, via)
		}
		return nil
	}
	return &client
}

func (m ManifestManager) download(ctx context.Context, source string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	response, err := m.httpClient().Do(request)
	if err != nil {
		return nil, reachError(err)
	}
	defer func() {
		// The bounded response is read-only and is verified before acceptance;
		// closing only affects HTTP connection reuse.
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK || response.ContentLength > limit {
		return nil, fmt.Errorf("manifest download failed status/size policy: %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("manifest download exceeds limit or failed")
	}
	return data, nil
}

func (m ManifestManager) readSource(ctx context.Context, source string) ([]byte, []byte, string, error) {
	parsed, err := url.Parse(source)
	if err == nil && parsed.Scheme != "" {
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, nil, "", failure.New(failure.Usage, errors.New("remote catalog source must be an absolute HTTP(S) URL without credentials, query, or fragment"))
		}
		manifestSource := parsed.String()
		signatureURL := *parsed
		signatureURL.Path += ".minisig"
		signatureURL.RawPath = ""
		signatureSource := signatureURL.String()
		data, err := m.download(ctx, manifestSource, MaxManifestSize)
		if err != nil {
			return nil, nil, "", err
		}
		signatureText, err := m.download(ctx, signatureSource, MaxSignatureSize)
		if err == nil && len(signatureText) == 0 {
			err = errors.New("catalog signature body is empty")
		}
		if err != nil && m.AllowUnsigned && parsed.Scheme == "https" {
			return data, nil, manifestSource, nil
		}
		return data, signatureText, manifestSource, err
	}
	absolute, err := filepath.Abs(source)
	if err != nil {
		return nil, nil, "", err
	}
	data, err := readLimitedFile(absolute, MaxManifestSize)
	if err != nil {
		return nil, nil, "", err
	}
	signatureText, err := readLimitedFile(absolute+".minisig", MaxSignatureSize)
	if err != nil && m.AllowUnsigned && errors.Is(err, os.ErrNotExist) {
		return data, nil, "local:" + absolute, nil
	}
	if err == nil && len(signatureText) == 0 {
		return nil, nil, "", errors.New("catalog signature file is empty")
	}
	return data, signatureText, "local:" + absolute, err
}

func (m ManifestManager) Sync(ctx context.Context, source string, allowDowngrade bool) (_ ManifestState, returnErr error) {
	if err := m.validate(); err != nil {
		return ManifestState{}, err
	}
	data, signatureText, provenance, err := m.readSource(ctx, source)
	if err != nil {
		return ManifestState{}, fmt.Errorf("fetch catalog %s: %w", source, err)
	}
	keyID := uint64(0)
	if len(signatureText) != 0 {
		keys, keyErr := m.keys()
		if keyErr != nil {
			return ManifestState{}, keyErr
		}
		keyID, err = verifyManifest(keys, data, signatureText)
		if err != nil {
			return ManifestState{}, err
		}
	} else if !m.unsignedAllowed() {
		return ManifestState{}, errors.New("image catalog has no detached signature")
	} else if !m.unsignedSourceMatchesRepository(provenance) {
		return ManifestState{}, errors.New("unsigned catalog source does not match the selected repository catalog.json")
	}
	catalog, err := strictCatalog(data)
	if err != nil {
		return ManifestState{}, err
	}
	digest := manifestDigest(data)
	lockContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	manifestLock, err := lock.Acquire(lockContext, m.lockPath(), false)
	if err != nil {
		return ManifestState{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, manifestLock, "manifest registry lock")
	}()
	stateKey, err := m.stateKey()
	if err != nil {
		return ManifestState{}, err
	}
	current, err := m.readState()
	if errors.Is(err, os.ErrNotExist) {
		if stateKey == "default" {
			current, err = embeddedState()
		} else {
			current = ManifestState{}
			err = nil
		}
	}
	if err != nil {
		return ManifestState{}, err
	}
	if stateKey == "default" {
		current, err = currentBaselineState(current)
		if err != nil {
			return ManifestState{}, err
		}
	}
	if current.KeyID != "" && len(signatureText) == 0 && !allowDowngrade {
		return ManifestState{}, errors.New("repository previously served a signed catalog; refusing an unsigned catalog without --allow-downgrade")
	}
	if current.ActiveVersion != 0 && catalog.Version == current.ActiveVersion && digest != current.ActiveDigest {
		return ManifestState{}, errors.New("manifest version equivocation: same version has different bytes")
	}
	if catalog.Version < current.HighestVersion && !allowDowngrade {
		return ManifestState{}, fmt.Errorf("manifest version %d is below accepted high-water mark %d", catalog.Version, current.HighestVersion)
	}
	filename := fmt.Sprintf("v%d-%s.json", catalog.Version, digest)
	manifestPath := filepath.Join(m.versions(), filename)
	if existing, err := os.ReadFile(manifestPath); err == nil {
		if !bytes.Equal(existing, data) {
			return ManifestState{}, errors.New("versioned manifest path contains different bytes")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := fsutil.AtomicWrite(manifestPath, data, 0o600); err != nil {
			return ManifestState{}, err
		}
	} else {
		return ManifestState{}, err
	}
	if len(signatureText) != 0 {
		signaturePath := versionSignaturePath(manifestPath, strings.ToUpper(strconv.FormatUint(keyID, 16)))
		if existing, err := os.ReadFile(signaturePath); err == nil {
			if !bytes.Equal(existing, signatureText) {
				return ManifestState{}, errors.New("versioned signature path contains different bytes")
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := fsutil.AtomicWrite(signaturePath, signatureText, 0o600); err != nil {
				return ManifestState{}, err
			}
		} else {
			return ManifestState{}, err
		}
	}
	highest := current.HighestVersion
	if catalog.Version > highest {
		highest = catalog.Version
	}
	keyText := ""
	if keyID != 0 {
		keyText = strings.ToUpper(strconv.FormatUint(keyID, 16))
	}
	state := ManifestState{Schema: ManifestStateSchema, HighestVersion: highest, Active: filename, ActiveVersion: catalog.Version, ActiveDigest: digest, KeyID: keyText, Source: provenance, AcceptedAt: time.Now().UTC(), Downgrade: current.HighestVersion != 0 && catalog.Version < current.HighestVersion}
	if err := m.writeState(state); err != nil {
		return ManifestState{}, err
	}
	return state, nil
}

func (m ManifestManager) Reset(ctx context.Context) (_ ManifestState, returnErr error) {
	if err := m.validate(); err != nil {
		return ManifestState{}, err
	}
	lockContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	manifestLock, err := lock.Acquire(lockContext, m.lockPath(), false)
	if err != nil {
		return ManifestState{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, manifestLock, "manifest registry lock")
	}()
	stateKey, err := m.stateKey()
	if err != nil {
		return ManifestState{}, err
	}
	if stateKey != "default" {
		current, readErr := m.readState()
		if errors.Is(readErr, os.ErrNotExist) {
			embedded, embeddedErr := embeddedState()
			if embeddedErr != nil {
				return ManifestState{}, embeddedErr
			}
			embedded.AcceptedAt = time.Now().UTC()
			return embedded, nil
		}
		if readErr != nil {
			return ManifestState{}, readErr
		}
		embedded, err := embeddedState()
		if err != nil {
			return ManifestState{}, err
		}
		embedded.HighestVersion = current.HighestVersion
		embedded.KeyID = current.KeyID
		embedded.AcceptedAt = time.Now().UTC()
		if err := m.writeState(embedded); err != nil {
			return ManifestState{}, err
		}
		return embedded, nil
	}
	current, err := m.readState()
	if errors.Is(err, os.ErrNotExist) {
		current, err = embeddedState()
	}
	if err != nil {
		return ManifestState{}, err
	}
	embedded, err := embeddedState()
	if err != nil {
		return ManifestState{}, err
	}
	if current.HighestVersion > embedded.HighestVersion {
		embedded.HighestVersion = current.HighestVersion
	}
	embedded.AcceptedAt = time.Now().UTC()
	if err := m.writeState(embedded); err != nil {
		return ManifestState{}, err
	}
	return embedded, nil
}
