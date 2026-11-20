package main

import (
	"strings"
	"testing"
)

func TestUpsertBlockAppendsReplacesAndKeepsTheRest(t *testing.T) {
	original := "save-exact=true\n@other:registry=https://example.com/\n"
	team := Team{NPM: "https://hopper.pkg.polypack.io/npm/", NPMScopes: []string{"@hopper"}, Token: "polypack_one"}

	once := upsertBlock(original, "hopper", npmBlockLines(team))
	want := original + "# >>> polypack hopper\n@hopper:registry=https://hopper.pkg.polypack.io/npm/\n//hopper.pkg.polypack.io/npm/:_authToken=polypack_one\n# <<< polypack hopper\n"
	if once != want {
		t.Fatalf("first write:\n%s\nwant:\n%s", once, want)
	}

	team.Token = "polypack_two"
	twice := upsertBlock(once+"color=false\n", "hopper", npmBlockLines(team))
	if strings.Count(twice, "# >>> polypack hopper") != 1 || !strings.Contains(twice, "_authToken=polypack_two") || strings.Contains(twice, "polypack_one") {
		t.Fatalf("rewrite did not replace in place:\n%s", twice)
	}
	if !strings.HasPrefix(twice, original) || !strings.HasSuffix(twice, "color=false\n") {
		t.Fatalf("lines outside the block changed:\n%s", twice)
	}

	if removed := removeBlock(twice, "hopper"); removed != original+"color=false\n" {
		t.Fatalf("remove left:\n%s", removed)
	}
	if removeBlock(original, "hopper") != original {
		t.Fatal("removing a missing block changed the file")
	}
}

func TestUpsertBlockAddsANewlineToAFileWithout(t *testing.T) {
	if got := upsertBlock("a=b", "x", []string{"c=d"}); got != "a=b\n# >>> polypack x\nc=d\n# <<< polypack x\n" {
		t.Fatalf("got %q", got)
	}
}

func TestNpmAuthKey(t *testing.T) {
	for registry, want := range map[string]string{
		"https://hopper.pkg.polypack.io/npm/":       "//hopper.pkg.polypack.io/npm/",
		"http://hopper.registry.localhost:8080/npm": "//hopper.registry.localhost:8080/npm/",
	} {
		if got := npmAuthKey(registry); got != want {
			t.Errorf("npmAuthKey(%q) = %q, want %q", registry, got, want)
		}
	}
}
