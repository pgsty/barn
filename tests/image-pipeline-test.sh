#!/usr/bin/env bash
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd -P)
pipeline=${repo}/packaging/image-pipeline/build.sh
temporary_parent=$(cd "${TMPDIR:-/tmp}" && pwd -P)
temporary=$(mktemp -d "${temporary_parent}/barn-image-pipeline-test.XXXXXX")
temporary=$(cd "${temporary}" && pwd -P)
cleanup() {
  case ${temporary} in
    "${temporary_parent}"/barn-image-pipeline-test.*) rm -rf -- "${temporary}" ;;
    *) printf 'refuse unsafe test cleanup: %s\n' "${temporary}" >&2 ;;
  esac
}
trap cleanup EXIT
umask 077
mkdir "${temporary}/bin"

cat >"${temporary}/bin/qemu-img" <<'FAKE_QEMU'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == --version ]]; then
  printf 'qemu-img version 11.1.0-test\n'
  exit 0
fi
command=${1:-}
image=${!#}
scenario=$(sed -n '1p' "${image}")
safe='{"format":"qcow2","virtual-size":4294967296,"cluster-size":65536,"dirty-flag":false,"format-specific":{"type":"qcow2","data":{"compat":"1.1","compression-type":"zlib","corrupt":false,"extended-l2":false,"incompatible-features":[]}}}'
case ${command} in
  info)
    chain=false
    for argument in "$@"; do
      [[ ${argument} == --backing-chain ]] && chain=true
    done
    if [[ ${chain} == true ]]; then
      if [[ ${scenario} == FAKEQCOW:chain ]]; then
        printf '[%s,%s]\n' "${safe}" "${safe}"
      else
        printf '[%s]\n' "${safe}"
      fi
      exit 0
    fi
    case ${scenario} in
      FAKEQCOW:backing) printf '{"format":"qcow2","virtual-size":4294967296,"backing-filename":"/tmp/base.qcow2"}\n' ;;
      FAKEQCOW:data) printf '{"format":"qcow2","virtual-size":4294967296,"format-specific":{"type":"qcow2","data":{"data-file":"/tmp/data.raw"}}}\n' ;;
      FAKEQCOW:encrypted) printf '{"format":"qcow2","virtual-size":4294967296,"encrypted":true}\n' ;;
      FAKEQCOW:incompatible) printf '{"format":"qcow2","virtual-size":4294967296,"format-specific":{"type":"qcow2","data":{"incompatible-features":["mystery-bit"]}}}\n' ;;
      FAKEQCOW:extended) printf '{"format":"qcow2","virtual-size":4294967296,"format-specific":{"type":"qcow2","data":{"extended-l2":true}}}\n' ;;
      FAKEQCOW:corrupt) printf '{"format":"qcow2","virtual-size":4294967296,"format-specific":{"type":"qcow2","data":{"corrupt":true}}}\n' ;;
      FAKEQCOW:dirty) printf '{"format":"qcow2","virtual-size":4294967296,"dirty-flag":true}\n' ;;
      FAKEQCOW:raw) printf '{"format":"raw","virtual-size":4294967296}\n' ;;
      *) printf '%s\n' "${safe}" ;;
    esac
    ;;
  check)
    if [[ ${scenario} == FAKEQCOW:check-error ]]; then
      printf '{"corruptions":0,"check-errors":1}\n'
    else
      printf '{"corruptions":0,"check-errors":0}\n'
    fi
    ;;
  *) printf 'unexpected fake qemu-img command: %s\n' "${command}" >&2; exit 64 ;;
esac
FAKE_QEMU
chmod 0755 "${temporary}/bin/qemu-img"

cat >"${temporary}/bin/virt-customize" <<'FAKE_CUSTOMIZE'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == --version ]]; then
  printf 'virt-customize 1.54.0-test\n'
  exit 0
fi
for secret in AWS_SECRET_ACCESS_KEY GITHUB_TOKEN COSIGN_PASSWORD; do
  [[ -z ${!secret:-} ]] || { printf 'host secret leaked to customizer: %s\n' "${secret}" >&2; exit 42; }
