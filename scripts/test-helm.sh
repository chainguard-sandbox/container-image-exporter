#!/usr/bin/env bash
set -euo pipefail

K3D="${K3D:-k3d}"
HELM_TEST_CLUSTER="${HELM_TEST_CLUSTER:-cie-helm-test}"
HELM_TEST_IMAGE_NAME="${HELM_TEST_IMAGE_NAME:-container-image-exporter}"
HELM_TEST_IMAGE_TAG="${HELM_TEST_IMAGE_TAG:-helm-test}"
HELM_TEST_NAMESPACE="${HELM_TEST_NAMESPACE:-container-image-exporter}"
HELM_MONITORING_NS="${HELM_MONITORING_NS:-monitoring}"

cleanup() {
    echo "==> Cleaning up"
    kill "${PF_PID:-}" 2>/dev/null || true
    "${K3D}" cluster delete "${HELM_TEST_CLUSTER}" 2>/dev/null || true
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

echo "==> Creating k3d cluster"
"${K3D}" cluster delete "${HELM_TEST_CLUSTER}" 2>/dev/null || true
"${K3D}" cluster create "${HELM_TEST_CLUSTER}" --no-lb --wait
until kubectl get serviceaccount default -n default >/dev/null 2>&1; do sleep 1; done

# ---------------------------------------------------------------------------
# Image
# ---------------------------------------------------------------------------

echo "==> Building and importing image"
docker build -t "${HELM_TEST_IMAGE_NAME}:${HELM_TEST_IMAGE_TAG}" .
"${K3D}" image import "${HELM_TEST_IMAGE_NAME}:${HELM_TEST_IMAGE_TAG}" -c "${HELM_TEST_CLUSTER}"

# ---------------------------------------------------------------------------
# Prometheus Operator
# ---------------------------------------------------------------------------

echo "==> Installing kube-prometheus-stack"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update
helm install prometheus prometheus-community/kube-prometheus-stack \
    --namespace "${HELM_MONITORING_NS}" --create-namespace \
    --set grafana.enabled=false \
    --set alertmanager.enabled=false \
    --set prometheusOperator.admissionWebhooks.enabled=false \
    --set prometheusOperator.admissionWebhooks.patch.enabled=false \
    --set prometheusOperator.tls.enabled=false \
    --set prometheus-node-exporter.enabled=false \
    --set kubeStateMetrics.enabled=false \
    --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
    --wait --timeout=10m

# ---------------------------------------------------------------------------
# Chart
# ---------------------------------------------------------------------------

echo "==> Installing chart"
helm install container-image-exporter ./deploy/chart \
    --namespace "${HELM_TEST_NAMESPACE}" --create-namespace \
    --set image.repository="${HELM_TEST_IMAGE_NAME}" \
    --set image.tag="${HELM_TEST_IMAGE_TAG}" \
    --set image.pullPolicy=Never \
    --set exporter.serviceMonitor.enabled=true \
    --set nodeExporter.serviceMonitor.enabled=true \
    --set nodeExporter.criSocket=/run/k3s/containerd/containerd.sock \
    --set grafana.dashboards.enabled=true

echo "==> Waiting for exporter Deployment to be ready"
kubectl rollout status deployment \
    -n "${HELM_TEST_NAMESPACE}" --timeout=2m

echo "==> Waiting for node-exporter DaemonSet to be ready"
kubectl rollout status daemonset \
    -n "${HELM_TEST_NAMESPACE}" --timeout=2m

# ---------------------------------------------------------------------------
# Verify metrics
# ---------------------------------------------------------------------------

echo "==> Checking Grafana dashboard ConfigMaps"
COUNT=$(kubectl get configmap -n "${HELM_TEST_NAMESPACE}" \
    -l grafana_dashboard=1 -o name | wc -l | tr -d ' ')
[ "${COUNT}" -eq 2 ] || { echo "FAIL: expected 2 dashboard ConfigMaps, got ${COUNT}"; exit 1; }
echo "  ${COUNT} dashboard ConfigMaps found OK"

echo "==> Waiting for Prometheus pod to be ready"
kubectl wait -n "${HELM_MONITORING_NS}" pod \
    -l app.kubernetes.io/name=prometheus \
    --for=condition=Ready \
    --timeout=5m

echo "==> Port-forwarding Prometheus"
kubectl port-forward -n "${HELM_MONITORING_NS}" svc/prometheus-operated 9090:9090 &
PF_PID=$!
until curl -sf http://localhost:9090/-/ready >/dev/null 2>&1; do sleep 2; done

wait_for_metric() {
    local metric="$1"
    echo "==> Checking ${metric}"
    for i in $(seq 1 30); do
        if curl -sf "http://localhost:9090/api/v1/query?query=${metric}" \
                | grep -q '"result":\[{'; then
            echo "  ${metric} OK"
            return 0
        fi
        echo "  attempt ${i}/30, retrying in 10s..."
        sleep 10
    done
    echo "FAIL: ${metric} not found in Prometheus"
    return 1
}

wait_for_metric container_image_up
wait_for_metric container_image_container_info
wait_for_metric container_image_node_exporter_up
wait_for_metric container_image_container_os_info

echo "==> All metrics verified"
