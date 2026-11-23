package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type fakeProvenance struct {
	key      *ecdsa.PrivateKey
	keyID    string
	tarball  []byte
	served   []byte // what the registry serves as the tarball
	expires  *string
	sign     func(payload []byte) []byte
	envelope []byte
}

func newFakeProvenance(t *testing.T, tarball []byte) *fakeProvenance {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvenance{key: key, keyID: "SHA256:test", tarball: tarball, served: tarball}
	f.sign = func(payload []byte) []byte {
		sum := sha256.Sum256(pae(inTotoPayload, payload))
		sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	return f
}

func (f *fakeProvenance) statement(name string) []byte {
	digest := sha512.Sum512(f.tarball)
	st := map[string]any{
		"_type":         statementType,
		"predicateType": slsaPredicate,
		"subject":       []any{map[string]any{"name": name, "digest": map[string]string{"sha512": hex.EncodeToString(digest[:])}}},
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"externalParameters":   map[string]any{"workflow": map[string]string{"ref": "refs/tags/v1.1.0", "repository": "https://github.com/hopper-digital/ui", "path": ".github/workflows/release.yml"}},
				"resolvedDependencies": []any{map[string]any{"digest": map[string]string{"gitCommit": "a1b2c3d"}}},
			},
			"runDetails": map[string]any{
				"builder":  map[string]string{"id": "https://github.com/actions/runner/github-hosted"},
				"metadata": map[string]string{"invocationId": "https://github.com/hopper-digital/ui/actions/runs/3003/attempts/1", "startedOn": "2026-11-23T10:00:00Z"},
			},
		},
	}
	data, _ := json.Marshal(st)
	return data
}

func (f *fakeProvenance) server(t *testing.T) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /-/polypack/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		der, _ := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"keyid": f.keyID, "key": base64.StdEncoding.EncodeToString(der), "expires": f.expires}}})
	})
	mux.HandleFunc("GET /-/polypack/v1/provenance/@hopper/ui/1.1.0", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer polypack_secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		payload := f.statement("pkg:npm/%40hopper/ui@1.1.0")
		if f.envelope != nil {
			_, _ = w.Write(f.envelope)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"payloadType": inTotoPayload,
			"payload":     base64.StdEncoding.EncodeToString(payload),
			"signatures":  []any{map[string]string{"keyid": f.keyID, "sig": base64.StdEncoding.EncodeToString(f.sign(payload))}},
		})
	})
	mux.HandleFunc("GET /-/polypack/v1/provenance/@hopper/ui/1.0.0", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"@hopper/ui@1.0.0 has no provenance: it was not published through trusted publishing."}`))
	})
	mux.HandleFunc("GET /npm/@hopper%2fui", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"versions":{"1.1.0":{"dist":{"tarball":"` + server.URL + `/npm/@hopper/ui/-/ui-1.1.0.tgz"}}}}`))
	})
	mux.HandleFunc("GET /npm/@hopper/ui/-/ui-1.1.0.tgz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(f.served)
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func signedIn(t *testing.T, server *httptest.Server) {
	t.Helper()
	t.Setenv("POLYPACK_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	config := &Config{Teams: map[string]Team{"hopper": {Name: "Hopper Digital", Token: "polypack_secret", NPM: server.URL + "/npm/"}}}
	if err := config.save(); err != nil {
		t.Fatal(err)
	}
}

func verifyNpm(target string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run([]string{"verify", "npm", target}, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVerifyChecksTheSignatureAndWhatTheRegistryServes(t *testing.T) {
	f := newFakeProvenance(t, []byte("tarball bytes"))
	signedIn(t, f.server(t))

	code, out, errOut := verifyNpm("@hopper/ui@1.1.0")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"Verified @hopper/ui@1.1.0", "https://github.com/hopper-digital/ui", "a1b2c3d (refs/tags/v1.1.0)", ".github/workflows/release.yml", "/actions/runs/3003/attempts/1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestVerifyRefusesWhatDoesNotMatch(t *testing.T) {
	cases := map[string]struct {
		change func(*fakeProvenance)
		target string
		want   string
	}{
		"a changed tarball": {func(f *fakeProvenance) { f.served = []byte("other bytes") }, "@hopper/ui@1.1.0", "not what was published"},
		"another key's signature": {func(f *fakeProvenance) {
			other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			f.sign = func(payload []byte) []byte {
				sum := sha256.Sum256(pae(inTotoPayload, payload))
				sig, _ := ecdsa.SignASN1(rand.Reader, other, sum[:])
				return sig
			}
		}, "@hopper/ui@1.1.0", "signature does not match"},
		"a key retired before it signed": {func(f *fakeProvenance) { retired := "2026-01-01T00:00:00Z"; f.expires = &retired }, "@hopper/ui@1.1.0", "was retired"},
		"no provenance":                  {func(f *fakeProvenance) {}, "@hopper/ui@1.0.0", "not published through trusted publishing"},
		"not a version":                  {func(f *fakeProvenance) {}, "hopper-ui", "is not @scope/name@version"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeProvenance(t, []byte("tarball bytes"))
			c.change(f)
			signedIn(t, f.server(t))

			code, _, errOut := verifyNpm(c.target)
			if code != 1 || !strings.Contains(errOut, c.want) {
				t.Fatalf("exit %d, stderr %q, want %q", code, errOut, c.want)
			}
		})
	}
}
