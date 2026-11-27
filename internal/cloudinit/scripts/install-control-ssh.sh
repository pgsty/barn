#!/bin/bash
set -euo pipefail
# __BARN_CONTROL_SSH_VARS__
entry=$(getent passwd "${user}")
IFS=: read -r _ _ uid gid _ home _ <<<"${entry}"
[[ "${uid}" =~ ^[0-9]+$ && "${gid}" =~ ^[0-9]+$ ]]
[[ "${home}" == /home/* && -d "${home}" && ! -L "${home}" ]]
[[ "$(readlink -f -- "${home}")" == "${home}" ]]
ssh_dir="${home}/.ssh"
if [[ -e "${ssh_dir}" || -L "${ssh_dir}" ]]; then
  [[ -d "${ssh_dir}" && ! -L "${ssh_dir}" ]]
fi
if [[ ! -e /var/lib/barn/control-id_ed25519 && ! -e /var/lib/barn/control-ssh-config ]]; then
  # The first successful installation consumes the staged secret. A later
  # repair must validate the installed files instead of requiring it again.
  if [[ ! -f "${ssh_dir}/id_ed25519" || -L "${ssh_dir}/id_ed25519" ]]; then
    echo "node-to-node SSH key is missing or unsafe; restore the original deployment private key to ${ssh_dir}/id_ed25519 (owner ${uid}:${gid}, mode 0600); management SSH remains available" >&2
    exit 1
  fi
  [[ ! -L "${ssh_dir}/id_ed25519" && ! -L "${ssh_dir}/config" ]]
  [[ "$(stat -c '%u:%g:%a' "${ssh_dir}/id_ed25519")" == "${uid}:${gid}:600" ]]
  [[ "$(stat -c '%u:%g:%a' "${ssh_dir}/config")" == "${uid}:${gid}:600" ]]
  exit 0
fi
[[ "$(stat -c '%u:%g:%a' /var/lib/barn/control-id_ed25519)" == "0:0:600" && ! -L /var/lib/barn/control-id_ed25519 ]]
[[ "$(stat -c '%u:%g:%a' /var/lib/barn/control-ssh-config)" == "0:0:600" && ! -L /var/lib/barn/control-ssh-config ]]
install -d -o "${uid}" -g "${gid}" -m 0700 "${ssh_dir}"
install -o "${uid}" -g "${gid}" -m 0600 /var/lib/barn/control-id_ed25519 "${ssh_dir}/id_ed25519"
install -o "${uid}" -g "${gid}" -m 0600 /var/lib/barn/control-ssh-config "${ssh_dir}/config"
rm -f -- /var/lib/barn/control-id_ed25519 /var/lib/barn/control-ssh-config
[[ "$(stat -c '%u:%g:%a' "${ssh_dir}/id_ed25519")" == "${uid}:${gid}:600" ]]
[[ "$(stat -c '%u:%g:%a' "${ssh_dir}/config")" == "${uid}:${gid}:600" ]]
