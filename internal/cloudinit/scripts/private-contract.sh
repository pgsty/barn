#!/bin/bash
set -euo pipefail
# __BARN_PRIVATE_VARS__
interface=
for candidate in /sys/class/net/*; do
  [[ -r "${candidate}/address" ]] || continue
  read -r actual_mac <"${candidate}/address" || continue
  if [[ "${actual_mac,,}" == "${expected_mac}" ]]; then
    [[ -z "${interface}" ]] || { echo "multiple interfaces have private MAC ${expected_mac}" >&2; exit 1; }
    interface=${candidate##*/}
  fi
done
[[ -n "${interface}" ]] || { echo "private interface with MAC ${expected_mac} was not found" >&2; exit 1; }
ip link show dev "${interface}" >/dev/null
if ! ip link show dev "${interface}" | grep -Eq '<[^>]*UP[^>]*>'; then
  ip link set dev "${interface}" up
fi
if ! ip -4 -o address show dev "${interface}" scope global | awk '{print $4}' | grep -Fxq "${expected}"; then
  ip address add "${expected}" dev "${interface}"
fi
if ip -4 route show default dev "${interface}" | grep -q .; then
  echo "private interface ${interface} owns a default route" >&2
  exit 1
fi
if command -v resolvectl >/dev/null 2>&1; then
  dns=$(resolvectl dns "${interface}" 2>/dev/null || true)
  dns=${dns#*:}
  dns=$(printf '%s' "${dns}" | tr -d '[:space:]')
  if [[ -n "${dns}" && "${dns}" != "-" ]]; then
    echo "private interface ${interface} has DNS: ${dns}" >&2
    exit 1
  fi
elif command -v nmcli >/dev/null 2>&1; then
  dns=$(nmcli -g IP4.DNS device show "${interface}" 2>/dev/null || true)
  if [[ -n "${dns}" ]]; then
    echo "private interface ${interface} has DNS: ${dns}" >&2
    exit 1
  fi
fi
printf 'barn-arp-refresh\n' >"/dev/udp/${expected_host}/9"
printf 'private interface %s %s is up with no default route or DNS\n' "${interface}" "${expected}"
