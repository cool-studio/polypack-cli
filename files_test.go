package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeFiles is a Files Pack `acme-cli` on a fake registry: reservations,
// PUTs to a separate "storage" server (as presigned URLs are), completion,
// publishing, deleting from a draft, index.json, SHA256SUMS and its
// signature, and downloads that redirect to storage.
type fakeFiles struct {
	t        *testing.T
	registry *httptest.Server
	storage  *httptest.Server
	key      *ecdsa.PrivateKey
	sign     bool
	tamper   bool

	mu        sync.Mutex
	draft     map[string][]byte // name => bytes, completed
	pending   map[string]string // upload id => name
	objects   map[string][]byte // upload id => bytes PUT
	published map[string]map[string][]byte
	puts      int
	failPut   bool
}

const filesToken = "polypack_files_token"

func newFakeFiles(t *testing.T) *fakeFiles {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFiles{t: t, key: key, sign: true, draft: map[string][]byte{}, pending: map[string]string{}, objects: map[string][]byte{}, published: map[string]map[string][]byte{}}

	storage := http.NewServeMux()
	storage.HandleFunc("PUT /upload/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("a Token was sent to storage")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.puts++
		if f.failPut {
			f.failPut = false
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		f.objects[r.PathValue("id")] = body.Bytes()
	})
	storage.HandleFunc("GET /object/{version}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("a Token was sent to storage")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		data := f.published[r.PathValue("version")][r.PathValue("name")]
		if f.tamper {
			data = append(append([]byte{}, data...), 'x')
		}
		_, _ = w.Write(data)
	})
	f.storage = httptest.NewServer(storage)
	t.Cleanup(f.storage.Close)

	mux := http.NewServeMux()
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+filesToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"You need a PolyPack Token."}`))
			return false
		}
		return true
	}
	mux.HandleFunc("POST /files/acme-cli/{version}/uploads", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var body struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.published[r.PathValue("version")] != nil {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"acme-cli 1.0.0 is published, so its Files cannot change."}`))
			return
		}
		if _, ok := f.draft[body.Name]; ok {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"1.0.0 already has a File named ` + body.Name + `."}`))
			return
		}
		for _, name := range f.pending {
			if name == body.Name {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"An upload of ` + body.Name + ` is already in progress."}`))
				return
			}
		}
		id := fmt.Sprintf("u%d", len(f.pending)+len(f.draft)+f.puts)
		f.pending[id] = body.Name
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "method": "PUT", "url": f.storage.URL + "/upload/" + id,
			"headers":      map[string]string{"Content-Length": fmt.Sprint(body.Size)},
			"complete_url": f.registry.URL + "/files/acme-cli/" + r.PathValue("version") + "/uploads/" + id + "/complete",
		})
	})
	mux.HandleFunc("POST /files/acme-cli/{version}/uploads/{id}/complete", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		name := f.pending[id]
		f.draft[name] = f.objects[id]
		delete(f.pending, id)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("DELETE /files/acme-cli/{version}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.published[r.PathValue("version")] != nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
		delete(f.draft, r.PathValue("name"))
		for id, name := range f.pending {
			if name == r.PathValue("name") {
				delete(f.pending, id)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /files/acme-cli/{version}/publish", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.published[r.PathValue("version")] = f.draft
		f.draft = map[string][]byte{}
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /files/acme-cli/index.json", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		type file struct {
			Name   string `json:"name"`
			Size   int    `json:"size"`
			SHA256 string `json:"sha256"`
		}
		var versions []map[string]any
		for v, files := range f.published {
			var list []file
			for name, data := range files {
				sum := sha256.Sum256(data)
				list = append(list, file{name, len(data), hex.EncodeToString(sum[:])})
			}
			versions = append(versions, map[string]any{"version": v, "files": list})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "acme-cli", "channels": map[string]string{"latest": "1.0.0", "beta": "1.1.0-beta.1"}, "versions": versions})
	})
	mux.HandleFunc("GET /files/acme-cli/{version}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		version, name := r.PathValue("version"), r.PathValue("name")
		switch name {
		case "SHA256SUMS":
			_, _ = w.Write(f.sums(version))
		case "SHA256SUMS.sig":
			if !f.sign {
				http.NotFound(w, r)
				return
			}
			digest := sha256.Sum256(f.sums(version))
			sig, _ := ecdsa.SignASN1(rand.Reader, f.key, digest[:])
			w.Header().Set("X-PolyPack-Key-Id", "SHA256:test")
			_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(sig) + "\n"))
		default:
			http.Redirect(w, r, f.storage.URL+"/object/"+version+"/"+name, http.StatusFound)
		}
	})
	mux.HandleFunc("GET /-/polypack/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		der, _ := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{"keyid": "SHA256:test", "key": base64.StdEncoding.EncodeToString(der), "expires": nil}}})
	})
	f.registry = httptest.NewServer(mux)
	t.Cleanup(f.registry.Close)

	dir := t.TempDir()
	t.Setenv("POLYPACK_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("POLYPACK_TOKEN", filesToken)
	return f
}

