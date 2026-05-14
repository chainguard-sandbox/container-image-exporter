//go:build nodeintegration

package nodeexporter

import (
	"os"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const defaultNodeCRISocket = "/run/containerd/containerd.sock"

// TestNode_Collect runs the Exporter against the real CRI socket on the
// current node. It is gated by the nodeintegration build tag so it never
// runs in standard CI.
//
// Run with:
//
//	go test -tags nodeintegration ./internal/nodeexporter/...
//
// Override the CRI socket or proc root via env vars:
//
//	CRI_SOCKET=/run/k3s/containerd/containerd.sock \
//	  go test -tags nodeintegration ./internal/nodeexporter/...
func TestNode_Collect(t *testing.T) {
	criSocket := os.Getenv("CRI_SOCKET")
	if criSocket == "" {
		criSocket = defaultNodeCRISocket
	}
	// When running directly on the node (e.g. inside a k3d container) /proc is
	// the right value. When running as a DaemonSet pod, use /host/proc.
	procRoot := os.Getenv("PROC_ROOT")
	if procRoot == "" {
		procRoot = "/proc"
	}

	if _, err := os.Stat(criSocket); os.IsNotExist(err) {
		t.Skipf("CRI socket not found at %s (set CRI_SOCKET to override): not running on a Kubernetes node", criSocket)
	}

	conn, err := grpc.NewClient(
		"unix://"+criSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dialing CRI socket %s: %v", criSocket, err)
	}
	t.Cleanup(func() { conn.Close() })

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(conn, WithProcRoot(procRoot)))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	// The up metric must be present and equal to 1, proving the CRI
	// ListContainers call succeeded.
	up := findGatheredMetric(mfs, "container_image_node_exporter_up", nil)
	if up == nil {
		t.Fatal("container_image_node_exporter_up metric not found")
	}
	if up.GetGauge().GetValue() != 1 {
		t.Fatalf("node_exporter_up = %v, want 1 (CRI ListContainers failed)", up.GetGauge().GetValue())
	}

	// Find all node_container_info metrics. It is valid (though unusual) for a
	// node to have no containers with a readable /etc/os-release — scratch
	// images and distroless images do not have one. Log rather than fail in
	// that case.
	var infoFamily *dto.MetricFamily
	for _, mf := range mfs {
		if mf.GetName() == "container_image_node_container_info" {
			infoFamily = mf
			break
		}
	}
	if infoFamily == nil || len(infoFamily.GetMetric()) == 0 {
		t.Log("no container_image_node_container_info metrics found: all running containers may be scratch/distroless images")
		return
	}

	t.Logf("found %d container(s) with node_container_info metrics", len(infoFamily.GetMetric()))

	// Structural checks on every emitted metric.
	for _, m := range infoFamily.GetMetric() {
		labels := make(map[string]string, len(m.GetLabel()))
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}

		// These labels must always be non-empty.
		for _, required := range []string{"id", "namespace", "pod", "container"} {
			if labels[required] == "" {
				t.Errorf("node_container_info metric for container %q has empty required label %q; full labels: %v",
					labels["id"], required, labels)
			}
		}

		// Pause containers must never appear — they are filtered in CRIClient.
		if labels["container"] == pauseContainerName {
			t.Errorf("node_container_info metric emitted for pause container (id=%s)", labels["id"])
		}

		// The image_id label is sourced from CRI ImageRef and must be non-empty
		// and start with "sha256:".
		d := labels["image_id"]
		if d == "" {
			t.Errorf("image_id label is empty for container %s — ImageRef not populated by runtime", labels["id"])
		} else if !strings.HasPrefix(d, "sha256:") {
			t.Errorf("image_id label %q does not start with 'sha256:' for container %s", d, labels["id"])
		}

		// image is the user-specified reference; it should be non-empty and
		// must not look like a bare digest (that would mean UserSpecifiedImage
		// wasn't populated by the runtime).
		img := labels["image"]
		if img == "" {
			t.Errorf("container %s: image label is empty (UserSpecifiedImage not set by runtime)", labels["id"])
		}
		t.Logf("container %s (%s/%s): image=%q image_id=%q",
			labels["id"], labels["namespace"], labels["pod"], img, labels["image_id"])

		// Gauge value is always 1 for a present metric.
		if m.GetGauge().GetValue() != 1 {
			t.Errorf("node_container_info gauge = %v for container %s, want 1",
				m.GetGauge().GetValue(), labels["id"])
		}
	}
}
