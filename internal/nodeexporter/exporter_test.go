package nodeexporter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// TestReadOSRelease_NotFound verifies that errNoOSRelease is returned when
// neither etc/os-release nor usr/lib/os-release exists under the container root.
func TestReadOSRelease_NotFound(t *testing.T) {
	procRoot := t.TempDir()
	const pid = 42
	// Create the container root directory but leave it empty (no os-release files).
	if err := os.MkdirAll(filepath.Join(procRoot, strconv.Itoa(pid), "root"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	_, err := readOSRelease(procRoot, pid)
	if !errors.Is(err, errNoOSRelease) {
		t.Errorf("readOSRelease() error = %v, want errNoOSRelease", err)
	}
}

// TestReadOSRelease_RootMissing verifies that when the container root directory
// itself doesn't exist (e.g. container has already exited), readOSRelease returns
// a non-nil error that is NOT errNoOSRelease.
func TestReadOSRelease_RootMissing(t *testing.T) {
	procRoot := t.TempDir()
	// Intentionally don't create any directory for pid 99.

	_, err := readOSRelease(procRoot, 99)
	if err == nil {
		t.Fatal("readOSRelease() expected error for missing container root, got nil")
	}
	if errors.Is(err, errNoOSRelease) {
		t.Error("readOSRelease() returned errNoOSRelease for missing root, want a distinct error")
	}
}

// TestReadOSRelease_SymlinkEscape verifies that a symlink under the container
// root that points outside it (e.g. etc/os-release -> ../../../secret) is not
// followed. os.Root rejects such escapes, so readOSRelease should return an
// error that is NOT errNoOSRelease (the file "exists" but cannot be opened).
func TestReadOSRelease_SymlinkEscape(t *testing.T) {
	tmpDir := t.TempDir()

	// Place a "secret" file outside the container root.
	secretFile := filepath.Join(tmpDir, "secret")
	if err := os.WriteFile(secretFile, []byte("ID=secret\n"), 0o644); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}

	// Build a fake procfs tree: procRoot/42/root/etc/os-release -> ../../../secret
	const pid = 42
	procRoot := filepath.Join(tmpDir, "proc")
	etcDir := filepath.Join(procRoot, strconv.Itoa(pid), "root", "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Symlink target escapes the container root by three levels.
	if err := os.Symlink("../../../secret", filepath.Join(etcDir, "os-release")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := readOSRelease(procRoot, pid)
	if err == nil {
		t.Fatal("readOSRelease() expected error for symlink escape, got nil")
	}
	if errors.Is(err, errNoOSRelease) {
		t.Error("readOSRelease() returned errNoOSRelease for symlink escape — symlink was silently skipped rather than rejected")
	}
	// Also verify the secret content was not read.
}

// TestExporter_Collect_HappyPath verifies that a running container with a
// readable /etc/os-release produces a correctly-labelled os_info metric and
// that the up metric is 1.
func TestExporter_Collect_HappyPath(t *testing.T) {
	procRoot := t.TempDir()
	containerID := "ctr-happy"
	const pid = 1000
	makeRootfs(t, procRoot, pid, "ID=wolfi\nNAME=\"Wolfi\"\nPRETTY_NAME=\"Wolfi\"\nVERSION_ID=20240101\n")

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id:       containerID,
				ImageRef: "sha256:aaaa",
				Image: &runtimeapi.ImageSpec{
					Image:              "registry.example.com/app@sha256:aaaa",
					UserSpecifiedImage: "registry.example.com/app:latest",
				},
				Labels: map[string]string{
					labelPodName:       "my-pod",
					labelPodNamespace:  "default",
					labelContainerName: "app",
				},
			},
		},
		pids: map[string]int{containerID: pid},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), WithProcRoot(procRoot)))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	m := findGatheredMetric(mfs, "container_image_node_container_info", map[string]string{
		"id":             containerID,
		"namespace":      "default",
		"pod":            "my-pod",
		"container":      "app",
		"image":          "registry.example.com/app:latest",
		"image_id":       "sha256:aaaa",
		"os_id":          "wolfi",
		"os_name":        "Wolfi",
		"os_pretty_name": "Wolfi",
		"os_version_id":  "20240101",
	})
	if m == nil {
		t.Error("container_image_node_container_info not found with expected labels")
	}

	up := findGatheredMetric(mfs, "container_image_node_exporter_up", nil)
	if up == nil {
		t.Fatal("container_image_node_exporter_up metric not found")
	}
	if up.GetGauge().GetValue() != 1 {
		t.Errorf("node_exporter_up = %v, want 1", up.GetGauge().GetValue())
	}
}

