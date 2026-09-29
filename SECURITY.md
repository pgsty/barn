# Security

## Reporting a vulnerability

Report suspected vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/pgsty/farrow/security/advisories/new)
for this repository, or by email to <repo@pigsty.cc>. Please do not open a
public issue for an unfixed vulnerability.

Include the Farrow version (`farrow --json version`), the host platform, and the
smallest reproduction you have. You should get an acknowledgement within five
working days.

Farrow is pre-1.0. Fixes land in the next tagged release; there is no separate
backport line yet.

## Privilege boundary

Farrow runs unprivileged. It asks for administrator access for exactly three
kinds of host transaction, each announced before it happens and each also
reachable as an individual command:

1. **Installing missing packages during `farrow setup`.** On Linux, setup runs
   the system package manager (`apt-get` or `dnf`) through `sudo` for QEMU,
   firmware, and OpenSSH after printing the exact plan and asking once. On
   macOS, Homebrew runs as the invoking user and never needs root.
2. **Installing the host-global fixed-IP network.** `farrow network install`
   prints the complete privileged plan and applies nothing without `--yes`.
   `farrow network uninstall` reverses it, restoring the recorded original
   ownership and mode of anything it changed.
3. **Installing and invoking the hosts helper.** `farrow-hosts-helper` is a
   separate, minimal, root-owned binary at
   `/opt/farrow/libexec/farrow-hosts-helper`. It is the only component that
   ever writes the system hosts file; `farrow hosts install --yes` invokes it
   through `sudo -n` with a digest-matched, marker-bounded plan.

Everything else — image cache, deployment state, SSH material, QEMU processes —
is owned by the invoking user under `$FARROW_HOME` and never needs root.

`farrow mac` adds no privileged transaction: macOS virtual machines, their
private networks and their desktops run as the invoking user. The only sudo
call in the Mac path is the hidden `farrow mac migrate` removing the root
network daemon that unreleased development builds installed; it is announced
first, removes exactly that daemon, and can be repeated safely.

### What the hosts helper will refuse

The helper is deliberately hard to misuse. It rejects any invocation that does
not satisfy every one of these:

- a target that is exactly the platform's native hosts path, and a staging path
  that is absolute;
- an effective UID of 0, with a target owned by `root:root`, unwritable by group
  and other, and with a link count of exactly one;
- a staging file with mode `0600`, a link count of one, and ownership matching
  `SUDO_UID`;
- a before-digest that still matches the target and an after-digest that matches
  the staging file, so a plan reviewed against a since-changed hosts file is
  refused rather than applied;
- a post-write re-read whose digest, ownership, link count, and mode all match
  what was promised.

The CLI only ever invokes a helper whose SHA-256 matches the digest compiled
into that exact Farrow build, so a mismatched or substituted helper is not run.

## Supply chain

- Release archives, Linux packages, SPDX SBOMs, the Homebrew formula, the
  installer, and the release metadata are all listed in `checksums.txt`.
- GitHub Actions checks out the exact tag, runs the complete source gates,
  builds the release assets, and creates a draft Release only after package,
  archive, SBOM, and checksum verification succeeds.
- Archive and package payloads bind the tagged commit and its
  `SOURCE_DATE_EPOCH`; binaries use `-trimpath` and an empty Go build ID.
- The Go toolchain, GoReleaser, nFPM, Syft, Staticcheck, and govulncheck are
  version-pinned in `packaging/toolchain.env` and verified at release time.

The user-scoped installer always verifies the selected archive against the
`checksums.txt` produced by that GitHub Release.

Verify a release before trusting it:

```bash
shasum -a 256 -c checksums.txt --ignore-missing
```

## Guest images

Official guest images are built from distribution-owned artifacts and pinned
by SHA-256 in a catalog signed with the keys embedded in the binary. Every
fetch verifies the digest, the byte count, the qcow2 structure, and the virtual
size before the image is published into the cache. Farrow refuses catalog
upstream URLs that point at a moving path such as `latest` or `current`. Those
URLs record the artifact used to build an official image; runtime downloads
use the selected repository and do not fall back to provenance bytes. Both
`repo.pigsty.io/farrow` and `repo.pigsty.cc/farrow` always require a trusted
catalog signature, including when passed explicitly with `--repo`.

Official Farrow repository images may be normalized or customized from the
named upstream cloud image. The catalog digest, artifact size, and virtual size
identify the final repository qcow2 that Farrow verifies and runs.
