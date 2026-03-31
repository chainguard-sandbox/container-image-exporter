package controller

import (
	"context"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	namespace = "container_image"
)

var (
	metricUp = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "up"),
		"1 if the last metrics collection completed successfully (all resource types listed), 0 otherwise.",
		nil, nil,
	)
	metricContainerInfo = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "container_info"),
		"Info about containers running in the cluster, including the image digest resolved by the exporter.",
		[]string{"group", "version", "kind", "namespace", "name", "jsonpath", "image", "digest"}, nil,
	)
	metricAnnotation = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "annotation"),
		"Annotations from the image manifest.",
		[]string{"digest", "key", "value"}, nil,
	)
	metricLabel = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "label"),
		"Labels from the image config.",
		[]string{"digest", "key", "value"}, nil,
	)
	metricSize = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "size_bytes"),
		"The size of the image in the registry.",
		[]string{"digest"}, nil,
	)
	metricCreated = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "created"),
		"The created date from the image config. Expressed as a Unix Epoch Time.",
		[]string{"digest"}, nil,
	)
	metricCacheOldestEntry = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "cache_oldest_entry_timestamp"),
		"Unix timestamp of the oldest entry in the image cache. Use with container_image_cache_duration_seconds to detect stale caches: (time() - container_image_cache_oldest_entry_timestamp) > container_image_cache_duration_seconds.",
		nil, nil,
	)
	metricCacheDuration = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "cache_duration_seconds"),
		"Configured cache duration in seconds.",
		nil, nil,
	)
)

// Exporter exports metrics about container images in Kubernetes
type Exporter struct {
	Client              client.Client
	Cache               ContainerImageCache
	Resources           []watchedResource
	CacheDuration       time.Duration
	AnnotationAllowlist []string
	LabelAllowlist      []string
}

// Describe all the metrics provided by the Exporter
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- metricUp
	ch <- metricContainerInfo
	ch <- metricAnnotation
	ch <- metricLabel
	ch <- metricSize
	ch <- metricCreated
	ch <- metricCacheOldestEntry
	ch <- metricCacheDuration
}

// Collect metrics
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Issue a metric that describes how long we keep items in the cache.
	// Useful for debugging cache issues.
	ch <- prometheus.MustNewConstMetric(
		metricCacheDuration, prometheus.GaugeValue, e.CacheDuration.Seconds(),
	)

	// List all the watched resources. If we can't list a particular
	// resource, abort and set up = 0
	type resourceList struct {
		resource watchedResource
		items    []unstructured.Unstructured
	}
	all := make([]resourceList, 0, len(e.Resources))
	for _, r := range e.Resources {
		ul := &unstructured.UnstructuredList{}
		ul.SetGroupVersionKind(r.groupVersionKind)
		if err := e.Client.List(ctx, ul); err != nil {
			ctrl.Log.Error(err, "listing resources", "gvk", r.groupVersionKind)
			ch <- prometheus.MustNewConstMetric(metricUp, prometheus.GaugeValue, 0)
			return
		}
		all = append(all, resourceList{r, ul.Items})
	}

	// Iterate over the resources and emit metrics for each of the
	// containers
	var (
		activeRefs  []name.Reference
		oldestEntry time.Time
		digests     = map[string]struct{}{}
	)
	for _, rl := range all {
		for _, item := range rl.items {
			for _, container := range containerSpecs(&item, rl.resource.containerPaths) {
				// If we can parse the image reference, use it to
				// fetch the image from the cache
				var (
					digestStr string
					cached    *CachedContainerImage
				)
				ref, err := name.ParseReference(container.Image)
				if err == nil {
					activeRefs = append(activeRefs, ref)
					cached, err = e.Cache.Get(ctx, ref)
					if err == nil {
						digestStr = cached.Digest
					}
				}

				// This metric describes each container defined
				// within the resource, including the resolved
				// digest (if available)
				ch <- prometheus.MustNewConstMetric(
					metricContainerInfo,
					prometheus.GaugeValue,
					1.0,
					item.GroupVersionKind().Group,
					item.GroupVersionKind().Version,
					item.GroupVersionKind().Kind,
					item.GetNamespace(),
					item.GetName(),
					container.JSONPath,
					container.Image,
					digestStr,
				)

				// If we didn't find the image in the cache then there
				// are no more metrics to emit
				if cached == nil {
					continue
				}

				// Only process digest-specific metrics once.
				if _, ok := digests[cached.Digest]; ok {
					continue
				}
				digests[cached.Digest] = struct{}{}

				// Track the oldest cache entry to detect stuck reconcilers.
				if oldestEntry.IsZero() || cached.Time.Before(oldestEntry) {
					oldestEntry = cached.Time
				}

				ch <- prometheus.MustNewConstMetric(
					metricSize, prometheus.GaugeValue, float64(cached.Size), cached.Digest,
				)
				ch <- prometheus.MustNewConstMetric(
					metricCreated, prometheus.GaugeValue, float64(cached.Created.Unix()), cached.Digest,
				)

				for k, v := range cached.Annotations {
					if !allowed(e.AnnotationAllowlist, k) {
						continue
					}
					ch <- prometheus.MustNewConstMetric(
						metricAnnotation,
						prometheus.GaugeValue,
						1.0,
						cached.Digest,
						k,
						v,
					)
				}
				for k, v := range cached.Labels {
					if !allowed(e.LabelAllowlist, k) {
						continue
					}
					ch <- prometheus.MustNewConstMetric(
						metricLabel,
						prometheus.GaugeValue,
						1.0,
						cached.Digest,
						k,
						v,
					)
				}
			}
		}
	}

	// Emit cache staleness metrics. The oldest entry timestamp combined with
	// the cache duration allows alerting on stuck reconcilers:
	//   (time() - container_image_cache_oldest_entry_timestamp) > container_image_cache_duration_seconds
	if !oldestEntry.IsZero() {
		ch <- prometheus.MustNewConstMetric(
			metricCacheOldestEntry, prometheus.GaugeValue, float64(oldestEntry.Unix()),
		)
	}

	// Evict cache entries for images no longer referenced by any active
	// workload. All lists succeeded so activeRefs is complete.
	if err := e.Cache.Evict(ctx, activeRefs); err != nil {
		ctrl.Log.Error(err, "evicting stale cache entries")
	}

	ch <- prometheus.MustNewConstMetric(metricUp, prometheus.GaugeValue, 1)
}

// allowed returns true if key should be emitted given allowlist. An empty
// allowlist means all keys are allowed.
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