func (f *fakeFiles) sums(version string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for name := range f.published[version] {
		names = append(names, name)
	}
	sort.Strings(names)
	var out bytes.Buffer
	for _, name := range names {
		sum := sha256.Sum256(f.published[version][name])
		fmt.Fprintf(&out, "%x  %s\n", sum, name)
	}
	return out.Bytes()
}

func writeDist(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runFiles(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(append([]string{"files"}, args...), &out, &errOut)
	return code, out.String() + errOut.String()
}

func TestFilesUploadPublishAndDownload(t *testing.T) {
	f := newFakeFiles(t)
	dist := writeDist(t, map[string]string{"acme-linux-amd64": "linux binary", "acme-darwin-arm64": "mac binary"})

	// v1.0.0, as a Git tag names it: the v is dropped; the glob is expanded here.
	code, out := runFiles(t, "upload", "acme-cli@v1.0.0", filepath.Join(dist, "*"), "--registry", f.registry.URL, "--publish")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"Uploaded acme-linux-amd64 (12 B) to acme-cli 1.0.0.", "Uploaded acme-darwin-arm64 (10 B)", "Published acme-cli 1.0.0."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if string(f.published["1.0.0"]["acme-linux-amd64"]) != "linux binary" {
		t.Fatalf("published %v", f.published)
	}

	into := t.TempDir()
	code, out = runFiles(t, "download", "acme-cli", "--registry", f.registry.URL, "--out", into)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(into, "acme-darwin-arm64")); string(data) != "mac binary" {
		t.Errorf("downloaded %q", data)
	}
	if !strings.Contains(out, "Checked against SHA256SUMS, signed by PolyPack (SHA256:test).") {
		t.Errorf("output:\n%s", out)
	}

	// One File, by Version.
	only := t.TempDir()
	if code, out := runFiles(t, "download", "acme-cli@1.0.0", "--file", "acme-linux-amd64", "--registry", f.registry.URL, "--out", only); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if entries, _ := os.ReadDir(only); len(entries) != 1 {
		t.Errorf("downloaded %d files", len(entries))
	}
}

