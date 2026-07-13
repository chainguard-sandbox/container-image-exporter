#!/usr/bin/env bash
set -euo pipefail

CLUSTER_CHART_DIR="${CLUSTER_CHART_DIR:-./deploy/charts/cluster-exporter}"
NODE_CHART_DIR="${NODE_CHART_DIR:-./deploy/charts/node-exporter}"

echo "==> helm lint (cluster-exporter)"
helm lint "${CLUSTER_CHART_DIR}" --set image.repository=foo

echo "==> helm lint (node-exporter)"
helm lint "${NODE_CHART_DIR}" --set image.repository=foo

# Render each chart under representative value combinations. --debug surfaces
# any schema / required-value errors non-silently.
render_cluster() {
    local desc="$1"; shift
    echo "==> helm template cluster-exporter: ${desc}"
    helm template container-image-exporter-cluster "${CLUSTER_CHART_DIR}" --debug \
        --set image.repository=foo "$@" >/dev/null
}

render_node() {
    local desc="$1"; shift
    echo "==> helm template node-exporter: ${desc}"
    helm template container-image-exporter-node "${NODE_CHART_DIR}" --debug \
        --set image.repository=foo "$@" >/dev/null
}

render_cluster "default values"
render_cluster "all optional features enabled" \
    --set serviceMonitor.enabled=true \
    --set grafana.dashboards.enabled=true
render_cluster "RBAC disabled" --set rbac.create=false
render_cluster "service account not created" --set serviceAccount.create=false
render_cluster "registryPullSecrets configured" \
    --set 'registryPullSecrets[0]=my-registry' \
    --set 'registryPullSecrets[1]=other-registry'

render_node "default values"
render_node "all optional features enabled" \
    --set serviceMonitor.enabled=true \
    --set grafana.dashboards.enabled=true
render_node "service account not created" --set serviceAccount.create=false

echo "==> All lint checks passed"
