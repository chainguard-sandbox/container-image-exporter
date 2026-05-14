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
EXPECTED_VERSION=$(git describe --tags --always --dirty)
EXPECTED_COMMIT=$(git rev-parse --short HEAD)
make docker DOCKER_IMAGE="${HELM_TEST_IMAGE_NAME}:${HELM_TEST_IMAGE_TAG}"
"${K3D}" image import "${HELM_TEST_IMAGE_NAME}:${HELM_TEST_IMAGE_TAG}" -c "${HELM_TEST_CLUSTER}"

# Pick an OCI label that we know is set on the base image
# (cgr.dev/chainguard/static) and is inherited by our build, so we have a
# concrete (key, value) pair to assert against node_image_labels.
EXPECTED_LABEL_KEY="org.opencontainers.image.vendor"
EXPECTED_LABEL_VALUE=$(docker image inspect "${HELM_TEST_IMAGE_NAME}:${HELM_TEST_IMAGE_TAG}" \
    --format "{{index .Config.Labels \"${EXPECTED_LABEL_KEY}\"}}")
if [ -z "${EXPECTED_LABEL_VALUE}" ]; then
    echo "FAIL: image has no ${EXPECTED_LABEL_KEY} label; cannot assert node_image_labels content"
    echo "      Update EXPECTED_LABEL_KEY in $0 to a label the image actually carries."
    exit 1
fi
echo "  ${EXPECTED_LABEL_KEY}=${EXPECTED_LABEL_VALUE}"

# Docker's image .Id is the OCI config blob digest — the same value the CRI
# ImageService returns as Image.id and our node_image_* metrics key on.
# Assumes a single-platform image (what `docker build` produces by default);
# a multi-arch index would expose the index digest here instead.
IMAGE_ID=$(docker image inspect "${HELM_TEST_IMAGE_NAME}:${HELM_TEST_IMAGE_TAG}" --format='{{.Id}}')
[ -n "${IMAGE_ID}" ] || { echo "FAIL: docker image inspect returned no .Id"; exit 1; }
echo "  image_id=${IMAGE_ID}"

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

echo "==> Running helm test (in-cluster health check)"
helm test container-image-exporter -n "${HELM_TEST_NAMESPACE}"

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

prom_query() {
    curl -sf --get "http://localhost:9090/api/v1/query" \
        --data-urlencode "query=$1"
}

wait_for_query() {
    local desc="$1"
    local expr="$2"
    local attempts=60
    echo "==> Checking: ${desc}"
    for i in $(seq 1 "${attempts}"); do
        if prom_query "${expr}" | grep -q '"result":\[{'; then
            echo "  OK"
            return 0
        fi
        echo "  attempt ${i}/${attempts}, retrying in 2s..."
        sleep 2
    done
    echo "FAIL: ${desc}"
    echo "  Query: ${expr}"
    return 1
}

wait_for_query "exporter up" 'container_image_up'
wait_for_query "node-exporter up" 'container_image_node_exporter_up'
wait_for_query "exporter build_info carries the build-arg VERSION and COMMIT" \
    "container_image_exporter_build_info{version=\"${EXPECTED_VERSION}\",revision=\"${EXPECTED_COMMIT}\"}"
wait_for_query "node-exporter build_info carries the build-arg VERSION and COMMIT" \
    "container_image_node_exporter_build_info{version=\"${EXPECTED_VERSION}\",revision=\"${EXPECTED_COMMIT}\"}"
wait_for_query "exporter observed its own pod image" \
    "container_image_container_info{image=\"${HELM_TEST_IMAGE_NAME}:${HELM_TEST_IMAGE_TAG}\"}"
wait_for_query "node-exporter resolved wolfi os-release from /proc/<pid>/root" \
    "container_image_node_container_info{image=\"${HELM_TEST_IMAGE_NAME}:${HELM_TEST_IMAGE_TAG}\",os_id=\"wolfi\"}"
wait_for_query "node-exporter parsed ${EXPECTED_LABEL_KEY} from the image config" \
    "container_image_node_image_labels{image_id=\"${IMAGE_ID}\",key=\"${EXPECTED_LABEL_KEY}\",value=\"${EXPECTED_LABEL_VALUE}\"}"
wait_for_query "node-exporter parsed image creation timestamp" \
    "container_image_node_image_created{image_id=\"${IMAGE_ID}\"}"
# node_container_info must report the same image_id as node_image_* for the
# dashboards' on(image_id) joins to resolve.
wait_for_query "node-exporter reports the same image_id on node_container_info" \
    "container_image_node_container_info{image_id=\"${IMAGE_ID}\"}"

echo "==> All metrics verified"
