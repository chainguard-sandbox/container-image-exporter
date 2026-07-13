#!/usr/bin/env bash
set -euo pipefail

CLUSTER_CHART_DIR="${CLUSTER_CHART_DIR:-./deploy/charts/cluster-exporter}"
NODE_CHART_DIR="${NODE_CHART_DIR:-./deploy/charts/node-exporter}"
UNITTEST="${UNITTEST:-untt}"

echo "==> helm-unittest (cluster-exporter)"
"${UNITTEST}" "${CLUSTER_CHART_DIR}"

echo "==> helm-unittest (node-exporter)"
"${UNITTEST}" "${NODE_CHART_DIR}"