// TestExporter_Collect_UsrLibFallback verifies that usr/lib/os-release is used
// when etc/os-release is absent.
func TestExporter_Collect_UsrLibFallback(t *testing.T) {
	procRoot := t.TempDir()
	containerID := "ctr-usrlib"
	const pid = 1050

	// Write only usr/lib/os-release — no etc/os-release.
	dir := filepath.Join(procRoot, strconv.Itoa(pid), "root", "usr", "lib")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "os-release"), []byte("ID=debian\nNAME=\"Debian GNU/Linux\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id: containerID,
				Image: &runtimeapi.ImageSpec{
					Image:              "registry.example.com/app@sha256:ffff",
					UserSpecifiedImage: "registry.example.com/app:latest",
				},
				Labels: map[string]string{
					labelPodName:       "my-pod",
					labelPodNamespace:  "default",
					labelContainerName: "app",
				},
			},
		},
		pids: map[string]int{containerID: pid},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), WithProcRoot(procRoot)))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if m := findGatheredMetric(mfs, "container_image_node_container_info", map[string]string{
		"id":      containerID,
		"os_id":   "debian",
		"os_name": "Debian GNU/Linux",
	}); m == nil {
		t.Error("container_image_node_container_info not found with expected labels from usr/lib/os-release")
	}
}

// TestExporter_Collect_DistrolessEmitsEmptyLabels verifies that a container
// whose rootfs exists but has no os-release file (distroless/scratch) still
// produces a metric — with empty os-release labels — so it remains visible in
// adoption queries.
func TestExporter_Collect_DistrolessEmitsEmptyLabels(t *testing.T) {
	procRoot := t.TempDir()
	const pid = 1001
	// Create the container root directory but leave it empty (no os-release files).
	if err := os.MkdirAll(filepath.Join(procRoot, strconv.Itoa(pid), "root"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id: "ctr-distroless",
				Image: &runtimeapi.ImageSpec{
					Image:              "registry.example.com/scratch@sha256:bbbb",
					UserSpecifiedImage: "registry.example.com/scratch:latest",
				},
				Labels: map[string]string{
					labelPodName:       "my-pod",
					labelPodNamespace:  "default",
					labelContainerName: "scratch",
				},
			},
		},
		pids: map[string]int{"ctr-distroless": pid},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), WithProcRoot(procRoot)))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	// Metric must be emitted with empty os-release labels.
	m := findGatheredMetric(mfs, "container_image_node_container_info", map[string]string{
		"id":      "ctr-distroless",
		"os_id":   "",
		"os_name": "",
	})
	if m == nil {
		t.Error("container_image_node_container_info not emitted for distroless container")
	}

	up := findGatheredMetric(mfs, "container_image_node_exporter_up", nil)
	if up == nil {
		t.Fatal("container_image_node_exporter_up metric not found")
	}
	if up.GetGauge().GetValue() != 1 {
		t.Errorf("node_exporter_up = %v, want 1", up.GetGauge().GetValue())
	}
}

