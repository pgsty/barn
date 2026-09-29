package image

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"
)

const (
	developmentPublicRoot1  = "untrusted comment: minisign public key: E87A2D0D9F49B03B\nRWQ7sEmfDS166ELVheon0eL5PZYT1QNdSvZN+cDhd1F204TzYGJR3ml7"
	developmentPublicRoot2  = "untrusted comment: minisign public key: 10EDAA24D2B13348\nRWRIM7HSJKrtEBNN6GnxF5GuBvl24Haql+TDHXVH3jqmS+i6XtW5UVS/"
	developmentPrivateRoot1 = "untrusted comment: minisign encrypted secret key\nRWQAAEIyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAO7BJnw0teujbJ9oOq043ZD95Ob7BcsdvNrFUMDvMUw3dfUEnsZAEG0LVheon0eL5PZYT1QNdSvZN+cDhd1F204TzYGJR3ml7AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	developmentPrivateRoot2 = "untrusted comment: minisign encrypted secret key\nRWQAAEIyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAASDOx0iSq7RDpxLHyY4Sf8sm1VxTMy8qK17JtEK35cySQAFgg+gXWWxNN6GnxF5GuBvl24Haql+TDHXVH3jqmS+i6XtW5UVS/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
)

func testManifestRoots(t *testing.T) []minisign.PublicKey {
	t.Helper()
	result := make([]minisign.PublicKey, 0, 2)
	for _, text := range []string{developmentPublicRoot1, developmentPublicRoot2} {
		var key minisign.PublicKey
		if err := key.UnmarshalText([]byte(text)); err != nil {
			t.Fatal(err)
		}
		result = append(result, key)
	}
	return result
}

func privateKey(t *testing.T, text string) minisign.PrivateKey {
	t.Helper()
	var key minisign.PrivateKey
	if err := key.UnmarshalText([]byte(text)); err != nil {
		t.Fatal(err)
	}
	return key
}

