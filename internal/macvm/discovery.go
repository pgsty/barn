package macvm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func appleRestoreURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && appleDownloadHost(u.Hostname())
}

// Discovery runs only on an explicit image update. Ordinary setup/up always
// use the pinned catalog or the already selected base.
type restoreDiscovery struct {
	RestoreMetadata
	URL string `json:"url"`
}

func (m *Manager) discoverRestoreImage(ctx context.Context) (restoreDiscovery, error) {
	var discovered restoreDiscovery
	if err := m.Runner.Call(ctx, nil, &discovered, "discover"); err != nil {
		return discovered, err
	}
	if !strings.HasPrefix(discovered.Version, "27.") || !safeID(discovered.Build) || !validSHA256(discovered.HardwareModelHash) || !appleRestoreURL(discovered.URL) {
		return discovered, errors.New("restore image discovery did not return a compatible official macOS 27 IPSW; existing images were preserved")
	}
	return discovered, nil
}

func (discovered restoreDiscovery) installerSpec(ctx context.Context) (IPSWSpec, error) {
	if discovered.Build == DefaultIPSW().Build {
		return DefaultIPSW(), nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, discovered.URL, nil)
	if err != nil {
		return IPSWSpec{}, err
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 || !appleRestoreURL(req.URL.String()) {
			return errors.New("IPSW metadata redirect left Apple's HTTPS servers")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return IPSWSpec{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.ContentLength <= 0 {
		return IPSWSpec{}, fmt.Errorf("restore image metadata request failed: HTTP %d, length %d", response.StatusCode, response.ContentLength)
	}
	spec := IPSWSpec{Version: discovered.Version, Build: discovered.Build, URL: discovered.URL, SizeBytes: response.ContentLength, SHA256: response.Header.Get("x-amz-meta-digest-sha256")}
	return spec, spec.validate(false)
}