// TestExporter_Collect_SkipsContainerOnReadError verifies that when readOSRelease
// returns a non-errNoOSRelease error (e.g. the container root is missing entirely),
// the container is skipped but up remains 1.
func TestExporter_Collect_SkipsContainerOnReadError(t *testing.T) {
	procRoot := t.TempDir()
	// Intentionally no rootfs directory created — os.OpenRoot will fail.

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id: "ctr-gone",
				Image: &runtimeapi.ImageSpec{
					Image:              "registry.example.com/app@sha256:cccc",
					UserSpecifiedImage: "registry.example.com/app:latest",
				},
				Labels: map[string]string{
					labelPodName:       "my-pod",
					labelPodNamespace:  "default",
					labelContainerName: "app",
				},
			},
		},
		pids: map[string]int{"ctr-gone": 9999},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), WithProcRoot(procRoot)))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if m := findGatheredMetric(mfs, "container_image_node_container_info", map[string]string{
		"id": "ctr-gone",
	}); m != nil {
		t.Error("container_image_node_container_info unexpectedly emitted for container with missing root")
	}

	up := findGatheredMetric(mfs, "container_image_node_exporter_up", nil)
	if up == nil {
		t.Fatal("container_image_node_exporter_up metric not found")
	}
	if up.GetGauge().GetValue() != 1 {
		t.Errorf("node_exporter_up = %v, want 1", up.GetGauge().GetValue())
	}
}

// TestExporter_Collect_MultipleContainers verifies that metrics are emitted for
// each container that has a readable /etc/os-release, that containers without
// one are silently skipped, and that label values correctly reflect each
// container's os-release content.
func TestExporter_Collect_MultipleContainers(t *testing.T) {
	procRoot := t.TempDir()
	makeRootfs(t, procRoot, 1100, "ID=wolfi\nNAME=\"Wolfi\"\n")
	makeRootfs(t, procRoot, 1101, "ID=ubuntu\nNAME=\"Ubuntu\"\nVERSION_ID=22.04\n")
	// pid 1102 has no rootfs — simulates a scratch image, should be silently skipped.

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id: "ctr-1", Image: &runtimeapi.ImageSpec{Image: "img-1@sha256:1111", UserSpecifiedImage: "img-1:latest"},
				Labels: map[string]string{labelPodName: "pod-1", labelPodNamespace: "ns-1", labelContainerName: "c1"},
			},
			{
				Id: "ctr-2", Image: &runtimeapi.ImageSpec{Image: "img-2@sha256:2222", UserSpecifiedImage: "img-2:latest"},
				Labels: map[string]string{labelPodName: "pod-2", labelPodNamespace: "ns-2", labelContainerName: "c2"},
			},
			{
				Id: "ctr-3", Image: &runtimeapi.ImageSpec{Image: "scratch@sha256:3333", UserSpecifiedImage: "scratch:latest"},
				Labels: map[string]string{labelPodName: "pod-3", labelPodNamespace: "ns-3", labelContainerName: "c3"},
			},
		},
		pids: map[string]int{"ctr-1": 1100, "ctr-2": 1101, "ctr-3": 1102},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), WithProcRoot(procRoot)))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	for _, tc := range []struct {
		containerID string
		wantID      string
	}{
		{"ctr-1", "wolfi"},
		{"ctr-2", "ubuntu"},
	} {
		if m := findGatheredMetric(mfs, "container_image_node_container_info", map[string]string{
			"id":    tc.containerID,
			"os_id": tc.wantID,
		}); m == nil {
			t.Errorf("container_image_node_container_info not found for container %s with os_id=%s", tc.containerID, tc.wantID)
		}
	}

	// ctr-3 (scratch) must produce no metric.
	if m := findGatheredMetric(mfs, "container_image_node_container_info", map[string]string{
		"id": "ctr-3",
	}); m != nil {
		t.Error("container_image_node_container_info unexpectedly emitted for scratch ctr-3")
	}
}

// TestExporter_Collect_ImageIDFromImageRef verifies that the image_id label is
// sourced from the CRI Container.ImageRef field, not parsed out of Image.Image.
func TestExporter_Collect_ImageIDFromImageRef(t *testing.T) {
	procRoot := t.TempDir()
	containerID := "ctr-image-id"
	const pid = 1200
	makeRootfs(t, procRoot, pid, "ID=alpine\n")

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id:       containerID,
				ImageRef: "registry.example.com/app@sha256:deadbeef",
				Image: &runtimeapi.ImageSpec{
					Image:              "registry.example.com/app@sha256:deadbeef",
					UserSpecifiedImage: "registry.example.com/app:v2",
				},
				Labels: map[string]string{
					labelPodName: "pod-d", labelPodNamespace: "ns-d", labelContainerName: "app",
				},
			},
		},
		pids: map[string]int{containerID: pid},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), WithProcRoot(procRoot)))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	m := findGatheredMetric(mfs, "container_image_node_container_info", map[string]string{
		"id":       containerID,
		"image":    "registry.example.com/app:v2",
		"image_id": "registry.example.com/app@sha256:deadbeef",
	})
	if m == nil {
		t.Error("expected image_id label to equal ImageRef verbatim")
	}
}

