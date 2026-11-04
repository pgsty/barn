// Command catalogcheck enforces the official image catalog gate before release.
// It verifies signatures using the runtime trust roots, compares the exact
// embedded bytes, and checks that every stable artifact still exists at its
// signed size. -verify-bytes additionally streams and hashes every artifact.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/pgsty/barn/internal/image"
)

type report struct {
	Repository      string                `json:"repository"`
	Revision        uint64                `json:"revision"`
	SHA256          string                `json:"sha256"`
	KeyID           string                `json:"key_id"`
	Defaults        image.CatalogDefaults `json:"defaults"`
	StableArtifacts int                   `json:"stable_artifacts"`
	VerifiedBytes   bool                  `json:"verified_bytes"`
}

func main() {
	verifyBytes := flag.Bool("verify-bytes", false, "also download and hash every stable image (several GB; no image files retained)")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: catalogcheck [-verify-bytes]")
		os.Exit(2)
	}
	timeout := 2 * time.Minute
	if *verifyBytes {
		// Stable images can be several GB. Keep the ordinary HEAD gate short,
		// but allow full streaming verification on slower mirror connections.
		timeout = 30 * time.Minute
	}
	client := &http.Client{Timeout: timeout}
	for _, repository := range []string{image.GlobalRepositoryURL, image.ChinaRepositoryURL} {
		result, err := checkRepository(context.Background(), client, repository, *verifyBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", repository, err)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func checkRepository(ctx context.Context, client *http.Client, repository string, verifyBytes bool) (result report, err error) {
	root, err := os.MkdirTemp("", "barn-release-catalog-*")
	if err != nil {
		return result, err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(root); err == nil {
			err = cleanupErr
		}
	}()
	manager := image.ManifestManager{DataRoot: root, Repository: repository, HTTPClient: client}
	source, err := image.RepositoryCatalogSource(repository)
	if err != nil {
		return result, err
	}
	accepted, err := manager.Sync(ctx, source, false)
	if err != nil {
		return result, fmt.Errorf("verify and activate catalog: %w", err)
	}
	embedded, err := image.EmbeddedCatalogBytes()
	if err != nil {
		return result, err
	}
	if err := checkIdentity(embedded, accepted); err != nil {
		return result, err
	}
	catalog, _, err := manager.Current()
	if err != nil {
		return result, err
	}
	result = report{Repository: repository, Revision: catalog.Version, SHA256: accepted.ActiveDigest,
		KeyID: accepted.KeyID, Defaults: catalog.Defaults, VerifiedBytes: verifyBytes}
	for _, entry := range catalog.Entries() {
		if !slices.Contains(entry.Channels, "stable") {
			continue
		}
		url, err := image.RepositoryArtifactSource(repository, entry.File)
		if err != nil {
			return result, err
		}
		if err := checkArtifact(ctx, client, url, entry, verifyBytes); err != nil {
			return result, fmt.Errorf("%s/%s: %w", entry.Alias, entry.Arch, err)
		}
		result.StableArtifacts++
		if verifyBytes {
			fmt.Fprintf(os.Stderr, "verified %s %s/%s: %d bytes\n", repository, entry.Alias, entry.Arch, entry.ArtifactSize)
		}
	}
	if result.StableArtifacts == 0 {
		return result, fmt.Errorf("catalog has no stable artifacts")
	}
	return result, nil
}

func checkIdentity(embedded []byte, accepted image.ManifestState) error {
	digest := sha256.Sum256(embedded)
	if accepted.KeyID == "" || accepted.Active == "embedded" {
		return fmt.Errorf("remote catalog was not activated with a trusted signature")
	}
	if accepted.ActiveVersion != image.EmbeddedManifestVersion || accepted.ActiveDigest != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("signed catalog revision %d (%s) differs from embedded revision %d (%x); synchronize default-catalog.json, EmbeddedManifestVersion, embedded time and repo.yaml before release",
			accepted.ActiveVersion, accepted.ActiveDigest, image.EmbeddedManifestVersion, digest)
	}
	return nil
}

func checkArtifact(ctx context.Context, client *http.Client, url string, entry image.Entry, verifyBytes bool) error {
	method := http.MethodHead
	if verifyBytes {
		method = http.MethodGet
	}
	request, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("artifact %s returned HTTP %d", url, response.StatusCode)
	}
	if response.ContentLength != entry.ArtifactSize {
		return fmt.Errorf("artifact size %d differs from signed size %d: %s", response.ContentLength, entry.ArtifactSize, url)
	}
	if verifyBytes {
		digest := sha256.New()
		size, err := io.Copy(digest, io.LimitReader(response.Body, entry.ArtifactSize+1))
		if err != nil {
			return err
		}
		if size != entry.ArtifactSize || hex.EncodeToString(digest.Sum(nil)) != entry.SHA256 {
			return fmt.Errorf("artifact bytes differ from signed size/digest: %s", url)
		}
	}
	return nil
}
