package nodeexporter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
)

const metricNamespace = "container_image"

// maxLabelValueBytes is the cap applied to OCI image label values before they
// are emitted as Prometheus label values. OCI labels are entirely
// builder-controlled and can be arbitrarily large (some images embed SBOMs or
// other JSON blobs as labels); without a cap a single label could blow up
// the exposition response. Values longer than this are truncated and
// suffixed with "…" so consumers can tell the value was cut.
const maxLabelValueBytes = 1024

var errNoOSRelease = errors.New("no os-release file under container root")

var (
	metricNodeContainerInfo = prometheus.NewDesc(
		prometheus.BuildFQName(metricNamespace, "node_container", "info"),
		"Information about running containers from the local CRI runtime, plus OS release details from `/etc/os-release`.",
		[]string{"id", "namespace", "pod", "container", "image", "image_id",
			"os_build_id", "os_id", "os_id_like", "os_image_id", "os_image_version", "os_name", "os_pretty_name",
			"os_variant", "os_variant_id", "os_version", "os_version_codename", "os_version_id"}, nil,
	)
	metricNodeImageLabels = prometheus.NewDesc(
		prometheus.BuildFQName(metricNamespace, "node_image", "labels"),
		"Labels from the image config.",
		[]string{"image_id", "key", "value"}, nil,
	)
	metricNodeImageCreated = prometheus.NewDesc(
		prometheus.BuildFQName(metricNamespace, "node_image", "created"),
		"The created date from the image config. Expressed as a Unix Epoch Time.",
		[]string{"image_id"}, nil,
	)
	metricUp = prometheus.NewDesc(
		prometheus.BuildFQName(metricNamespace, "node_exporter", "up"),
		"1 if the collection completed successfully, 0 otherwise.",
		nil, nil,
	)
)

// defaultProcRoot is where the host's /proc is typically mounted when the
// exporter is deployed as a DaemonSet.
const defaultProcRoot = "/host/proc"

// Exporter implements prometheus.Collector. On each Prometheus scrape it:
//  1. Lists all running containers on this node via the CRI socket.
//  2. Reads /etc/os-release from each container's rootfs via /proc/{pid}/root
//     and emits container_image_node_container_info.
//  3. Lists images cached by the CRI runtime (or only those referenced by a
//     running container, when OnlyImagesInUse is set), enriches each via
//     ImageStatus(verbose=true), and emits container_image_node_image_labels
//     and container_image_node_image_created.
type Exporter struct {
	cri             *CRIClient
	procRoot        string
	onlyImagesInUse bool
	labelAllowlist  []string
}

// Option configures an Exporter.
type Option func(*Exporter)

// WithProcRoot sets the path where the host's /proc filesystem is mounted.
// Defaults to /host/proc (the typical DaemonSet mount path); use /proc when
// running directly on the node.
func WithProcRoot(path string) Option {
	return func(e *Exporter) { e.procRoot = path }
}

// WithOnlyImagesInUse, when set true, restricts node_image_* metrics to
// images referenced by a running container on this node. When false, every
// image cached by the local CRI runtime is reported.
func WithOnlyImagesInUse(only bool) Option {
	return func(e *Exporter) { e.onlyImagesInUse = only }
}

// WithLabelAllowlist restricts which OCI image label keys are emitted as
// container_image_node_image_labels series. When empty (the default), all
// keys are emitted.
func WithLabelAllowlist(keys []string) Option {
	return func(e *Exporter) { e.labelAllowlist = keys }
}

