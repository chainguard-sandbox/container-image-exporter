package nodeexporter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	// Standard labels set by kubelet on CRI containers.
	labelPodName       = "io.kubernetes.pod.name"
	labelPodNamespace  = "io.kubernetes.pod.namespace"
	labelContainerName = "io.kubernetes.container.name"

	// The pause/infra container has this container name.
	pauseContainerName = "POD"
)

// ContainerInfo holds the relevant fields for a running container on this node.
type ContainerInfo struct {
	// ID is the CRI/containerd container ID.
	ID string

	// PID is the container's init process ID on the node. It is nil when the
	// CRI runtime did not report one. A value of 0 indicates the kernel
	// swapper process; callers should skip both cases.
	PID *int

	// UserSpecifiedImage is the image reference as specified by the user
	// (e.g. "cgr.dev/chainguard/nginx:latest").
	UserSpecifiedImage string

	// Image is the imageID or imageDigest
	Image string

	// ImageRef is the CRI Container.image_ref field — the digested reference
	// to the image in use (e.g. "sha256:abc123").
	ImageRef string

	PodName       string
	PodNamespace  string
	ContainerName string
}

// Image represents an image known to the local CRI runtime, enriched with
// metadata from CRI ImageStatus(verbose=true).
type Image struct {
	// ID is the runtime's internal identifier — typically the config blob's
	// sha256 digest (e.g. "sha256:abc123…").
	ID string

	// Labels are the OCI image config labels, sourced from the runtime's
	// verbose info JSON. Nil when the runtime didn't report any.
	Labels map[string]string

	// Created is the image creation timestamp, sourced from the runtime's
	// verbose info JSON. Nil when the runtime didn't report it.
	Created *time.Time
}

// CRIClient wraps the CRI RuntimeService and ImageService gRPC clients.
type CRIClient struct {
	runtime runtimeapi.RuntimeServiceClient
	image   runtimeapi.ImageServiceClient
}

// NewCRIClient creates a CRIClient from an existing gRPC connection. CRI
// runtimes typically expose both the RuntimeService and ImageService on the
// same socket, so a single connection is sufficient.
func NewCRIClient(conn *grpc.ClientConn) *CRIClient {
	return &CRIClient{
		runtime: runtimeapi.NewRuntimeServiceClient(conn),
		image:   runtimeapi.NewImageServiceClient(conn),
	}
}

// ListRunningContainers returns all running, non-pause containers on this node.
// Pod metadata (name, namespace, container name) is sourced from the kubelet
// labels that containerd sets on each container via the CRI.
func (c *CRIClient) ListRunningContainers(ctx context.Context) ([]*ContainerInfo, error) {
	resp, err := c.runtime.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{
			State: &runtimeapi.ContainerStateValue{
				State: runtimeapi.ContainerState_CONTAINER_RUNNING,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers via CRI: %w", err)
	}

	containers := make([]*ContainerInfo, 0, len(resp.Containers))
	for _, ctr := range resp.Containers {
		podName := ctr.Labels[labelPodName]
		podNamespace := ctr.Labels[labelPodNamespace]
		containerName := ctr.Labels[labelContainerName]

		// Skip infrastructure/pause containers and containers missing pod metadata.
		if containerName == pauseContainerName || podName == "" || podNamespace == "" {
			continue
		}

		var userSpecifiedImage, image string
		if ctr.Image != nil {
			userSpecifiedImage = ctr.Image.UserSpecifiedImage
			image = ctr.Image.Image
		}

		pid, err := containerPID(ctx, c.runtime, ctr.Id)
		if err != nil {
			slog.Warn("fetching container PID", "container_id", ctr.Id, "err", err)
		}

		containers = append(containers, &ContainerInfo{
			ID:                 ctr.Id,
			PID:                pid,
			UserSpecifiedImage: userSpecifiedImage,
			Image:              image,
			ImageRef:           ctr.ImageRef,
			PodName:            podName,
			PodNamespace:       podNamespace,
			ContainerName:      containerName,
		})
	}
	return containers, nil
}

// ListImages returns every image cached by the local CRI runtime, enriched
// with labels and creation timestamps from ImageStatus(verbose=true).
//
// Images for which ImageStatus fails (e.g. the image was removed between
// calls) or returns an empty ID are silently skipped — only OCI labels and
// the creation timestamp are emitted downstream and both come from
// ImageStatus's verbose info, so a bare ID alone has no consumer. A
// failure of the underlying ListImages RPC is returned to the caller.
func (c *CRIClient) ListImages(ctx context.Context) ([]*Image, error) {
	resp, err := c.image.ListImages(ctx, &runtimeapi.ListImagesRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing images via CRI: %w", err)
	}

	out := make([]*Image, 0, len(resp.Images))
	for _, img := range resp.Images {
		enriched, err := c.ImageStatus(ctx, img.Id)
		if err != nil {
			slog.Warn("fetching image status", "image_id", img.Id, "err", err)
			continue
		}
		if enriched == nil || enriched.ID == "" {
			continue
		}
		out = append(out, enriched)
	}
	return out, nil
}

// ImageStatus returns the Image for the given imageID, enriched from CRI
// ImageStatus(verbose=true). An empty imageID, or a runtime that responds
// with no Image (resp.Image == nil or empty Id) short-circuits to a
// zero-value Image with no error — so callers don't emit metrics keyed by
// IDs the runtime never confirmed.
func (c *CRIClient) ImageStatus(ctx context.Context, imageID string) (*Image, error) {
	if imageID == "" {
		return &Image{}, nil
	}
	resp, err := c.image.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{
		Image:   &runtimeapi.ImageSpec{Image: imageID},
		Verbose: true,
	})
	if err != nil {
		return nil, fmt.Errorf("image status: %w", err)
	}

	if resp.Image == nil || resp.Image.Id == "" {
		return &Image{}, nil
	}
	meta := parseImageInfo(resp.Info)
	return &Image{
		ID:      resp.Image.Id,
		Labels:  meta.Labels,
		Created: meta.Created,
	}, nil
}

