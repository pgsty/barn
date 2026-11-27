#!/bin/bash
set -euo pipefail
# __BARN_IDENTITY_USER__
entry=$(getent passwd "${user}")
IFS=: read -r _ _ uid gid _ home shell <<<"${entry}"
# __BARN_IDENTITY_CONTRACT__
[[ -d "${home}" && ! -L "${home}" ]]
printf 'login identity %s uid=%s gid=%s home=%s\n' "${user}" "${uid}" "${gid}" "${home}"
