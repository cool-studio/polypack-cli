// Command polypack signs a machine in to PolyPack and sets up npm, Composer
// and docker for its Teams. See README.md.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is stamped at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `polypack: sign this machine in to PolyPack and set up its package managers.

Usage:
  polypack login [--host URL] [--name NAME] [--no-setup]
  polypack setup [--npm] [--composer] [--docker]
  polypack whoami
  polypack logout [--team SLUG]
  polypack verify npm @scope/name@version | oci name:tag [--team SLUG]
  polypack files upload <pack>@<version> <file>... [--team SLUG] [--publish]
  polypack files download <pack>[@<version or channel>] [--file NAME] [--out DIR]
  polypack ci <pack> --team SLUG [--files]   (in GitHub Actions: trusted publishing)
  polypack doctor [--team SLUG]     (check npm, Composer and docker here)
  polypack update [--version vX.Y.Z] [--channel NAME]
  polypack version

The host is https://polypack.io unless --host or POLYPACK_HOST says otherwise.
Once a day it checks for a newer release; POLYPACK_NO_UPDATE_CHECK=1 turns that off.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main, testable: it returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	finish := startUpdateNotice(args[0], stderr)
	defer finish()

	var err error
	switch args[0] {
	case "login":
		err = cmdLogin(args[1:], stdout, stderr)
	case "setup":
		err = cmdSetup(args[1:], stdout, stderr)
	case "whoami":
		err = cmdWhoami(args[1:], stdout)
	case "logout":
		err = cmdLogout(args[1:], stdout, stderr)
	case "verify":
		err = cmdVerify(args[1:], stdout, stderr)
	case "files":
		err = cmdFiles(args[1:], stdout, stderr)
	case "ci":
		err = cmdCI(args[1:], stdout, stderr)
	case "doctor":
		err = cmdDoctor(args[1:], stdout, stderr)
	case "update":
		err = cmdUpdate(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "polypack %s\n", version)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "polypack: unknown command %q\n\n%s", args[0], usage)
		return 2
	}

	if err != nil {
		fmt.Fprintf(stderr, "polypack: %v\n", err)
		return 1
	}
	return 0
}
