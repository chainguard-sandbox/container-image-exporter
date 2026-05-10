package exporter

import (
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/prometheus/client_golang/prometheus"
)

// Option is a functional option that configures a controller
type Option func(*options)

type options struct {
	k8sKeychain         bool
	cacheDuration       time.Duration
	platform            *v1.Platform
	registryConcurrency int
	registryRPS         float64
	registryTimeout     time.Duration
	metricsRegistry     prometheus.Registerer
	annotationAllowlist []string
	labelAllowlist      []string
	installNamespace    string
	imagePullSecrets    []string
}

// WithCacheDuration is a functional option that configures the amount of time
// the controller will cache image details before making another request to the
// registry
func WithCacheDuration(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			return
		}
		o.cacheDuration = d
	}
}

// WithK8sKeychain is a functional option that configures whether the controller
// will fetch credentials from pull secrets in the cluster
func WithK8sKeychain(k8sKeychain bool) Option {
	return func(o *options) {
		o.k8sKeychain = k8sKeychain
	}
}

// WithPlatform is a functional option that configures the default platform that
// the conrtroller will resolve multi-architecture images to
func WithPlatform(platform *v1.Platform) Option {
	return func(o *options) {
		o.platform = platform
	}
}

// WithRegistryConcurrency sets the maximum number of concurrent requests per
// registry hostname. A value of 0 disables the limit.
func WithRegistryConcurrency(n int) Option {
	return func(o *options) {
		o.registryConcurrency = n
	}
}

// WithRegistryRPS sets the maximum number of requests per second per registry
// hostname. A value of 0 disables the limit.
func WithRegistryRPS(rps float64) Option {
	return func(o *options) {
		o.registryRPS = rps
	}
}

// WithRegistryTimeout sets the per-image timeout for registry requests.
// A value of 0 disables the timeout.
func WithRegistryTimeout(d time.Duration) Option {
	return func(o *options) {
		o.registryTimeout = d
	}
}

// WithMetricsRegistry sets a custom Prometheus Registerer for the exporter.
// Defaults to the controller-runtime global registry if not set.
func WithMetricsRegistry(r prometheus.Registerer) Option {
	return func(o *options) {
		o.metricsRegistry = r
	}
}

// WithAnnotationAllowlist restricts which annotation keys are emitted as
// container_image_annotation metrics. When empty (the default), all keys are
// emitted.
func WithAnnotationAllowlist(keys []string) Option {
	return func(o *options) {
		o.annotationAllowlist = keys
	}
}

// WithLabelAllowlist restricts which label keys are emitted as
// container_image_label metrics. When empty (the default), all keys are
// emitted.
func WithLabelAllowlist(keys []string) Option {
	return func(o *options) {
		o.labelAllowlist = keys
	}
}

// WithInstallNamespace describes the namespace the exporter is installed
// in. Used to lookup pull secrets specified by WithImagePullSecrets.
func WithInstallNamespace(namespace string) Option {
	return func(o *options) {
		o.installNamespace = namespace
	}
}

// WithImagePullSecrets configures a list of Secret names in the install
// namespace to use as registry credentials.
func WithImagePullSecrets(names []string) Option {
	return func(o *options) {
		o.imagePullSecrets = names
	}
}
