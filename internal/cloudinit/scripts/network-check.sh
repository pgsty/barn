#!/bin/bash
set -euo pipefail
for attempt in 1 2 3; do
  if response=$(timeout 10s /bin/bash -c '
exec 3<>/dev/tcp/example.com/80
printf "HEAD / HTTP/1.0\r\nHost: example.com\r\n\r\n" >&3
IFS= read -r response <&3
printf "%s\n" "${response}"
'); then
    if [[ "${response}" == HTTP/* ]]; then
      printf '%s\n' "${response}"
      exit 0
    fi
  fi
  if (( attempt < 3 )); then
    sleep 2
  fi
done
echo 'management egress probe failed after 3 bounded attempts' >&2
exit 1