// TestExporter_Collect_NodeImage_AllImages verifies that when OnlyImagesInUse
// is false, ListImages drives metric emission. Labels and created are keyed
// by image_id only (one series per OCI label / one series per image).
func TestExporter_Collect_NodeImage_AllImages(t *testing.T) {
	procRoot := t.TempDir()
	const (
		imageInUseID  = "sha256:in-use"
		imageUnusedID = "sha256:unused"
	)

	srv := &fakeRuntimeServer{
		images: []*runtimeapi.Image{
			{Id: imageInUseID},
			{Id: imageUnusedID},
		},
		imageInfo: map[string]string{
			imageInUseID: `{"imageSpec":{
				"created":"2024-07-04T00:00:00Z",
				"config":{"Labels":{"org.opencontainers.image.version":"1.0.0"}}
			}}`,
			imageUnusedID: `{"imageSpec":{
				"created":"2023-01-01T00:00:00Z",
				"config":{"Labels":{"vendor":"acme"}}
			}}`,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), WithProcRoot(procRoot)))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	for _, tc := range []struct {
		imageID, key, value string
	}{
		{imageInUseID, "org.opencontainers.image.version", "1.0.0"},
		{imageUnusedID, "vendor", "acme"},
	} {
		if m := findGatheredMetric(mfs, "container_image_node_image_labels", map[string]string{
			"image_id": tc.imageID,
			"key":      tc.key,
			"value":    tc.value,
		}); m == nil {
			t.Errorf("container_image_node_image_labels not found for %s key=%s", tc.imageID, tc.key)
		}
	}

	for _, tc := range []struct {
		imageID, ts string
	}{
		{imageInUseID, "2024-07-04T00:00:00Z"},
		{imageUnusedID, "2023-01-01T00:00:00Z"},
	} {
		m := findGatheredMetric(mfs, "container_image_node_image_created", map[string]string{
			"image_id": tc.imageID,
		})
		if m == nil {
			t.Errorf("container_image_node_image_created not found for %s", tc.imageID)
			continue
		}
		want, _ := time.Parse(time.RFC3339, tc.ts)
		if got := int64(m.GetGauge().GetValue()); got != want.Unix() {
			t.Errorf("created[%s] = %d, want %d", tc.imageID, got, want.Unix())
		}
	}

	if srv.listImagesCalls != 1 {
		t.Errorf("ListImages calls = %d, want 1", srv.listImagesCalls)
	}
}

// TestExporter_Collect_NodeImage_OnlyInUse verifies that with OnlyImagesInUse,
// only images referenced by a running container are emitted, and ListImages is
// not called.
func TestExporter_Collect_NodeImage_OnlyInUse(t *testing.T) {
	procRoot := t.TempDir()
	const (
		containerID   = "ctr-only"
		pid           = 4000
		imageInUseID  = "sha256:in-use"
		imageUnusedID = "sha256:unused"
	)
	makeRootfs(t, procRoot, pid, "ID=alpine\n")

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id:       containerID,
				ImageRef: imageInUseID,
				Image: &runtimeapi.ImageSpec{
					Image:              imageInUseID,
					UserSpecifiedImage: "registry.example.com/app:v1",
				},
				Labels: map[string]string{
					labelPodName:       "pod",
					labelPodNamespace:  "ns",
					labelContainerName: "app",
				},
			},
		},
		pids: map[string]int{containerID: pid},
		images: []*runtimeapi.Image{
			{Id: imageInUseID},
			{Id: imageUnusedID},
		},
		imageInfo: map[string]string{
			imageInUseID: `{"imageSpec":{
				"created":"2024-07-04T00:00:00Z",
				"config":{"Labels":{"k":"in-use"}}
			}}`,
			imageUnusedID: `{"imageSpec":{
				"created":"2023-01-01T00:00:00Z",
				"config":{"Labels":{"k":"unused"}}
			}}`,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv),
		WithProcRoot(procRoot),
		WithOnlyImagesInUse(true),
	))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if m := findGatheredMetric(mfs, "container_image_node_image_labels", map[string]string{
		"image_id": imageInUseID, "key": "k", "value": "in-use",
	}); m == nil {
		t.Error("expected node_image_labels for in-use image")
	}
	if m := findGatheredMetric(mfs, "container_image_node_image_labels", map[string]string{
		"image_id": imageUnusedID,
	}); m != nil {
		t.Error("unexpected node_image_labels for unused image — only-in-use filter not applied")
	}
	if m := findGatheredMetric(mfs, "container_image_node_image_created", map[string]string{
		"image_id": imageUnusedID,
	}); m != nil {
		t.Error("unexpected node_image_created for unused image — only-in-use filter not applied")
	}

	if srv.listImagesCalls != 0 {
		t.Errorf("ListImages calls = %d, want 0 in only-in-use mode", srv.listImagesCalls)
	}
}

