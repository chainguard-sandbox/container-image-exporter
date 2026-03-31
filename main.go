package main

import (
	"fmt"
	"os"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/chainguard-sandbox/container-image-exporter/internal/controller"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(controller.AddToScheme(scheme))
}

var (
	metricsAddr          string
	probeAddr            string
	cacheDuration        time.Duration
	platform             string
	k8sKeychain          bool
	registryConcurrency  int
	registryRPS          float64
	namespaces           []string
	annotationAllowlist  []string
	labelAllowlist       []string
)

var rootCmd = &cobra.Command{
	Use:   "container-image-exporter",
	Short: "Export metrics about container images in a Kubernetes cluster.",
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

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: scheme,
			Metrics: metricsserver.Options{
				BindAddress: metricsAddr,
			},
			HealthProbeBindAddress: probeAddr,
			Cache:                 cacheOpts,
		})
		if err != nil {
			return fmt.Errorf("creating a new manager: %w", err)
		}

		var p *v1.Platform
		if platform != "" {
			p, err = v1.ParsePlatform(platform)
			if err != nil {
				return fmt.Errorf("parsing platform: %w", err)
			}
		}

		if err = controller.SetupControllers(
			mgr,
			controller.WithCacheDuration(cacheDuration),
			controller.WithK8sKeychain(k8sKeychain),
			controller.WithPlatform(p),
			controller.WithRegistryConcurrency(registryConcurrency),
			controller.WithRegistryRPS(registryRPS),
			controller.WithAnnotationAllowlist(annotationAllowlist),
			controller.WithLabelAllowlist(labelAllowlist),
		); err != nil {
			return fmt.Errorf("setting up controllers: %w", err)
		}

		if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
			return fmt.Errorf("adding healthz check: %w", err)
		}
		if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
			return fmt.Errorf("adding readyz check: %w", err)
		}

		return mgr.Start(ctrl.SetupSignalHandler())
	},
}

func init() {
	rootCmd.Flags().StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	rootCmd.Flags().StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	rootCmd.Flags().StringVar(&platform, "platform", "linux/amd64", "The default platform to resolve multi-arch images to.")
	rootCmd.Flags().DurationVar(&cacheDuration, "cache-duration", 1*time.Hour, "How long to cache image details for before querying the registry again.")
	rootCmd.Flags().BoolVar(&k8sKeychain, "k8s-keychain", true, "Whether to fetch credentials from pulls secrets in the cluster.")
	rootCmd.Flags().IntVar(&registryConcurrency, "registry-concurrency", 10, "Maximum number of concurrent requests per registry. Set to 0 to disable.")
	rootCmd.Flags().Float64Var(&registryRPS, "registry-rps", 5, "Maximum requests per second per registry. Set to 0 to disable.")
	rootCmd.Flags().StringArrayVar(&namespaces, "namespaces", nil, "Namespaces to watch (can be specified multiple times). Watches all namespaces if not set.")
	rootCmd.Flags().StringArrayVar(&annotationAllowlist, "annotation-allowlist", nil, "Annotation keys to include in container_image_annotation metrics (can be specified multiple times). Emits all annotations if not set.")
	rootCmd.Flags().StringArrayVar(&labelAllowlist, "label-allowlist", nil, "Label keys to include in container_image_label metrics (can be specified multiple times). Emits all labels if not set.")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