done
image=
no_network=false
run_command=
previous=
for argument in "$@"; do
  if [[ ${previous} == -a ]]; then image=${argument}; fi
  if [[ ${previous} == --run-command ]]; then run_command=${argument}; fi
  [[ ${argument} == --no-network ]] && no_network=true
  previous=${argument}
done
[[ ${no_network} == true && -n ${image} && -f ${image} ]] || exit 43
profile=$(printf '%s\n' "${run_command}" | awk '{ print $4 }')
[[ ${profile} =~ ^(base|el8|el9|d12|d13)$ ]] || exit 45
printf '\nOFFLINE-NORMALIZED\nPROFILE=%s\n' "${profile}" >>"${image}"
FAKE_CUSTOMIZE
chmod 0755 "${temporary}/bin/virt-customize"

cat >"${temporary}/bin/virt-cat" <<'FAKE_CAT'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == --version ]]; then
  printf 'virt-cat 1.54.0-test\n'
  exit 0
fi
guest_path=${!#}
image=
previous=
for argument in "$@"; do
  if [[ ${previous} == -a ]]; then image=${argument}; fi
  previous=${argument}
done
profile=$(sed -n 's/^PROFILE=//p' "${image}" | tail -1)
[[ -n ${profile} ]] || profile=base
python3_status=not-requested
xfsprogs_status=not-requested
legacy_network_status=not-requested
sshd_include_status=upstream
locale_default_status=not-requested
locale_en_us_status=not-requested
[[ ${profile} == el8 ]] && python3_status=verified
[[ ${profile} == d12 || ${profile} == d13 ]] && xfsprogs_status=verified
if [[ ${profile} == d12 || ${profile} == d13 ]]; then
  locale_default_status=C.UTF-8
  locale_en_us_status=verified
fi
[[ ${profile} == el8 || ${profile} == el9 ]] && legacy_network_status=removed
[[ ${profile} == el8 ]] && sshd_include_status=verified
case ${guest_path} in
  /var/lib/barn-image/normalization.json)
    printf '{"schema":1,"recipe":"barn-official-image-normalization-v1","profile":"%s","source_user":"ubuntu","source_date_epoch":1787486400,"dba_uid":88,"admin_gid":88,"credential_hygiene":"applied","python3":"%s","xfsprogs":"%s","legacy_network":"%s","sshd_include":"%s","locale_default":"%s","locale_en_us":"%s"}\n' \
      "${profile}" "${python3_status}" "${xfsprogs_status}" "${legacy_network_status}" "${sshd_include_status}" "${locale_default_status}" "${locale_en_us_status}"
    ;;
  /etc/passwd) printf 'root:x:0:0:root:/root:/bin/bash\ndba:x:88:88::/home/dba:/bin/bash\n' ;;
  /etc/group) printf 'root:x:0:\nadmin:x:88:dba\n' ;;
  *) printf 'unexpected guest path: %s\n' "${guest_path}" >&2; exit 44 ;;
esac
FAKE_CAT
chmod 0755 "${temporary}/bin/virt-cat"

digest() {
  python3 - "$1" <<'PY'
import hashlib
import pathlib
import sys
print(hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest())
PY
}

make_source() {
  local path=$1 scenario=$2
  printf 'FAKEQCOW:%s\nfixture-payload\n' "${scenario}" >"${path}"
}

run_candidate() {
  local source=$1 output=$2 mode=${3:-validate}
  shift 3 || true
  local source_sha
  source_sha=$(digest "${source}")
  "${pipeline}" \
    --mode "${mode}" \
    --source "${source}" \
    --expected-sha256 "${source_sha}" \
    --output "${output}" \
    --name u24 --release 20260801.0.0 --arch amd64 \
    --source-user ubuntu --boot uefi --license NOASSERTION \
    --source-uri 'https://cloud-images.example.invalid/noble/20260801/noble-amd64.qcow2' \
    --artifact-url 'https://images.example.invalid/u24/20260801.0.0/{sha256}.qcow2' \
    --source-date-epoch 1787486400 --manifest-version 2026082403 \
    --qemu-img "${temporary}/bin/qemu-img" \
    --virt-customize "${temporary}/bin/virt-customize" \
    --virt-cat "${temporary}/bin/virt-cat" \
    "$@"
}