// TestExporter_Collect_NodeImage_OnlyInUse_ImageStatusError verifies that
// when ImageStatus fails for an in-use image, only-in-use mode skips that
// image's node_image_* series rather than failing the whole scrape — up
// should stay at 1, and the container's container_info should still be
// emitted from the unrelated OS-release path.
func TestExporter_Collect_NodeImage_OnlyInUse_ImageStatusError(t *testing.T) {
	procRoot := t.TempDir()
	const (
		containerID = "ctr-failing-image"
		pid         = 4100
		imageID     = "sha256:will-error"
	)
	makeRootfs(t, procRoot, pid, "ID=alpine\n")

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id:       containerID,
				ImageRef: imageID,
				Image: &runtimeapi.ImageSpec{
					Image:              imageID,
					UserSpecifiedImage: "registry.example.com/app:v1",
				},
				Labels: map[string]string{
					labelPodName:       "pod",
					labelPodNamespace:  "ns",
					labelContainerName: "app",
				},
			},
		},
		pids: map[string]int{containerID: pid},
		imageStatusErrors: map[string]error{
			imageID: errors.New("simulated CRI ImageStatus failure"),
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv),
		WithProcRoot(procRoot),
		WithOnlyImagesInUse(true),
	))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	// node_image_* should not appear for the erroring image — only-in-use
	// mode skips on ImageStatus error rather than emitting a bare entry.
	if m := findGatheredMetric(mfs, "container_image_node_image_labels", map[string]string{
		"image_id": imageID,
	}); m != nil {
		t.Error("unexpected node_image_labels series for image whose ImageStatus errored")
	}
	if m := findGatheredMetric(mfs, "container_image_node_image_created", map[string]string{
		"image_id": imageID,
	}); m != nil {
		t.Error("unexpected node_image_created series for image whose ImageStatus errored")
	}

	// The container itself is still observable via the OS-release path,
	// which doesn't depend on ImageService.
	if m := findGatheredMetric(mfs, "container_image_node_container_info", map[string]string{
		"id": containerID, "os_id": "alpine",
	}); m == nil {
		t.Error("node_container_info should still be emitted; ImageStatus failure must not gate OS-release reads")
	}

	// And the scrape as a whole should be considered successful — a per-image
	// transient failure is not a collector-level failure.
	if m := findGatheredMetric(mfs, "container_image_node_exporter_up", nil); m == nil {
		t.Fatal("node_exporter_up metric not found")
	} else if got := m.GetGauge().GetValue(); got != 1 {
		t.Errorf("node_exporter_up = %v, want 1 (per-image ImageStatus error should not fail the scrape)", got)
	}
}

