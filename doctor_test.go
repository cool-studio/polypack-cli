package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Secrets of the right shape; the fake host decides what each one is.
var (
	goodSecret    = "polypack_" + strings.Repeat("a", 34) + "GOOD"
	expiredSecret = "polypack_" + strings.Repeat("b", 34) + "OLD1"
	officeSecret  = "polypack_" + strings.Repeat("c", 34) + "OFFC"
	soonSecret    = "polypack_" + strings.Repeat("d", 34) + "SOON"
)

// fakeHosts answers GET /-/polypack/v1/token for any *.example.test host,
// and every request from doctorClient reaches it, whatever the host.
func fakeHosts(t *testing.T) *httptest.Server {
	t.Helper()
	until := time.Now().Add(90 * 24 * time.Hour).UTC().Format(time.RFC3339)
	soon := time.Now().Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/-/polypack/v1/token" {
			http.NotFound(w, r)
			return
		}
		slug, _, _ := strings.Cut(r.Host, ".")
		if slug == "gone" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("Not found."))
			return
		}
		oci := strings.Contains(r.Host, ".cr.")
		w.Header().Set("Content-Type", "application/json")
		secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch secret {
		case "":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":"none","message":"No Token was sent."}`))
		case goodSecret, soonSecret:
			expires := until
			if secret == soonSecret {
				expires = soon
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "active", "name": "CLI on laptop", "kind": "member", "username": "ada@example.com",
				"abilities": []string{"read"}, "expires_at": expires, "packs": nil, "team": slug,
			})
		case expiredSecret:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":"expired","message":"This Token expired on 1 Nov 2026."}`))
		case officeSecret:
			w.WriteHeader(http.StatusForbidden)
			if oci {
				_, _ = w.Write([]byte(`{"errors":[{"code":"DENIED","message":"This Token is not allowed from 198.51.100.1."}]}`))
			} else {
				_, _ = w.Write([]byte(`{"error":"This Token is not allowed from 198.51.100.1."}`))
			}
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":"unknown","message":"PolyPack knows no such Token."}`))
		}
	}))
	t.Cleanup(server.Close)

	address := server.Listener.Addr().String()
	old := doctorClient
	doctorClient = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "down.") {
				return nil, errors.New("connection refused")
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}}
	t.Cleanup(func() { doctorClient = old })
	return server
}

type doctorHome struct {
	dir, npmrc, composerAuth, dockerConfig, project string
}

// doctorSetup gives the doctor a fresh home and an empty project directory
// to run in, with nothing on PATH.
func doctorSetup(t *testing.T) doctorHome {
	t.Helper()
	fakeHosts(t)
	dir := t.TempDir()
	h := doctorHome{
		dir:          dir,
		npmrc:        filepath.Join(dir, ".npmrc"),
		composerAuth: filepath.Join(dir, "composer", "auth.json"),
		dockerConfig: filepath.Join(dir, "docker", "config.json"),
		project:      filepath.Join(dir, "project"),
	}
	t.Setenv("HOME", dir)
	t.Setenv("POLYPACK_CONFIG", filepath.Join(dir, "polypack.json"))
	t.Setenv("npm_config_userconfig", h.npmrc)
	t.Setenv("COMPOSER_HOME", filepath.Join(dir, "composer"))
	t.Setenv("COMPOSER_AUTH", "")
	t.Setenv("DOCKER_CONFIG", filepath.Join(dir, "docker"))
	t.Setenv("POLYPACK_REGISTRY_DOMAIN", "pkg.example.test")
	t.Setenv("PATH", "")
	if err := os.MkdirAll(h.project, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(h.project)
	return h
}

func hopper(token string) Team {
	return Team{
		Name: "Hopper Digital", Email: "ada@example.com", Host: "http://polypack.example.test", Token: token,
		NPM: "http://hopper.pkg.example.test/npm/", NPMScopes: []string{"@hopper"},
		Composer: "http://hopper.pkg.example.test/composer", OCIHost: "hopper.cr.example.test",
	}
}

func signIn(t *testing.T, teams map[string]Team) {
	t.Helper()
	if err := (&Config{Teams: teams}).save(); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setUpEverything writes what `polypack setup` would, for one Team.
func setUpEverything(t *testing.T, h doctorHome, team Team, dockerSecret string) {
	t.Helper()
	writeFile(t, h.npmrc, upsertBlock("", "hopper", npmBlockLines(team)))
	if err := setComposerBearer(h.composerAuth, composerAuthHost(team.Composer), team.Token); err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("token:" + dockerSecret))
	writeFile(t, h.dockerConfig, `{"auths":{"hopper.cr.example.test":{"auth":"`+auth+`"}}}`)
}

func doctorRun(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"doctor"}, args...), &stdout, &stderr)
	out := stdout.String() + stderr.String()
	t.Log("\n" + out)
	for _, secret := range []string{goodSecret, expiredSecret, officeSecret, soonSecret} {
		if strings.Contains(out, secret) {
			t.Fatalf("the doctor printed a Token:\n%s", out)
		}
	}
	return code, out
}

