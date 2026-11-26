package main

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

// Every pinned key's id is the SHA-256 of its DER, unpadded base64, as
// PolyPack computes it (SigningKey::idOf), and it is an ECDSA P-256 key.
func TestPinnedKeysMatchTheirIDs(t *testing.T) {
	if len(pinnedKeys) == 0 {
		t.Fatal("no pinned keys")
	}
	for id, key := range pinnedKeys {
		der, err := base64.StdEncoding.DecodeString(key)
		if err != nil {
			t.Fatalf("%s: not base64: %v", id, err)
		}
		sum := sha256.Sum256(der)
		if want := "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "="); want != id {
			t.Errorf("pinned under %s, but the key's id is %s", id, want)
		}
		parsed, err := x509.ParsePKIXPublicKey(der)
		if public, ok := parsed.(*ecdsa.PublicKey); err != nil || !ok || public.Curve.Params().Name != "P-256" {
			t.Errorf("%s is not an ECDSA P-256 public key", id)
		}
	}
}

// install.sh carries the same keys, as PEM, for POLYPACK_SOURCE=polypack.
func TestInstallScriptPinsTheSameKeys(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for id, key := range pinnedKeys {
		var wrapped strings.Builder
		for i := 0; i < len(key); i += 64 {
			wrapped.WriteString(key[i:min(i+64, len(key))] + "\n")
		}
		if !strings.Contains(script, "-----BEGIN PUBLIC KEY-----\n"+wrapped.String()+"-----END PUBLIC KEY-----") || !strings.Contains(script, id) {
			t.Errorf("install.sh does not pin %s as pinned.go does", id)
		}
	}
}
