#!/bin/bash
set -euo pipefail
json_string() {
  printf '%s' "$1" | LC_ALL=C tr '\000-\037\177' ' ' | cut -c1-400 | sed 's/\\/\\\\/g; s/"/\\"/g'
}
printf '{"stage":"%s","detail":"%s"}\n' "$(json_string "$1")" "$(json_string "$2")" >>/var/lib/barn/warnings.jsonl
