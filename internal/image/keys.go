package image

import "aead.dev/minisign"

// Production catalog signing keys. The active key signs current catalogs; the
// standby exists so signing can rotate to a pre-trusted key without shipping
// new binaries first. Verification accepts either. The private keys live on
// the repository build host (m0:/data/nginx/keys/barn); signing and rotation
// use tools/catalogsign; operator policy lives at
// https://barn.pgsty.com/docs/reference/images/.
var productionManifestKeys = mustPublicKeys(
	// barn-catalog-active, key ID 4686B39A40F9B562
	"RWRitflAmrOGRpW2vJzySbsKSA2m94Fmp7CRjvOM8Cf7hE/im1xHVz7u",
	// barn-catalog-standby, key ID B3170D11FD89AD9A
	"RWSarYn9EQ0Xs6QhJm9Ha5MXgGQSy61YwZNmjCYgXpGCeFnj+MUmHhmT",
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
