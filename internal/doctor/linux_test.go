package doctor

import (
	"testing"

	linuxnet "github.com/pgsty/barn/internal/network/linux"
)

func TestParseLinuxFamily(t *testing.T) {
	t.Parallel()
	for content, expected := range map[string]linuxnet.Family{
		"ID=ubuntu\nID_LIKE=debian\n":                linuxnet.Debian,
		"ID=rocky\nID_LIKE=\"rhel centos fedora\"\n": linuxnet.RPM,
		"ID=unknown\n": "",
	} {
		if actual := parseLinuxFamily(content); actual != expected {
			t.Errorf("parse family %q = %q, want %q", content, actual, expected)
		}
	}
}
