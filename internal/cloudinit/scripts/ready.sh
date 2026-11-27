#!/bin/bash
set -euo pipefail
install -d -m 0755 /var/lib/barn
warnings=
if [[ -f /var/lib/barn/warnings.jsonl ]]; then
  warnings=$(paste -sd, /var/lib/barn/warnings.jsonl)
fi
# __BARN_READY_WRITE__
chmod 0644 /var/lib/barn/ready.json.tmp
mv /var/lib/barn/ready.json.tmp /var/lib/barn/ready.json
