package cmd

import (
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/chainguard-sandbox/container-image-exporter/internal/nodeexporter"
)

var (
	neMetricsAddr     string
	neCRISocket       string
	neProcRoot        string
	neOnlyImagesInUse bool
	neLabelAllowlist  []string
)

var nodeExporterCmd = &cobra.Command{
	Use:   "node-exporter",
	Short: "Export container image metrics from the local CRI socket.",
	Long: `node-exporter is a DaemonSet component that runs on each Kubernetes node and
exports Prometheus metrics about container images without making remote registry requests.

It sources data from:
  - The local CRI RuntimeService to enumerate running containers and resolve their PIDs
  - The local CRI ImageService to enumerate cached images and fetch OCI labels and creation timestamps
  - The host /proc filesystem to read /etc/os-release from each container's rootfs

Metrics exported:
  container_image_node_container_info  - Per-running-container info (identity + OS release fields)
  container_image_node_image_labels    - OCI image config labels per cached image (one series per (image_id, OCI label))
  container_image_node_image_created   - OCI image creation timestamp per cached image
  container_image_node_exporter_up     - 1 if collection succeeded, 0 otherwise`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Connect to the CRI socket. grpc.NewClient is non-blocking; the
		// actual connection is established on the first RPC call.
		conn, err := grpc.NewClient(
			"unix://"+neCRISocket,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			return fmt.Errorf("dialing CRI socket %s: %w", neCRISocket, err)
		}
		defer conn.Close()

		reg := prometheus.NewRegistry()
		collector := nodeexporter.NewExporter(conn,
			nodeexporter.WithProcRoot(neProcRoot),
			nodeexporter.WithOnlyImagesInUse(neOnlyImagesInUse),
			nodeexporter.WithLabelAllowlist(neLabelAllowlist),
		)
		reg.MustRegister(collector)
		reg.MustRegister(newBuildInfoCollector("container_image_node_exporter"))

		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		srv := &http.Server{
			Addr:              neMetricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		return srv.ListenAndServe()
	},
}

func init() {
	nodeExporterCmd.Flags().StringVar(&neMetricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	nodeExporterCmd.Flags().StringVar(&neCRISocket, "cri-socket", "/run/containerd/containerd.sock", "Path to the CRI socket.")
	nodeExporterCmd.Flags().StringVar(&neProcRoot, "proc-root", "/host/proc", "Path where the host's /proc is mounted.")
	nodeExporterCmd.Flags().BoolVar(&neOnlyImagesInUse, "only-images-in-use", true,
		"If true (default), only export node_image_* metrics for images currently in use by a running container. "+
			"If false, every image cached by the local CRI runtime is reported.")
	nodeExporterCmd.Flags().StringArrayVar(&neLabelAllowlist, "label-allowlist", nil,
		"OCI image label keys to include in container_image_node_image_labels metrics (can be specified multiple times). Emits all labels if not set.")
}
