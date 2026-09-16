package relmanifest

import (
	"log/slog"
	"strings"
)

// BakedKeys holds the minisign public keys this build accepts, as one or more
// base64 key lines separated by commas or whitespace.
//
// **Stamped at build time, like internal/version, and empty by default.**
// RELEASE_MANIFEST.md # Signing puts the pubkey in the binary: the box has no
// key directory, no CA and no transparency log, so the build is the only place
// trust can be established. Stamp it with:
//
//	go build -ldflags "-X github.com/onmoose/moose/internal/hostagent/relmanifest.BakedKeys=<key>[,<key>]"
//
// **There is deliberately no environment-variable override.** The point of
// baking the key in is that changing which releases a box will accept requires
// a new binary, which arrives through apt and is itself signed. An env override
// would move that decision to whoever can edit a unit file, and "the box only
// runs software we signed" would then rest on file permissions.
//
// Empty is the state of every build today, because no release-signing key
// exists yet (# Signing defers key custody until there is a release to sign).
// Such a build refuses every manifest and does not poll at all — inert, not
// credulous.
var BakedKeys = ""

// VerifierFromBakedKeys builds the verifier this binary trusts. Unparseable
// entries are logged and skipped rather than fatal: a box whose key list is
// half-broken should still boot and still refuse manifests, because the brain
// is how anyone finds out what is wrong.
func VerifierFromBakedKeys() *Verifier {
	var keys []PublicKey
	for _, field := range strings.FieldsFunc(BakedKeys, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
	}) {
		pk, err := ParsePublicKey(field)
		if err != nil {
			slog.Error("release manifest: baked public key unusable, skipping", "err", err)
			continue
		}
		keys = append(keys, pk)
	}
	return NewVerifier(keys...)
}
