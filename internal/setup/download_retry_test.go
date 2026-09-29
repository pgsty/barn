package setup

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/pgsty/barn/internal/activity"
)

func TestSocketVMNetDownloadRecovery(t *testing.T) {
	t.Parallel()
	const content = "verified archive fixture"
	for _, test := range []struct {
		name     string
		attempts int
		wantErr  bool
	}{
		{"temporary HTTP", 2, false},
		{"connection reset", 2, false},
		{"truncated body", 2, false},
		{"persistent outage", 3, true},
		{"not found", 1, true},
		{"wrong digest", 1, true},
		{"invalid archive", 1, true},
		{"server cooldown", 1, true},
		{"oversized body", 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			attempts := 0
			client := doerFunc(func(request *http.Request) (*http.Response, error) {
				attempts++
				status, data, size := http.StatusOK, content, int64(len(content))
				headers := make(http.Header)
				if attempts == 1 || test.name == "persistent outage" {
					switch test.name {
					case "temporary HTTP", "persistent outage":
						status = http.StatusServiceUnavailable
					case "connection reset":
						return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
					case "truncated body":
						data = content[:4]
					case "not found":
						status = http.StatusNotFound
					case "wrong digest":
						data = strings.Repeat("x", len(content))
					case "oversized body":
						size = maxSocketVMNetArchive + 1
					case "server cooldown":
						status = http.StatusTooManyRequests
						headers.Set("Retry-After", "60")
					}
				}
				return &http.Response{StatusCode: status, Status: http.StatusText(status), Request: request, Header: headers,
					Body: io.NopCloser(strings.NewReader(data)), ContentLength: size}, nil
			})
			cache := t.TempDir()
			release := sourcesFixtureRelease("https://fixture.invalid/socket_vmnet-test.tar.gz")
			verify := contentVerifier(content)
			if test.name == "invalid archive" {
				verify = func(string, string) error { return io.ErrUnexpectedEOF }
			}
			result, err := downloadSocketVMNetRelease(context.Background(), release, "arm64", cache, client, Sources{}, verify)
			if (err != nil) != test.wantErr || attempts != test.attempts {
				t.Fatalf("attempts=%d want=%d, result=%#v err=%v", attempts, test.attempts, result, err)
			}
			files, readErr := os.ReadDir(cache)
			if readErr != nil {
				t.Fatal(readErr)
			}
			wantFiles := 1
			if test.wantErr {
				wantFiles = 0
			} else if data, err := os.ReadFile(result.Path); err != nil || string(data) != content || !result.Downloaded {
				t.Fatalf("recovered download = %q, %v, %#v", data, err, result)
			}
			if len(files) != wantFiles {
				t.Fatalf("download left temporary or invalid artifacts: %v", files)
			}
		})
	}
}

func TestSocketVMNetDownloadCancelsDuringRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable", Request: request,
			Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	sources := Sources{Progress: func(event activity.Event) {
		if event.Phase == "download-retry" {
			cancel()
		}
	}}
	release := sourcesFixtureRelease("https://fixture.invalid/socket_vmnet-test.tar.gz")
	_, err := downloadSocketVMNetRelease(ctx, release, "arm64", t.TempDir(), client, sources, contentVerifier("expected"))
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("retry did not stop on cancellation: attempts=%d err=%v", attempts, err)
	}
}
