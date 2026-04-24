#!/usr/bin/env bash
set -euo pipefail

CHART_DIR="${CHART_DIR:-./deploy/chart}"
UNITTEST="${UNITTEST:-untt}"

echo "==> helm-unittest"
"${UNITTEST}" "${CHART_DIR}"