verify_bundle() {
  python3 - "$1" <<'PY'
import hashlib
import json
import pathlib
import stat
import sys

root = pathlib.Path(sys.argv[1])
files = sorted(p.name for p in root.iterdir())
assert len(files) == (9 if (root / "package-lock.json").exists() else 8), files
assert {
    "build-recipe.json", "checksums.txt", "manifest-candidate.json", "normalize-guest.sh",
    "provenance.intoto.json", "sbom.spdx.json", "validation.json",
}.issubset(files)
artifacts = [p for p in root.iterdir() if p.suffix == ".qcow2"]
assert len(artifacts) == 1
assert stat.S_IMODE(artifacts[0].stat().st_mode) == 0o444
assert int(artifacts[0].stat().st_mtime) == 1787486400
records = {}
for line in (root / "checksums.txt").read_text().splitlines():
    checksum, name = line.split("  ", 1)
    assert "/" not in name and name not in records
    records[name] = checksum
assert len(records) == len(files) - 1
for name, expected in records.items():
    assert hashlib.sha256((root / name).read_bytes()).hexdigest() == expected
manifest = json.loads((root / "manifest-candidate.json").read_text())
entry = manifest["images"]["u24"]["releases"]["20260801.0.0"]["amd64"]
assert entry["status"] == "testing" and entry["format"] == "qcow2"
assert entry["sha256"] == hashlib.sha256(artifacts[0].read_bytes()).hexdigest()
assert entry["sha256"] in entry["url"]
validation = json.loads((root / "validation.json").read_text())
assert validation["promotion"]["eligible"] is False
assert validation["signing"]["performed"] is False
assert validation["inspection_after"]["one_element_backing_chain"] is True
sbom = json.loads((root / "sbom.spdx.json").read_text())
assert sbom["spdxVersion"] == "SPDX-2.3"
provenance = json.loads((root / "provenance.intoto.json").read_text())
assert provenance["predicateType"] == "https://slsa.dev/provenance/v1"
PY
}

good=${temporary}/good.qcow2
make_source "${good}" success
run_candidate "${good}" "${temporary}/validate-one" validate >"${temporary}/validate.stdout"
grep -q 'NATIVE MUTATION NOT RUN' "${temporary}/validate.stdout"
verify_bundle "${temporary}/validate-one"
python3 - "${temporary}/validate-one" "${good}" <<'PY'
import json
import pathlib
import sys
root, source = map(pathlib.Path, sys.argv[1:])
artifact = next(root.glob("*.qcow2"))
assert artifact.read_bytes() == source.read_bytes()
validation = json.loads((root / "validation.json").read_text())
assert validation["native_mutation"]["status"] == "not-run-validation-only"
manifest = json.loads((root / "manifest-candidate.json").read_text())
entry = manifest["images"]["u24"]["releases"]["20260801.0.0"]["amd64"]
assert entry["provenance"].startswith("UNPUBLISHABLE VALIDATION-ONLY")
assert entry["source_user"] == "ubuntu"
PY

