package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/chainguard-sandbox/container-image-exporter/internal/controller"
)

// pushedIndex holds an OCI image index pushed to the test registry. The
// reconciler stores the index digest (desc.Digest from remote.Get), not the
// per-platform manifest digest. Creation times on the platform images differ
// so tests can verify which platform was actually resolved by inspecting
// container_image_created.
type pushedIndex struct {
	ref          string
	indexDigest  string
	amd64Created time.Time
	arm64Created time.Time
}

// pushIndex builds a two-platform OCI image index (linux/amd64 and
// linux/arm64) with distinct creation timestamps, pushes it to the test
// registry, and returns the reference, index digest, and per-platform
// creation times.
func pushIndex(t *testing.T, imageName string) pushedIndex {
	t.Helper()

	ref := testRegistryHost + "/" + imageName + ":latest"

	amd64Created := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	arm64Created := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

	buildManifest := func(platform *ggcr.Platform, created time.Time) ggcr.Image {
		img, err := random.Image(512, 1)
		if err != nil {
			t.Fatalf("creating random image: %v", err)
		}
		cf, err := img.ConfigFile()
		if err != nil {
			t.Fatalf("getting config file: %v", err)
		}
		cf = cf.DeepCopy()
		cf.OS = platform.OS
		cf.Architecture = platform.Architecture
		cf.Created = ggcr.Time{Time: created}
		img, err = mutate.ConfigFile(img, cf)
		if err != nil {
			t.Fatalf("setting config file: %v", err)
		}
		return img
	}

	amd64Platform := &ggcr.Platform{OS: "linux", Architecture: "amd64"}
	arm64Platform := &ggcr.Platform{OS: "linux", Architecture: "arm64"}

	amd64Img := buildManifest(amd64Platform, amd64Created)
	arm64Img := buildManifest(arm64Platform, arm64Created)

	idx := mutate.AppendManifests(
		mutate.IndexMediaType(empty.Index, types.OCIImageIndex),
		mutate.IndexAddendum{Add: amd64Img, Descriptor: ggcr.Descriptor{Platform: amd64Platform}},
		mutate.IndexAddendum{Add: arm64Img, Descriptor: ggcr.Descriptor{Platform: arm64Platform}},
	)

	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parsing reference %s: %v", ref, err)
	}
	if err := remote.WriteIndex(parsed, idx); err != nil {
		t.Fatalf("pushing index %s: %v", ref, err)
	}

	idxDigest, err := idx.Digest()
	if err != nil {
		t.Fatalf("getting index digest: %v", err)
	}

	return pushedIndex{
		ref:          ref,
		indexDigest:  idxDigest.String(),
		amd64Created: amd64Created,
		arm64Created: arm64Created,
	}
}

// waitDeadline returns a deadline 30 seconds from now.
func waitDeadline() time.Time {
	return time.Now().Add(30 * time.Second)
}

// TestMultiArch_PlatformMatch verifies that when WithPlatform is set and a
// matching manifest exists in the index, the resolved image's creation time
// corresponds to that platform's manifest.
func TestMultiArch_PlatformMatch(t *testing.T) {
	idx := pushIndex(t, "test/multiarch-match")

	p, _ := ggcr.ParsePlatform("linux/amd64")
	gather := setupAllowlistManager(t, controller.WithPlatform(p))

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "multiarch-match-deploy", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "multiarch-match"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "multiarch-match"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: idx.ref}}},
			},
		},
	}
	if err := testK8sClient.Create(testCtx, deploy); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, deploy) })

	// Wait for reconciliation: container_image_size_bytes is emitted with the
	// index digest once the image has been resolved.
	deadline := waitDeadline()
	for time.Now().Before(deadline) {
		if findMetric(gather(), "container_image_size_bytes", map[string]string{
			"digest": idx.indexDigest,
		}) != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if findMetric(gather(), "container_image_size_bytes", map[string]string{
		"digest": idx.indexDigest,
	}) == nil {
		t.Fatalf("timed out waiting for size metric for index digest %s", idx.indexDigest)
	}

	// container_image_created reflects the resolved platform image's config.
	// For linux/amd64 it should equal amd64Created.
	m := findMetric(gather(), "container_image_created", map[string]string{"digest": idx.indexDigest})
	if m == nil {
		t.Fatal("container_image_created metric not found")
	}
	got := time.Unix(int64(m.GetGauge().GetValue()), 0).UTC()
	if got != idx.amd64Created {
		t.Errorf("container_image_created: got %v, want amd64 created %v", got, idx.amd64Created)
	}
}

// TestMultiArch_PlatformFallback verifies that when WithPlatform is set but no
// matching manifest exists in the index, the first manifest is used as a
// fallback.
func TestMultiArch_PlatformFallback(t *testing.T) {
	idx := pushIndex(t, "test/multiarch-fallback")

	p, _ := ggcr.ParsePlatform("linux/s390x") // not in the index
	gather := setupAllowlistManager(t, controller.WithPlatform(p))

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "multiarch-fallback-deploy", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "multiarch-fallback"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "multiarch-fallback"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: idx.ref}}},
			},
		},
	}
	if err := testK8sClient.Create(testCtx, deploy); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, deploy) })

	deadline := waitDeadline()
	for time.Now().Before(deadline) {
		if findMetric(gather(), "container_image_size_bytes", map[string]string{
			"digest": idx.indexDigest,
		}) != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if findMetric(gather(), "container_image_size_bytes", map[string]string{
		"digest": idx.indexDigest,
	}) == nil {
		t.Fatalf("timed out waiting for size metric for index digest %s", idx.indexDigest)
	}

	// The first manifest in the index is amd64, so creation time should match.
	m := findMetric(gather(), "container_image_created", map[string]string{"digest": idx.indexDigest})
	if m == nil {
		t.Fatal("container_image_created metric not found")
	}
	got := time.Unix(int64(m.GetGauge().GetValue()), 0).UTC()
	if got != idx.amd64Created {
		t.Errorf("container_image_created: got %v, want amd64 created (fallback) %v", got, idx.amd64Created)
	}
}

