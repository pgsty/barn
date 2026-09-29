package webclient

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestProxyEnvironment(t *testing.T) {
	for _, test := range []struct {
		name, target, want string
		env                map[string]string
	}{
		{"all", "https://repo.pigsty.io/barn", "socks5://127.0.0.1:1080", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1080"}},
		{"lowercase", "http://example.com", "http://proxy.example:8080", map[string]string{"all_proxy": "http://proxy.example:8080"}},
		{"scheme wins", "https://example.com", "http://https-proxy:8080", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1080", "HTTPS_PROXY": "http://https-proxy:8080"}},
		{"excluded domain", "https://repo.pigsty.io/barn", "", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1080", "NO_PROXY": "pigsty.io"}},
		{"excluded network", "https://10.10.10.10", "", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1080", "NO_PROXY": "10.0.0.0/8"}},
		{"localhost", "http://127.0.0.1:8080", "", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1080"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD"} {
				t.Setenv(key, "")
			}
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			client := New(42 * time.Second)
			if client.Timeout != 42*time.Second {
				t.Fatal("timeout changed")
			}
			request, err := http.NewRequest(http.MethodGet, test.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			proxy, err := client.Transport.(*http.Transport).Proxy(request)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if proxy != nil {
				got = proxy.String()
			}
			if got != test.want {
				t.Fatalf("proxy = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNewWithRedirectPolicy(t *testing.T) {
	sentinel := errors.New("refused")
	client := NewWithRedirectPolicy(time.Minute, func(*http.Request, []*http.Request) error { return sentinel })
	if client.Timeout != time.Minute || client.Transport.(*http.Transport).Proxy == nil {
		t.Fatalf("client = %#v", client)
	}
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, sentinel) {
		t.Fatalf("redirect policy: %v", err)
	}
}
