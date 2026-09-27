package macvm

import "testing"

func TestAppleRestoreURLAcceptsOfficialCDNAndRejectsOtherOrigins(t *testing.T) {
	for _, address := range []string{DefaultIPSW().URL, "https://updates.cdn-apple.com/fixture.ipsw", "https://updates.apple.com/fixture.ipsw", "https://apple.com/fixture.ipsw"} {
		if !appleRestoreURL(address) {
			t.Errorf("rejected official Apple restore URL: %s", address)
		}
	}
	for _, address := range []string{
		"http://updates.cdn-apple.com/fixture.ipsw",
		"https://user:password@updates.cdn-apple.com/fixture.ipsw",
		"https://updates.cdn-apple.com.attacker.invalid/fixture.ipsw",
		"https://evilcdn-apple.com/fixture.ipsw",
		"https://evilapple.com/fixture.ipsw",
		"https://example.com/updates.cdn-apple.com/fixture.ipsw",
		"file:///tmp/fixture.ipsw",
		"https://%zz/fixture.ipsw",
	} {
		if appleRestoreURL(address) {
			t.Errorf("accepted non-Apple or unsafe restore URL: %s", address)
		}
	}
}
