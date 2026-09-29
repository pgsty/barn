# Contributing to Barn

## Before you start

Barn is pre-1.0 and deliberately small. The Linux deployment path has a few
ratified non-negotiables, and a change that contradicts one of them will be
declined no matter how well it is implemented:

- **Exactly one deployment per user.** No projects, no leases, no workspaces.
- **One inventory format.** A Pigsty-compatible Ansible inventory is the only
  configuration. `barn.yml` is preferred over `pigsty.yml` only as a filename.
- **The `vm_*` namespace is strict; everything else is opaque.** An unknown
  `vm_*` key is a hard error. A non-`vm_*` key is never validated.
- **Absence never destroys.** Removing a host from the inventory does not delete
  a machine. Destruction is always explicit and confirmed.
- **One helper binary.** Privileged work goes through `barn-hosts-helper` or
  the reviewed network plan, and nowhere else.

The independent `barn mac` command manages named macOS machines without
reading the Linux inventory, and runs entirely as the invoking user: each
machine's network lives inside its own runner process. Keep it that way; see
[the Mac guide](docs/mac.md).

Open an issue before a large change so we can agree on the shape first.

## Test-lab recovery

Optimize ordinary failures for a usable lab. Keep management SSH and instance
identity as the readiness boundary; retry unfinished guest setup through `up`,
without a separate repair command. Healthy stages and running VMs stay intact.
Barn data disks contain disposable test data: reuse working filesystems and
reset unusable ones to the configured filesystem, reporting discarded data.
This also applies to persistent disks; persistence controls destroy/recreate,
not retention of corrupt contents. Missing devices, failed probes, busy mounts,
and host I/O failures are not proof of filesystem damage. Do not reset those
or change root disks, host shares, or unrelated devices as a recovery action.
Continue independent work and state exactly which features remain unavailable.

## Development loop

```bash
make build     # build ./bin/barn and ./bin/barn-hosts-helper
make test      # unit tests
make race      # race detector
make check     # everything CI runs, before you push
```

`make check` runs module verification, shell syntax checks, unit and race tests,
`go vet`, Staticcheck, four-target dead-code intersection, errcheck,
`govulncheck`, cross-compilation for all four supported targets, the
image-pipeline boundary tests, installer tests, maintenance ownership, and the
dependency-license inventory. It must
pass before a pull request is ready.

The toolchain is pinned in `packaging/toolchain.env` and CI asserts the exact
versions, so install those versions locally:

```bash
go install honnef.co/go/tools/cmd/staticcheck@v0.8.1
go install golang.org/x/tools/cmd/deadcode@v0.49.0
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
go install golang.org/x/vuln/cmd/govulncheck@v1.7.0
```

Changes under `tools/`, `packaging/`, `.goreleaser.yaml`, or the `Makefile` also
run the `packaging` workflow, which builds and verifies a complete snapshot
release. Run it locally with `make release-snapshot` if you have GoReleaser,
nFPM, and Syft at the pinned versions.

## Maintenance and release tools

Catalog maintenance stays separate from the user-facing `barn repo` command.
`tools/catalogexport` writes the exact catalog embedded in the current source;
the destination must be an absolute path that does not exist. `tools/catalogsign`
manages Minisign keys and signatures accepted by the runtime. Its password is
read only from inherited `CATALOGSIGN_PASSWORD` (empty is allowed), never from a
command-line argument. Production private keys never enter this repository or
CI; they stay on the repository host.

```bash
make catalog-export CATALOG_OUTPUT=/absolute/new/catalog.json
make catalog-keygen CATALOG_KEY_DIR=/absolute/private CATALOG_KEY_NAME=barn-catalog-next
make catalog-sign CATALOG_KEY=/absolute/private/barn-catalog-next.key CATALOG_FILE=/absolute/catalog.json
make catalog-verify CATALOG_PUBLIC_KEY=/absolute/private/barn-catalog-next.pub CATALOG_FILE=/absolute/catalog.json
```

Catalog Minisign signatures are consumed by Barn and remain independent of
application delivery. Application archives and packages are built by GitHub
Actions and listed in `checksums.txt`; no separate application-release signing
or provenance bundle is produced.

The native macOS guest app is built on macOS 27 and attached to the ordinary
Darwin arm64 archive before its SBOM/checksums are finalized. It is optional:
without the Developer ID signing and notarization secrets, the release ships
the CLI alone; with them, a failed Mac build stops the release. See
[Mac release preparation](docs/mac-release.md).
`make mac-native-test` covers the native components, and
`tests/mac-release-test.py` exercises payload boundaries as part of
`make install-test`.

`make release-dev VERSION=0.2.1-dev.1 SOURCE_DATE_EPOCH=$(git show -s --format=%ct HEAD)`
builds and verifies the older unsigned development-archive path. The packaging
workflow runs it alongside the GoReleaser snapshot; neither path publishes
anything.

`packaging/image-pipeline/build-official.py --list` shows the fixed eight-image
candidate matrix. A build requires explicit source, package-cache, and output
directories; it refuses to replace an existing bundle. After all architecture
builds are copied to one host, repeat `--assemble-from` for each bundle root to
create a new unsigned candidate repository and run `barn repo build/verify`.
The command never edits `packaging/image-repository/repo.yaml`, signs a catalog,
or publishes files; those remain separate owner-controlled promotion gates.

