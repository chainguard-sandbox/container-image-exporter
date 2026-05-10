#!/usr/bin/env bash
set -euo pipefail

CHART_DIR="${CHART_DIR:-./deploy/chart}"

echo "==> helm lint"
helm lint "${CHART_DIR}" --set image.repository=foo

# Render the chart under representative value combinations. --debug surfaces
# any schema / required-value errors non-silently.
render() {
    local desc="$1"; shift
    echo "==> helm template: ${desc}"
    helm template cie "${CHART_DIR}" --debug --set image.repository=foo "$@" >/dev/null
}

render "default values"
render "all components enabled + all optional features" \
    --set exporter.serviceMonitor.enabled=true \
    --set nodeExporter.serviceMonitor.enabled=true \
    --set grafana.dashboards.enabled=true
render "exporter disabled" --set exporter.enabled=false
render "node-exporter disabled" --set nodeExporter.enabled=false
render "both components disabled" \
    --set exporter.enabled=false \
    --set nodeExporter.enabled=false
render "RBAC disabled" --set exporter.rbac.create=false
render "service accounts not created" \
    --set exporter.serviceAccount.create=false \
    --set nodeExporter.serviceAccount.create=false
render "imagePullSecrets configured" \
    --set 'exporter.imagePullSecrets[0]=my-registry' \
    --set 'exporter.imagePullSecrets[1]=other-registry'

echo "==> All lint checks passed"
