package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSetComposerBearerMergesAndRemoves(t *testing.T) {
	file := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(file, []byte(`{"github-oauth": {"github.com": "gho_x"}, "bearer": {"other.example": "t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := setComposerBearer(file, "hopper.pkg.polypack.io", "polypack_one"); err != nil {
		t.Fatal(err)
	}
	auth := readJSON(t, file)
	if auth["github-oauth"].(map[string]any)["github.com"] != "gho_x" {
		t.Fatal("other keys were lost")
	}
	bearer := auth["bearer"].(map[string]any)
	if bearer["hopper.pkg.polypack.io"] != "polypack_one" || bearer["other.example"] != "t" {
		t.Fatalf("bearer = %v", bearer)
	}
	if info, _ := os.Stat(file); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}

	if err := setComposerBearer(file, "hopper.pkg.polypack.io", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := readJSON(t, file)["bearer"].(map[string]any)["hopper.pkg.polypack.io"]; ok {
		t.Fatal("not removed")
	}
}

func TestComposerAuthHostKeepsThePort(t *testing.T) {
	if got := composerAuthHost("http://hopper.registry.localhost:8080/composer"); got != "hopper.registry.localhost:8080" {
		t.Fatalf("got %q", got)
	}
}

func readJSON(t *testing.T, file string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
