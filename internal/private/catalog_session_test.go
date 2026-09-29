package private

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/spec"
)

func TestLifecycleCatalogSessionNeverTouchesTheRepository(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	defaultRepository := image.DefaultRepositoryURL
	image.DefaultRepositoryURL = server.URL
	t.Cleanup(func() { image.DefaultRepositoryURL = defaultRepository })
	t.Setenv("BARN_REPO", "")
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "barn-home"))

	manager := Manager{}
	if err := manager.ensureImageSession(); err != nil {
		t.Fatal(err)
	}
	resolved := spec.Resolved{Nodes: []spec.Node{
		{Name: "el9", Image: "el9"},
		{Name: "el10", Image: "el10"},
		{Name: "d12", Image: "d12"},
		{Name: "d13", Image: "d13"},
		{Name: "u22", Image: "u22"},
		{Name: "u24", Image: "u24"},
		{Name: "u26", Image: "u26"},
	}}
	profile := platform.Profile{Arch: "arm64", RequiresUEFI: true}
	for attempt := 0; attempt < 2; attempt++ {
		boot, err := manager.resolveBootMode(context.Background(), profile, resolved)
		if err != nil || boot != "uefi" {
			t.Fatalf("resolve boot attempt %d = %q, %v", attempt+1, boot, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("lifecycle image resolution made %d implicit catalog requests", requests.Load())
	}
	if manager.imageSession == nil || manager.imageSession.Manifest().Source != "embedded" {
		t.Fatalf("lifecycle session did not use the embedded catalog: %#v", manager.imageSession)
	}
}
