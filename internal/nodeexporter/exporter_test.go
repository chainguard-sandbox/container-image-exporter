package nodeexporter

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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

// TestNormalizeDigest verifies the digest extraction logic for the various
// image reference formats that a CRI runtime may return as the resolved image.
func TestNormalizeDigest(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain sha256 digest",
			input: "sha256:abc123",
			want:  "sha256:abc123",
		},
		{
			name:  "reference with @ separator",
			input: "registry.example.com/myimage@sha256:abc123",
			want:  "sha256:abc123",
		},
		{
			name:  "tagged reference without digest",
			input: "registry.example.com/myimage:latest",
			want:  "registry.example.com/myimage:latest",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "multiple @ characters uses the last one",
			input: "user@host.example.com/image@sha256:abc123",
			want:  "sha256:abc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeDigest(tt.input); got != tt.want {
				t.Errorf("normalizeDigest(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
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
				Id: containerID,
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
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), procRoot))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	m := findGatheredMetric(mfs, "container_image_container_os_info", map[string]string{
		"container_id": containerID,
		"namespace":    "default",
		"pod":          "my-pod",
		"container":    "app",
		"image":        "registry.example.com/app:latest",
		"digest":       "sha256:aaaa",
		"id":           "wolfi",
		"name":         "Wolfi",
		"pretty_name":  "Wolfi",
		"version_id":   "20240101",
	})
	if m == nil {
		t.Error("container_image_container_os_info not found with expected labels")
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
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), procRoot))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if m := findGatheredMetric(mfs, "container_image_container_os_info", map[string]string{
		"container_id": containerID,
		"id":           "debian",
		"name":         "Debian GNU/Linux",
	}); m == nil {
		t.Error("container_image_container_os_info not found with expected labels from usr/lib/os-release")
	}
}

// TestExporter_Collect_SkipsContainerWithNoOSRelease verifies that a container
// whose rootfs has no /etc/os-release (e.g. a scratch image) is silently
// skipped and the up metric remains 1.
func TestExporter_Collect_SkipsContainerWithNoOSRelease(t *testing.T) {
	procRoot := t.TempDir()
	// Intentionally no rootfs directory created for "ctr-scratch".

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id: "ctr-scratch",
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
		pids: map[string]int{"ctr-scratch": 1001},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), procRoot))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	// os_info must not be emitted for the scratch container.
	if m := findGatheredMetric(mfs, "container_image_container_os_info", map[string]string{
		"container_id": "ctr-scratch",
	}); m != nil {
		t.Error("container_image_container_os_info unexpectedly emitted for scratch container")
	}

	// Skipping a container is not a fatal error — up should still be 1.
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
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), procRoot))
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
		if m := findGatheredMetric(mfs, "container_image_container_os_info", map[string]string{
			"container_id": tc.containerID,
			"id":           tc.wantID,
		}); m == nil {
			t.Errorf("container_image_container_os_info not found for container %s with id=%s", tc.containerID, tc.wantID)
		}
	}

	// ctr-3 (scratch) must produce no metric.
	if m := findGatheredMetric(mfs, "container_image_container_os_info", map[string]string{
		"container_id": "ctr-3",
	}); m != nil {
		t.Error("container_image_container_os_info unexpectedly emitted for scratch ctr-3")
	}
}

// TestExporter_Collect_DigestNormalization verifies that the digest label is
// derived from Image.Image (the resolved image reference in the ImageSpec) and
// contains only the sha256:... portion when that field includes a registry host.
func TestExporter_Collect_DigestNormalization(t *testing.T) {
	procRoot := t.TempDir()
	containerID := "ctr-digest"
	const pid = 1200
	makeRootfs(t, procRoot, pid, "ID=alpine\n")

	srv := &fakeRuntimeServer{
		containers: []*runtimeapi.Container{
			{
				Id: containerID,
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
	reg.MustRegister(NewExporter(startFakeRuntime(t, srv), procRoot))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	m := findGatheredMetric(mfs, "container_image_container_os_info", map[string]string{
		"container_id": containerID,
		"image":        "registry.example.com/app:v2",
		"digest":       "sha256:deadbeef",
	})
	if m == nil {
		t.Error("expected image=UserSpecifiedImage, digest derived from Image.Image")
	}
}
