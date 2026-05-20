//go:build nodeintegration

package nodeexporter

import (
	"archive/tar"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const pauseImageRepo = "pause.local/pause"

var (
	pods       = flag.Int("pods", 100, "number of pause pods to deploy before running tests; 0 skips deployment")
	pauseMode  = flag.Bool("pause", false, "block forever (entrypoint of generated pause images)")
	criSocket  = flag.String("cri.socket", "/run/containerd/containerd.sock", "path to the CRI socket")
	procRoot   = flag.String("proc.root", "/proc", "path to the proc filesystem (use /host/proc when running as a DaemonSet pod)")
	kubeconfig = flag.String("kubeconfig", "/etc/rancher/k3s/k3s.yaml", "path to the kubeconfig used to deploy pause pods")
)

func TestMain(m *testing.M) {
	flag.Parse()

	// When -pause is set, this binary blocks on signals. Acting as the
	// entrypoint of the pause pods we spin up.
	if *pauseMode {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		os.Exit(0)
	}

	// Generate unique images and deploy N pause pods ahead of running the
	// tests
	if *pods > 0 {
		if err := deployPausePods(*pods); err != nil {
			log.Fatalf("deploying pause pods: %v", err)
		}
	}
	os.Exit(m.Run())
}

// TestNode_Collect runs the Exporter against the real CRI socket on the
// current node. It is gated by the nodeintegration build tag so it never
// runs in standard CI.
//
// Run with:
//
//	go test -tags nodeintegration ./internal/nodeexporter/...
//
// Override the CRI socket or proc root via flags:
//
//	go test -tags nodeintegration ./internal/nodeexporter/... \
//	  -cri.socket=/run/k3s/containerd/containerd.sock
func TestNode_Collect(t *testing.T) {
	if _, err := os.Stat(*criSocket); os.IsNotExist(err) {
		t.Skipf("CRI socket not found at %s (override with -cri.socket): not running on a Kubernetes node", *criSocket)
	}

	conn, err := grpc.NewClient(
		"unix://"+*criSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dialing CRI socket %s: %v", *criSocket, err)
	}
	t.Cleanup(func() { conn.Close() })

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(conn, WithProcRoot(*procRoot)))
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

// TestNode_CollectDuration runs against the real CRI socket and asserts
// Collect() stays under a per-pod budget. When -pods>0, TestMain has
// already deployed that many pause pods (each with a distinct
// /etc/os-release layer); when -pods=0, the assertion runs against
// whatever's on the node.
func TestNode_CollectDuration(t *testing.T) {
	expected := *pods

	if _, err := os.Stat(*criSocket); os.IsNotExist(err) {
		t.Skipf("CRI socket not found at %s", *criSocket)
	}

	conn, err := grpc.NewClient(
		"unix://"+*criSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dialing CRI socket %s: %v", *criSocket, err)
	}
	t.Cleanup(func() { conn.Close() })

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewExporter(conn, WithProcRoot(*procRoot)))

	start := time.Now()
	mfs, err := reg.Gather()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	infoFamily := findFamily(mfs, "container_image_node_container_info")
	if infoFamily == nil {
		t.Fatal("container_image_node_container_info family not found")
	}

	pauseOSIDs := map[string]struct{}{}
	for _, m := range infoFamily.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "os_id" && strings.HasPrefix(lp.GetValue(), "pause-") {
				pauseOSIDs[lp.GetValue()] = struct{}{}
			}
		}
	}

	// Budget: 100ms fixed (gRPC + ListContainers + /proc setup) plus
	// 10ms/pod (one ImageStatus RPC + a /proc/<pid>/root/etc/os-release
	// read each). Measured baseline is ~0.5ms/pod; the 20x headroom
	// tolerates CI variance while still catching algorithmic regressions
	// — an O(N^2) Collect would blow this budget well before reaching
	// the typical 10s scrape timeout.
	budget := 100*time.Millisecond + time.Duration(expected)*10*time.Millisecond

	t.Logf("Collect duration: %v (budget %v)", elapsed, budget)
	t.Logf("node_container_info total series: %d", len(infoFamily.GetMetric()))
	t.Logf("distinct pause-* os_id values: %d (expected %d)", len(pauseOSIDs), expected)

	if len(pauseOSIDs) < expected {
		t.Errorf("distinct pause os_id = %d, want >= %d", len(pauseOSIDs), expected)
	}
	if elapsed > budget {
		t.Errorf("Collect duration %v exceeded budget %v", elapsed, budget)
	}
}