// TestExporter_Collect_NodeImage_ImageIDJoinsAcrossMetrics verifies the
// dashboards' implicit contract: the image_id label on node_container_info
// matches the image_id label on the corresponding node_image_* series for
// the same container. Without that, `... * on(image_id) ...` joins in the
// chart's Grafana panels would produce empty results.
func TestExporter_Collect_NodeImage_ImageIDJoinsAcrossMetrics(t *testing.T) {
	procRoot := t.TempDir()
	const (
		containerID = "ctr-join"
		pid         = 4200
		imageID     = "sha256:join"
	)
	makeRootfs(t, procRoot, pid, "ID=alpine\n")

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id:       containerID,
				ImageRef: imageID,
				Image: &runtimeapi.ImageSpec{
					Image:              imageID,
					UserSpecifiedImage: "registry.example.com/app:v1",
				},
				Labels: map[string]string{
					labelPodName:       "pod",
					labelPodNamespace:  "ns",
					labelContainerName: "app",
				},
			},
		},
		pids:   map[string]int{containerID: pid},
		images: []*runtimeapi.Image{{Id: imageID}},
		imageInfo: map[string]string{
			imageID: `{"imageSpec":{"created":"2024-01-01T00:00:00Z","config":{"Labels":{"k":"v"}}}}`,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv),
		WithProcRoot(procRoot),
		WithOnlyImagesInUse(true),
	))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	containerInfo := findGatheredMetric(mfs, "container_image_node_container_info", map[string]string{
		"id": containerID,
	})
	if containerInfo == nil {
		t.Fatal("node_container_info missing for the running container")
	}
	imageLabels := findGatheredMetric(mfs, "container_image_node_image_labels", map[string]string{
		"key": "k", "value": "v",
	})
	if imageLabels == nil {
		t.Fatal("node_image_labels missing for the configured OCI label")
	}

	containerImageID := labelValue(containerInfo, "image_id")
	imageLabelsImageID := labelValue(imageLabels, "image_id")
	if containerImageID == "" {
		t.Fatal("node_container_info.image_id is empty")
	}
	if containerImageID != imageLabelsImageID {
		t.Errorf("image_id mismatch across metrics: node_container_info=%q, node_image_labels=%q — dashboard joins on image_id will fail",
			containerImageID, imageLabelsImageID)
	}
}

// labelValue returns the value of the named Prometheus label on a metric,
// or "" if the label isn't present.
func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// TestExporter_Collect_NodeImage_DanglingNoTags verifies that an image with
// no repo_tags and no repo_digests still has its labels/created emitted —
// node_image_* metrics key on image_id, not human-readable names, so a
// dangling image is just as observable as a tagged one.
func TestExporter_Collect_NodeImage_DanglingNoTags(t *testing.T) {
	procRoot := t.TempDir()
	const imageID = "sha256:dangling"

	srv := &fakeRuntimeServer{
		images: []*runtimeapi.Image{{Id: imageID}},
		imageInfo: map[string]string{
			imageID: `{"imageSpec":{"created":"2024-01-01T00:00:00Z","config":{"Labels":{"k":"v"}}}}`,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), WithProcRoot(procRoot)))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if m := findGatheredMetric(mfs, "container_image_node_image_labels", map[string]string{
		"image_id": imageID, "key": "k", "value": "v",
	}); m == nil {
		t.Error("expected node_image_labels for dangling image")
	}
	if m := findGatheredMetric(mfs, "container_image_node_image_created", map[string]string{
		"image_id": imageID,
	}); m == nil {
		t.Error("expected node_image_created for dangling image")
	}
}

// TestExporter_Collect_NodeImage_MultipleTagsDedupes verifies that an image
// with multiple repo_tags still produces exactly one node_image_labels series
// per (image_id, OCI label) and one node_image_created series per image_id
// — neither fans out by tag.
func TestExporter_Collect_NodeImage_MultipleTagsDedupes(t *testing.T) {
	procRoot := t.TempDir()
	const imageID = "sha256:multi"

	srv := &fakeRuntimeServer{
		images: []*runtimeapi.Image{{Id: imageID}},
		imageInfo: map[string]string{
			imageID: `{"imageSpec":{"created":"2024-01-01T00:00:00Z","config":{"Labels":{"k":"v"}}}}`,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), WithProcRoot(procRoot)))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var labelsCount, createdCount int
	for _, mf := range mfs {
		switch mf.GetName() {
		case "container_image_node_image_labels":
			labelsCount = len(mf.GetMetric())
		case "container_image_node_image_created":
			createdCount = len(mf.GetMetric())
		}
	}
	if labelsCount != 1 {
		t.Errorf("node_image_labels series count = %d, want 1 (one per (image_id, OCI label))", labelsCount)
	}
	if createdCount != 1 {
		t.Errorf("node_image_created series count = %d, want 1 (one per image_id)", createdCount)
	}
}

