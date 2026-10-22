# Barn documentation and history

Current usage is documented on [the Barn website](https://barn.pgsty.com/docs/).
The [Mac guide](mac.md) and [Mac release checklist](mac-release.md) describe
the current native component. The [0.9.1 release checklist](release-0.9.1.md)
and [release notes](../.github/releases/0.9.1.md) record the release baseline.

## Release history

The [changelog](../CHANGELOG.md) retains the changes from 0.1.0 through 0.9.1.
Original release notes are available for
[0.1.0](../.github/releases/0.1.0.md),
[0.2.0](../.github/releases/0.2.0.md),
[0.3.0](../.github/releases/0.3.0.md),
[0.4.0](../.github/releases/0.4.0.md),
[0.5.0](../.github/releases/0.5.0.md),
[0.6.0](../.github/releases/0.6.0.md),
[0.7.0](../.github/releases/0.7.0.md),
[0.8.0](../.github/releases/0.8.0.md), and
[0.9.0](../.github/releases/0.9.0.md).

Historical notes use Barn naming throughout. Their dates, version numbers,
results, and limitations retain their original meaning. Old release downloads
are no longer available; use the current installation guide for installation.

## Design and validation records

| Record | Scope |
| --- | --- |
| [macOS feasibility study](../MACOS-VM-RESEARCH-20260921.md) | 2026-09-21 research, platform constraints, and implementation options |
| [Mac product proposal](../MACOS-VM-PRODUCT-20260921.md) | Original two-slot design and its trade-offs |
| [Initial Mac acceptance](history/mac-implementation-20260928.md) | 2026-09-26–28 native tests, failures, reviews, and evidence paths |
| [Mac network helper design](history/mac-network-20260928.md) | Historical privileged helper and protocol, superseded by per-machine networks |
| [Mac complexity review](mac-complexity-review.md) | 2026-09-28 simplification, retained safeguards, and live regression results |
| [Named Mac machines](mac-implementation-log.md) | 2026-09-29 redesign, native acceptance, and remaining checks |
| [0.9.0 naming change](rename-barn-0.9.0.md) | Scope and validation of the namespace change |

## Recovery provenance

These records were recovered on 2026-10-03. Release notes, changelog history,
and the latest Mac log come from `5431502`, before deletion in `0bfb7c3`.
The Mac research, product proposal, complexity review, and earlier acceptance
log come from `07ebfd4^`; the retired network-helper document comes from
`5d0b7ed^`. The website's release articles and validation history come from
`4c03fc9`, before deletion in `c058428` in the documentation repository.

Recovery preserves the original content with Barn naming and dated archive
notes. It does not recreate release assets or rerun the historical VM tests.
