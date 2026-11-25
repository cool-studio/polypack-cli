package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeReleases serves releases/latest/download/… as a redirect to tag, and
// that release's binary and SHA256SUMS. lookups counts the redirects asked for.
type fakeReleases struct {
	server  *httptest.Server
	tag     string
	binary  []byte
	sums    string
	broken  bool
	lookups atomic.Int32
}

func newFakeReleases(t *testing.T, tag string) *fakeReleases {
	t.Helper()
	f := &fakeReleases{tag: tag, binary: []byte("new polypack " + tag)}
	sum := sha256.Sum256(f.binary)
	f.sums = hex.EncodeToString(sum[:]) + "  " + assetName() + "\n" + strings.Repeat("0", 64) + "  polypack-plan9-amd64\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/latest/download/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		f.lookups.Add(1)
		if f.broken {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/cool-studio/polypack-cli/releases/download/"+f.tag+"/SHA256SUMS", http.StatusFound)
	})
	mux.HandleFunc("GET /download/{tag}/{file}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("tag") != f.tag {
			http.NotFound(w, r)
			return
		}
		switch r.PathValue("file") {
		case "SHA256SUMS":
			_, _ = io.WriteString(w, f.sums)
		case assetName():
			_, _ = w.Write(f.binary)
		default:
			http.NotFound(w, r)
		}
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	t.Setenv("POLYPACK_RELEASES_URL", f.server.URL)
	return f
}

func setVersion(t *testing.T, v string) {
	t.Helper()
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

// A binary to replace, in a temporary directory.
func fakeExecutable(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "polypack")
	if err := os.WriteFile(exe, []byte("old polypack"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := executablePath
	executablePath = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executablePath = old })
	return exe
}

func TestParseVersionAndIsNewer(t *testing.T) {
	for _, v := range []string{"v0.4.0", "v1.0.0", "v10.20.30"} {
		if _, ok := parseVersion(v); !ok {
			t.Errorf("parseVersion(%q) failed", v)
		}
	}
	for _, v := range []string{"dev", "0.4.0", "v0.4", "v0.4.0-rc.1", "v0.04.0", "v0.4.x", ""} {
		if _, ok := parseVersion(v); ok {
			t.Errorf("parseVersion(%q) parsed", v)
		}
	}
	cases := []struct {
		current, candidate string
		newer              bool
	}{
		{"v0.4.0", "v0.5.0", true},
		{"v0.4.0", "v0.4.1", true},
		{"v0.9.0", "v0.10.0", true},
		{"v0.4.0", "v1.0.0", true},
		{"v0.5.0", "v0.5.0", false},
		{"v0.5.0", "v0.4.9", false},
		{"dev", "v0.5.0", false},
		{"v0.4.0", "nightly", false},
		{"v0.4.0", "", false},
	}
	for _, c := range cases {
		if got := isNewer(c.current, c.candidate); got != c.newer {
			t.Errorf("isNewer(%q, %q) = %v", c.current, c.candidate, got)
		}
	}
}

func TestLatestReleaseReadsTheRedirect(t *testing.T) {
	f := newFakeReleases(t, "v0.5.0")
	tag, err := latestRelease(t.Context())
	if err != nil || tag != "v0.5.0" {
		t.Fatalf("latestRelease = %q, %v", tag, err)
	}
	f.broken = true
	if _, err := latestRelease(t.Context()); err == nil {
		t.Fatal("a failing server gave a release")
	}
}

func TestUpdateReplacesTheBinary(t *testing.T) {
	newFakeReleases(t, "v0.5.0")
	setVersion(t, "v0.4.0")
	exe := fakeExecutable(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"update"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "polypack updated: v0.4.0 → v0.5.0") {
		t.Errorf("stdout = %q", stdout.String())
	}
	data, _ := os.ReadFile(exe)
	if string(data) != "new polypack v0.5.0" {
		t.Errorf("binary = %q", data)
	}
	if info, _ := os.Stat(exe); runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", info.Mode())
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".polypack-update-*"))
	if len(leftovers) != 0 {
		t.Errorf("left behind %v", leftovers)
	}
}

func TestUpdateToAChosenVersion(t *testing.T) {
	newFakeReleases(t, "v0.3.0")
	setVersion(t, "v0.4.0")
	exe := fakeExecutable(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"update", "--version", "v0.3.0"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if data, _ := os.ReadFile(exe); string(data) != "new polypack v0.3.0" {
		t.Errorf("binary = %q", data)
	}
	if code := run([]string{"update", "--version", "latest"}, &stdout, &stderr); code != 1 {
		t.Errorf("--version latest: exit %d", code)
	}
}

func TestUpdateRefusesABadChecksum(t *testing.T) {
	for name, sums := range map[string]string{
		"mismatch": strings.Repeat("a", 64) + "  " + assetName() + "\n",
		"missing":  strings.Repeat("a", 64) + "  polypack-plan9-amd64\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeReleases(t, "v0.5.0")
			f.sums = sums
			setVersion(t, "v0.4.0")
			exe := fakeExecutable(t)

			var stdout, stderr bytes.Buffer
			if code := run([]string{"update"}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit %d", code)
			}
			if !strings.Contains(stderr.String(), "checksum mismatch for "+assetName()+" v0.5.0; not updating") {
				t.Errorf("stderr = %q", stderr.String())
			}
			if data, _ := os.ReadFile(exe); string(data) != "old polypack" {
				t.Errorf("binary replaced: %q", data)
			}
		})
	}
}