// TestMultiArch_NoPlatform verifies that when WithPlatform is not set, the
// first manifest in the index is used.
func TestMultiArch_NoPlatform(t *testing.T) {
	idx := pushIndex(t, "test/multiarch-noplatform")

	gather := setupAllowlistManager(t) // no WithPlatform

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "multiarch-noplatform-deploy", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "multiarch-noplatform"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "multiarch-noplatform"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: idx.ref}}},
			},
		},
	}
	if err := testK8sClient.Create(testCtx, deploy); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, deploy) })

	deadline := waitDeadline()
	for time.Now().Before(deadline) {
		if findMetric(gather(), "container_image_size_bytes", map[string]string{
			"digest": idx.indexDigest,
		}) != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if findMetric(gather(), "container_image_size_bytes", map[string]string{
		"digest": idx.indexDigest,
	}) == nil {
		t.Fatalf("timed out waiting for size metric for index digest %s", idx.indexDigest)
	}

	// With no platform set, the first manifest (amd64) should be used.
	m := findMetric(gather(), "container_image_created", map[string]string{"digest": idx.indexDigest})
	if m == nil {
		t.Fatal("container_image_created metric not found")
	}
	got := time.Unix(int64(m.GetGauge().GetValue()), 0).UTC()
	if got != idx.amd64Created {
		t.Errorf("container_image_created: got %v, want amd64 created (first manifest) %v", got, idx.amd64Created)
	}
}

// TestNamespaceFiltering verifies that when the manager is restricted to a
// single namespace, the exporter only reports metrics for workloads in that
// namespace.
func TestNamespaceFiltering(t *testing.T) {
	imgWatched := pushImage(t, "test/ns-watched", nil, nil, time.Time{})
	imgIgnored := pushImage(t, "test/ns-ignored", nil, nil, time.Time{})

	// Create the two namespaces.
	for _, ns := range []string{"ns-watched", "ns-ignored"} {
		obj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		if err := testK8sClient.Create(testCtx, obj); err != nil {
			t.Fatalf("creating namespace %s: %v", ns, err)
		}
		t.Cleanup(func() {
			_ = testK8sClient.Delete(testCtx, obj)
		})
	}

	// Build a fresh manager restricted to "ns-watched" only.
	nsMap := map[string]cache.Config{"ns-watched": {}}
	cacheOpts := cache.Options{DefaultNamespaces: nsMap}
	mgrCtx, mgrCancel := context.WithCancel(testCtx)
	t.Cleanup(mgrCancel)

	skipValidation := true
	mgr, err := ctrl.NewManager(testCfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  cacheOpts,
		Controller:             config.Controller{SkipNameValidation: &skipValidation},
	})
	if err != nil {
		t.Fatalf("creating manager: %v", err)
	}

	// Use a dedicated Prometheus registry so we don't pollute the global one.
	reg := prometheus.NewRegistry()

	if err := controller.SetupControllers(
		mgr,
		controller.WithCacheDuration(5*time.Minute),
		controller.WithK8sKeychain(false),
		controller.WithMetricsRegistry(reg),
	); err != nil {
		t.Fatalf("setting up controllers: %v", err)
	}

	go func() {
		if err := mgr.Start(mgrCtx); err != nil && mgrCtx.Err() == nil {
			panic("namespace-filter manager exited unexpectedly: " + err.Error())
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(mgrCtx) {
		t.Fatal("cache failed to sync")
	}

	// Create a pod in each namespace.
	watchedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-watched", Namespace: "ns-watched"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: imgWatched.ref}}},
	}
	ignoredPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-ignored", Namespace: "ns-ignored"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: imgIgnored.ref}}},
	}
	if err := testK8sClient.Create(testCtx, watchedPod); err != nil {
		t.Fatalf("creating watched pod: %v", err)
	}
	t.Cleanup(func() { _ = testK8sClient.Delete(testCtx, watchedPod) })
	if err := testK8sClient.Create(testCtx, ignoredPod); err != nil {
		t.Fatalf("creating ignored pod: %v", err)
	}
	t.Cleanup(func() { _ = testK8sClient.Delete(testCtx, ignoredPod) })

	// gather collects container_image_* metrics from our dedicated registry.
	gather := func() map[string]*dto.MetricFamily {
		mfs, _ := reg.Gather()
		result := make(map[string]*dto.MetricFamily)
		for _, mf := range mfs {
			if strings.HasPrefix(mf.GetName(), "container_image_") {
				result[mf.GetName()] = mf
			}
		}
		return result
	}

	// Wait until the watched image's size metric appears. This requires the
	// reconciler to have fetched the image and populated the local cache, which
	// only happens for namespaces in the watch scope.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if m := findMetric(gather(), "container_image_size_bytes", map[string]string{
			"digest": imgWatched.digest,
		}); m != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if findMetric(gather(), "container_image_size_bytes", map[string]string{
		"digest": imgWatched.digest,
	}) == nil {
		t.Fatal("timed out waiting for ns-watched image size metric")
	}

	// The ignored namespace pod's image must never be fetched into the cache,
	// so its size metric must not appear.
	if findMetric(gather(), "container_image_size_bytes", map[string]string{
		"digest": imgIgnored.digest,
	}) != nil {
		t.Error("unexpected size metric for image in ns-ignored: reconciler should not process pods outside the watched namespace")
	}
}
