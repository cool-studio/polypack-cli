package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeActions serves GitHub's ID token request and PolyPack's exchange.
func fakeActions(t *testing.T, refuse bool) (*httptest.Server, *string) {
	t.Helper()
	var audience string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer request-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		audience = r.URL.Query().Get("audience")
		_, _ = w.Write([]byte(`{"value":"github-id-token"}`))
	})
	exchange := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer github-id-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if refuse {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"No trusted publisher of @hopper/ui matches this workflow."}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"polypack_publisher","expires_at":"2026-11-23T10:15:00+00:00"}`))
	}
	mux.HandleFunc("POST /-/npm/v1/oidc/token/exchange/package/{name}", exchange)
	mux.HandleFunc("POST /-/oidc/token/exchange/pack/{name...}", exchange)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", server.URL+"/token?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "request-token")
	t.Setenv("POLYPACK_ID_TOKEN", "")
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("PATH", "") // no docker, no helm
	return server, &audience
}

func TestCIExchangesAndSetsUpNpm(t *testing.T) {
	server, audience := fakeActions(t, false)
	npmrc := filepath.Join(t.TempDir(), ".npmrc")
	t.Setenv("npm_config_userconfig", npmrc)
	registry := server.URL + "/npm/"

	var out, errOut bytes.Buffer
	if code := run([]string{"ci", "@hopper/ui", "--registry", registry}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if *audience != "npm:127.0.0.1" {
		t.Errorf("audience %q", *audience)
	}
	if !strings.Contains(out.String(), "::add-mask::polypack_publisher\n") {
		t.Errorf("the Token is not masked:\n%s", out.String())
	}
	content, _ := os.ReadFile(npmrc)
	for _, want := range []string{"@hopper:registry=" + registry, npmAuthKey(registry) + ":_authToken=polypack_publisher"} {
		if !strings.Contains(string(content), want) {
			t.Errorf(".npmrc lacks %q:\n%s", want, content)
		}
	}
}

func TestCIFailsWithPolyPacksReason(t *testing.T) {
	server, _ := fakeActions(t, true)
	t.Setenv("npm_config_userconfig", filepath.Join(t.TempDir(), ".npmrc"))

	var out, errOut bytes.Buffer
	code := run([]string{"ci", "@hopper/ui", "--registry", server.URL + "/npm/"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "No trusted publisher of @hopper/ui matches this workflow.") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestCIForAnImageWithoutDockerOrHelm(t *testing.T) {
	server, audience := fakeActions(t, false)

	var out, errOut bytes.Buffer
	if code := run([]string{"ci", "hopper/api", "--oci", server.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if *audience != "oci:127.0.0.1" {
		t.Errorf("audience %q", *audience)
	}
}

func TestCIOutsideGitHubActions(t *testing.T) {
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
	t.Setenv("POLYPACK_ID_TOKEN", "")

	var out, errOut bytes.Buffer
	code := run([]string{"ci", "@hopper/ui", "--team", "hopper"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "id-token: write") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}