func signedCatalog(t *testing.T, directory string, version uint64, key minisign.PrivateKey, mutate func(*Catalog)) string {
	t.Helper()
	catalog := EmbeddedCatalog()
	catalog.Version = version
	if mutate != nil {
		mutate(&catalog)
	}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join(directory, "images.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	signature := minisign.SignWithComments(key, data, "timestamp:1787486400", "barn test manifest")
	if err := os.WriteFile(path+".minisig", signature, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestManifestManagerFailsClosedWithoutProductionRoots(t *testing.T) {
	t.Parallel()
	dataRoot := filepath.Join(t.TempDir(), "data")
	untrusted := ManifestManager{DataRoot: dataRoot}
	if catalog, state, err := untrusted.Current(); err != nil || state.Active != "embedded" || catalog.Version != EmbeddedManifestVersion {
		t.Fatalf("embedded catalog without external roots = %#v %#v %v", catalog, state, err)
	}
	path := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion+1, privateKey(t, developmentPrivateRoot1), nil)
	if _, err := untrusted.Sync(context.Background(), path, false); err == nil || !strings.Contains(err.Error(), "unknown key ID") {
		t.Fatalf("test-signed catalog was accepted by the embedded production verifier: %v", err)
	}
	trusted := ManifestManager{DataRoot: dataRoot, Keys: testManifestRoots(t)}
	if _, err := trusted.Sync(context.Background(), path, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := untrusted.Current(); err == nil || !strings.Contains(err.Error(), "unknown key ID") {
		t.Fatalf("active external manifest was accepted without its signing roots: %v", err)
	}
	if _, err := untrusted.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, state, err := untrusted.Current(); err != nil || state.Active != "embedded" {
		t.Fatalf("embedded reset without external roots = %#v %v", state, err)
	}
}

// TestExportDevelopmentRepository is an opt-in helper for the disposable M0
// repository. Production catalogs must be signed by separately held keys.
func TestExportDevelopmentRepository(t *testing.T) {
	directory := os.Getenv("BARN_TEST_REPO_OUTPUT")
	if directory == "" {
		t.Skip("BARN_TEST_REPO_OUTPUT is not set")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := EmbeddedCatalogBytes()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, CatalogFilename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	key := privateKey(t, developmentPrivateRoot1)
	signature := minisign.SignWithComments(key, data, "timestamp:1787702400", "barn M0 development catalog")
	if err := os.WriteFile(path+".minisig", signature, 0o644); err != nil {
		t.Fatal(err)
	}
	var sums strings.Builder
	if os.Getenv("BARN_TEST_REPO_COMPLETE") == "1" {
		entries := EmbeddedCatalog().Entries()
		sort.Slice(entries, func(i, j int) bool { return entries[i].File < entries[j].File })
		for _, entry := range entries {
			fmt.Fprintf(&sums, "%s  %s\n", entry.SHA256, entry.File)
		}
	} else {
		artifacts, globErr := filepath.Glob(filepath.Join(directory, "*", "*.qcow2"))
		if globErr != nil {
			t.Fatal(globErr)
		}
		sort.Strings(artifacts)
		for _, artifact := range artifacts {
			digest, _, digestErr := digestFile(artifact)
			if digestErr != nil {
				t.Fatal(digestErr)
			}
			relative, relativeErr := filepath.Rel(directory, artifact)
			if relativeErr != nil {
				t.Fatal(relativeErr)
			}
			fmt.Fprintf(&sums, "%s  %s\n", digest, filepath.ToSlash(relative))
		}
	}
	sumsPath := filepath.Join(directory, "SHA256SUMS")
	sumsBytes := []byte(sums.String())
	if err := os.WriteFile(sumsPath, sumsBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	sumsSignature := minisign.SignWithComments(key, sumsBytes, "timestamp:1787702400", "barn M0 development checksums")
	if err := os.WriteFile(sumsPath+".minisig", sumsSignature, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestManifestSyncTwoKeysRollbackAndReset(t *testing.T) {
	t.Parallel()
	dataRoot := filepath.Join(t.TempDir(), "data")
	manager := ManifestManager{DataRoot: dataRoot, Keys: testManifestRoots(t)}
	directory1 := t.TempDir()
	path1 := signedCatalog(t, directory1, EmbeddedManifestVersion+2, privateKey(t, developmentPrivateRoot1), nil)
	state, err := manager.Sync(context.Background(), path1, false)
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveVersion != EmbeddedManifestVersion+2 || state.KeyID == "" || state.Downgrade {
		t.Fatalf("state = %#v", state)
	}
	catalog, current, err := manager.Current()
	if err != nil || catalog.Version != state.ActiveVersion || current.ActiveDigest != state.ActiveDigest {
		t.Fatalf("current = %#v %#v %v", catalog, current, err)
	}

	directory2 := t.TempDir()
	path2 := signedCatalog(t, directory2, EmbeddedManifestVersion+1, privateKey(t, developmentPrivateRoot2), nil)
	if _, err := manager.Sync(context.Background(), path2, false); err == nil {
		t.Fatal("manifest rollback unexpectedly accepted")
	}
	state, err = manager.Sync(context.Background(), path2, true)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Downgrade || state.HighestVersion != EmbeddedManifestVersion+2 || state.ActiveVersion != EmbeddedManifestVersion+1 {
		t.Fatalf("downgrade state = %#v", state)
	}
	reset, err := manager.Reset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reset.Active != "embedded" || reset.HighestVersion != EmbeddedManifestVersion+2 {
		t.Fatalf("reset state = %#v", reset)
	}
}

func TestManifestRejectsTamperUnknownKeyAndEquivocation(t *testing.T) {
	t.Parallel()
	manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data"), Keys: testManifestRoots(t)}
	directory := t.TempDir()
	key := privateKey(t, developmentPrivateRoot1)
	path := signedCatalog(t, directory, EmbeddedManifestVersion+1, key, nil)
	if _, err := manager.Sync(context.Background(), path, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-2] ^= 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Sync(context.Background(), path, false); err == nil {
		t.Fatal("tampered manifest unexpectedly accepted")
	}

	unknownPublic, unknownPrivate, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = unknownPublic
	unknownDir := t.TempDir()
	unknownPath := signedCatalog(t, unknownDir, EmbeddedManifestVersion+2, unknownPrivate, nil)
	if _, err := manager.Sync(context.Background(), unknownPath, false); err == nil {
		t.Fatal("unknown signing key unexpectedly accepted")
	}

	equivocationDir := t.TempDir()
	equivocation := signedCatalog(t, equivocationDir, EmbeddedManifestVersion+1, key, func(catalog *Catalog) {
		imageRecord := catalog.Images["u24"]
		version := imageRecord.Versions["20260801.0.0"]
		artifact := version.Variants["arm64"]
		artifact.Provenance = "different bytes"
		version.Variants["arm64"] = artifact
		imageRecord.Versions["20260801.0.0"] = version
		catalog.Images["u24"] = imageRecord
	})
	if _, err := manager.Sync(context.Background(), equivocation, true); err == nil {
		t.Fatal("same-version equivocation unexpectedly accepted")
	}
}

func TestManifestHTTPSSyncAndDowngradeRedirect(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := signedCatalog(t, directory, EmbeddedManifestVersion+3, privateKey(t, developmentPrivateRoot1), nil)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := os.ReadFile(path + ".minisig")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, ".minisig") {
			_, _ = writer.Write(signature)
			return
		}
		_, _ = writer.Write(data)
	}))
	defer server.Close()
	manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data"), Keys: testManifestRoots(t), HTTPClient: server.Client()}
	state, err := manager.Sync(context.Background(), server.URL+"/images.json", false)
	if err != nil || state.ActiveVersion != EmbeddedManifestVersion+3 {
		t.Fatalf("HTTPS sync = %#v, %v", state, err)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write(data)
	}))
	defer plain.Close()
	redirect := httptest.NewTLSServer(http.RedirectHandler(plain.URL+"/images.json", http.StatusFound))
	defer redirect.Close()
	blocked := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "blocked"), Keys: testManifestRoots(t), HTTPClient: redirect.Client()}
	if _, err := blocked.Sync(context.Background(), redirect.URL, false); err == nil {
		t.Fatal("HTTPS-to-HTTP manifest redirect unexpectedly accepted")
	}
}

