# Exporter

A cluster-wide Deployment that watches Kubernetes resources and fetches image
metadata from remote registries.

Doesn't require host-level access to containers.

```mermaid
graph TB
    subgraph "Kubernetes Cluster"
        POD[Pods]
        DEPLOY[Deployments]
        STS[StatefulSets]
        DS[DaemonSets]
        JOB[Jobs]
        CRON[CronJobs]
        CRD[Tekton / Knative / Argo CRDs]
    end

    subgraph "Container Image Exporter"
        RECONCILER[ContainerImageReconciler]
        CACHE[ContainerImageCache<br/>Caches image metadata]
        EXPORTER[Prometheus Exporter<br/>:8080/metrics]
    end

    REGISTRY[Container Registry<br/>Docker Hub, GCR, ECR, etc.]
    PROM[Prometheus<br/>Scraper]

    POD --> RECONCILER
    DEPLOY --> RECONCILER
    STS --> RECONCILER
    DS --> RECONCILER
    JOB --> RECONCILER
    CRON --> RECONCILER
    CRD --> RECONCILER

    RECONCILER -->|Fetch metadata| REGISTRY
    REGISTRY -->|Image metadata| RECONCILER
    RECONCILER -->|Cache image metadata| CACHE

    EXPORTER -->|Query image metadata| CACHE
    EXPORTER -->|List resources| RECONCILER
    PROM -->|Scrape metrics| EXPORTER

    style CACHE fill:#e1f5ff
    style RECONCILER fill:#fff3e1
    style EXPORTER fill:#f3e1ff
    style REGISTRY fill:#ffe1e1
```

## Installation

Build and push the image:

```
make docker DOCKER_IMAGE=your.registry/container-image-exporter:latest
docker push your.registry/container-image-exporter:latest
```

To deploy only the exporter (with the node-exporter DaemonSet disabled):

```
helm install container-image-exporter ./deploy/chart \
    --namespace container-image-exporter \
    --create-namespace \
    --set image.repository=your.registry/container-image-exporter \
    --set image.tag=latest \
    --set nodeExporter.enabled=false
```

If you are using the Prometheus Operator, enable the ServiceMonitor:

```
--set exporter.serviceMonitor.enabled=true
```

If you are using Grafana (via kube-prometheus-stack), enable automatic
dashboard provisioning:

```
--set grafana.dashboards.enabled=true
```

