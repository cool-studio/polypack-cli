package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A fake PolyPack: pending, then slow_down, then approved; and the
// registry's whoami and logout.
func fakePolyPack(t *testing.T) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var polls atomic.Int32
	revoked := &atomic.Bool{}
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cli/v1/device", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_name"] != "test box" {
			t.Errorf("client_name = %q", body["client_name"])
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "BCDF-GHJK", "verification_uri_complete": server.URL + "/app/device?code=BCDF-GHJK", "expires_in": 60, "interval": 0})
	})
	mux.HandleFunc("POST /cli/v1/device/token", func(w http.ResponseWriter, r *http.Request) {
		switch polls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"slow_down"}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "polypack_secret", "expires_at": "2027-02-18T12:00:00Z",
				"user":     map[string]string{"email": "ada@example.com"},
				"team":     map[string]string{"slug": "hopper", "name": "Hopper Digital"},
				"registry": map[string]any{"npm": server.URL + "/npm/", "npm_scopes": []string{"@hopper"}, "composer": server.URL + "/composer", "oci_host": nil},
			})
		}
	})
	mux.HandleFunc("GET /npm/-/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer polypack_secret" || revoked.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"username":"ada@example.com"}`))
	})
	mux.HandleFunc("DELETE /npm/-/user/token/{token}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("token") != "polypack_secret" || r.Header.Get("Authorization") != "Bearer polypack_secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		revoked.Store(true)
		_, _ = w.Write([]byte(`{}`))
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, revoked
}

func TestLoginSetupWhoamiLogout(t *testing.T) {
	server, revoked := fakePolyPack(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("POLYPACK_CONFIG", filepath.Join(home, "polypack.json"))
	t.Setenv("npm_config_userconfig", filepath.Join(home, ".npmrc"))
	t.Setenv("COMPOSER_HOME", filepath.Join(home, "composer"))
	t.Setenv("POLYPACK_NO_BROWSER", "1")
	t.Setenv("PATH", "") // no docker, no composer
	slowDownStep = time.Millisecond

	var out, errOut bytes.Buffer
	if code := run([]string{"login", "--host", server.URL, "--name", "test box"}, &out, &errOut); code != 0 {
		t.Fatalf("login exited %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "BCDF-GHJK") || !strings.Contains(out.String(), "Signed in to Hopper Digital as ada@example.com") {
		t.Fatalf("login said:\n%s", out.String())
	}

	npmrc, _ := os.ReadFile(filepath.Join(home, ".npmrc"))
	if !strings.Contains(string(npmrc), "@hopper:registry="+server.URL+"/npm/") || !strings.Contains(string(npmrc), ":_authToken=polypack_secret") {
		t.Fatalf(".npmrc:\n%s", npmrc)
	}
	auth := readJSON(t, filepath.Join(home, "composer", "auth.json"))
	if auth["bearer"].(map[string]any)[strings.TrimPrefix(server.URL, "http://")] != "polypack_secret" {
		t.Fatalf("auth.json = %v", auth)
	}
	if info, _ := os.Stat(filepath.Join(home, "polypack.json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v", info.Mode().Perm())
	}

	out.Reset()
	if code := run([]string{"whoami"}, &out, &errOut); code != 0 || out.String() != "hopper: ada@example.com (Hopper Digital)\n" {
		t.Fatalf("whoami (%d): %q", code, out.String())
	}

	out.Reset()
	if code := run([]string{"logout"}, &out, &errOut); code != 0 {
		t.Fatalf("logout exited %d: %s", code, errOut.String())
	}
	if !revoked.Load() {
		t.Fatal("the Token was not revoked")
	}
	npmrc, _ = os.ReadFile(filepath.Join(home, ".npmrc"))
	if strings.Contains(string(npmrc), "polypack") {
		t.Fatalf(".npmrc still has the block:\n%s", npmrc)
	}
	if _, ok := readJSON(t, filepath.Join(home, "composer", "auth.json"))["bearer"]; ok {
		t.Fatal("auth.json still has the bearer")
	}
	config, _ := loadConfig()
	if len(config.Teams) != 0 {
		t.Fatalf("config still has %v", config.Teams)
	}
}

func TestSetupOnlyWhatIsNamed(t *testing.T) {
	server, _ := fakePolyPack(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("POLYPACK_CONFIG", filepath.Join(home, "polypack.json"))
	t.Setenv("npm_config_userconfig", filepath.Join(home, ".npmrc"))
	t.Setenv("COMPOSER_HOME", filepath.Join(home, "composer"))
	t.Setenv("POLYPACK_NO_BROWSER", "1")
	t.Setenv("PATH", "")
	slowDownStep = time.Millisecond

	var out, errOut bytes.Buffer
	if code := run([]string{"login", "--host", server.URL, "--name", "test box", "--no-setup"}, &out, &errOut); code != 0 {
		t.Fatalf("login exited %d: %s", code, errOut.String())
	}
	npmrc := filepath.Join(home, ".npmrc")
	authJSON := filepath.Join(home, "composer", "auth.json")

	// --composer alone: auth.json, and no .npmrc.
	if code := run([]string{"setup", "--composer"}, &out, &errOut); code != 0 {
		t.Fatalf("setup --composer exited %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(authJSON); err != nil {
		t.Fatalf("setup --composer wrote no auth.json: %v", err)
	}
	if _, err := os.Stat(npmrc); !os.IsNotExist(err) {
		t.Fatal("setup --composer wrote .npmrc")
	}

	// No flags: all of them.
	if code := run([]string{"setup"}, &out, &errOut); code != 0 {
		t.Fatalf("setup exited %d: %s", code, errOut.String())
	}
	if content, _ := os.ReadFile(npmrc); !strings.Contains(string(content), ":_authToken=polypack_secret") {
		t.Fatalf("setup with no flags left .npmrc:\n%s", content)
	}

	// The old opt-out flags are gone.
	if code := run([]string{"setup", "--no-docker"}, &out, &errOut); code == 0 {
		t.Fatal("setup --no-docker was accepted")
	}
}

func TestLoginReportsADenial(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cli/v1/device", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"BCDF-GHJK","verification_uri_complete":"x","expires_in":60,"interval":0}`))
	})
	mux.HandleFunc("POST /cli/v1/device/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("POLYPACK_CONFIG", filepath.Join(t.TempDir(), "c.json"))
	t.Setenv("POLYPACK_NO_BROWSER", "1")

	var out, errOut bytes.Buffer
	if code := run([]string{"login", "--host", server.URL}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "denied") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestUnknownCommandAndVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if run([]string{"nope"}, &out, &errOut) != 2 {
		t.Fatal("unknown command did not exit 2")
	}
	out.Reset()
	if run([]string{"version"}, &out, &errOut) != 0 || out.String() != "polypack dev\n" {
		t.Fatalf("version: %q", out.String())
	}
}