func TestExplicitHTTPRepositoryRejectsEmptySignatureBody(t *testing.T) {
	t.Parallel()
	catalog := EmbeddedCatalog()
	catalog.Version = 1
	data, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, ".minisig") {
			writer.WriteHeader(http.StatusOK)
			return
		}
		_, _ = writer.Write(data)
	}))
	defer server.Close()
	manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data"), Repository: server.URL, AllowUnsigned: true, HTTPClient: server.Client()}
	if _, err := manager.Sync(context.Background(), server.URL+"/"+CatalogFilename, false); err == nil || !strings.Contains(err.Error(), "signature body is empty") {
		t.Fatalf("empty HTTP signature error = %v", err)
	}
}

func TestManifestRemoteSourceRejectsSecretsAndAmbiguousSuffixes(t *testing.T) {
	t.Parallel()
	manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data")}
	for _, source := range []string{
		"https://user:secret@example.test/catalog.json",
		"https://example.test/catalog.json?token=secret",
		"https://example.test/catalog.json#release",
	} {
		if _, _, _, err := manager.readSource(context.Background(), source); err == nil || !strings.Contains(err.Error(), "without credentials, query, or fragment") {
			t.Errorf("source %q error = %v", source, err)
		}
	}
}

func TestPriorEmbeddedStateAdvancesToNewBinaryBaseline(t *testing.T) {
	t.Parallel()
	manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data")}
	if err := os.MkdirAll(manager.root(), 0o700); err != nil {
		t.Fatal(err)
	}
	prior := ManifestState{
		Schema: ManifestStateSchema, HighestVersion: EmbeddedManifestVersion - 1,
		Active: "embedded", ActiveVersion: EmbeddedManifestVersion - 1,
		ActiveDigest: strings.Repeat("0", 64), Source: "embedded",
		AcceptedAt: embeddedManifestGeneratedAt.Add(-time.Hour),
	}
	data, err := json.Marshal(ManifestRegistry{Schema: ManifestRegistrySchema, Repositories: map[string]ManifestState{"default": prior}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.statePath(), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, state, err := manager.Current()
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Version != EmbeddedManifestVersion || state.ActiveVersion != EmbeddedManifestVersion || state.HighestVersion != EmbeddedManifestVersion || state.ActiveDigest == prior.ActiveDigest {
		t.Fatalf("advanced catalog/state = %#v %#v", catalog, state)
	}
	reset, err := manager.Reset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reset.ActiveVersion != EmbeddedManifestVersion || reset.HighestVersion != EmbeddedManifestVersion {
		t.Fatalf("persisted reset = %#v", reset)
	}
}

func TestPriorSignedStateAdvancesAndCanSyncCurrentBaseline(t *testing.T) {
	t.Parallel()
	manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data"), Keys: testManifestRoots(t)}
	if err := manager.validate(); err != nil {
		t.Fatal(err)
	}
	key := privateKey(t, developmentPrivateRoot1)
	priorPath := signedCatalog(t, t.TempDir(), EmbeddedManifestVersion-1, key, nil)
	priorData, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatal(err)
	}
	priorSignature, err := os.ReadFile(priorPath + ".minisig")
	if err != nil {
		t.Fatal(err)
	}
	priorCatalog, err := strictCatalog(priorData)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := verifyManifest(manager.Keys, priorData, priorSignature)
	if err != nil {
		t.Fatal(err)
	}
	priorDigest := manifestDigest(priorData)
	priorName := fmt.Sprintf("v%d-%s.json", priorCatalog.Version, priorDigest)
	if err := os.WriteFile(filepath.Join(manager.versions(), priorName), priorData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(versionSignaturePath(filepath.Join(manager.versions(), priorName), strconv.FormatUint(keyID, 16)), priorSignature, 0o600); err != nil {
		t.Fatal(err)
	}
	priorState := ManifestState{
		Schema: ManifestStateSchema, HighestVersion: priorCatalog.Version,
		Active: priorName, ActiveVersion: priorCatalog.Version, ActiveDigest: priorDigest,
		KeyID: strings.ToUpper(strconv.FormatUint(keyID, 16)), Source: "local:" + priorPath,
		AcceptedAt: time.Now().UTC(),
	}
	stateData, err := json.Marshal(ManifestRegistry{Schema: ManifestRegistrySchema, Repositories: map[string]ManifestState{"default": priorState}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.statePath(), append(stateData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	catalog, state, err := manager.Current()
	if err != nil || catalog.Version != EmbeddedManifestVersion || state.Active != "embedded" || state.HighestVersion != EmbeddedManifestVersion {
		t.Fatalf("advanced signed baseline = %#v %#v %v", catalog, state, err)
	}

	currentData, err := EmbeddedCatalogBytes()
	if err != nil {
		t.Fatal(err)
	}
	currentPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(currentPath, currentData, 0o600); err != nil {
		t.Fatal(err)
	}
	currentSignature := minisign.SignWithComments(key, currentData, "timestamp:1787894400", "barn current baseline")
	if err := os.WriteFile(currentPath+".minisig", currentSignature, 0o600); err != nil {
		t.Fatal(err)
	}
	accepted, err := manager.Sync(context.Background(), currentPath, false)
	if err != nil || accepted.ActiveVersion != EmbeddedManifestVersion || accepted.Active == "embedded" || accepted.HighestVersion != EmbeddedManifestVersion {
		t.Fatalf("sync current signed baseline = %#v %v", accepted, err)
	}
}

func TestSignedBaselineVersionRejectsDifferentBytes(t *testing.T) {
	t.Parallel()
	state := ManifestState{
		Schema: ManifestStateSchema, HighestVersion: EmbeddedManifestVersion,
		Active:        fmt.Sprintf("v%d-%s.json", EmbeddedManifestVersion, strings.Repeat("f", 64)),
		ActiveVersion: EmbeddedManifestVersion, ActiveDigest: strings.Repeat("f", 64),
		KeyID: "E87A2D0D9F49B03B", Source: "local:/fixture/catalog.json", AcceptedAt: time.Now().UTC(),
	}
	if _, err := currentBaselineState(state); err == nil || !strings.Contains(err.Error(), "equivocation") {
		t.Fatalf("signed baseline equivocation error = %v", err)
	}
}

func TestExplicitUnsignedLocalRepositoriesKeepIndependentHighWaterState(t *testing.T) {
	t.Parallel()
	dataRoot := filepath.Join(t.TempDir(), "data")
	write := func(root, alias string) string {
		t.Helper()
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		catalog := EmbeddedCatalog()
		catalog.Version = 1
		record := catalog.Images["d13"]
		record.Aliases = append(record.Aliases, alias)
		catalog.Images["d13"] = record
		data, err := json.MarshalIndent(catalog, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, CatalogFilename)
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	root1 := filepath.Join(t.TempDir(), "repo1")
	root2 := filepath.Join(t.TempDir(), "repo2")
	path1 := write(root1, "custom-one")
	path2 := write(root2, "custom-two")
	manager1 := ManifestManager{DataRoot: dataRoot, Repository: root1, AllowUnsigned: true}
	manager2 := ManifestManager{DataRoot: dataRoot, Repository: root2, AllowUnsigned: true}
	if state, err := manager1.Sync(context.Background(), path1, false); err != nil || state.ActiveVersion != 1 || state.KeyID != "" {
		t.Fatalf("repo1 sync = %#v, %v", state, err)
	}
	if _, err := manager1.Sync(context.Background(), path2, false); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("cross-repository unsigned sync error = %v", err)
	}
	if state, err := manager2.Sync(context.Background(), path2, false); err != nil || state.ActiveVersion != 1 || state.KeyID != "" {
		t.Fatalf("repo2 sync = %#v, %v", state, err)
	}
	catalog1, _, err1 := manager1.Current()
	catalog2, _, err2 := manager2.Current()
	if err1 != nil || err2 != nil {
		t.Fatalf("current repositories: %v, %v", err1, err2)
	}
	if _, _, err := catalog1.canonicalImage("custom-one"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog2.canonicalImage("custom-two"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog1.canonicalImage("custom-two"); err == nil {
		t.Fatal("repo1 observed repo2 alias")
	}
	if _, _, err := (ManifestManager{DataRoot: dataRoot, Repository: root1}).Current(); err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("unsigned cached catalog crossed a signed policy boundary: %v", err)
	}
	registry, err := manager1.readRegistry()
	if err != nil || registry.Schema != ManifestRegistrySchema || len(registry.Repositories) != 2 {
		t.Fatalf("manifest registry = %#v, %v", registry, err)
	}
	if _, err := manager1.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, err := manager1.readState(); err != nil || state.Active != "embedded" || state.HighestVersion != 1 {
		t.Fatalf("custom repository reset lost high-water state: %#v, %v", state, err)
	}
	if _, err := manager1.Sync(context.Background(), path1, false); err != nil {
		t.Fatalf("custom repository could not resync revision 1 after reset: %v", err)
	}
}

func TestIndependentRepositoriesCanSignIdenticalCatalogWithDifferentKeys(t *testing.T) {
	t.Parallel()
	dataRoot := filepath.Join(t.TempDir(), "data")
	root1 := t.TempDir()
	root2 := t.TempDir()
	path1 := signedCatalog(t, root1, EmbeddedManifestVersion+10, privateKey(t, developmentPrivateRoot1), nil)
	path2 := signedCatalog(t, root2, EmbeddedManifestVersion+10, privateKey(t, developmentPrivateRoot2), nil)
	manager1 := ManifestManager{DataRoot: dataRoot, Repository: root1, Keys: testManifestRoots(t)}
	manager2 := ManifestManager{DataRoot: dataRoot, Repository: root2, Keys: testManifestRoots(t)}
	if _, err := manager1.Sync(context.Background(), path1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := manager2.Sync(context.Background(), path2, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager1.Current(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager2.Current(); err != nil {
		t.Fatal(err)
	}
}

func TestSignedRepositoryRefusesUnsignedDowngradeWithoutExplicitOverride(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataRoot := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(root, CatalogFilename)
	key := privateKey(t, developmentPrivateRoot1)
	write := func(revision uint64, signed bool) {
		t.Helper()
		catalog := EmbeddedCatalog()
		catalog.Version = revision
		data, err := json.MarshalIndent(catalog, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		signaturePath := path + ".minisig"
		if signed {
			signature := minisign.SignWithComments(key, data, "timestamp:1787961600", "barn signed repository fixture")
			if err := os.WriteFile(signaturePath, signature, 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Remove(signaturePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	manager := ManifestManager{DataRoot: dataRoot, Repository: root, AllowUnsigned: true, Keys: testManifestRoots(t)}
	write(1, true)
	if _, err := manager.Sync(context.Background(), path, false); err != nil {
		t.Fatal(err)
	}
	if state, err := manager.Reset(context.Background()); err != nil || state.Active != "embedded" || state.HighestVersion != 1 || state.KeyID == "" {
		t.Fatalf("signed repository reset lost trust state: %#v, %v", state, err)
	}
	write(2, false)
	if _, err := manager.Sync(context.Background(), path, false); err == nil || !strings.Contains(err.Error(), "previously served a signed catalog") {
		t.Fatalf("signature downgrade error = %v", err)
	}
	if state, err := manager.Sync(context.Background(), path, true); err != nil || state.KeyID != "" || state.ActiveVersion != 2 {
		t.Fatalf("explicit signature downgrade = %#v, %v", state, err)
	}
}

func TestManifestRegistryRejectsRetiredSingleRepositoryState(t *testing.T) {
	manager := ManifestManager{DataRoot: filepath.Join(t.TempDir(), "data")}
	if err := os.MkdirAll(manager.root(), 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := embeddedState()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.statePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Current(); err == nil || !strings.Contains(err.Error(), "unsupported manifest registry schema") {
		t.Fatalf("retired manifest state accepted: %v", err)
	}
}
