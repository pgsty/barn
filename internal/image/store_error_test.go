package image

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/failure"
)

func TestUnreachableSourceNamesHostAndKeepsRetryableCause(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	_, getErr := http.Get("http://" + address + "/catalog.json")
	err = reachError(getErr)
	if got, want := err.Error(), "cannot reach "+address+": connection refused"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if _, _, next := failure.Classify(err); !strings.Contains(next, "--mirror") {
		t.Fatalf("next = %q", next)
	}
	if !retrySource(err) {
		t.Fatal("rewording hid the retryable transport error")
	}
}

func TestValidateCachedDigestMismatchHasFactualError(t *testing.T) {
	t.Parallel()
	dataRoot := t.TempDir()
	pathname := filepath.Join(dataRoot, "images", "u24", "fixture.qcow2")
	if err := os.MkdirAll(filepath.Dir(pathname), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathname, []byte("corrupt"), 0o444); err != nil {
		t.Fatal(err)
	}
	entry := Entry{File: "u24/fixture.qcow2", SHA256: strings.Repeat("0", 64), Format: "qcow2", ArtifactSize: int64(len("corrupt"))}
	_, _, err := (Store{DataRoot: dataRoot}).ValidateCached(context.Background(), entry)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") || strings.Contains(err.Error(), "%!w") || strings.Contains(err.Error(), "<nil>") {
		t.Fatalf("digest mismatch error = %v", err)
	}
}
