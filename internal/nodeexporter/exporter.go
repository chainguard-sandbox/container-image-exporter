package nodeexporter

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
)

const metricNamespace = "container_image"

var (
	metricContainerOSInfo = prometheus.NewDesc(
		prometheus.BuildFQName(metricNamespace, "container", "os_info"),
		"OS release information sourced from /etc/os-release inside each running container.",
		[]string{"container_id", "namespace", "pod", "container", "image", "digest",
			"build_id", "id", "id_like", "image_id", "image_version", "name", "pretty_name",
			"variant", "variant_id", "version", "version_codename", "version_id"}, nil,
	)
	metricUp = prometheus.NewDesc(
		prometheus.BuildFQName(metricNamespace, "node_exporter", "up"),
		"1 if the last collection completed successfully, 0 otherwise.",
		nil, nil,
	)
)

// Exporter implements prometheus.Collector. On each Prometheus scrape it:
//  1. Lists all running containers on this node via the CRI socket.
//  2. Reads /etc/os-release from each container's rootfs via /proc/{pid}/root.
//  3. Emits container_image_container_os_info metrics.
type Exporter struct {
	cri      *CRIClient
	procRoot string
}

// NewExporter creates an Exporter from an existing gRPC connection to the CRI
// socket and the path where the host's /proc is mounted (typically /host/proc
// when running as a DaemonSet, or /proc when running directly on the node).
func NewExporter(conn *grpc.ClientConn, procRoot string) *Exporter {
	return &Exporter{
		cri:      NewCRIClient(conn),
		procRoot: procRoot,
	}
}

// Describe implements prometheus.Collector.
func (c *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- metricContainerOSInfo
	ch <- metricUp
}

// Collect implements prometheus.Collector.
func (c *Exporter) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	containers, err := c.cri.ListRunningContainers(ctx)
	if err != nil {
		slog.Error("listing running containers", "err", err)
		ch <- prometheus.MustNewConstMetric(metricUp, prometheus.GaugeValue, 0)
		return
	}

	for _, container := range containers {
		digest := normalizeDigest(container.Image)

		if container.PID == nil {
			slog.Warn("skipping container: PID not available from CRI", "container_id", container.ID)
			continue
		}

		// Read OS release info from the container's filesystem via procfs.
		osr, err := readOSRelease(c.procRoot, container.PID.PID)
		if err != nil {
			slog.Error("reading /etc/os-release file", "err", err)
			continue
		}

		ch <- prometheus.MustNewConstMetric(
			metricContainerOSInfo, prometheus.GaugeValue, 1,
			container.ID,
			container.PodNamespace,
			container.PodName,
			container.ContainerName,
			container.UserSpecifiedImage,
			digest,
			osr["BUILD_ID"],
			osr["ID"],
			osr["ID_LIKE"],
			osr["IMAGE_ID"],
			osr["IMAGE_VERSION"],
			osr["NAME"],
			osr["PRETTY_NAME"],
			osr["VARIANT"],
			osr["VARIANT_ID"],
			osr["VERSION"],
			osr["VERSION_CODENAME"],
			osr["VERSION_ID"],
		)
	}

	ch <- prometheus.MustNewConstMetric(metricUp, prometheus.GaugeValue, 1)
}

func normalizeDigest(imageRef string) string {
	if i := strings.LastIndex(imageRef, "@"); i >= 0 {
		return imageRef[i+1:]
	}
	return imageRef
}

func readOSRelease(procRoot string, pid int) (map[string]string, error) {
	root := filepath.Join(procRoot, strconv.Itoa(pid), "root")
	for _, rel := range []string{"etc/os-release", "usr/lib/os-release"} {
		path := filepath.Join(root, rel)
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			// Other errors are transient (container stopping between list and read).
			return nil, fmt.Errorf("opening %s: %w", path, err)
		}
		defer f.Close()
		return ParseOSRelease(f), nil
	}
	// Neither file exists — expected for scratch/distroless images.
	return nil, fmt.Errorf("no os-release found under %s (tried etc/os-release, usr/lib/os-release)", root)
}