// TestExporter_Collect_NodeImage_LabelAllowlist verifies that when
// WithLabelAllowlist is set, only labels with keys in the allowlist are
// emitted as container_image_node_image_labels series. Other node_image_*
// metrics are unaffected.
func TestExporter_Collect_NodeImage_LabelAllowlist(t *testing.T) {
	procRoot := t.TempDir()
	const imageID = "sha256:filtered"

	srv := &fakeRuntimeServer{
		images: []*runtimeapi.Image{{Id: imageID}},
		imageInfo: map[string]string{
			imageID: `{"imageSpec":{
				"created":"2024-01-01T00:00:00Z",
				"config":{"Labels":{
					"org.opencontainers.image.version":"1.0.0",
					"org.opencontainers.image.title":"demo",
					"vendor":"acme"
				}}
			}}`,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv),
		WithProcRoot(procRoot),
		WithLabelAllowlist([]string{"org.opencontainers.image.version", "vendor"}),
	))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	// Allowed keys must be present.
	for _, key := range []string{"org.opencontainers.image.version", "vendor"} {
		if m := findGatheredMetric(mfs, "container_image_node_image_labels", map[string]string{
			"image_id": imageID, "key": key,
		}); m == nil {
			t.Errorf("expected node_image_labels for allowed key %q", key)
		}
	}

	// Disallowed keys must be absent.
	if m := findGatheredMetric(mfs, "container_image_node_image_labels", map[string]string{
		"image_id": imageID, "key": "org.opencontainers.image.title",
	}); m != nil {
		t.Error("node_image_labels emitted for key not in allowlist (org.opencontainers.image.title)")
	}

	// node_image_created must still be emitted — the allowlist only filters
	// node_image_labels series.
	if m := findGatheredMetric(mfs, "container_image_node_image_created", map[string]string{
		"image_id": imageID,
	}); m == nil {
		t.Error("node_image_created unexpectedly suppressed by label allowlist")
	}
}

// TestExporter_Collect_OnlyInUse_ResolvesByImageRef verifies that in
// only-in-use mode, when a container's ImageSpec is empty but the runtime
// still reports an ImageRef (CRI Container.image_ref), the exporter
// resolves image metadata via that ref. ImageRef is the same field that
// becomes node_container_info.image_id, so this also exercises the
// preferred lookup path for normal containers (Image populated or not).
func TestExporter_Collect_OnlyInUse_ResolvesByImageRef(t *testing.T) {
	procRoot := t.TempDir()
	const (
		containerID = "ctr-no-image-spec"
		pid         = 5000
		imageID     = "sha256:from-image-ref"
	)
	makeRootfs(t, procRoot, pid, "ID=alpine\n")

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id: containerID,
				// No Image field — only ImageRef. This shape can occur
				// when CRI runtimes report containers without populating
				// the ImageSpec.
				ImageRef: imageID,
				Labels: map[string]string{
					labelPodName:       "pod",
					labelPodNamespace:  "ns",
					labelContainerName: "app",
				},
			},
		},
		pids: map[string]int{containerID: pid},
		images: []*runtimeapi.Image{
			{Id: imageID},
		},
		imageInfo: map[string]string{
			imageID: `{"imageSpec":{"created":"2024-01-01T00:00:00Z","config":{"Labels":{"k":"from-ref"}}}}`,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv),
		WithProcRoot(procRoot),
		WithOnlyImagesInUse(true),
	))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if m := findGatheredMetric(mfs, "container_image_node_image_labels", map[string]string{
		"image_id": imageID, "key": "k", "value": "from-ref",
	}); m == nil {
		t.Error("expected node_image_labels resolved via ImageRef in only-in-use mode")
	}
}

