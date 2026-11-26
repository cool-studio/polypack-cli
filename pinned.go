package main

// PolyPack's public signing keys, pinned in the CLI (step 75, ADR-0034 as
// amended): `polypack update` checks a release's SHA256SUMS from PolyPack
// against these, never against whatever keys the host serves, so a
// compromised host cannot vouch for a binary of its own.
//
// Key id → public key (base64 DER), as /-/polypack/v1/keys gives them.
// SHA256:+ROG5… was confirmed out of band on 2026-11-26: Jamie read
// SigningKey::keyId() on the production server, and it matched the key the
// keys endpoint served.
//
// Rotating PolyPack's key (docs/operations.md § Signing key): release a CLI
// that pins the new key, beside the old one, before rotating. A CLI that
// meets a key it does not pin updates from GitHub instead.
var pinnedKeys = map[string]string{
	"SHA256:+ROG5yRGsHpaitw35aQcW0nNM/1YwDautofcDD7YR/A": "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE5vdIn5q3cmdzjHv9dcXlOpiLmMErlI9zWZRHhXUADQbnCpV2GOOwGPj3laRmIALF/Df7Oa2mAShg9JE94Q4wKg==",
}
