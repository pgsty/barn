package setup

import (
	"reflect"
	"testing"
)

func TestProxyEnvironmentNamesReturnsOnlyConfiguredStandardNames(t *testing.T) {
	for _, name := range standardProxyEnvironment {
		t.Setenv(name, "")
	}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:8118")
	t.Setenv("no_proxy", "localhost,127.0.0.1")
	t.Setenv("BARN_NOT_A_PROXY", "must-not-cross-sudo")

	want := []string{"HTTP_PROXY", "no_proxy"}
	if got := ProxyEnvironmentNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("proxy environment names = %v, want %v", got, want)
	}
}