func TestUpdateWhenAlreadyLatest(t *testing.T) {
	newFakeReleases(t, "v0.5.0")
	setVersion(t, "v0.5.0")
	exe := fakeExecutable(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"update"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "polypack v0.5.0 is the latest version.") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if data, _ := os.ReadFile(exe); string(data) != "old polypack" {
		t.Errorf("binary replaced: %q", data)
	}
}

func TestUpdateRefusesADevBuild(t *testing.T) {
	f := newFakeReleases(t, "v0.5.0")
	setVersion(t, "dev")
	fakeExecutable(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"update"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "built from source") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if f.lookups.Load() != 0 {
		t.Error("a dev build asked for the latest release")
	}
}

// noticeSetup makes stderr a "terminal", with a fresh config directory and
// none of the variables that turn the notice off.
func noticeSetup(t *testing.T, tag string) (*fakeReleases, string) {
	t.Helper()
	f := newFakeReleases(t, tag)
	setVersion(t, "v0.4.0")
	dir := t.TempDir()
	t.Setenv("POLYPACK_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("POLYPACK_NO_UPDATE_CHECK", "")
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	old := stderrIsTerminal
	stderrIsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { stderrIsTerminal = old })
	return f, filepath.Join(dir, "update-check.json")
}

const notice = "A new version of polypack is available: v0.4.0 → v0.5.0\nRun `polypack update` to install it.\n"

func TestNoticeOnceADay(t *testing.T) {
	f, cache := noticeSetup(t, "v0.5.0")

	var stdout, stderr bytes.Buffer
	run([]string{"version"}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), notice) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	var saved updateCheck
	data, _ := os.ReadFile(cache)
	if err := json.Unmarshal(data, &saved); err != nil || saved.Latest != "v0.5.0" {
		t.Fatalf("cache = %s", data)
	}

	// Within the day: the cache answers, with no request.
	stderr.Reset()
	run([]string{"version"}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), notice) || f.lookups.Load() != 1 {
		t.Errorf("second run: %d lookups, stderr %q", f.lookups.Load(), stderr.String())
	}

	// A day on: asked again, and the cache rewritten.
	f.tag = "v0.6.0"
	old := now
	now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	t.Cleanup(func() { now = old })
	stderr.Reset()
	run([]string{"version"}, &stdout, &stderr)
	if f.lookups.Load() != 2 || !strings.Contains(stderr.String(), "v0.4.0 → v0.6.0") {
		t.Errorf("stale cache: %d lookups, stderr %q", f.lookups.Load(), stderr.String())
	}
}

func TestNoNoticeWhenCurrent(t *testing.T) {
	_, _ = noticeSetup(t, "v0.4.0")
	var stdout, stderr bytes.Buffer
	run([]string{"version"}, &stdout, &stderr)
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestNoNoticeWhenTurnedOff(t *testing.T) {
	for name, set := range map[string]func(t *testing.T){
		"POLYPACK_NO_UPDATE_CHECK": func(t *testing.T) { t.Setenv("POLYPACK_NO_UPDATE_CHECK", "1") },
		"CI":                       func(t *testing.T) { t.Setenv("CI", "true") },
		"GITHUB_ACTIONS":           func(t *testing.T) { t.Setenv("GITHUB_ACTIONS", "true") },
		"not a terminal": func(t *testing.T) {
			stderrIsTerminal = func(io.Writer) bool { return false }
		},
		"dev build": func(t *testing.T) { version = "dev" },
	} {
		t.Run(name, func(t *testing.T) {
			f, cache := noticeSetup(t, "v0.5.0")
			set(t)
			var stdout, stderr bytes.Buffer
			run([]string{"version"}, &stdout, &stderr)
			if strings.Contains(stderr.String(), "new version") || f.lookups.Load() != 0 {
				t.Errorf("%d lookups, stderr %q", f.lookups.Load(), stderr.String())
			}
			if _, err := os.Stat(cache); err == nil {
				t.Error("cache written")
			}
		})
	}
}

func TestNoNoticeForCIUpdateOrHelp(t *testing.T) {
	f, _ := noticeSetup(t, "v0.5.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("POLYPACK_ID_TOKEN", "")
	for _, args := range [][]string{{"ci"}, {"help"}, {"nonsense"}} {
		var stdout, stderr bytes.Buffer
		run(args, &stdout, &stderr)
		if strings.Contains(stderr.String(), "new version") {
			t.Errorf("%v: stderr %q", args, stderr.String())
		}
	}
	if f.lookups.Load() != 0 {
		t.Errorf("%d lookups", f.lookups.Load())
	}
}

func TestNoticeFailureIsSilent(t *testing.T) {
	f, cache := noticeSetup(t, "v0.5.0")
	f.broken = true
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr.String())
	}
	// Recorded, so it waits a day before asking again.
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("no cache: %v", err)
	}
	run([]string{"version"}, &stdout, &stderr)
	if f.lookups.Load() != 1 {
		t.Errorf("%d lookups", f.lookups.Load())
	}
}

func TestNoticeNeverWaitsLong(t *testing.T) {
	_, _ = noticeSetup(t, "v0.5.0")
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)
	t.Setenv("POLYPACK_RELEASES_URL", hang.URL)

	start := time.Now()
	var stdout, stderr bytes.Buffer
	run([]string{"version"}, &stdout, &stderr)
	if elapsed := time.Since(start); elapsed > updateCheckLimit+time.Second {
		t.Errorf("waited %v", elapsed)
	}
}
