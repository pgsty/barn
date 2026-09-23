# Contributing to Farrow

## Before you start

Farrow is pre-1.0 and deliberately small. The design has a few ratified
non-negotiables, and a change that contradicts one of them will be declined no
matter how well it is implemented:

- **Exactly one deployment per user.** No projects, no leases, no workspaces.
- **One inventory format.** A Pigsty-compatible Ansible inventory is the only
  configuration. `farrow.yml` is preferred over `pigsty.yml` only as a filename.
- **The `vm_*` namespace is strict; everything else is opaque.** An unknown
  `vm_*` key is a hard error. A non-`vm_*` key is never validated.
- **Absence never destroys.** Removing a host from the inventory does not delete
  a machine. Destruction is always explicit and confirmed.
- **One helper binary.** Privileged work goes through `farrow-hosts-helper` or
  the reviewed network plan, and nowhere else.

Open an issue before a large change so we can agree on the shape first.

## Test-lab recovery

Optimize ordinary failures for a usable lab. Keep management SSH and instance
identity as the readiness boundary; retry unfinished guest setup through `up`,
without a separate repair command. Healthy stages and running VMs stay intact.
Farrow data disks contain disposable test data: reuse working filesystems and
reset unusable ones to the configured filesystem, reporting discarded data.
This also applies to persistent disks; persistence controls destroy/recreate,
not retention of corrupt contents. Missing devices, failed probes, busy mounts,
and host I/O failures are not proof of filesystem damage. Do not reset those
or change root disks, host shares, or unrelated devices as a recovery action.
Continue independent work and state exactly which features remain unavailable.

## Development loop