func TestFilesDownloadRefusesWhatDoesNotCheck(t *testing.T) {
	f := newFakeFiles(t)
	f.published["1.0.0"] = map[string][]byte{"a": []byte("one")}

	// A File that is not what SHA256SUMS says is never saved.
	f.tamper = true
	into := t.TempDir()
	code, out := runFiles(t, "download", "acme-cli", "--registry", f.registry.URL, "--out", into)
	if code != 1 || !strings.Contains(out, "SHA256SUMS says") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if entries, _ := os.ReadDir(into); len(entries) != 0 {
		t.Errorf("left %v behind", entries)
	}
	f.tamper = false

	// A signature by another key is refused before anything is downloaded.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	real := f.key
	f.key = other
	der, _ := x509.MarshalPKIXPublicKey(&real.PublicKey)
	keys := []signingKey{{KeyID: "SHA256:test", Key: base64.StdEncoding.EncodeToString(der)}}
	digest := sha256.Sum256([]byte("x"))
	sig, _ := ecdsa.SignASN1(rand.Reader, other, digest[:])
	if err := verifySums([]byte("x"), base64.StdEncoding.EncodeToString(sig), "SHA256:test", keys); err == nil {
		t.Error("another key's signature verified")
	}
	f.key = real

	// Not signed: checked against SHA256SUMS alone, and said so.
	f.sign = false
	code, out = runFiles(t, "download", "acme-cli", "--registry", f.registry.URL, "--out", t.TempDir())
	if code != 0 || !strings.Contains(out, "does not sign") {
		t.Fatalf("exit %d:\n%s", code, out)
	}

	// A channel the Token may not read, and one that resolves.
	code, out = runFiles(t, "download", "acme-cli@nightly", "--registry", f.registry.URL)
	if code != 1 || !strings.Contains(out, "no Version on the nightly channel") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestFilesUploadRetriesARerunAndAFailedPut(t *testing.T) {
	f := newFakeFiles(t)
	dist := writeDist(t, map[string]string{"a": "new bytes"})
	f.draft["a"] = []byte("old bytes") // a rerun of a job that uploaded it already
	f.failPut = true                   // and the first PUT fails

	code, out := runFiles(t, "upload", "acme-cli@1.0.0", filepath.Join(dist, "a"), "--registry", f.registry.URL)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if string(f.draft["a"]) != "new bytes" || !strings.Contains(out, "is a draft: publish it with --publish") {
		t.Errorf("draft %q:\n%s", f.draft["a"], out)
	}
}

func TestFilesUploadNeverTouchesAPublishedVersion(t *testing.T) {
	f := newFakeFiles(t)
	f.published["1.0.0"] = map[string][]byte{"a": []byte("one")}
	dist := writeDist(t, map[string]string{"a": "two"})

	code, out := runFiles(t, "upload", "acme-cli@1.0.0", filepath.Join(dist, "a"), "--registry", f.registry.URL)
	if code != 1 || !strings.Contains(out, "is published, so its Files cannot change") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if string(f.published["1.0.0"]["a"]) != "one" {
		t.Error("a published File changed")
	}
}

func TestFilesUploadArguments(t *testing.T) {
	newFakeFiles(t)
	dir := t.TempDir()
	for args, want := range map[string]string{
		"acme-cli dist/a": "names no Version",
		"acme-cli@1.0.0":  "usage: polypack files upload",
		"acme-cli@1.0.0 " + filepath.Join(dir, "*.none"): "no file matches",
		"acme-cli@1.0.0 " + dir:                          "is a directory",
	} {
		code, out := runFiles(t, append([]string{"upload"}, append(strings.Fields(args), "--registry", "http://127.0.0.1:1")...)...)
		if code == 0 || !strings.Contains(out, want) {
			t.Errorf("%s: exit %d:\n%s", args, code, out)
		}
	}
	t.Setenv("POLYPACK_TOKEN", "")
	dist := writeDist(t, map[string]string{"a": "x"})
	if code, out := runFiles(t, "upload", "acme-cli@1.0.0", filepath.Join(dist, "a"), "--registry", "http://127.0.0.1:1"); code == 0 || !strings.Contains(out, "no Token") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestCIForAFilesPackExportsTheToken(t *testing.T) {
	server, audience := fakeActions(t, false)
	env := filepath.Join(t.TempDir(), "github_env")
	t.Setenv("GITHUB_ENV", env)

	var out, errOut bytes.Buffer
	if code := run([]string{"ci", "acme-cli", "--files", "--registry", server.URL + "/npm/"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if *audience != "files:127.0.0.1" {
		t.Errorf("audience %q", *audience)
	}
	data, _ := os.ReadFile(env)
	if string(data) != "POLYPACK_TOKEN=polypack_publisher\n" {
		t.Errorf("GITHUB_ENV = %q", data)
	}
	if !strings.Contains(out.String(), "::add-mask::polypack_publisher") || !strings.Contains(out.String(), "polypack files upload can publish acme-cli") {
		t.Errorf("stdout:\n%s", out.String())
	}
}
