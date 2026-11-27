#!/bin/bash
set -euo pipefail
install -d -o root -g root -m 0755 /var/lib/barn
exec 9>/var/lib/barn/finalize.lock
flock -n 9 || { echo 'guest setup is already running' >&2; exit 1; }
# Empty selection runs initial setup; up passes only stages needing a retry.
selected=" $* "
previous_warnings=$(cat /var/lib/barn/warnings.jsonl 2>/dev/null || true)
stage=identity
stage_err=/var/lib/barn/stage.err
finalize_exit() {
  local status=$?
  local error_tmp= detail=
  trap - EXIT
  if (( status != 0 )); then
    if [[ -s "${stage_err}" ]]; then
      detail=$(tail -n 1 "${stage_err}" | LC_ALL=C tr -d '\r\n' | LC_ALL=C tr '\000-\037\177' ' ' | cut -c1-200 | sed 's/\\/\\\\/g; s/"/\\"/g')
    fi
    if error_tmp=$(mktemp /var/lib/barn/error.json.XXXXXX); then
      if printf '{"exit_status":%d,"stage":"%s","detail":"%s"}\n' "${status}" "${stage}" "${detail}" > "${error_tmp}" &&
        chown root:root "${error_tmp}" && chmod 0644 "${error_tmp}"; then
        mv -f -- "${error_tmp}" /var/lib/barn/error.json || rm -f -- "${error_tmp}" || true
      else
        rm -f -- "${error_tmp}" || true
      fi
    fi
  fi
  rm -f -- "${stage_err}" || true
  exit "${status}"
}
# run_stage records the stage name and keeps its stderr so the error marker
# can carry the failing command's last line to the host.
run_stage() {
  stage=$1
  shift
  : >"${stage_err}"
  local rc=0
  "$@" 2>"${stage_err}" || rc=$?
  cat "${stage_err}" >&2
  return "${rc}"
}
run_optional() {
  local name=$1 budget=$2 detail
  shift 2
  if [[ "${selected}" != '  ' && "${selected}" != *" ${name} "* ]]; then
    # Retain unselected limitations, but reset notices describe only one run.
    printf '%s\n' "${previous_warnings}" | grep -F "\"stage\":\"${name}\"" >>/var/lib/barn/warnings.jsonl || true
    return 0
  fi
  if run_stage "${name}" timeout --kill-after=5s "${budget}" "$@"; then
    return 0
  fi
  detail=$(tail -n 1 "${stage_err}")
  /usr/local/libexec/barn-warning "${name}" "${detail:-setup unavailable or timed out}"
}
trap finalize_exit EXIT
rm -f -- /var/lib/barn/ready.json /var/lib/barn/error.json
: >/var/lib/barn/warnings.jsonl
chmod 0644 /var/lib/barn/warnings.jsonl
run_stage identity /usr/local/libexec/barn-identity-contract
run_optional hosts 30s /usr/local/libexec/barn-hosts
# Internet access is diagnostic only: an offline lab must still initialize its
# disks, shares and private network. barn-network-check remains available for
# an explicit guest connectivity check.
run_optional data-disks 90s /usr/local/libexec/barn-init-disks
# __BARN_FINALIZE_OPTIONAL__
run_stage ready /usr/local/libexec/barn-ready