// NewExporter creates an Exporter from an existing gRPC connection to the CRI
// socket and the given options.
func NewExporter(conn *grpc.ClientConn, opts ...Option) *Exporter {
	e := &Exporter{
		cri:      NewCRIClient(conn),
		procRoot: defaultProcRoot,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Describe implements prometheus.Collector.
func (c *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- metricNodeContainerInfo
	ch <- metricNodeImageLabels
	ch <- metricNodeImageCreated
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
		c.collectOSInfo(ch, container)
	}

	if err := c.collectImages(ctx, ch, containers); err != nil {
		slog.Error("collecting image metrics", "err", err)
		ch <- prometheus.MustNewConstMetric(metricUp, prometheus.GaugeValue, 0)
		return
	}

	ch <- prometheus.MustNewConstMetric(metricUp, prometheus.GaugeValue, 1)
}

// collectOSInfo emits the container_image_node_container_info metric for the
// given container by reading /etc/os-release from its rootfs.
func (c *Exporter) collectOSInfo(ch chan<- prometheus.Metric, container *ContainerInfo) {
	// nil means the CRI runtime didn't report a PID. 0 is the kernel
	// swapper — it has no /proc/0/root. Skip both.
	if container.PID == nil || *container.PID == 0 {
		slog.Debug("skipping container: PID not available", "container_id", container.ID)
		return
	}

	osr, err := readOSRelease(c.procRoot, *container.PID)
	switch {
	case errors.Is(err, errNoOSRelease):
		// Distroless/scratch — emit the metric with empty os-release labels so
		// the container is still visible in adoption queries.
		osr = nil
	case err != nil:
		slog.Error("reading /etc/os-release file", "err", err, "container_id", container.ID)
		return
	}

	ch <- prometheus.MustNewConstMetric(
		metricNodeContainerInfo, prometheus.GaugeValue, 1,
		container.ID,
		container.PodNamespace,
		container.PodName,
		container.ContainerName,
		container.UserSpecifiedImage,
		container.ImageRef,
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

// collectImages emits container_image_node_image_{labels,created} for each
// image discovered. The set of images depends on OnlyImagesInUse: when set,
// only images referenced by a running container are looked up; otherwise the
// full ListImages result is enriched and emitted.
func (c *Exporter) collectImages(ctx context.Context, ch chan<- prometheus.Metric, containers []*ContainerInfo) error {
	images, err := c.gatherImages(ctx, containers)
	if err != nil {
		return err
	}
	for _, img := range images {
		c.emitImageMetrics(ch, img)
	}
	return nil
}

func (c *Exporter) gatherImages(ctx context.Context, containers []*ContainerInfo) ([]*Image, error) {
	if !c.onlyImagesInUse {
		return c.cri.ListImages(ctx)
	}

	// Only-in-use mode: walk running containers' image identifiers,
	// deduplicating, and enrich each via ImageStatus. Prefer ImageRef
	// (the same field that becomes node_container_info.image_id) so this
	// path dedupes on the identifier our metrics expose, falling back to
	// the user-specified image string only when the runtime didn't report
	// one.
	seen := make(map[string]bool)
	var images []*Image
	for _, ctr := range containers {
		id := ctr.ImageRef
		if id == "" {
			id = ctr.Image
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		img, err := c.cri.ImageStatus(ctx, id)
		if err != nil {
			slog.Warn("fetching image status", "image_id", id, "err", err)
			continue
		}
		images = append(images, img)
	}
	return images, nil
}

// emitImageMetrics writes the per-image metric series to ch. Both metrics
// are keyed on image_id alone — one node_image_labels series per (image_id,
// OCI label key) and one node_image_created series per image_id. When
// labelAllowlist is non-empty, only OCI labels with keys in the allowlist
// are emitted.
func (c *Exporter) emitImageMetrics(ch chan<- prometheus.Metric, img *Image) {
	if img == nil || img.ID == "" {
		return
	}

	if img.Created != nil {
		ch <- prometheus.MustNewConstMetric(
			metricNodeImageCreated, prometheus.GaugeValue, float64(img.Created.Unix()),
			img.ID,
		)
	}
	for k, v := range img.Labels {
		if !allowed(c.labelAllowlist, k) {
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			metricNodeImageLabels, prometheus.GaugeValue, 1,
			img.ID, k, truncateLabelValue(v),
		)
	}
}

// truncateLabelValue caps an OCI label value at maxLabelValueBytes, replacing
// the tail with "…" when the original was longer. The marker is one rune
// (3 UTF-8 bytes) so the result always fits within maxLabelValueBytes. The
// cut position is walked back to the start of a UTF-8 rune so the truncated
// value never contains a partial multi-byte sequence.
func truncateLabelValue(v string) string {
	if len(v) <= maxLabelValueBytes {
		return v
	}
	const marker = "…"
	end := maxLabelValueBytes - len(marker)
	for end > 0 && !utf8.RuneStart(v[end]) {
		end--
	}
	return v[:end] + marker
}

// allowed returns true if key is in allowlist, or true for any key when
// allowlist is empty (the "no filter set" default).
func allowed(allowlist []string, key string) bool {
	if len(allowlist) == 0 {
		return true
	}
	for _, k := range allowlist {
		if k == key {
			return true
		}
	}
	return false
}

func readOSRelease(procRoot string, pid int) (map[string]string, error) {
	root, err := os.OpenRoot(filepath.Join(procRoot, strconv.Itoa(pid), "root"))
	if err != nil {
		return nil, fmt.Errorf("opening container root: %w", err)
	}
	defer root.Close()

	for _, rel := range []string{"etc/os-release", "usr/lib/os-release"} {
		f, err := root.Open(rel)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", rel, err)
		}
		defer f.Close()
		return ParseOSRelease(f)
	}
	return nil, errNoOSRelease
}
