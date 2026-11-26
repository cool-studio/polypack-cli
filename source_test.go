package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakePolyPackCLI is the CLI's Files Pack on a fake PolyPack (step 75):
// index.json with its channels, and each release's binary, SHA256SUMS and
// its signature by a key the test pins in place of PolyPack's.
type fakePolyPackCLI struct {
	server   *httptest.Server
	key      *ecdsa.PrivateKey
	keyID    string
	channels map[string]string
	signer   *ecdsa.PrivateKey // signs SHA256SUMS; another key than the pinned one forges
	signedAs string            // X-PolyPack-Key-Id
	unsigned bool
	tamper   bool
	broken   bool         // index.json answers 500
	sums     string       // served as SHA256SUMS (and signed) in place of the real ones
	served   atomic.Int32 // binaries served
	lookups  atomic.Int32 // index.json requests
}

func newFakePolyPackCLI(t *testing.T, channels map[string]string) *fakePolyPackCLI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	sum := sha256.Sum256(der)
	f := &fakePolyPackCLI{key: key, keyID: "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "="), channels: channels, signer: key}
	f.signedAs = f.keyID

	old := pinnedKeys
	pinnedKeys = map[string]string{f.keyID: base64.StdEncoding.EncodeToString(der)}
	t.Cleanup(func() { pinnedKeys = old })

	binary := func(version string) []byte { return []byte("polypack from PolyPack v" + version) }
	sums := func(version string) []byte {
		if f.sums != "" {
			return []byte(f.sums)
		}
		b := binary(version)
		if f.tamper {
			b = []byte("something else")
		}
		s := sha256.Sum256(b)
		return []byte(fmt.Sprintf("%x  %s\n", s, assetName()))
	}

	// Only the Versions its channels name exist, as on the real PolyPack.
	has := func(w http.ResponseWriter, r *http.Request) bool {
		for _, v := range f.channels {
			if v == r.PathValue("version") {
				return true
			}
		}
		http.NotFound(w, r)
		return false
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /index.json", func(w http.ResponseWriter, r *http.Request) {
		f.lookups.Add(1)
		if f.broken {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "polypack-cli", "channels": f.channels, "versions": []any{}})
	})
	mux.HandleFunc("GET /{version}/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		if !has(w, r) {
			return
		}
		_, _ = w.Write(sums(r.PathValue("version")))
	})
	mux.HandleFunc("GET /{version}/SHA256SUMS.sig", func(w http.ResponseWriter, r *http.Request) {
		if f.unsigned || !has(w, r) {
			if f.unsigned {
				http.NotFound(w, r)
			}
			return
		}
		digest := sha256.Sum256(sums(r.PathValue("version")))
		sig, _ := ecdsa.SignASN1(rand.Reader, f.signer, digest[:])
		w.Header().Set("X-PolyPack-Key-Id", f.signedAs)
		_, _ = io.WriteString(w, base64.StdEncoding.EncodeToString(sig)+"\n")
	})
	mux.HandleFunc("GET /{version}/{file}", func(w http.ResponseWriter, r *http.Request) {
		if !has(w, r) {
			return
		}
		if r.PathValue("file") != assetName() {
			http.NotFound(w, r)
			return
		}
		f.served.Add(1)
		_, _ = w.Write(binary(r.PathValue("version")))
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	t.Setenv("POLYPACK_CLI_FILES_URL", f.server.URL)
	return f
}

func runUpdate(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"update"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func installed(t *testing.T, exe string) string {
	t.Helper()
	data, _ := os.ReadFile(exe)
	return string(data)
}

func updateSetup(t *testing.T, channels map[string]string) (*fakePolyPackCLI, string) {
	t.Helper()
	polypack := newFakePolyPackCLI(t, channels)
	setVersion(t, "v0.4.0")
	t.Setenv("POLYPACK_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	return polypack, fakeExecutable(t)
}

func TestUpdateFromPolyPackCheckedAgainstThePinnedKey(t *testing.T) {
	polypack, exe := updateSetup(t, map[string]string{"latest": "0.5.0"})

	code, stdout, stderr := runUpdate(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if installed(t, exe) != "polypack from PolyPack v0.5.0" || !strings.Contains(stdout, "polypack updated: v0.4.0 → v0.5.0") {
		t.Errorf("installed %q:\n%s%s", installed(t, exe), stdout, stderr)
	}
	if polypack.served.Load() != 1 {
		t.Errorf("PolyPack served %d binaries", polypack.served.Load())
	}
}

// No other source exists (ADR-0037): whatever PolyPack cannot vouch for is
// refused, saying why, and nothing is replaced.
func TestUpdateRefusesWhenPolyPackCannotVouch(t *testing.T) {
	for name, c := range map[string]struct {
		setUp func(f *fakePolyPackCLI)
		says  string
	}{
		"a key this CLI does not pin": {func(f *fakePolyPackCLI) {
			other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			f.signer, f.signedAs = other, "SHA256:rotated"
		}, "PolyPack signs with SHA256:rotated, which this polypack does not know. Reinstall it: " + reinstall},
		"not signed":            {func(f *fakePolyPackCLI) { f.unsigned = true }, "PolyPack does not sign v0.5.0"},
		"PolyPack is down":      {func(f *fakePolyPackCLI) { f.server.Close() }, "PolyPack could not be reached"},
		"no stable release yet": {func(f *fakePolyPackCLI) { f.channels = map[string]string{} }, "PolyPack has no release on the latest channel"},
	} {
		t.Run(name, func(t *testing.T) {
			polypack, exe := updateSetup(t, map[string]string{"latest": "0.5.0"})
			c.setUp(polypack)

			code, _, stderr := runUpdate(t)
			if code != 1 || !strings.Contains(stderr, c.says) {
				t.Fatalf("exit %d:\n%s", code, stderr)
			}
			if installed(t, exe) != "old polypack" {
				t.Errorf("replaced with %q", installed(t, exe))
			}
		})
	}
}

func TestUpdateRefusesWhatThePinnedKeyDoesNotVouchFor(t *testing.T) {
	for name, setUp := range map[string]func(f *fakePolyPackCLI){
		// Signed by another key, claiming to be the pinned one: a forgery.
		"a bad signature": func(f *fakePolyPackCLI) {
			f.signer, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		},
		// SHA256SUMS signed, but the binary is not what it says.
		"a binary that is not in SHA256SUMS": func(f *fakePolyPackCLI) { f.tamper = true },
	} {
		t.Run(name, func(t *testing.T) {
			polypack, exe := updateSetup(t, map[string]string{"latest": "0.5.0"})
			setUp(polypack)

			code, _, stderr := runUpdate(t)
			if code != 1 || !strings.Contains(stderr, "not updating") {
				t.Fatalf("exit %d:\n%s", code, stderr)
			}
			if installed(t, exe) != "old polypack" {
				t.Errorf("replaced with %q", installed(t, exe))
			}
		})
	}
}

func TestUpdateFollowsAChannelAndTheNoticeRemembersIt(t *testing.T) {
	_, exe := updateSetup(t, map[string]string{"latest": "0.5.0", "beta": "0.6.0-beta.1"})

	code, stdout, stderr := runUpdate(t, "--channel", "beta")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if installed(t, exe) != "polypack from PolyPack v0.6.0-beta.1" || !strings.Contains(stdout, "v0.4.0 → v0.6.0-beta.1") {
		t.Fatalf("installed %q:\n%s", installed(t, exe), stdout)
	}
	cached, _ := loadUpdateCheck()
	if cached.Channel != "beta" {
		t.Errorf("channel %q not remembered", cached.Channel)
	}

	// The notice follows the beta channel from then on.
	setVersion(t, "v0.6.0-beta.1")
	t.Setenv("POLYPACK_NO_UPDATE_CHECK", "")
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	oldTerminal := stderrIsTerminal
	stderrIsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { stderrIsTerminal = oldTerminal })

	newFakePolyPackCLI(t, map[string]string{"latest": "0.5.0", "beta": "0.6.0-beta.2"})
	var out, errOut bytes.Buffer
	run([]string{"version"}, &out, &errOut)
	if !strings.Contains(errOut.String(), "v0.6.0-beta.1 → v0.6.0-beta.2") {
		t.Errorf("notice:\n%s", errOut.String())
	}

	// And back to stable.
	if code, stdout, _ := runUpdate(t, "--channel", "latest"); code != 0 || !strings.Contains(stdout, "is the latest version") {
		t.Errorf("exit %d:\n%s", code, stdout)
	}
	if cached, _ := loadUpdateCheck(); cached.Channel != "" {
		t.Errorf("channel %q, want latest", cached.Channel)
	}
}
