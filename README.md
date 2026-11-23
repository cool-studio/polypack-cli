# polypack

Sign a machine in to [PolyPack](https://polypack.io) and set up npm, Composer
and docker for your Team, without pasting a Token anywhere.

```sh
polypack login
```

The CLI shows a code and opens PolyPack in your browser. Check the code, pick
your Team, and approve. The CLI then gets a read-only Token for this machine,
valid for 90 days, and sets up:

- **npm**: your Team's scopes and the Token, in a marked block in `~/.npmrc`;
- **Composer**: the Token in Composer's global `auth.json`. It prints the
  `composer config repositories…` line to run in each project;
- **docker**: `docker login` to your Team's image registry, when your Plan
  includes it and docker is installed.

It never writes into a project, so nothing it sets up can be committed by
mistake.

## Install

macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/cool-studio/polypack-cli/main/install.sh | sh
```

Or download a binary for your platform from the
[releases](https://github.com/cool-studio/polypack-cli/releases), and check it
against `SHA256SUMS`.

## Commands

| Command | What it does |
|---|---|
| `polypack login [--name NAME] [--no-setup]` | Signs this machine in through the browser, then runs `setup` |
| `polypack setup [--npm] [--composer] [--docker]` | Sets up npm, Composer and docker again, for each Team you are signed in to; name any of them to set up only those |
| `polypack whoami` | Who each Team's Token belongs to |
| `polypack logout [--team SLUG]` | Revokes the Token and removes what `setup` wrote |
| `polypack ci <pack> --team SLUG` | In GitHub Actions (`id-token: write`): exchanges GitHub's ID token for a Token that publishes that one Pack, and sets npm, or docker and helm, up with it. Fails with PolyPack's reason. The action below runs it |
| `polypack verify npm @scope/name@version` / `polypack verify oci name:tag` `[--team SLUG]` | Checks a Version's provenance: PolyPack's signature, and that the registry serves what was published. Prints the repository, commit, workflow and run it came from |
| `polypack version` | The version |

The Token appears under **My Tokens** in PolyPack as "CLI on {machine}", and
you can revoke it there at any time. The CLI keeps its settings in
`~/.config/polypack/config.json` (your platform's config directory), readable
only by you.

`POLYPACK_HOST` (or `--host`) points the CLI at another PolyPack, such as a
demo. `POLYPACK_NO_BROWSER=1` stops it opening a browser; it always prints the
link.

## Development

This repository is published from `cli/` in PolyPack's own repository; send
changes there. You need Go and nothing else (the standard library only):

```sh
go vet ./... && go test ./...
go build -ldflags "-X main.version=dev" -o polypack .
```

## GitHub Action

Publish to PolyPack from GitHub Actions with trusted publishing: no PolyPack
Token stored in GitHub. Add a trusted publisher to the Pack in PolyPack, then:

```yaml
permissions:
  contents: read
  id-token: write
steps:
  - uses: actions/checkout@v7
  - uses: actions/setup-node@v7
    with:
      node-version: 24
  - uses: cool-studio/polypack-cli@v0.4.0
    with:
      team: hopper
      pack: '@hopper/ui'   # or an image, e.g. api
  - run: npm publish     # or docker build and docker push
```

Inputs: `team`, `pack`, and, for an instance other than polypack.io,
`registry` and `oci`. It installs this CLI (its own version) and runs
`polypack ci`.