// containerPID returns the init PID of the container by calling ContainerStatus
// with verbose=true and parsing the runtime info JSON. Returns nil if the
// runtime did not include PID information (e.g. the "info" key is absent).
//
// The "pid" key in the Info map is a de facto standard across containerd,
// CRI-O, and other CRI runtimes — it is not part of the formal CRI spec but is
// universally present in practice.
func containerPID(ctx context.Context, rt runtimeapi.RuntimeServiceClient, id string) (*int, error) {
	resp, err := rt.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{
		ContainerId: id,
		Verbose:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("container status: %w", err)
	}
	raw, ok := resp.Info["info"]
	if !ok {
		return nil, nil
	}
	var v struct {
		PID *int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("parsing info JSON: %w", err)
	}
	return v.PID, nil
}

// ociImageConfig is the subset of the OCI image config we care about. The
// field tags match the canonical OCI image-spec layout
// (https://github.com/opencontainers/image-spec/blob/main/config.md): `created`
// at the top level, and `Labels` (capitalized) under `config`.
type ociImageConfig struct {
	Created *time.Time `json:"created"`
	Config  struct {
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
}

// imageMetadata is the parsed result of the runtime-specific verbose info JSON.
type imageMetadata struct {
	Labels  map[string]string
	Created *time.Time
}

// parseImageInfo extracts labels and the created timestamp from the verbose
// info map returned by ImageStatus. Returns a zero-value imageMetadata when the
// runtime did not populate any recognised field — never returns an error so
// that an unfamiliar JSON shape degrades gracefully rather than failing the
// whole scrape.
//
// The shape of this JSON varies between runtimes; this function tolerates the
// two most common shapes:
//
//   - containerd: {"chainID": "…", "imageSpec": {OCI image config}}
//   - CRI-O:      OCI image config-shaped object at the top level
func parseImageInfo(verbose map[string]string) imageMetadata {
	raw, ok := verbose["info"]
	if !ok || raw == "" {
		return imageMetadata{}
	}
	var out imageMetadata

	// containerd shape: {"chainID": "…", "imageSpec": <OCI image config>}
	var ctd struct {
		ImageSpec ociImageConfig `json:"imageSpec"`
	}
	if err := json.Unmarshal([]byte(raw), &ctd); err == nil {
		if ctd.ImageSpec.Created != nil {
			t := *ctd.ImageSpec.Created
			out.Created = &t
		}
		if len(ctd.ImageSpec.Config.Labels) > 0 {
			out.Labels = ctd.ImageSpec.Config.Labels
		}
	}

	// CRI-O / generic shape: OCI image config at top level. Only fill in
	// fields that the containerd shape didn't already supply.
	if out.Created == nil || out.Labels == nil {
		var top ociImageConfig
		if err := json.Unmarshal([]byte(raw), &top); err == nil {
			if out.Created == nil && top.Created != nil {
				t := *top.Created
				out.Created = &t
			}
			if out.Labels == nil && len(top.Config.Labels) > 0 {
				out.Labels = top.Config.Labels
			}
		}
	}

	return out
}