func findFamily(mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

// deployPausePods builds N images that all share this same test binary as
// their /pause entrypoint, plus a per-image /etc/os-release layer that
// gives each one a distinct ID and config digest. The images are
// imported into the local containerd (k8s.io namespace) and N pods are
// applied via kubectl and waited on. All side effects happen on the
// node this test binary is currently running on.
func deployPausePods(n int) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating self: %w", err)
	}
	selfBytes, err := os.ReadFile(self)
	if err != nil {
		return fmt.Errorf("reading self: %w", err)
	}
	binLayer, err := layerFromFile("pause", selfBytes, 0o755)
	if err != nil {
		return fmt.Errorf("building pause layer: %w", err)
	}

	refs := map[string]v1.Image{}
	for i := 0; i < n; i++ {
		osRelease := fmt.Sprintf(
			"NAME=\"Pause %04d\"\nID=pause-%04d\nVERSION_ID=%04d\nPRETTY_NAME=\"Pause Image %04d\"\n",
			i, i, i, i,
		)
		osLayer, err := layerFromFile("etc/os-release", []byte(osRelease), 0o644)
		if err != nil {
			return fmt.Errorf("os-release layer %d: %w", i, err)
		}
		img, err := mutate.AppendLayers(empty.Image, binLayer, osLayer)
		if err != nil {
			return fmt.Errorf("appending layers %d: %w", i, err)
		}
		cfg, err := img.ConfigFile()
		if err != nil {
			return fmt.Errorf("config %d: %w", i, err)
		}
		cfg = cfg.DeepCopy()
		cfg.Config.Entrypoint = []string{"/pause", "-pause"}
		cfg.OS = "linux"
		cfg.Architecture = runtime.GOARCH
		cfg.Config.Labels = map[string]string{
			"org.opencontainers.image.title":   fmt.Sprintf("pause-%04d", i),
			"org.opencontainers.image.version": fmt.Sprintf("0.0.%d", i),
			"org.opencontainers.image.vendor":  "container-image-exporter test",
		}
		r := rand.New(rand.NewSource(int64(i + 1)))
		cfg.Created = v1.Time{
			Time: time.Now().Add(-time.Duration(r.Int63n(int64(365 * 24 * time.Hour)))),
		}
		img, err = mutate.ConfigFile(img, cfg)
		if err != nil {
			return fmt.Errorf("setting config %d: %w", i, err)
		}
		refs[fmt.Sprintf("%s:%d", pauseImageRepo, i)] = img
	}

	tarballPath := "/tmp/pause.tar"
	if err := crane.MultiSave(refs, tarballPath); err != nil {
		return fmt.Errorf("saving image tarball: %w", err)
	}
	log.Printf("wrote %d pause images to %s", n, tarballPath)

	if err := importToContainerd(*criSocket, tarballPath); err != nil {
		return fmt.Errorf("importing images: %w", err)
	}

	if err := createAndWaitPausePods(n); err != nil {
		return fmt.Errorf("creating pause pods: %w", err)
	}
	return nil
}

// createAndWaitPausePods creates N pause pods in the default namespace
// referencing the pre-imported pause.local/pause:i images, then polls
// the API server until all of them report Ready (or the timeout fires).
func createAndWaitPausePods(n int) error {
	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return fmt.Errorf("loading kubeconfig %s: %w", *kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("building clientset: %w", err)
	}

	const namespace = "default"
	const labelSelector = "app=cie-pause"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	log.Printf("creating %d pause pods in namespace %q", n, namespace)
	for i := 0; i < n; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   fmt.Sprintf("pause-%04d", i),
				Labels: map[string]string{"app": "cie-pause"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:            "pause",
					Image:           fmt.Sprintf("%s:%d", pauseImageRepo, i),
					ImagePullPolicy: corev1.PullNever,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1m"),
							corev1.ResourceMemory: resource.MustParse("4Mi"),
						},
					},
				}},
			},
		}
		if _, err := cs.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating pod %s: %w", pod.Name, err)
		}
	}

	log.Printf("waiting for %d pause pods to be Ready", n)
	for {
		list, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return fmt.Errorf("listing pods: %w", err)
		}
		ready := 0
		for _, p := range list.Items {
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					ready++
					break
				}
			}
		}
		if ready >= n {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("only %d/%d pods Ready before timeout", ready, n)
		case <-time.After(2 * time.Second):
		}
	}
}

// importToContainerd loads a docker-save tarball into containerd's k8s.io
// namespace using the containerd Go client directly — same effect as
// `ctr -n k8s.io images import <tar>` but without shelling out.
func importToContainerd(socketPath, tarballPath string) error {
	c, err := client.New(socketPath)
	if err != nil {
		return fmt.Errorf("connecting to containerd at %s: %w", socketPath, err)
	}
	defer c.Close()

	f, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", tarballPath, err)
	}
	defer f.Close()

	ctx := namespaces.WithNamespace(context.Background(), "k8s.io")
	if _, err := c.Import(ctx, f); err != nil {
		return fmt.Errorf("Import: %w", err)
	}
	return nil
}

func layerFromFile(name string, data []byte, mode int64) (v1.Layer, error) {
	return tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: mode,
			Size: int64(len(data)),
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
		if err := tw.Close(); err != nil {
			return nil, err
		}
		return io.NopCloser(&buf), nil
	})
}

