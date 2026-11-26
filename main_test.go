package main

import (
	"os"
	"testing"
)

// No test ever asks the real PolyPack for the CLI's releases (step 75): the
// tests that want that source fake it with POLYPACK_CLI_FILES_URL.
func TestMain(m *testing.M) {
	os.Setenv("POLYPACK_CLI_FILES_URL", "off")
	os.Exit(m.Run())
}