```bash
make build     # build ./bin/farrow and ./bin/farrow-hosts-helper
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

Catalog maintenance stays separate from the user-facing `farrow repo` command.
`tools/catalogexport` writes the exact catalog embedded in the current source;
the destination must be an absolute path that does not exist. `tools/catalogsign`
manages Minisign keys and signatures accepted by the runtime. Its password is
read only from inherited `CATALOGSIGN_PASSWORD` (empty is allowed), never from a
command-line argument. Production private keys never enter this repository or
CI; they stay on the repository host.

```bash
make catalog-export CATALOG_OUTPUT=/absolute/new/catalog.json
make catalog-keygen CATALOG_KEY_DIR=/absolute/private CATALOG_KEY_NAME=farrow-catalog-next
make catalog-sign CATALOG_KEY=/absolute/private/farrow-catalog-next.key CATALOG_FILE=/absolute/catalog.json
make catalog-verify CATALOG_PUBLIC_KEY=/absolute/private/farrow-catalog-next.pub CATALOG_FILE=/absolute/catalog.json
```

Catalog Minisign signatures are consumed by Farrow and remain independent of
application delivery. Application archives and packages are built by GitHub
Actions and listed in `checksums.txt`; no separate application-release signing
or provenance bundle is produced.

`make release-dev VERSION=0.2.1-dev.1 SOURCE_DATE_EPOCH=$(git show -s --format=%ct HEAD)`
builds and verifies the older unsigned development-archive path. The packaging
workflow runs it alongside the GoReleaser snapshot; neither path publishes
anything.

`packaging/image-pipeline/build-official.py --list` shows the fixed eight-image
candidate matrix. A build requires explicit source, package-cache, and output
directories; it refuses to replace an existing bundle. After all architecture
builds are copied to one host, repeat `--assemble-from` for each bundle root to
create a new unsigned candidate repository and run `farrow repo build/verify`.
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
  `packaging/homebrew/farrow.rb.tmpl`, `packaging/install.sh`, and
  `packaging/nfpm.yaml`.
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
- **No compatibility shims for formats that were never released.** Farrow reads
  exactly one catalog schema and one inventory format.

## Errors

Every failure a user sees passes through one renderer: `error: <message>`, the
failing program's stderr tail, and an optional `next:` line. Keep messages in
that shape:

1. Lowercase, no final period, one line. Extra context goes in `next`, never
   after `\n\n`.
2. Name the object, then the problem: `inventory farrow.yml: …`,
   `node meta: …`. Quote user-typed values with `%q`; print paths bare.
3. Give a next action only when there is one real thing to do, via
   `failure.New(...).Then(...)` or `failure.WithNext`. A destructive next action
   says what it deletes.
4. Never surface a bare OS or exec error. Say what Farrow was doing, in the
   user's words; turn not-found, lookup, and deadline errors into sentences.
5. Wrap external command failures with `%w`. `execx.CommandError` already names
   the program and carries its stderr; do not restate either.
6. Classify with `internal/failure` where the fact is known. The exit code, the
   JSON `error`, and hints depend on error identity, never on message text.
7. Unclassified is `runtime`. `integrity` means a verified digest, identity, or
   ownership did not match — nothing else.
8. Use the documentation's words: inventory, node, deployment, Farrow network.
   Never show private, v1, manifest, contract, proof, or check codes.
9. Say "will not X: reason" or state the problem; do not write "refuse X".
10. Anything that waits longer than a second says what it is waiting for.

`cmd/farrow/error_contract_test.go` pins the exit code, JSON, and text of the
common failures; change it together with the message.

## Tests

- A bug fix comes with a test that fails before it and passes after.
- Tests must not depend on the machine they run on: no hardcoded UIDs, home
  directories, network access, or the presence of a specific QEMU build. Skip
  cleanly when an external tool is genuinely required.
- Prefer testing the contract over the implementation. Assert the behaviour a
  user or a script would observe.

## Compatibility expiry

Farrow tolerates old local state only when a released build or a pre-0.1
development build wrote it. It never guesses at formats that Farrow never
wrote. Every exception has an expiry gate; reaching the version alone is not
enough unless the migration or refusal condition is also satisfied.

| ID | Path and old form | Last writer | Must remain supported through | Removal gate |
| --- | --- | --- | --- | --- |
| `process-start-v0` | `internal/process/identity.go`, `internal/private/manager.go`: locale-dependent `ps lstart` birth text and its legacy argv binding | pre-0.1 development builds | 0.2.x | Earliest 0.3.0, after 0.2 release notes require `farrow status` while each retained VM is live and migration tests prove the persisted identity was rewritten to `procstat:`/`kinfo:` plus the native argv hash. |
| `user-network-state-v0` | `cmd/farrow/main.go`: a deployment whose resolved network is `user` gets the fixed-IP redesign refusal instead of being interpreted as current state | pre-0.1 development builds | 0.2.x | Earliest 0.3.0, after the 0.2 migration window and release notes have told users to preserve disks and rebuild; never add a parser for another user-NAT shape. |
| `manifest-state-v1` | `internal/image/manifest_store.go`: the single-repository manifest state is wrapped as the `default` entry in the registry | pre-0.1 development builds | 0.2.x | Earliest 0.3.0, after a 0.2 release rewrites the registry in place on successful sync/reset and the real legacy fixture proves that migration. |
| `linux-network-backend-v0` | `internal/network/linux/plan.go`: a root-owned network manifest without `backend` means `systemd-networkd` | pre-0.1 development builds | 0.2.x | Earliest 0.3.0, after 0.2 has rewritten or explicitly uninstalled every accepted backend-less manifest and the uninstall fixture no longer needs the default. |
| `guest-hosts-marker-v0` | `internal/cloudinit/render.go`: marker-owned guest `/etc/hosts` rows use `# farrow-project-host` | 0.1.0 | 0.3.x | 0.2.x and 0.3.x write `# farrow-deployment-host` and remove both markers; remove the old marker no earlier than 0.4.0 after two minor release lines have converged guests. |
| `inherited-files-v0` | `internal/private/shares.go`: an empty typed inherited-file list may still describe the Darwin network FD in argv; shares never use that representation | pre-0.1 development builds | 0.2.x | Earliest 0.3.0, after recreate has rewritten retained invocations with typed inherited files and the legacy network-FD fixture has been retired; never accept an untyped share. |

## Release checklist

- Review this table and remove expired compatibility entries only when both the
  version window and the row-specific migration gate have passed.
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
pre-releases and need an explicit `FARROW_VERSION` for installation.

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
