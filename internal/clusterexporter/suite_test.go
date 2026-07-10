package clusterexporter_test

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/chainguard-sandbox/container-image-exporter/internal/clusterexporter"
)

var (
	testCtx          context.Context
	testCancel       context.CancelFunc
	testK8sClient    client.Client
	testRegistryHost string
	testCfg          *rest.Config
	testScheme       *runtime.Scheme
)

func TestMain(m *testing.M) {
	testScheme = runtime.NewScheme()
	utilruntime.Must(clusterexporter.AddToScheme(testScheme))

	// Start an in-process container registry. go-containerregistry treats
	// 127.0.0.1 as an insecure registry automatically, so plain HTTP works
	// without any custom transport configuration.
	regServer := httptest.NewServer(registry.New())
	testRegistryHost = regServer.Listener.Addr().String()

	// Start the test Kubernetes API server. Requires the KUBEBUILDER_ASSETS
	// env var to point at the kube-apiserver and etcd binaries; see the
	// Makefile's `make test` target which sets this via setup-envtest.
	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{"testdata/crds"},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		panic("starting test environment: " + err.Error())
	}
	testCfg = cfg

	testCtx, testCancel = context.WithCancel(context.Background())

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		panic("creating manager: " + err.Error())
	}

	if err := clusterexporter.SetupControllers(
		mgr,
		clusterexporter.WithCacheDuration(5*time.Minute),
		clusterexporter.WithK8sKeychain(false),
	); err != nil {
		panic("setting up controllers: " + err.Error())
	}

	testK8sClient = mgr.GetClient()

	go func() {
		if err := mgr.Start(testCtx); err != nil && testCtx.Err() == nil {
			panic("manager exited unexpectedly: " + err.Error())
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(testCtx) {
		panic("cache failed to sync")
	}

	code := m.Run()

	testCancel()
	if err := testEnv.Stop(); err != nil {
		panic("stopping test environment: " + err.Error())
	}
	regServer.Close()

	os.Exit(code)
}
