#!/usr/bin/env bash
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"${DIR}/upgrader-stop.sh" || true
"${DIR}/upgrader-start.sh"