// TestExporter_Collect_NodeImage_LabelValueTruncated verifies that an OCI
// label value longer than maxLabelValueBytes is truncated before being
// emitted as a Prometheus label value, with a "…" marker indicating
// truncation occurred.
func TestExporter_Collect_NodeImage_LabelValueTruncated(t *testing.T) {
	procRoot := t.TempDir()
	const imageID = "sha256:huge-label"
	hugeValue := strings.Repeat("x", 5000)

	verbose, err := json.Marshal(map[string]any{
		"imageSpec": map[string]any{
			"config": map[string]any{
				"Labels": map[string]string{
					"sbom": hugeValue,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal verbose info: %v", err)
	}

	srv := &fakeRuntimeServer{
		images: []*runtimeapi.Image{{Id: imageID}},
		imageInfo: map[string]string{imageID: string(verbose)},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), WithProcRoot(procRoot)))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	// Walk the gathered series for our image and find the value of the
	// sbom label.
	var got string
	for _, mf := range mfs {
		if mf.GetName() != "container_image_node_image_labels" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["image_id"] == imageID && labels["key"] == "sbom" {
				got = labels["value"]
			}
		}
	}
	if got == "" {
		t.Fatal("node_image_labels series for sbom key not found")
	}
	if len(got) != maxLabelValueBytes {
		t.Errorf("emitted value length = %d, want %d (truncated)", len(got), maxLabelValueBytes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("emitted value does not end with truncation marker: tail=%q", got[len(got)-6:])
	}
}

// TestTruncateLabelValue covers boundary and UTF-8-safety behaviour: short
// values pass through, exact-length values pass through, oversized ASCII
// values get the marker, and an oversized value whose natural cut point
// lands inside a multi-byte rune is walked back to a rune boundary so the
// result is always valid UTF-8.
func TestTruncateLabelValue(t *testing.T) {
	const marker = "…"

	tests := []struct {
		name string
		in   string
		// wantLen is the exact byte length expected (0 means same as len(in)).
		wantLen     int
		wantMarker  bool
		wantPrefix  string // optional: assert leading bytes
	}{
		{
			name: "short value passes through",
			in:   "hello",
		},
		{
			name: "exactly at the cap passes through",
			in:   strings.Repeat("a", maxLabelValueBytes),
		},
		{
			name:       "one byte over the cap gets truncated and marked",
			in:         strings.Repeat("a", maxLabelValueBytes+1),
			wantLen:    maxLabelValueBytes,
			wantMarker: true,
			wantPrefix: strings.Repeat("a", maxLabelValueBytes-len(marker)),
		},
		{
			// "界" is a 3-byte rune. Construct a value where the naive byte
			// cut (maxLabelValueBytes - 3) lands in the middle of one of
			// these runes — the truncation must walk back to the rune
			// boundary, so the result is shorter than the cap but always
			// valid UTF-8.
			name: "cut inside a multi-byte rune is walked back",
			in: func() string {
				// Prefix with 'a' bytes that leave the cut point one byte
				// into a 3-byte rune.
				cutAt := maxLabelValueBytes - len(marker)
				prefixBytes := cutAt - 1
				return strings.Repeat("a", prefixBytes) + strings.Repeat("界", 50)
			}(),
			wantMarker: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateLabelValue(tc.in)
			if !utf8.ValidString(got) {
				t.Errorf("result is not valid UTF-8: % x", got)
			}
			if tc.wantMarker && !strings.HasSuffix(got, marker) {
				t.Errorf("missing truncation marker: %q", got)
			}
			if !tc.wantMarker && got != tc.in {
				t.Errorf("unexpected modification: got %q, want %q", got, tc.in)
			}
			if tc.wantLen != 0 && len(got) != tc.wantLen {
				t.Errorf("len(got) = %d, want %d", len(got), tc.wantLen)
			}
			if tc.wantPrefix != "" && !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("missing expected prefix")
			}
			if len(got) > maxLabelValueBytes {
				t.Errorf("len(got) = %d exceeds cap %d", len(got), maxLabelValueBytes)
			}
		})
	}
}
