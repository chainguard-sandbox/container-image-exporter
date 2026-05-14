# Node Exporter

A DaemonSet that runs on each node and exports per-container OS release
information plus per-image OCI labels and creation timestamps, all sourced
locally from the CRI runtime.

## Metrics

| Metric                              | Description                                                                                                      | Labels                                                                                                                                                                                                            |
| ----------------------------------- | ---------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| container_image_node_container_info | Information about running containers from the local CRI runtime, plus OS release details from `/etc/os-release`. | id, namespace, pod, container, image, image_id, os_build_id, os_id, os_id_like, os_image_id, os_image_version, os_name, os_pretty_name, os_variant, os_variant_id, os_version, os_version_codename, os_version_id |
| container_image_node_image_labels   | Labels from the image config.                                                                                    | image_id, key, value                                                                                                                                                                                              |
| container_image_node_image_created  | The created date from the image config. Expressed as a Unix Epoch Time.                                          | image_id                                                                                                                                                                                                          |
| container_image_node_exporter_up    | 1 if the last collection completed successfully, 0 otherwise.                                                    | —                                                                                                                                                                                                                 |

## Configuration

Configuration is expressed through the Helm chart's values.

### CRI socket

Path to the CRI runtime socket on the host. Defaults to
`/run/containerd/containerd.sock`.

```
--set nodeExporter.criSocket=/run/crio/crio.sock
```

### proc root

The node exporter reads each container's `/etc/os-release` via
`/host/proc/{pid}/root`. If your cluster mounts the host `/proc` at a different
path, override it with:

```
--set nodeExporter.procRoot=/host/proc
```

### Only images in use

By default the node-exporter only reports metrics for images in a running
container. To report every image cached by the local CRI runtime:

```
--set nodeExporter.onlyImagesInUse=false
```

### Label allowlist

Control cardinality by only exporting specific labels: 

```
--set 'nodeExporter.labelAllowlist[0]=org.opencontainers.image.title' \
--set 'nodeExporter.labelAllowlist[1]=org.opencontainers.image.vendor'
```

When unset (the default), every label on every reported image is emitted.

## Example Queries

### Percentage of running containers based on Chainguard or Wolfi

```
  (
      count(container_image_node_container_info{os_id=~"chainguard|wolfi"})
    /
      count(container_image_node_container_info)
  )
*
  100
```

### Images older than X number of days

Frequently rebuilding your images from up to date base images is an effective
way to ensure you are incorporating CVE fixes.

You can use `container_image_node_image_created` to identify running images
that weren't built recently. This query returns series for images that were
created more than 14 days ago.

```
    time()
  -
    (
        max by (image_id, image) (container_image_node_container_info)
      * on (image_id) group_left ()
        container_image_node_image_created
    )
>
  86400 * 14
```

It's worth noting that not all build tools will set the `created` timestamp when
they build an image.

### OS distribution breakdown across running containers

The node-exporter parses `/etc/os-release` from each container's rootfs, so you
can see which OS distributions are actually running across the cluster:

```
count by (os_id) (
  label_replace(container_image_node_container_info, "os_id", "<UNKNOWN>", "os_id", "")
)
```
