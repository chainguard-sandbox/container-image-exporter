# container-image-exporter

> [!WARNING]
> This project is in the early stages of development and may not be suitable
> for production usage. Maintenance is provided on a best-effort basis.

Exports Prometheus metrics about container images in a Kubernetes cluster.

Particularly useful for tracking usage and adoption of Chainguard images across
a cluster based on image or container metadata.

![dashboard](images/dashboard.png)

## Components

The project consists of two components, each of which can be deployed
independently or together.

### Node Exporter ([docs](./docs/node-exporter.md))

A DaemonSet that runs on each node and exports per-container OS release
information plus per-image OCI labels and creation timestamps, all sourced
locally from the CRI runtime.

The most reliable method for inferring whether a running container is
Chainguard-based, but it does need to be ran as root on the host.

```mermaid
graph TB
    subgraph "Kubernetes Node"
        subgraph "Node Exporter (DaemonSet pod)"
            NE[Prometheus Exporter<br/>:8080/metrics]
        end

        CRI[CRI socket]
        PROC[Host /proc<br/>mounted at /host/proc]
    end

    PROM[Prometheus]

    NE -->|"List containers"| CRI
    NE -->|"List images"| CRI
    NE -->|"Read /etc/os-release"| PROC
    PROM -->|Scrape each node| NE

    style NE fill:#e1ffe1
    style CRI fill:#ffe1e1
    style PROC fill:#ffe1e1
```

### Cluster Exporter ([docs](./docs/cluster-exporter.md))

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

For now, you need to build and host the image yourself:

```
make docker DOCKER_IMAGE=your.registry/container-image-exporter:latest
docker push your.registry/container-image-exporter:latest
```

Each component has its own Helm chart. Install one or both, depending on which
components you want to deploy:

- [Node Exporter](./docs/node-exporter.md#installation)
- [Cluster Exporter](./docs/cluster-exporter.md#installation)

## Dashboards

The respective Helm charts contain Grafana dashboards for each component:

- [Node Exporter](./deploy/charts/node-exporter/dashboards)
- [Cluster Exporter](./deploy/charts/cluster-exporter/dashboards)

## Development

### Running the tests

The tests use [envtest](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest)
to run a real Kubernetes API server and etcd in-process, and an in-process
container registry to push and pull images against. You do not need a running
Kubernetes cluster.

The `make test` target installs the required binaries via
[setup-envtest](https://pkg.go.dev/sigs.k8s.io/controller-runtime/tools/envtest/cmd/setup-envtest)
and then runs the suite.

```
make test
```

The binaries are cached in `./bin/` and only downloaded once. To install them
without running tests:

```
make envtest
```

### Node integration tests

The node exporter has an additional integration test suite (build tag
`nodeintegration`) that runs against a real CRI socket. The `make test-node`
target is self-contained: it installs [k3d](https://k3d.io) into `./bin/`,
creates a temporary cluster, deploys a workload, compiles and runs the test
binary inside the cluster node, then tears everything down.

```
make test-node
```

## Demo

The `make demo` target creates a self-contained cluster that demonstrates the
exporters running. 

### Prerequisites

- [Helm](https://helm.sh)
- [Docker](https://docs.docker.com/get-docker/)
- [chainctl](https://edu.chainguard.dev/chainguard/administration/how-to-install-chainctl/) — logged in to your Chainguard organisation
- These Helm charts in your Chainguard organization:
  - `cert-manager` (`oci://cgr.dev/<org>/charts/cert-manager`)
  - `kube-prometheus-stack` (`oci://cgr.dev/<org>/charts/kube-prometheus-stack`)

### Running

```
make demo ORG=<org-name>
```

Once deployed, the script prints port-forward commands and credentials for
Prometheus and Grafana. Open the Grafana dashboards to see the
container-image-exporter metrics visualised in real time.

To stop the demo and clean up the cluster, press Ctrl+C.