func assertContains(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorWhenEverythingIsSetUp(t *testing.T) {
	h := doctorSetup(t)
	team := hopper(goodSecret)
	signIn(t, map[string]Team{"hopper": team})
	setUpEverything(t, h, team, goodSecret)
	_ = h

	code, out := doctorRun(t)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	assertContains(t, out,
		"✓ Signed in to hopper",
		"hopper (Hopper Digital)",
		"✓ hopper.pkg.example.test answers.",
		`✓ The CLI's Token: "CLI on laptop", a read-only Member Token of ada@example.com, until`,
		"✓ @hopper installs from http://hopper.pkg.example.test/npm/ (~/.npmrc).",
		"✓ npm sends the CLI's Token (polypack_…GOOD, ~/.npmrc).",
		"✓ Composer sends the CLI's Token (polypack_…GOOD, bearer, ~/composer/auth.json).",
		"✓ hopper.cr.example.test answers.",
		`✓ docker sends polypack_…GOOD (~/docker/config.json): "CLI on laptop"`,
		"Everything looks right.",
	)
}

func TestDoctorFindsWhatIsWrong(t *testing.T) {
	h := doctorSetup(t)
	team := hopper(goodSecret)
	signIn(t, map[string]Team{"hopper": team})
	setUpEverything(t, h, team, officeSecret)
	// Composer sends an expired Token, from this project's auth.json, which wins.
	writeFile(t, filepath.Join(h.project, "auth.json"), `{"bearer":{"hopper.pkg.example.test":"`+expiredSecret+`"}}`)
	// This project's .npmrc sends @hopper to npmjs.
	writeFile(t, filepath.Join(h.project, "package.json"), `{}`)
	writeFile(t, filepath.Join(h.project, ".npmrc"), "@hopper:registry=https://registry.npmjs.org/\n")

	code, out := doctorRun(t)
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	assertContains(t, out,
		"✗ @hopper installs from https://registry.npmjs.org/ (this project's .npmrc), not from http://hopper.pkg.example.test/npm/.",
		"✗ Composer sends polypack_…OLD1 (bearer, this project's auth.json): This Token expired on 1 Nov 2026.",
		"Issue a new Token in PolyPack",
		"✗ docker sends polypack_…OFFC (~/docker/config.json): This Token is not allowed from 198.51.100.1.",
		"An Admin can change the addresses",
		"polypack: 3 problems found",
	)
}

func TestDoctorWhenNothingIsSetUpButTheSignIn(t *testing.T) {
	doctorSetup(t)
	signIn(t, map[string]Team{"hopper": hopper(expiredSecret)})

	code, out := doctorRun(t)
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	assertContains(t, out,
		"✗ The CLI's Token: This Token expired on 1 Nov 2026.",
		"Run polypack login to get a new one.",
		"✗ @hopper has no registry set, so npm looks for it on registry.npmjs.org.",
		"✗ Composer sends no Token to hopper.pkg.example.test",
		"· docker is not installed.",
		"Run polypack setup --composer.",
	)
}

func TestDoctorWarnsOfATokenExpiringSoon(t *testing.T) {
	h := doctorSetup(t)
	team := hopper(soonSecret)
	team.OCIHost = ""
	signIn(t, map[string]Team{"hopper": team})
	setUpEverything(t, h, team, soonSecret)

	code, out := doctorRun(t)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	assertContains(t, out, "! The CLI's Token:", "It expires soon. Run polypack login to get a new one.", "✓ npm sends the CLI's Token", "No problems, 1 warning.")
	if strings.Contains(out, "docker") {
		t.Errorf("checked docker for a Team without an OCI host:\n%s", out)
	}
}

func TestDoctorFindsTeamsInNpmrcWithoutASignIn(t *testing.T) {
	h := doctorSetup(t)
	t.Setenv("ACME_TOKEN", goodSecret)
	writeFile(t, h.npmrc, "registry=https://registry.npmjs.org/\n@acme:registry=http://acme.pkg.example.test/npm/\n//acme.pkg.example.test/npm/:_authToken=${ACME_TOKEN}\n@other:registry=https://npm.example.com/\n")

	code, out := doctorRun(t)
	assertContains(t, out,
		"· Not signed in with polypack login",
		"acme, found in your config; not signed in with the CLI",
		"✓ @acme installs from http://acme.pkg.example.test/npm/",
		"✓ npm sends polypack_…GOOD",
		"✗ Composer sends no Token to acme.pkg.example.test",
	)
	if code != 1 || strings.Contains(out, "npm.example.com") {
		t.Errorf("exit %d:\n%s", code, out)
	}

	t.Setenv("ACME_TOKEN", "")
	_, out = doctorRun(t)
	assertContains(t, out, "✗ npm's Token (~/.npmrc) is ${ACME_TOKEN}, which is not set.")
}

func TestDoctorWhenAHostCannotBeReached(t *testing.T) {
	doctorSetup(t)
	down := hopper(goodSecret)
	down.NPM, down.Composer = "http://down.pkg.example.test/npm/", "http://down.pkg.example.test/composer"
	gone := hopper(goodSecret)
	gone.NPM, gone.Composer = "http://gone.pkg.example.test/npm/", "http://gone.pkg.example.test/composer"
	signIn(t, map[string]Team{"down": down, "gone": gone})

	code, out := doctorRun(t)
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	assertContains(t, out,
		"✗ Cannot reach down.pkg.example.test: connection refused",
		"✗ gone.pkg.example.test is not a PolyPack Team's registry.",
		"polypack: 2 problems found",
	)
	if strings.Contains(out, "npm\n") {
		t.Errorf("checked npm for a host it cannot reach:\n%s", out)
	}
}

func TestDoctorChecksTheProjectsComposerJson(t *testing.T) {
	h := doctorSetup(t)
	team := hopper(goodSecret)
	team.OCIHost = ""
	signIn(t, map[string]Team{"hopper": team})
	setUpEverything(t, h, team, goodSecret)

	writeFile(t, filepath.Join(h.project, "composer.json"), `{"require":{"hopper/widgets":"^1.0"}}`)
	_, out := doctorRun(t)
	assertContains(t, out, "· This project's composer.json has no repository for http://hopper.pkg.example.test/composer.",
		"run: composer config repositories.hopper composer http://hopper.pkg.example.test/composer")

	writeFile(t, filepath.Join(h.project, "composer.json"), `{"repositories":{"hopper":{"type":"composer","url":"http://hopper.pkg.example.test/composer/"}}}`)
	_, out = doctorRun(t)
	assertContains(t, out, "✓ This project's composer.json uses http://hopper.pkg.example.test/composer.")
}

func TestDoctorWithNothingAtAll(t *testing.T) {
	doctorSetup(t)
	code, out := doctorRun(t)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, out, "✗ No PolyPack registry is set up in npm or Composer here.", "Run polypack login")

	code, out = doctorRun(t, "--team", "nobody")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, out, `no Team "nobody" is signed in, or set up in npm or Composer`)
}

func TestTeamSlugOf(t *testing.T) {
	domains := []string{"pkg.polypack.io"}
	for host, want := range map[string]string{
		"hopper.pkg.polypack.io":      "hopper",
		"hopper.pkg.polypack.io:443":  "hopper",
		"a.b.pkg.polypack.io":         "",
		"pkg.polypack.io":             "",
		"registry.npmjs.org":          "",
		"hopper.pkg.polypack.io.evil": "",
	} {
		if got := teamSlugOf(host, domains); got != want {
			t.Errorf("teamSlugOf(%q) = %q, want %q", host, got, want)
		}
	}
}
