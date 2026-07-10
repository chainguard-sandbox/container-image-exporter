#!/usr/bin/env bash
set -euo pipefail

K3D="${K3D:-k3d}"
ORG="${ORG:-}"
DEMO_CLUSTER="${DEMO_CLUSTER:-cie-demo}"
DEMO_NAMESPACE="${DEMO_NAMESPACE:-container-image-exporter}"
HELM_MONITORING_NS="${HELM_MONITORING_NS:-monitoring}"

# ttl.sh is an anonymous, public registry where image names must be unique
# per push and the tag is the TTL. The image is auto-deleted when the TTL
# expires (here: 24h), so a per-run UUID is fine.
DEMO_IMAGE_REPO="${DEMO_IMAGE_REPO:-ttl.sh/container-image-exporter-$(uuidgen | tr 'A-Z' 'a-z')}"
DEMO_IMAGE_TAG="${DEMO_IMAGE_TAG:-24h}"
DEMO_IMAGE_REF="${DEMO_IMAGE_REPO}:${DEMO_IMAGE_TAG}"

if [[ -z "${ORG}" ]]; then
    echo "error: ORG env var is required (your cgr.dev org slug, e.g. 'my-org.com')" >&2
    echo "       Run: make demo ORG=<org>" >&2
    exit 1
fi

cleanup() {
    echo "==> Cleaning up"
    "${K3D}" cluster delete "${DEMO_CLUSTER}" 2>/dev/null || true
    rm -f /tmp/cgr-registries.yaml
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# Auth
# ---------------------------------------------------------------------------

echo "==> Fetching cgr.dev token"
CGR_TOKEN="$(chainctl auth token --audience=cgr.dev)"

cat > /tmp/cgr-registries.yaml <<EOF
configs:
  "cgr.dev":
    auth:
      username: _token
      password: ${CGR_TOKEN}
EOF

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

echo "==> Creating k3d cluster"
"${K3D}" cluster delete "${DEMO_CLUSTER}" 2>/dev/null || true
"${K3D}" cluster create "${DEMO_CLUSTER}" \
    --no-lb --wait \
    --registry-config /tmp/cgr-registries.yaml
until kubectl get serviceaccount default -n default >/dev/null 2>&1; do sleep 1; done

# ---------------------------------------------------------------------------
# Image
# ---------------------------------------------------------------------------

echo "==> Building and pushing image to ${DEMO_IMAGE_REF}"
docker build --push -t "${DEMO_IMAGE_REF}" .

# ---------------------------------------------------------------------------
# cert-manager
# ---------------------------------------------------------------------------

echo "==> Installing cert-manager"
helm install cert-manager \
    "oci://cgr.dev/${ORG}/charts/cert-manager" \
    --namespace cert-manager --create-namespace \
    --set crds.enabled=true \
    --wait --timeout=5m

# ---------------------------------------------------------------------------
# kube-prometheus-stack
# ---------------------------------------------------------------------------

echo "==> Installing kube-prometheus-stack"
helm install prometheus \
    "oci://cgr.dev/${ORG}/charts/kube-prometheus-stack" \
    --namespace "${HELM_MONITORING_NS}" --create-namespace \
    --set prometheusOperator.admissionWebhooks.enabled=false \
    --set prometheusOperator.admissionWebhooks.patch.enabled=false \
    --set prometheusOperator.tls.enabled=false \
    --set alertmanager.enabled=false \
    --set nodeExporter.enabled=false \
    --set kubeStateMetrics.enabled=false \
    --set prometheus.prometheusSpec.image.tag="" \
    --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
    --wait --timeout=10m

# ---------------------------------------------------------------------------
# Chart
# ---------------------------------------------------------------------------

echo "==> Creating docker config secret for cgr.dev"
kubectl create namespace "${DEMO_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret docker-registry cgr-docker-config \
    --namespace "${DEMO_NAMESPACE}" \
    --docker-server=cgr.dev \
    --docker-username=_token \
    --docker-password="${CGR_TOKEN}" \
    --dry-run=client -o yaml | kubectl apply -f -

echo "==> Installing cluster-exporter chart"
helm install container-image-exporter-cluster ./deploy/charts/cluster-exporter \
    --namespace "${DEMO_NAMESPACE}" --create-namespace \
    --set image.repository="${DEMO_IMAGE_REPO}" \
    --set image.tag="${DEMO_IMAGE_TAG}" \
    --set serviceMonitor.enabled=true \
    --set grafana.dashboards.enabled=true \
    --set "volumes[0].name=docker-config" \
    --set "volumes[0].secret.secretName=cgr-docker-config" \
    --set "volumes[0].secret.items[0].key=.dockerconfigjson" \
    --set "volumes[0].secret.items[0].path=config.json" \
    --set "volumeMounts[0].name=docker-config" \
    --set "volumeMounts[0].mountPath=/home/nonroot/.docker" \
    --set "volumeMounts[0].readOnly=true" \
    --wait --timeout=5m

echo "==> Installing node-exporter chart"
helm install container-image-exporter-node ./deploy/charts/node-exporter \
    --namespace "${DEMO_NAMESPACE}" --create-namespace \
    --set image.repository="${DEMO_IMAGE_REPO}" \
    --set image.tag="${DEMO_IMAGE_TAG}" \
    --set serviceMonitor.enabled=true \
    --set criSocket=/run/k3s/containerd/containerd.sock \
    --set grafana.dashboards.enabled=true \
    --wait --timeout=5m

# ---------------------------------------------------------------------------
# Access instructions
# ---------------------------------------------------------------------------

GRAFANA_SECRET=$(kubectl get secret -n "${HELM_MONITORING_NS}" \
    -l "app.kubernetes.io/name=grafana" -o jsonpath="{.items[0].metadata.name}")
GRAFANA_PASSWORD="$(kubectl get secret -n "${HELM_MONITORING_NS}" "${GRAFANA_SECRET}" \
    -o jsonpath="{.data.admin-password}" | base64 -d)"

echo ""
echo "==> Demo ready."
echo ""
echo "    Prometheus:"
echo "      kubectl port-forward -n ${HELM_MONITORING_NS} svc/prometheus-operated 9090:9090"
echo "      http://localhost:9090"
echo ""
echo "    Grafana (admin / ${GRAFANA_PASSWORD}):"
echo "      kubectl port-forward -n ${HELM_MONITORING_NS} svc/${GRAFANA_SECRET} 3000:80"
echo "      http://localhost:3000"
echo ""
echo "Press Ctrl+C to tear down the cluster."

while true; do sleep 60; done
