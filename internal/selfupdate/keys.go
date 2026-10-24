package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
)

// trustedKeys are the public halves of the keys that sign releases (ed25519, in
// base64): the one in use and, while the key changes, the next, so a release
// signed with either is taken. `go run ./tools/sign keygen` makes a pair: its
// public half goes here, its private half into the release workflow's
// BRIDGE_UPDATE_SIGNING_KEY secret and nowhere else. Until a key is here, this
// build never updates itself.
var trustedKeys = []string{}

// TrustedKeys are the keys this build takes a release's signature from.
func TrustedKeys() []ed25519.PublicKey {
	var keys []ed25519.PublicKey
	for _, k := range trustedKeys {
		raw, err := base64.StdEncoding.DecodeString(k)
		if err == nil && len(raw) == ed25519.PublicKeySize {
			keys = append(keys, ed25519.PublicKey(raw))
		}
	}
	return keys
}
