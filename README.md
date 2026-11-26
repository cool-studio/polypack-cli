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
| `polypack ci <pack> --team SLUG [--files]` | In GitHub Actions (`id-token: write`): exchanges GitHub's ID token for a Token that publishes that one Pack, and sets npm, or docker and helm, up with it. With `--files`, for a Files Pack, it puts the Token in `POLYPACK_TOKEN` for the job's later steps. Fails with PolyPack's reason. The action below runs it |
| `polypack files upload <pack>@<version> <file>... [--publish]` | Uploads Files to a Files Pack's Version (a draft until published), straight to storage, checked on arrival; `--publish` publishes it once every File is in. Files can be patterns such as `dist/*`; a leading `v` is dropped from the Version |
| `polypack files download <pack>[@<version or channel>] [--file NAME] [--out DIR]` | Downloads a Version's Files (the latest by default, or a channel such as `beta`), each checked against `SHA256SUMS` and PolyPack's signature before it is saved |
| `polypack verify npm @scope/name@version` / `polypack verify oci name:tag` `[--team SLUG]` | Checks a Version's provenance: PolyPack's signature, and that the registry serves what was published. Prints the repository, commit, workflow and run it came from |
| `polypack doctor [--team SLUG]` | Checks what npm, Composer and docker send to each Team's hosts, here and in this project, asks each host about each Token, and says what to fix. It changes nothing, and never prints a Token |
| `polypack update [--version vX.Y.Z]` | Replaces this binary with the latest release (or the one named), checked against the release's `SHA256SUMS` |
| `polypack version` | The version |

The Token appears under **My Tokens** in PolyPack as "CLI on {machine}", and
you can revoke it there at any time. The CLI keeps its settings in
`~/.config/polypack/config.json` (your platform's config directory), readable
only by you.

## When an install fails

```sh
polypack doctor
```

For each Team you are signed in to, and any PolyPack registry your npm or
Composer config points at, it checks:

- that the Team's hosts answer;
- **npm**: which registry each of the Team's scopes installs from (this
  project's `.npmrc` first, then yours), and the Token npm sends there,
  `${VARIABLES}` included;
- **Composer**: the Token it sends (this project's `auth.json`,
  `COMPOSER_AUTH`, or your global `auth.json`), and whether this project's
  `composer.json` has the Team's repository;
- **docker**: the Token in docker's config or credential helper, when your
  Plan includes images.

PolyPack says what it makes of each Token: who it belongs to, what it may do
and until when, or why it is refused (expired, revoked, another Team's, or
not allowed from your address). Each problem comes with the command that
fixes it. A Token is only ever shown as `polypack_…` and its last four
characters, as in the panel. It exits with `1` if it found a problem.

## Updating

```sh
polypack update
```

It downloads the latest release for your platform, checks it against the
release's `SHA256SUMS` as `install.sh` does, and replaces the binary it runs
from. If that is in a directory you cannot write, such as `/usr/local/bin`,
run `sudo polypack update`, or `install.sh` again. `--version v0.4.0` installs
a particular release.

At most once a day, the CLI asks GitHub for the latest release, alongside
whatever command you run, and if there is a newer one it says so after the
command:

```
A new version of polypack is available: v0.4.0 → v0.5.0
Run `polypack update` to install it.
```

It never waits more than 2 seconds for the answer, and it stays quiet when
the check fails, when its output is not a terminal, in CI (`CI` or
`GITHUB_ACTIONS` set), and in `polypack ci`. `POLYPACK_NO_UPDATE_CHECK=1`
turns it off. The answer is kept in `update-check.json`, beside the config.

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
  - uses: cool-studio/polypack-cli@v0.7.0
    with:
      team: hopper
      pack: '@hopper/ui'   # or an image, e.g. api
  - run: npm publish     # or docker build and docker push
```

For a Files Pack, the action also uploads the Files and publishes the
Version:

```yaml
  - run: make dist
  - uses: cool-studio/polypack-cli@v0.7.0
    with:
      team: hopper
      pack: hopper-cli
      files: dist/*
      version: ${{ github.ref_name }}   # v1.2.0 becomes 1.2.0
```

Inputs: `team`, `pack`, `files` and `version` (Files Packs), and, for an
instance other than polypack.io, `registry` and `oci`. It installs this CLI
(its own version) and runs `polypack ci`, then, with `files`,
`polypack files upload --publish`.

