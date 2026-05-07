package nodeexporter

import (
	"context"
	"errors"
	"testing"
	"time"

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

	if got := byID["ctr-with-pid"].PID; got == nil || *got != 42 {
		t.Errorf("ctr-with-pid: PID = %v, want pointer to 42", got)
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

// TestListRunningContainers_PIDAbsentInJSON verifies that when the CRI info JSON
// does not contain a "pid" key, ContainerInfo.PID is nil.
func TestListRunningContainers_PIDAbsentInJSON(t *testing.T) {
	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id: "ctr-no-pid-key",
				Labels: map[string]string{
					labelPodName: "pod-a", labelPodNamespace: "ns-a", labelContainerName: "app",
				},
			},
		},
	}
	// Override ContainerStatus to return info JSON with no pid key.
	srv.infoOverride = map[string]string{"ctr-no-pid-key": `{"other":"data"}`}

	conn := startFakeRuntime(t, srv)
	containers, err := NewCRIClient(conn).ListRunningContainers(context.Background())
	if err != nil {
		t.Fatalf("ListRunningContainers: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	if containers[0].PID != nil {
		t.Errorf("PID = %v, want nil when pid key absent from JSON", containers[0].PID)
	}
}

// TestListRunningContainers_PIDZeroInJSON verifies that when the CRI info JSON
// explicitly contains "pid":0, ContainerInfo.PID is a pointer to 0 (not nil).
func TestListRunningContainers_PIDZeroInJSON(t *testing.T) {
	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id: "ctr-pid-zero",
				Labels: map[string]string{
					labelPodName: "pod-b", labelPodNamespace: "ns-b", labelContainerName: "app",
				},
			},
		},
	}
	srv.infoOverride = map[string]string{"ctr-pid-zero": `{"pid":0}`}

	conn := startFakeRuntime(t, srv)
	containers, err := NewCRIClient(conn).ListRunningContainers(context.Background())
	if err != nil {
		t.Fatalf("ListRunningContainers: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	pid := containers[0].PID
	if pid == nil {
		t.Fatal("PID = nil, want pointer to 0 when pid key is explicitly 0 in JSON")
	}
	if *pid != 0 {
		t.Errorf("*PID = %d, want 0", *pid)
	}
}

// TestListRunningContainers_ImageRefDigest verifies that the CRI ImageRef field
// is propagated to ContainerInfo.Digest unchanged.
func TestListRunningContainers_ImageRefDigest(t *testing.T) {
	const imageRef = "registry.example.com/app@sha256:cafebabe"
	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id:       "ctr-ref",
				ImageRef: imageRef,
				Image: &runtimeapi.ImageSpec{
					Image:              "registry.example.com/app@sha256:cafebabe",
					UserSpecifiedImage: "registry.example.com/app:latest",
				},
				Labels: map[string]string{
					labelPodName:       "pod-r",
					labelPodNamespace:  "ns-r",
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
	if got := containers[0].ImageRef; got != imageRef {
		t.Errorf("ImageRef = %q, want %q", got, imageRef)
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

// TestParseImageInfo_Containerd verifies that the containerd verbose info shape
// (`{"chainID": "...", "imageSpec": <OCI image config>}`) is parsed correctly.
func TestParseImageInfo_Containerd(t *testing.T) {
	const raw = `{
		"chainID": "sha256:dead",
		"imageSpec": {
			"created": "2024-01-15T10:00:00Z",
			"architecture": "amd64",
			"os": "linux",
			"config": {
				"Labels": {
					"org.opencontainers.image.version": "1.2.3",
					"org.opencontainers.image.source": "https://example.com/repo"
				}
			}
		}
	}`

	info := parseImageInfo(map[string]string{"info": raw})

	if info.Created == nil {
		t.Fatal("Created = nil, want non-nil")
	}
	want, _ := time.Parse(time.RFC3339, "2024-01-15T10:00:00Z")
	if !info.Created.Equal(want) {
		t.Errorf("Created = %v, want %v", info.Created, want)
	}
	if got := info.Labels["org.opencontainers.image.version"]; got != "1.2.3" {
		t.Errorf("Labels[version] = %q, want %q", got, "1.2.3")
	}
	if got := info.Labels["org.opencontainers.image.source"]; got != "https://example.com/repo" {
		t.Errorf("Labels[source] = %q, want %q", got, "https://example.com/repo")
	}
}

// TestParseImageInfo_OCITopLevel verifies fallback parsing when the verbose
// info JSON is just the OCI image config at the top level (the shape some
// runtimes other than containerd use).
func TestParseImageInfo_OCITopLevel(t *testing.T) {
	const raw = `{
		"created": "2025-06-01T12:34:56Z",
		"config": {
			"Labels": {
				"key1": "value1"
			}
		}
	}`

	info := parseImageInfo(map[string]string{"info": raw})

	if info.Created == nil {
		t.Fatal("Created = nil, want non-nil")
	}
	want, _ := time.Parse(time.RFC3339, "2025-06-01T12:34:56Z")
	if !info.Created.Equal(want) {
		t.Errorf("Created = %v, want %v", info.Created, want)
	}
	if got := info.Labels["key1"]; got != "value1" {
		t.Errorf("Labels[key1] = %q, want %q", got, "value1")
	}
}

// TestParseImageInfo_Empty verifies that missing or empty info returns a
// zero-value imageMetadata and never errors.
func TestParseImageInfo_Empty(t *testing.T) {
	for name, verbose := range map[string]map[string]string{
		"nil-map":  nil,
		"no-info":  {"other": "data"},
		"empty":    {"info": ""},
		"unknown":  {"info": `{"unrelated":"shape"}`},
		"bad-json": {"info": `not json`},
	} {
		t.Run(name, func(t *testing.T) {
			got := parseImageInfo(verbose)
			if got.Created != nil {
				t.Errorf("Created = %v, want nil", got.Created)
			}
			if len(got.Labels) != 0 {
				t.Errorf("Labels = %v, want empty", got.Labels)
			}
		})
	}
}

// TestImageStatus_EmptyImageID verifies that an empty image ID short-circuits
// to a zero-value response without making an RPC.
func TestImageStatus_EmptyImageID(t *testing.T) {
	srv := &fakeRuntimeServer{}
	conn := startFakeRuntime(t, srv)
	got, err := NewCRIClient(conn).ImageStatus(context.Background(), "")
	if err != nil {
		t.Fatalf("ImageStatus: %v", err)
	}
	if got.Created != nil || len(got.Labels) != 0 || got.ID != "" {
		t.Errorf("expected zero-value Image, got %+v", got)
	}
	if srv.lastImageStatusReq != nil {
		t.Errorf("expected no RPC, got %+v", srv.lastImageStatusReq)
	}
}

// TestImageStatus_RuntimeReturnsNoImage verifies that when the runtime
// responds with resp.Image == nil (image disappeared, runtime quirk, etc.)
// ImageStatus returns a zero-value Image rather than fabricating one keyed
// by the input ID — so callers don't emit metrics with unverified IDs.
func TestImageStatus_RuntimeReturnsNoImage(t *testing.T) {
	// Empty fake server — no images registered, so any ImageStatus lookup
	// for a non-empty ID will land on the resp.Image = nil branch.
	srv := &fakeRuntimeServer{}
	conn := startFakeRuntime(t, srv)

	got, err := NewCRIClient(conn).ImageStatus(context.Background(), "sha256:unknown")
	if err != nil {
		t.Fatalf("ImageStatus: %v", err)
	}
	if got.ID != "" {
		t.Errorf("ID = %q, want empty (runtime returned no Image)", got.ID)
	}
	if got.Created != nil || len(got.Labels) != 0 {
		t.Errorf("expected zero-value Image, got %+v", got)
	}
}

// TestListImages_ImageStatusErrorSkipsImage verifies that when ImageStatus
// fails for an individual image, ListImages drops that image entirely
// rather than emitting a bare entry. The remaining images are still
// enriched and returned.
func TestListImages_ImageStatusErrorSkipsImage(t *testing.T) {
	const (
		erroringID = "sha256:erroring"
		workingID  = "sha256:working"
	)
	srv := &fakeRuntimeServer{
		images: []*runtimeapi.Image{
			{Id: erroringID},
			{Id: workingID},
		},
		imageStatusErrors: map[string]error{
			erroringID: errors.New("simulated CRI failure"),
		},
		imageInfo: map[string]string{
			workingID: `{"imageSpec":{"created":"2024-01-01T00:00:00Z","config":{"Labels":{"k":"v"}}}}`,
		},
	}

	conn := startFakeRuntime(t, srv)
	images, err := NewCRIClient(conn).ListImages(context.Background())
	if err != nil {
		t.Fatalf("ListImages returned error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("got %d images, want 1 (erroring image should be skipped)", len(images))
	}
	got := images[0]
	if got.ID != workingID {
		t.Errorf("ID = %q, want %q (erroring image should not be returned)", got.ID, workingID)
	}
	if got.Labels["k"] != "v" {
		t.Errorf("Labels[k] = %q, want %q", got.Labels["k"], "v")
	}
	if got.Created == nil {
		t.Error("Created = nil, want non-nil")
	}
}

// TestListImages_RuntimeReturnsNoImageStatus_Skips verifies that when
// ListImages surfaces an image but ImageStatus then comes back with no
// Image (a TOCTOU between the two RPCs, or a quirky runtime), ListImages
// drops the image — there's nothing useful left to emit for it.
func TestListImages_RuntimeReturnsNoImageStatus_Skips(t *testing.T) {
	const imageID = "sha256:disappeared"
	srv := &fakeRuntimeServer{
		// listImagesResult surfaces the image; images is empty, so
		// ImageStatus will return resp.Image = nil for it.
		listImagesResult: []*runtimeapi.Image{{Id: imageID}},
	}

	conn := startFakeRuntime(t, srv)
	images, err := NewCRIClient(conn).ListImages(context.Background())
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("got %d images, want 0 (image with no ImageStatus should be skipped)", len(images))
	}
}
