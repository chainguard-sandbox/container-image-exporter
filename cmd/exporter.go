package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	goruntime "runtime"
	"sync/atomic"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/chainguard-sandbox/container-image-exporter/internal/exporter"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(exporter.AddToScheme(scheme))
}

var (
	metricsAddr         string
	probeAddr           string
	cacheDuration       time.Duration
	platform            string
	k8sKeychain         bool
	registryConcurrency int
	registryRPS         float64
	registryTimeout     time.Duration
	installNamespace    string
	namespaces          []string
	annotationAllowlist []string
	labelAllowlist      []string
	imagePullSecrets    []string
)

var exporterCmd = &cobra.Command{
	Use:   "exporter",
	Short: "Watch Kubernetes resources and export image metadata fetched from registries.",
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := zap.Options{}
		ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

		cfg := ctrl.GetConfigOrDie()

		var cacheOpts cache.Options
		if len(namespaces) > 0 {
			nsMap := map[string]cache.Config{}
			for _, ns := range namespaces {
				nsMap[ns] = cache.Config{}
			}
			cacheOpts.DefaultNamespaces = nsMap
		}

		if len(imagePullSecrets) > 0 {
			if installNamespace == "" {
				return errors.New("--image-pull-secret requires --install-namespace to be set to the namespace the named Secrets live in")
			}
			if cacheOpts.ByObject == nil {
				cacheOpts.ByObject = map[client.Object]cache.ByObject{}
			}
			cacheOpts.ByObject[&corev1.Secret{}] = cache.ByObject{
				Namespaces: map[string]cache.Config{installNamespace: {}},
			}
		}

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: scheme,
			Metrics: metricsserver.Options{
				BindAddress: metricsAddr,
			},
			HealthProbeBindAddress: probeAddr,
			Cache:                  cacheOpts,
		})
		if err != nil {
			return fmt.Errorf("creating a new manager: %w", err)
		}

		if platform == "" {
			platform = goruntime.GOOS + "/" + goruntime.GOARCH
		}
		p, err := v1.ParsePlatform(platform)
		if err != nil {
			return fmt.Errorf("parsing platform: %w", err)
		}

		metrics.Registry.MustRegister(newBuildInfoCollector("container_image_exporter"))

		if err = exporter.SetupControllers(
			mgr,
			exporter.WithCacheDuration(cacheDuration),
			exporter.WithK8sKeychain(k8sKeychain),
			exporter.WithPlatform(p),
			exporter.WithRegistryConcurrency(registryConcurrency),
			exporter.WithRegistryRPS(registryRPS),
			exporter.WithRegistryTimeout(registryTimeout),
			exporter.WithAnnotationAllowlist(annotationAllowlist),
			exporter.WithLabelAllowlist(labelAllowlist),
			exporter.WithInstallNamespace(installNamespace),
			exporter.WithImagePullSecrets(imagePullSecrets),
		); err != nil {
			return fmt.Errorf("setting up controllers: %w", err)
		}

		if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
			return fmt.Errorf("adding healthz check: %w", err)
		}

		var cacheSynced atomic.Bool
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			if mgr.GetCache().WaitForCacheSync(ctx) {
				cacheSynced.Store(true)
			}
			return nil
		})); err != nil {
			return fmt.Errorf("adding cache-sync tracker: %w", err)
		}

		if err := mgr.AddReadyzCheck("cache-sync", func(_ *http.Request) error {
			if !cacheSynced.Load() {
				return errors.New("informer cache not synced")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("adding readyz check: %w", err)
		}

		return mgr.Start(ctrl.SetupSignalHandler())
	},
}

func init() {
	exporterCmd.Flags().StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	exporterCmd.Flags().StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	exporterCmd.Flags().StringVar(&platform, "platform", "", "The default platform to resolve multi-arch images to. Defaults to the platform the exporter is running on.")
	exporterCmd.Flags().DurationVar(&cacheDuration, "cache-duration", 1*time.Hour, "How long to cache image details for before querying the registry again.")
	exporterCmd.Flags().BoolVar(&k8sKeychain, "k8s-keychain", false, "Whether to fetch credentials from pulls secrets in the cluster.")
	exporterCmd.Flags().IntVar(&registryConcurrency, "registry-concurrency", 10, "Maximum number of concurrent requests per registry. Set to 0 to disable.")
	exporterCmd.Flags().Float64Var(&registryRPS, "registry-rps", 5, "Maximum requests per second per registry. Set to 0 to disable.")
	exporterCmd.Flags().DurationVar(&registryTimeout, "registry-timeout", 30*time.Second, "Per-image timeout for registry requests. Set to 0 to disable.")
	exporterCmd.Flags().StringVar(&installNamespace, "install-namespace", "", "Namespace the exporter is installed in. Used to look up the Secrets named via --image-pull-secret. The Helm chart sets this automatically to the release namespace.")
	exporterCmd.Flags().StringArrayVar(&namespaces, "namespaces", nil, "Namespaces to watch (can be specified multiple times). Watches all namespaces if not set.")
	exporterCmd.Flags().StringArrayVar(&annotationAllowlist, "annotation-allowlist", nil, "Annotation keys to include in container_image_annotation metrics (can be specified multiple times). Emits all annotations if not set.")
	exporterCmd.Flags().StringArrayVar(&labelAllowlist, "label-allowlist", nil, "Label keys to include in container_image_label metrics (can be specified multiple times). Emits all labels if not set.")
	exporterCmd.Flags().StringArrayVar(&imagePullSecrets, "image-pull-secret", nil, "Pull secrets to use as registry credentials. Must be provided with --install-namespace, which specifies the namespace the secrets are installed in.")
}