run_candidate "${good}" "${temporary}/validate-two" validate >/dev/null
cmp "${temporary}/validate-one/checksums.txt" "${temporary}/validate-two/checksums.txt"
for first in "${temporary}/validate-one"/*; do
  name=$(basename "${first}")
  cmp "${first}" "${temporary}/validate-two/${name}"
done

run_candidate "${good}" "${temporary}/offline" offline >"${temporary}/offline.stdout"
grep -q 'offline normalization completed' "${temporary}/offline.stdout"
verify_bundle "${temporary}/offline"
python3 - "${temporary}/offline" "$(digest "${good}")" <<'PY'
import json
import pathlib
import sys
root = pathlib.Path(sys.argv[1])
source_digest = sys.argv[2]
validation = json.loads((root / "validation.json").read_text())
assert validation["native_mutation"]["status"] == "completed"
assert validation["native_mutation"]["marker"]["dba_uid"] == 88
assert validation["artifact"]["sha256"] != source_digest
manifest = json.loads((root / "manifest-candidate.json").read_text())
entry = manifest["images"]["u24"]["releases"]["20260801.0.0"]["amd64"]
assert entry["source_user"] == "dba"
assert not entry["provenance"].startswith("UNPUBLISHABLE")
PY

# Previously the published Debian locale adjustment was absent from the
# checked-in recipe, and an image without either locale postcondition passed.
python3 - "${repo}/packaging/image-pipeline/pipeline.py" <<'PY'
import copy
import importlib.util
import json
from pathlib import Path
import sys

spec = importlib.util.spec_from_file_location("barn_pipeline", sys.argv[1])
pipeline_module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = pipeline_module
spec.loader.exec_module(pipeline_module)
for profile in ("d12", "d13"):
    valid = {
        "schema": 1, "recipe": "barn-official-image-normalization-v1",
        "profile": profile, "source_user": "debian", "source_date_epoch": 1787486400,
        "dba_uid": 88, "admin_gid": 88, "credential_hygiene": "applied",
        "python3": "not-requested", "xfsprogs": "verified",
        "legacy_network": "not-requested", "sshd_include": "upstream",
        "locale_default": "C.UTF-8", "locale_en_us": "verified",
    }
    cases = [(valid, True)]
    missing = copy.deepcopy(valid)
    del missing["locale_default"], missing["locale_en_us"]
    cases.append((missing, False))
    for field, value in (("locale_default", "en_US.UTF-8"), ("locale_en_us", "not-requested")):
        invalid = {**valid, field: value}
        cases.append((invalid, False))
    for marker, accepted in cases:
        def fake_tool(arguments, *unused):
            if arguments[-1] == "/var/lib/barn-image/normalization.json":
                return json.dumps(marker).encode()
            if arguments[-1] == "/etc/passwd":
                return b"dba:x:88:88::/home/dba:/bin/bash\n"
            if arguments[-1] == "/etc/group":
                return b"admin:x:88:dba\n"
            return b""
        pipeline_module.run_tool = fake_tool
        try:
            pipeline_module.normalize_offline(
                Path("customize"), Path("cat"), Path("image"), Path("script"),
                "debian", profile, [], 1787486400,
                {"virt_customize": 1, "virt_cat": 1}, {},
            )
        except pipeline_module.PipelineError as error:
            assert not accepted and "normalization marker mismatch" in str(error)
        else:
            assert accepted, f"{profile}: accepted absent or incorrect locale evidence"
PY

package_cache=${temporary}/package-cache
mkdir "${package_cache}"
printf 'locked python36 rpm fixture\n' >"${package_cache}/python36-1.x86_64.rpm"
printf 'locked python3-pip rpm fixture\n' >"${package_cache}/python3-pip-1.noarch.rpm"
python36_sha=$(digest "${package_cache}/python36-1.x86_64.rpm")
pip_sha=$(digest "${package_cache}/python3-pip-1.noarch.rpm")
package_lock=${temporary}/el8-amd64.lock.json
python3 - "${package_lock}" "${python36_sha}" "${pip_sha}" <<'PY'
import json
import pathlib
import sys

target, python36_sha, pip_sha = sys.argv[1:]
lock = {
    "arch": "amd64",
    "install": ["python36", "python3-pip"],
    "packages": [
        {
            "arch": "amd64",
            "filename": "python36-1.x86_64.rpm",
            "name": "python36",
            "sha256": python36_sha,
            "url": "https://packages.example.invalid/rocky/8.10/python36-1.x86_64.rpm",
            "version": "1",
        },
        {
            "arch": "noarch",
            "filename": "python3-pip-1.noarch.rpm",
            "name": "python3-pip",
            "sha256": pip_sha,
            "url": "https://packages.example.invalid/rocky/8.10/python3-pip-1.noarch.rpm",
            "version": "1",
        },
    ],
    "profile": "el8",
    "schema": 1,
}
pathlib.Path(target).write_text(json.dumps(lock, indent=2, sort_keys=True, separators=(",", ": ")) + "\n")
PY
run_candidate "${good}" "${temporary}/offline-packages" offline \
  --profile el8 --package-lock "${package_lock}" --package-cache "${package_cache}" \
  >"${temporary}/offline-packages.stdout"
verify_bundle "${temporary}/offline-packages"
python3 - "${temporary}/offline-packages" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
validation = json.loads((root / "validation.json").read_text())
assert validation["native_mutation"]["marker"]["profile"] == "el8"
assert validation["native_mutation"]["marker"]["python3"] == "verified"
assert validation["package_inputs"]["count"] == 2
assert validation["package_inputs"]["network_during_mutation"] is False
provenance = json.loads((root / "provenance.intoto.json").read_text())
dependencies = provenance["predicate"]["buildDefinition"]["resolvedDependencies"]
assert len(dependencies) == 6
sbom = json.loads((root / "sbom.spdx.json").read_text())
assert len(sbom["packages"]) == 4
PY

printf 'tampered\n' >>"${package_cache}/python36-1.x86_64.rpm"
if run_candidate "${good}" "${temporary}/tampered-package" offline \
  --profile el8 --package-lock "${package_lock}" --package-cache "${package_cache}" \
  >"${temporary}/tampered-package.stdout" 2>"${temporary}/tampered-package.stderr"; then
  printf 'tampered locked package was accepted\n' >&2
  exit 1
fi
[[ ! -e ${temporary}/tampered-package ]]
grep -q 'source SHA-256 mismatch' "${temporary}/tampered-package.stderr"

expect_policy_rejection() {
  local scenario=$1
  local source=${temporary}/${scenario}.qcow2 output=${temporary}/reject-${scenario}
  make_source "${source}" "${scenario}"
  if run_candidate "${source}" "${output}" validate >"${temporary}/${scenario}.stdout" 2>"${temporary}/${scenario}.stderr"; then
    printf 'unsafe scenario was accepted: %s\n' "${scenario}" >&2
    exit 1
  fi
  [[ ! -e ${output} ]]
}
for scenario in backing data encrypted incompatible extended corrupt dirty raw chain check-error; do
  expect_policy_rejection "${scenario}"
done

bad_output=${temporary}/bad-sha-output
if "${pipeline}" --mode validate --source "${good}" --expected-sha256 "$(printf '0%.0s' {1..64})" \
  --output "${bad_output}" --name u24 --release 1 --arch amd64 --source-user ubuntu --boot uefi \
  --license NOASSERTION --artifact-url 'https://images.example.invalid/u24/1/image.qcow2' \
  --source-date-epoch 1787486400 --manifest-version 1 --qemu-img "${temporary}/bin/qemu-img" \
  >"${temporary}/bad-sha.stdout" 2>"${temporary}/bad-sha.stderr"; then
  printf 'bad source SHA was accepted\n' >&2
  exit 1
fi
[[ ! -e ${bad_output} ]]
grep -q 'source SHA-256 mismatch' "${temporary}/bad-sha.stderr"

ln -s "${good}" "${temporary}/source-link.qcow2"
if run_candidate "${temporary}/source-link.qcow2" "${temporary}/symlink-output" validate >/dev/null 2>&1; then
  printf 'symlink source was accepted\n' >&2
  exit 1
fi
[[ ! -e ${temporary}/symlink-output ]]

existing=${temporary}/existing-output
mkdir "${existing}"
if run_candidate "${good}" "${existing}" validate >/dev/null 2>&1; then
  printf 'existing output was replaced\n' >&2
  exit 1
fi
[[ -d ${existing} && -z $(find "${existing}" -mindepth 1 -print -quit) ]]

mkdir "${temporary}/path-component"
noncanonical=${temporary}/path-component/../noncanonical-output
if run_candidate "${good}" "${noncanonical}" validate >/dev/null 2>"${temporary}/noncanonical.stderr"; then
  printf 'non-canonical output path was accepted\n' >&2
  exit 1
fi
[[ ! -e ${temporary}/noncanonical-output ]]
grep -q 'output path must already be canonical' "${temporary}/noncanonical.stderr"

moving=${temporary}/moving-output
good_sha=$(digest "${good}")
if "${pipeline}" --mode validate --source "${good}" --expected-sha256 "${good_sha}" \
  --output "${moving}" --name u24 --release 1 --arch amd64 --source-user ubuntu --boot uefi \
  --license NOASSERTION --artifact-url 'https://images.example.invalid/u24/latest/image.qcow2' \
  --source-date-epoch 1787486400 --manifest-version 1 --qemu-img "${temporary}/bin/qemu-img" \
  >/dev/null 2>"${temporary}/moving.stderr"; then
  printf 'moving artifact URL was accepted\n' >&2
  exit 1
fi
[[ ! -e ${moving} ]]

grep -q "usermod --password '!'" "${repo}/packaging/image-pipeline/normalize-guest.sh"
grep -q 'rm -f -- /etc/ssh/ssh_host_' "${repo}/packaging/image-pipeline/normalize-guest.sh"
grep -q 'restorecon -RF /home/dba' "${repo}/packaging/image-pipeline/normalize-guest.sh"
if grep -Eq '\b(curl|wget|ssh-keygen)\b' "${repo}/packaging/image-pipeline/normalize-guest.sh"; then
  printf 'offline normalization script unexpectedly uses network/key-generation tools\n' >&2
  exit 1
fi

"${repo}/packaging/image-pipeline/build-official.py" --list >"${temporary}/official-matrix.txt"
[[ $(wc -l <"${temporary}/official-matrix.txt" | tr -d ' ') == 8 ]]
grep -Fxq $'el8/amd64\t8.10.20240528.2\tel8\tpackages=2' "${temporary}/official-matrix.txt"
grep -Fxq $'el9/arm64\t9.8.20260525.2\tel9\tpackages=0' "${temporary}/official-matrix.txt"
grep -Fxq $'d12/arm64\t20260923.2610.1\td12\tpackages=3' "${temporary}/official-matrix.txt"
grep -Fxq $'d13/amd64\t20260914.2601.2\td13\tpackages=3' "${temporary}/official-matrix.txt"
python3 - "${repo}/packaging/image-pipeline/official-v1.json" <<'PY'
import json
import pathlib
import sys

matrix = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert len(matrix["targets"]) == 8
assert {f'{item["name"]}/{item["arch"]}' for item in matrix["targets"]} == {
    "d12/amd64", "d12/arm64", "d13/amd64", "d13/arm64",
    "el8/amd64", "el8/arm64", "el9/amd64", "el9/arm64",
}
allowed = {"python36", "python3-pip", "xfsprogs", "libinih1", "liburcu8", "libicu76"}
seen = {
    package["name"]
    for item in matrix["targets"]
    if item["package_lock"] is not None
    for package in item["package_lock"]["packages"]
}
assert seen == allowed
for forbidden in ("locale", "chrony", "unattended-upgrades", "kernel", "gzip", "zstd"):
    assert all(forbidden not in name for name in seen)
PY
mkdir "${temporary}/empty-bundles"
if "${repo}/packaging/image-pipeline/build-official.py" \
  --assemble-from "${temporary}/empty-bundles" \
  --output "${temporary}/unexpected-repository" \
  >"${temporary}/assemble.stdout" 2>"${temporary}/assemble.stderr"; then
  printf 'official repository assembly accepted an incomplete target set\n' >&2
  exit 1
fi
[[ ! -e ${temporary}/unexpected-repository ]]
grep -q 'has 0 candidate bundles' "${temporary}/assemble.stderr"

printf 'image pipeline validate/reproducibility/rejection and simulated offline boundaries passed\n'
printf 'SKIP native offline guest mutation: fake tools tested the boundary; run tests/image-pipeline-native-test.sh with an explicit source\n'
