package setup

import "os"

// standardProxyEnvironment is deliberately narrow. Setup package managers may
// cross a sudo boundary, so preserve only conventional proxy controls and
// never the caller's complete environment.
var standardProxyEnvironment = []string{
	"HTTP_PROXY",
	"HTTPS_PROXY",
	"ALL_PROXY",
	"NO_PROXY",
	"http_proxy",
	"https_proxy",
	"all_proxy",
	"no_proxy",
}

// ProxyEnvironmentNames returns the configured standard proxy variable names
// without their values. User-level package managers inherit those values from
// Barn directly; privileged package managers use the names to ask sudo to
// preserve the same narrow set.
func ProxyEnvironmentNames() []string {
	names := make([]string, 0, len(standardProxyEnvironment))
	for _, name := range standardProxyEnvironment {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			names = append(names, name)
		}
	}
	return names
}
