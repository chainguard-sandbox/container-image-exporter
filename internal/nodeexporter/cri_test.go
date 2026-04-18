package nodeexporter

import (
	"context"
	"testing"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// TestListRunningContainers_PID verifies that PID is populated from the fake
// server's pids map and is nil when the server returns no info.
func TestListRunningContainers_PID(t *testing.T) {
	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id:    "ctr-with-pid",
				Image: &runtimeapi.ImageSpec{Image: "img@sha256:1111", UserSpecifiedImage: "img:latest"},
				Labels: map[string]string{
					labelPodName:       "pod-a",
					labelPodNamespace:  "ns-a",
					labelContainerName: "app",
				},
			},
			{
				Id:    "ctr-no-pid",
				Image: &runtimeapi.ImageSpec{Image: "img@sha256:2222", UserSpecifiedImage: "img:latest"},
				Labels: map[string]string{
					labelPodName:       "pod-b",
					labelPodNamespace:  "ns-b",
					labelContainerName: "app",
				},
			},
		},
		pids: map[string]int{"ctr-with-pid": 42},
	}

	conn := startFakeRuntime(t, srv)
	containers, err := NewCRIClient(conn).ListRunningContainers(context.Background())
	if err != nil {
		t.Fatalf("ListRunningContainers: %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers))
	}

	byID := make(map[string]*ContainerInfo, len(containers))
	for _, c := range containers {
		byID[c.ID] = c
	}

	if got := byID["ctr-with-pid"].PID; got == nil || got.PID != 42 {
		t.Errorf("ctr-with-pid: PID = %v, want &PIDInfo{PID: 42}", got)
	}
	if got := byID["ctr-no-pid"].PID; got != nil {
		t.Errorf("ctr-no-pid: PID = %v, want nil", got)
	}
}

// TestListRunningContainers_Filtering verifies that pause containers and
// containers with missing pod metadata labels are excluded from results.
func TestListRunningContainers_Filtering(t *testing.T) {
	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			// Regular container — should be included.
			{
				Id: "ctr-a",
				Image: &runtimeapi.ImageSpec{
					Image:              "registry.example.com/app@sha256:aaaa",
					UserSpecifiedImage: "registry.example.com/app:v1",
				},
				Labels: map[string]string{
					labelPodName:       "pod-a",
					labelPodNamespace:  "ns-a",
					labelContainerName: "app",
				},
			},
			// Pause/infra container — should be skipped.
			{
				Id: "pause-a",
				Image: &runtimeapi.ImageSpec{
					Image:              "registry.k8s.io/pause@sha256:bbbb",
					UserSpecifiedImage: "registry.k8s.io/pause:3.9",
				},
				Labels: map[string]string{
					labelPodName:       "pod-a",
					labelPodNamespace:  "ns-a",
					labelContainerName: pauseContainerName,
				},
			},
			// Missing pod name label — should be skipped.
			{
				Id: "ctr-b",
				Image: &runtimeapi.ImageSpec{
					Image:              "registry.example.com/side@sha256:cccc",
					UserSpecifiedImage: "registry.example.com/side:v1",
				},
				Labels: map[string]string{
					labelPodNamespace:  "ns-a",
					labelContainerName: "side",
				},
			},
			// Missing pod namespace label — should be skipped.
			{
				Id: "ctr-c",
				Image: &runtimeapi.ImageSpec{
					Image:              "registry.example.com/job@sha256:dddd",
					UserSpecifiedImage: "registry.example.com/job:v1",
				},
				Labels: map[string]string{
					labelPodName:       "pod-c",
					labelContainerName: "job",
				},
			},
		},
	}

	conn := startFakeRuntime(t, srv)
	containers, err := NewCRIClient(conn).ListRunningContainers(context.Background())
	if err != nil {
		t.Fatalf("ListRunningContainers: %v", err)
	}

	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}

	c := containers[0]
	if c.ID != "ctr-a" {
		t.Errorf("ID = %q, want %q", c.ID, "ctr-a")
	}
	if c.PodName != "pod-a" {
		t.Errorf("PodName = %q, want %q", c.PodName, "pod-a")
	}
	if c.PodNamespace != "ns-a" {
		t.Errorf("PodNamespace = %q, want %q", c.PodNamespace, "ns-a")
	}
	if c.ContainerName != "app" {
		t.Errorf("ContainerName = %q, want %q", c.ContainerName, "app")
	}
	if c.UserSpecifiedImage != "registry.example.com/app:v1" {
		t.Errorf("UserSpecifiedImage = %q, want %q", c.UserSpecifiedImage, "registry.example.com/app:v1")
	}
	if c.Image != "registry.example.com/app@sha256:aaaa" {
		t.Errorf("ResolvedImage = %q, want %q", c.Image, "registry.example.com/app@sha256:aaaa")
	}
}

// TestListRunningContainers_NilImageField verifies that a container with a nil
// Image field (which containerd may return for containers whose image has been
// garbage-collected) is still included but with empty Image and ResolvedImage fields.
func TestListRunningContainers_NilImageField(t *testing.T) {
	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id:    "ctr-nil-image",
				Image: nil,
				Labels: map[string]string{
					labelPodName:       "pod-x",
					labelPodNamespace:  "ns-x",
					labelContainerName: "app",
				},
			},
		},
	}

	conn := startFakeRuntime(t, srv)
	containers, err := NewCRIClient(conn).ListRunningContainers(context.Background())
	if err != nil {
		t.Fatalf("ListRunningContainers: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}

	c := containers[0]
	if c.UserSpecifiedImage != "" {
		t.Errorf("UserSpecifiedImage: got %q, want empty string when Image field is nil", c.UserSpecifiedImage)
	}
	if c.Image != "" {
		t.Errorf("ResolvedImage: got %q, want empty string when Image field is nil", c.Image)
	}
}

// TestListRunningContainers_RequestsRunningState verifies that the CRI request
// filter is set to CONTAINER_RUNNING so only active containers are returned.
func TestListRunningContainers_RequestsRunningState(t *testing.T) {
	srv := &fakeRuntimeServer{}
	conn := startFakeRuntime(t, srv)

	_, _ = NewCRIClient(conn).ListRunningContainers(context.Background())

	if srv.lastRequest == nil {
		t.Fatal("no request was sent to the CRI server")
	}
	if srv.lastRequest.Filter == nil {
		t.Fatal("request sent with no filter")
	}
	if srv.lastRequest.Filter.State == nil {
		t.Fatal("request filter has no state constraint")
	}
	got := srv.lastRequest.Filter.State.State
	if got != runtimeapi.ContainerState_CONTAINER_RUNNING {
		t.Errorf("filter state = %v, want CONTAINER_RUNNING", got)
	}
}