See the [installation instructions](../README.md#installation) for
more details.

## Metrics

| Metric                          | Description                                                                                            | Labels                                                         |
| ------------------------------- | ------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------- |
| container_image_container_info  | Details about containers running in the cluster, including the image digest resolved by the exporter.  | group, version, kind, namespace, name, jsonpath, image, digest |
| container_image_annotation      | Annotations from the image manifest.                                                                   | digest, key, value                                             |
| container_image_label           | Labels from the image config.                                                                          | digest, key, value                                             |
| container_image_size_bytes      | The size of the image in the registry.                                                                 | digest                                                         |
| container_image_created         | The created date from the image config. Expressed as a Unix Epoch Time.                                | digest                                                         |
| container_image_up              | 1 if the last collection completed successfully (all resource types listed), 0 otherwise.              | —                                                              |
| container_image_registry_requests_total | Count of HTTP requests issued to container registries. `code="0"` indicates a transport-level error (no HTTP response). | host, method, code                                             |
| container_image_registry_request_duration_seconds | Histogram of HTTP request latency to container registries.                                  | host, method, code                                             |
| container_image_exporter_build_info | Always 1; labels carry the build version, git commit, and Go runtime version.                       | version, commit, goversion                                     |

## Example Queries

See the [Grafana dashboards](../deploy/chart/dashboards) for more complete
examples of how to consume the metrics.

### Percentage of Containers Based on Chainguard

Unless they are specifically overwritten, Docker will persist the labels from
the base image in the images it builds. This means you can use those labels to
infer various things about the images defined in your clusters.

For instance, you can use the presence of any of `dev.chainguard.image.title`,
`dev.chainguard.package.main`, or `org.opencontainers.image.vendor="Chainguard"`
to calculate the percentage of containers defined in the cluster that are
using Chainguard images or images based on Chainguard.

```
  (
      count(
        container_image_container_info{kind!="Pod"}
        * on (digest) group_left
          count by (digest) (
              container_image_label{key="dev.chainguard.image.title"}
            or
              container_image_label{key="dev.chainguard.package.main"}
            or
              container_image_label{key="org.opencontainers.image.vendor", value="Chainguard"}
          )
      )
    /
      count(
        container_image_container_info{kind!="Pod"}
      )
  )
*
  100
```

This query excludes pods so that it is measuring the container specs that are
directly configured by the user (i.e Deployments, StatefulSets, CronJobs).

This is typically a better way to measure the number of 'applications that
still need to be migrated to Chainguard' than looking at the running
containers, which can be skewed by, for instance, Deployments or Daemonsets
that run 100s of pods versus a StatefulSet that runs a handful.

### Images Older Than X Number of Days

Frequently rebuilding your images from up to date base images is an effective
way to ensure you are incorporating CVE fixes.

You can use `container_image_created` to identify images that weren't built
recently.

For instance, this query returns series for images that were created more than
14 days ago.

```
    time()
  -
    (
        max by (digest, image) (container_image_container_info)
      * on (digest) group_left ()
        container_image_created
    )
>
  86400 * 14
```

It's worth noting that not all build tools will set the `created` timestamp when
they build an image.

## Configuration

Configuration is expressed through the Helm chart's values. The examples below
use `--set` for brevity; the same keys can be set in a values file passed with
`-f`.

### Credentials

The exporter requires registry credentials to fetch metadata from privately
hosted images.

#### Ambient Credentials 

The exporter automatically loads credentials from the environment. That includes
ambient cloud provider credentials (AWS, GCP, Azure) as well as credentials
configured locally in `~/.docker/config.json`.

#### Pull Secrets

Create pull secrets in the release namespace and reference them by name. The
exporter looks them up in the namespace the chart is installed into.

```
--set 'exporter.imagePullSecrets[0]=my-registry' \
--set 'exporter.imagePullSecrets[1]=other-registry'
```

#### Cluster Wide Pull Secrets

Disabled by default. When enabled, the exporter fetches credentials from the
pull secrets configured for the target resources in the same way the kubelet
does. Requires cluster-wide access to Secrets and ServiceAccounts.

```
--set exporter.k8sKeychain=true
```

### Cache Duration

To reduce the number of requests made to upstream registries, the exporter
caches the response for each image for a configurable amount of time. The
default is 1 hour.

```
--set exporter.cacheDuration=6h
```

### Multi-Architecture Images

When the exporter encounters a multi-architecture image it resolves it to the
platform the exporter pod is running on (e.g. `linux/amd64`), or if that
platform is absent, the first image in the index. In general, images in the
same index share the same or very similar metadata, so the metrics returned are
typically still representative even if they aren't from the exact platform
deployed to a node.

Override the default platform with:

```
--set exporter.platform=linux/arm64
```

## Supported Resources

The exporter watches the following resource types and extracts container image
references from each.

### Built-in

| Group  | Resource      | Container paths                                                        |
| ------ | ------------- | ---------------------------------------------------------------------- |
| core   | Pod           | `spec.initContainers`, `spec.containers`, `spec.ephemeralContainers`   |
| apps   | Deployment    | `spec.template.spec.initContainers`, `spec.template.spec.containers`   |
| apps   | StatefulSet   | `spec.template.spec.initContainers`, `spec.template.spec.containers`   |
| apps   | DaemonSet     | `spec.template.spec.initContainers`, `spec.template.spec.containers`   |
| batch  | Job           | `spec.template.spec.initContainers`, `spec.template.spec.containers`   |
| batch  | CronJob       | `spec.jobTemplate.spec.template.spec.initContainers`, `spec.jobTemplate.spec.template.spec.containers` |

### CRDs (auto-discovered)

The following CRD types are watched automatically if they are installed in the
cluster. No configuration is required — the exporter queries the API server's
discovery endpoint at startup to detect which of these are available.

However, you will need to give the exporter permissions to list the resources
(see below).

| Group                | Resource              | Container paths                                                                                    |
| -------------------- | --------------------- | -------------------------------------------------------------------------------------------------- |
| tekton.dev           | Task                  | `spec.steps`, `spec.sidecars`                                                                      |
| tekton.dev           | TaskRun               | `spec.taskSpec.steps`, `spec.taskSpec.sidecars`                                                    |
| serving.knative.dev  | Service               | `spec.template.spec.containers`, `spec.template.spec.initContainers`                               |
| serving.knative.dev  | Revision              | `spec.containers`, `spec.initContainers`                                                           |
| argoproj.io          | Workflow              | `spec.templates[*].container`, `spec.templates[*].script`, `spec.templates[*].initContainers`, `spec.templates[*].sidecars` |
| argoproj.io          | WorkflowTemplate      | `spec.templates[*].container`, `spec.templates[*].script`, `spec.templates[*].initContainers`, `spec.templates[*].sidecars` |
| argoproj.io          | ClusterWorkflowTemplate | `spec.templates[*].container`, `spec.templates[*].script`, `spec.templates[*].initContainers`, `spec.templates[*].sidecars` |
| argoproj.io          | CronWorkflow          | `spec.workflowSpec.templates[*].container`, `spec.workflowSpec.templates[*].script`, `spec.workflowSpec.templates[*].initContainers`, `spec.workflowSpec.templates[*].sidecars` |

### Permissions for CRDs

The chart's default ClusterRole only grants permissions for the built-in
resource types. If any of the CRDs above are installed in your cluster and
you want the exporter to watch them, extend the ClusterRole via
`exporter.rbac.extraRules` in a values file or with `--set`.

For example, to add Tekton, Knative, and Argo Workflows support, use the
following values:

```yaml
exporter:
  rbac:
    extraRules:
      - apiGroups: ["tekton.dev"]
        resources: ["tasks", "taskruns"]
        verbs: ["get", "list", "watch"]
      - apiGroups: ["serving.knative.dev"]
        resources: ["services", "revisions"]
        verbs: ["get", "list", "watch"]
      - apiGroups: ["argoproj.io"]
        resources: ["workflows", "workflowtemplates", "clusterworkflowtemplates", "cronworkflows"]
        verbs: ["get", "list", "watch"]
```

At startup the exporter performs a test `list` against each discovered CRD to
verify it has permission. If the list is denied, that resource type is silently
skipped and a message is logged. The exporter will continue to function
normally for all other resource types.
