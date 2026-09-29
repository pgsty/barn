// Package webclient builds Barn's outbound HTTP clients. Every download —
// images, catalogs, socket_vmnet — honors the standard proxy environment
// (HTTP_PROXY, HTTPS_PROXY, ALL_PROXY, NO_PROXY) through an explicit
// transport rather than the mutable process-global default.
package webclient

import (
	"net/http"
	"net/url"
	"os"
	"time"

	"golang.org/x/net/http/httpproxy"
)

func transport() *http.Transport {
	config := httpproxy.FromEnvironment()
	fallback := os.Getenv("ALL_PROXY")
	if fallback == "" {
		fallback = os.Getenv("all_proxy")
	}
	if config.HTTPProxy == "" {
		config.HTTPProxy = fallback
	}
	if config.HTTPSProxy == "" {
		config.HTTPSProxy = fallback
	}
	selectProxy := config.ProxyFunc()
	proxy := func(request *http.Request) (*url.URL, error) { return selectProxy(request.URL) }
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{Proxy: proxy}
	}
	cloned := base.Clone()
	cloned.Proxy = proxy
	return cloned
}

// New returns a proxy-aware client with one overall request timeout.
func New(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transport()}
}

// NewWithRedirectPolicy is New plus a redirect gate.
func NewWithRedirectPolicy(timeout time.Duration, policy func(*http.Request, []*http.Request) error) *http.Client {
	client := New(timeout)
	client.CheckRedirect = policy
	return client
}
