package image

import "aead.dev/minisign"

// Official catalogs are signed locally with the active Minisign key.
// The standby is trusted for rotation by the same Barn release.
var productionManifestKeys = mustPublicKeys(
	// barn-catalog-active-20261126, key ID B848292042883667
	"RWRnNohCIClIuE9mVPBzP0kn8cfo63nSJguTCsflMO73bjoFFkQOUGYf",
	// barn-catalog-standby-20261126, key ID 3182E82817D2B25A
	"RWRastIXKOiCMc1UxUx8h2/yT8VnKWtCJ2n13AbDclZtj8c0Hc2UYrXt",
)

func mustPublicKeys(encoded ...string) []minisign.PublicKey {
	keys := make([]minisign.PublicKey, 0, len(encoded))
	for _, text := range encoded {
		var key minisign.PublicKey
		if err := key.UnmarshalText([]byte(text)); err != nil {
			panic("embedded catalog public key is invalid: " + err.Error())
		}
		keys = append(keys, key)
	}
	return keys
}
