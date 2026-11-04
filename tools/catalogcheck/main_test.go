package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pgsty/barn/internal/image"
)

func TestRejectsStaleOrUnverifiedCatalog(t *testing.T) {
	data, err := image.EmbeddedCatalogBytes()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	valid := image.ManifestState{Active: "remote.json", ActiveVersion: image.EmbeddedManifestVersion,
		ActiveDigest: hex.EncodeToString(digest[:]), KeyID: "trusted"}
	if err := checkIdentity(data, valid); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"new-revision", "changed-bytes", "unsigned", "embedded"} {
		t.Run(scenario, func(t *testing.T) {
			state := valid
			switch scenario {
			case "new-revision":
				state.ActiveVersion++
			case "changed-bytes":
				state.ActiveDigest = "different"
			case "unsigned":
				state.KeyID = ""
			case "embedded":
				state.Active = "embedded"
			}
			if checkIdentity(data, state) == nil {
				t.Fatal("unsafe release catalog accepted")
			}
		})
	}
}

func TestArtifactGate(t *testing.T) {
	digest := sha256.Sum256([]byte("image"))
	entry := image.Entry{ArtifactSize: 5, SHA256: hex.EncodeToString(digest[:])}
	for _, tc := range []struct {
		name            string
		status          int
		body            string
		verify, wantErr bool
	}{
		{"available", 200, "image", false, false},
		{"removed", 404, "image", false, true},
		{"wrong-size", 200, "bad", false, true},
		{"bytes-verified", 200, "image", true, false},
		{"same-size-tampering", 200, "wrong", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)))
				w.WriteHeader(tc.status)
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, tc.body)
				}
			}))
			defer server.Close()
			err := checkArtifact(context.Background(), server.Client(), server.URL, entry, tc.verify)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
