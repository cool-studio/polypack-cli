# polypack-cli (archived)

This repository is archived. The polypack CLI is now distributed by PolyPack:

    curl -fsSL https://polypack.io/install.sh | sh

Every release is checked against a `SHA256SUMS` signed by PolyPack's key,
which is built into the CLI. `polypack update` keeps it up to date.
Windows downloads, and every release, are listed at
https://polypack.io/docs/cli.

PolyPack's GitHub Action is now `cool-studio/polypack-actions`:

    - uses: cool-studio/polypack-actions@v1

Workflows still using `cool-studio/polypack-cli@v0.x` keep working for now,
but this repository will become private after notice, so move to
`polypack-actions@v1` when you next edit them.

The releases here stop at v0.9.0, which updates itself from PolyPack.
The code in this repository's history remains under the MIT licence.