The Debian 12/13 recipe installs digest-locked XFS packages and generates
`en_US.UTF-8`, while retaining `C.UTF-8` as the default. Both locale properties
are checked in the guest and in the returned normalization marker. This is part
of the recipe, including future base-image refreshes; do not apply it only to a
published binary. Ubuntu entries currently retain the dated Canonical cloud
image bytes and receive deployment-specific configuration through cloud-init.
Refreshes use new versioned filenames, retain older catalog versions and cached
images referenced by deployments, and calculate catalog hashes from the final
artifact after customization. Promote only after native startup and SSH checks
for each changed architecture; Debian checks also cover both locales and XFS.

The maintenance inventory below names files whose owner is otherwise indirect.
`make maintenance-check` fails when a new file under `tools/` or `packaging/`
has no Make, workflow, or inventory reference.

- GoReleaser hooks: `packaging/goreleaser-package-sbom.sh`,
  `packaging/goreleaser-package-stage.sh`, and `packaging/goreleaser-sbom.sh`.
- Archive/package composition: `packaging/binary-format.sh`,
  `packaging/payload-inventory.sh`, `packaging/render-homebrew.sh`,
  `packaging/homebrew/barn.rb.tmpl`, `packaging/install.sh`, and
  `packaging/nfpm.yaml`.
- Native Mac composition: `packaging/mac-release.py`, `packaging/build-mac.sh`,
  `packaging/build-mac-app.sh`, and `packaging/install-mac-app.sh`.
- Image construction: `packaging/image-pipeline/build.sh`,
  `packaging/image-pipeline/build-official.py`,
  `packaging/image-pipeline/normalize-guest.sh`,
  `packaging/image-pipeline/official-v1.json`,
  `packaging/image-pipeline/pipeline.py`,
  `packaging/image-pipeline/recipe-v1.json`, and
  `packaging/image-pipeline/.gitignore`.
- Repository fixture: `packaging/image-repository/repo.yaml`.

## House style

The code aims to read as one voice. Match what is already there.

- **Comments explain why, never what.** If a comment restates the code, delete
  it. If a line looks arbitrary, the comment says what would break otherwise.
- **Names are words, not abbreviations.** `resolved`, `candidate`, `imageRecord`
  — not `res`, `c`, `img`.
- **Errors follow one shape.** See [Errors](#errors).
- **No map iteration reaching output.** Sort keys. Identical input must produce
  identical output and identical errors.
- **Fail closed.** When identity, ownership, or a digest cannot be proven,
  refuse rather than proceed.
- **No compatibility shims for formats that were never released.** Barn reads
  exactly one catalog schema and one inventory format.

## Errors

Every failure a user sees passes through one renderer: `error: <message>`, the
failing program's stderr tail, and an optional `next:` line. Keep messages in
that shape:

1. Lowercase, no final period, one line. Extra context goes in `next`, never
   after `\n\n`.
2. Name the object, then the problem: `inventory barn.yml: …`,
   `node meta: …`. Quote user-typed values with `%q`; print paths bare.
3. Give a next action only when there is one real thing to do, via
   `failure.New(...).Then(...)` or `failure.WithNext`. A destructive next action
   says what it deletes.
4. Never surface a bare OS or exec error. Say what Barn was doing, in the
   user's words; turn not-found, lookup, and deadline errors into sentences.
5. Wrap external command failures with `%w`. `execx.CommandError` already names
   the program and carries its stderr; do not restate either.
6. Classify with `internal/failure` where the fact is known. The exit code, the
   JSON `error`, and hints depend on error identity, never on message text.
7. Unclassified is `runtime`. `integrity` means a verified digest, identity, or
   ownership did not match — nothing else.
8. Use the documentation's words: inventory, node, deployment, Barn network.
   Never show private, v1, manifest, contract, proof, or check codes.
9. Say "will not X: reason" or state the problem; do not write "refuse X".
10. Anything that waits longer than a second says what it is waiting for.

`cmd/barn/error_contract_test.go` pins the exit code, JSON, and text of the
common failures; change it together with the message.

## Tests

- A bug fix comes with a test that fails before it and passes after.
- Tests must not depend on the machine they run on: no hardcoded UIDs, home
  directories, network access, or the presence of a specific QEMU build. Skip
  cleanly when an external tool is genuinely required.
- Prefer testing the contract over the implementation. Assert the behaviour a
  user or a script would observe.

## Naming and state

Use `barn`, `Barn`, `BARN_*`, `barn_version`, and Barn-owned paths
throughout code, packages, and documentation. Keep interfaces simple and use
one current format; do not introduce migration shims for development builds.

## Release checklist

- Check that generated assets, module paths, package names, scripts and current
  documentation consistently use Barn; inspect any old-name match individually.
- Never republish an image Catalog revision with different bytes. The embedded
  default and public `catalog.json` must be byte-identical at the same revision;
  same-revision differences are rejected as signed equivocation.

For a new version, move the current changes from `Unreleased` into a dated
Changelog section, update README installation links, and add bilingual notes at
`.github/releases/<version>.md`. Publish the matching documentation after the
application assets are public.

Before tagging, require source CI to pass the release commit and a packaging
snapshot to pass the latest packaging changes. Push `v<version>` to run the tag
workflow. It verifies the final assets
and creates a draft; inspect those assets, publish the draft, then verify
anonymous downloads and the installer. Pre-1.0 versions remain GitHub
pre-releases and need an explicit `BARN_VERSION` for installation.

## Commits and pull requests

- One logical change per commit, with a subject in the imperative mood:
  `fix: reconcile SSH config across lifecycle`.
- Explain the reasoning in the body when it is not obvious from the diff.
- Note user-visible changes in `CHANGELOG.md` under `Unreleased`.

## Security issues

Do not open a public issue. Follow [SECURITY.md](SECURITY.md).

## License

Contributions are accepted under the Apache-2.0 license that covers this
repository.
