package nodeexporter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

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

// PIDInfo holds the init process ID of a running container as reported by the
// CRI runtime via ContainerStatus verbose info.
type PIDInfo struct {
	PID int
}

// ContainerInfo holds the relevant fields for a running container on this node.
type ContainerInfo struct {
	// ID is the CRI/containerd container ID.
	ID string

	// PID is the container's init process ID on the node. It is nil when the
	// CRI runtime did not report one. Callers must check for nil before use.
	PID *PIDInfo

	// UserSpecifiedImage is the image reference as specified by the user
	// (e.g. "cgr.dev/chainguard/nginx:latest").
	UserSpecifiedImage string

	// Image is the imageID or imageDigest
	Image string

	PodName       string
	PodNamespace  string
	ContainerName string
}

// CRIClient wraps the CRI RuntimeService gRPC client.
type CRIClient struct {
	runtime runtimeapi.RuntimeServiceClient
}

// NewCRIClient creates a CRIClient from an existing gRPC connection.
func NewCRIClient(conn *grpc.ClientConn) *CRIClient {
	return &CRIClient{runtime: runtimeapi.NewRuntimeServiceClient(conn)}
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

		pidInfo, err := containerPID(ctx, c.runtime, ctr.Id)
		if err != nil {
			slog.Warn("fetching container PID", "container_id", ctr.Id, "err", err)
		}

		containers = append(containers, &ContainerInfo{
			ID:                 ctr.Id,
			PID:                pidInfo,
			UserSpecifiedImage: userSpecifiedImage,
			Image:              image,
			PodName:            podName,
			PodNamespace:       podNamespace,
			ContainerName:      containerName,
		})
	}
	return containers, nil
}

// containerPID returns the init PID of the container by calling ContainerStatus
// with verbose=true and parsing the runtime info JSON. Returns nil if the
// runtime did not include PID information (e.g. the "info" key is absent).
//
// The "pid" key in the Info map is a de facto standard across containerd,
// CRI-O, and other CRI runtimes — it is not part of the formal CRI spec but is
// universally present in practice.
func containerPID(ctx context.Context, rt runtimeapi.RuntimeServiceClient, id string) (*PIDInfo, error) {
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
		PID int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("parsing info JSON: %w", err)
	}
	return &PIDInfo{PID: v.PID}, nil
}
