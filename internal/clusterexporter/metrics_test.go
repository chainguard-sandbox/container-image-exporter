package clusterexporter_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/chainguard-sandbox/container-image-exporter/internal/clusterexporter"
)

// --- helpers ---

type pushedImage struct {
	ref    string
	digest string
}

// pushImage creates a random image with the given labels, annotations, and
// creation time, pushes it to the test registry, and returns its reference and
// resolved digest.
func pushImage(t *testing.T, name string, labels, annotations map[string]string, created time.Time) pushedImage {
	t.Helper()

	ref := fmt.Sprintf("%s/%s:latest", testRegistryHost, name)

	img, err := random.Image(512, 1)
	if err != nil {
		t.Fatalf("creating random image: %v", err)
	}

	if annotations != nil {
		img = mutate.Annotations(img, annotations).(gcr.Image)
	}

	configFile, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("getting config file: %v", err)
	}
	configFile = configFile.DeepCopy()
	if labels != nil {
		configFile.Config.Labels = labels
	}
	if !created.IsZero() {
		configFile.Created = gcr.Time{Time: created}
	}
	img, err = mutate.ConfigFile(img, configFile)
	if err != nil {
		t.Fatalf("setting config file: %v", err)
	}

	if err := crane.Push(img, ref); err != nil {
		t.Fatalf("pushing image %s: %v", ref, err)
	}

	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("getting digest: %v", err)
	}

	return pushedImage{ref: ref, digest: digest.String()}
}

// gatherMetrics collects all container_image_* metrics from the global
// controller-runtime metrics registry.
func gatherMetrics(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	mfs, _ := ctrlmetrics.Registry.Gather()
	result := make(map[string]*dto.MetricFamily)
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "container_image_") {
			result[mf.GetName()] = mf
		}
	}
	return result
}

// findMetric returns the first metric in mf named name whose labels are a
// superset of the given labels, or nil if none matches.
func findMetric(mfs map[string]*dto.MetricFamily, name string, labels map[string]string) *dto.Metric {
	mf, ok := mfs[name]
	if !ok {
		return nil
	}
	for _, m := range mf.GetMetric() {
		if labelsMatch(m, labels) {
			return m
		}
	}
	return nil
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// collectLabelValues returns the set of values for labelName across all
// metrics in mfs[metricName] whose labels are a superset of filterLabels.
func collectLabelValues(mfs map[string]*dto.MetricFamily, metricName string, filterLabels map[string]string, labelName string) map[string]struct{} {
	result := map[string]struct{}{}
	mf, ok := mfs[metricName]
	if !ok {
		return result
	}
	for _, m := range mf.GetMetric() {
		if !labelsMatch(m, filterLabels) {
			continue
		}
		for _, lp := range m.GetLabel() {
			if lp.GetName() == labelName {
				result[lp.GetValue()] = struct{}{}
			}
		}
	}
	return result
}

// assertLabelValueSet fails the test if the set of values for labelName in
// mfs[metricName] (filtered by filterLabels) does not exactly match want.
func assertLabelValueSet(t *testing.T, mfs map[string]*dto.MetricFamily, metricName string, filterLabels map[string]string, labelName string, want map[string]struct{}) {
	t.Helper()
	got := collectLabelValues(mfs, metricName, filterLabels, labelName)
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("%s: expected %s=%q but it was absent; got set: %v", metricName, labelName, k, got)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: unexpected %s=%q; want set: %v", metricName, labelName, k, want)
		}
	}
}

// waitForMetric polls until the named metric with the given labels appears, or
// the test times out.
func waitForMetric(t *testing.T, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if m := findMetric(gatherMetrics(t), name, labels); m != nil {
			return m
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for metric %q with labels %v", name, labels)
	return nil
}

// waitForNoMetric polls until the named metric with the given labels is absent.
func waitForNoMetric(t *testing.T, name string, labels map[string]string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if findMetric(gatherMetrics(t), name, labels) == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for metric %q with labels %v to disappear", name, labels)
}

func int32Ptr(i int32) *int32 { return &i }

func deploymentSpec(name, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: image}}},
			},
		},
	}
}

// unstructuredObject builds a minimal unstructured object for a CRD resource.
func unstructuredObject(gvk schema.GroupVersionKind, namespace, name string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": gvk.Group + "/" + gvk.Version,
			"kind":       gvk.Kind,
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": spec,
		},
	}
}

// containerEntry builds a {"name": n, "image": img} map for use in container lists.
func containerEntry(name, image string) map[string]interface{} {
	return map[string]interface{}{"name": name, "image": image}
}

// setupAllowlistManager starts a manager with the given options and a dedicated
// Prometheus registry. It returns a gather function that returns all
// container_image_* metrics from it.
func setupAllowlistManager(t *testing.T, opts ...clusterexporter.Option) (gather func() map[string]*dto.MetricFamily) {
	t.Helper()

	reg := prometheus.NewRegistry()
	mgrCtx, mgrCancel := context.WithCancel(testCtx)
	t.Cleanup(mgrCancel)

	skipValidation := true
	mgr, err := ctrl.NewManager(testCfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: &skipValidation},
	})
	if err != nil {
		t.Fatalf("creating manager: %v", err)
	}

	allOpts := append([]clusterexporter.Option{
		clusterexporter.WithCacheDuration(5 * time.Minute),
		clusterexporter.WithK8sKeychain(false),
		clusterexporter.WithMetricsRegistry(reg),
	}, opts...)

	if err := clusterexporter.SetupControllers(mgr, allOpts...); err != nil {
		t.Fatalf("setting up controllers: %v", err)
	}

	go func() {
		if err := mgr.Start(mgrCtx); err != nil && mgrCtx.Err() == nil {
			panic("allowlist manager exited unexpectedly: " + err.Error())
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(mgrCtx) {
		t.Fatal("cache failed to sync")
	}

	return func() map[string]*dto.MetricFamily {
		mfs, _ := reg.Gather()
		result := make(map[string]*dto.MetricFamily)
		for _, mf := range mfs {
			if strings.HasPrefix(mf.GetName(), "container_image_") {
				result[mf.GetName()] = mf
			}
		}
		return result
	}
}

// --- tests ---

// TestExporter_ContainerInfo checks that container_image_cluster_container_info is
// emitted for each resource type with the correct kind, image, and digest labels.
func TestExporter_ContainerInfo(t *testing.T) {
	img := pushImage(t, "test/container-info", nil, nil, time.Time{})

	tests := []struct {
		kind      string
		group     string
		version   string
		namespace string
		obj       client.Object
	}{
		{
			kind:      "Pod",
			group:     "",
			version:   "v1",
			namespace: "default",
			obj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "container-info-pod", Namespace: "default"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: img.ref}},
				},
			},
		},
		{
			kind:      "Deployment",
			group:     "apps",
			version:   "v1",
			namespace: "default",
			obj:       deploymentSpec("container-info-deploy", img.ref),
		},
		{
			kind:      "StatefulSet",
			group:     "apps",
			version:   "v1",
			namespace: "default",
			obj: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "container-info-sts", Namespace: "default"},
				Spec: appsv1.StatefulSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "container-info-sts"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "container-info-sts"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: img.ref}}},
					},
				},
			},
		},
		{
			kind:      "DaemonSet",
			group:     "apps",
			version:   "v1",
			namespace: "default",
			obj: &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "container-info-ds", Namespace: "default"},
				Spec: appsv1.DaemonSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "container-info-ds"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "container-info-ds"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: img.ref}}},
					},
				},
			},
		},
		{
			kind:      "Job",
			group:     "batch",
			version:   "v1",
			namespace: "default",
			obj: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "container-info-job", Namespace: "default"},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers:    []corev1.Container{{Name: "main", Image: img.ref}},
						},
					},
				},
			},
		},
		{
			kind:      "CronJob",
			group:     "batch",
			version:   "v1",
			namespace: "default",
			obj: &batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{Name: "container-info-cron", Namespace: "default"},
				Spec: batchv1.CronJobSpec{
					Schedule: "0 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									RestartPolicy: corev1.RestartPolicyNever,
									Containers:    []corev1.Container{{Name: "main", Image: img.ref}},
								},
							},
						},
					},
				},
			},
		},
		{
			kind:      "Task",
			group:     "tekton.dev",
			version:   "v1",
			namespace: "default",
			obj: unstructuredObject(
				schema.GroupVersionKind{Group: "tekton.dev", Version: "v1", Kind: "Task"},
				"default", "container-info-task",
				map[string]interface{}{
					"steps": []interface{}{containerEntry("main", img.ref)},
				},
			),
		},
		{
			kind:      "TaskRun",
			group:     "tekton.dev",
			version:   "v1",
			namespace: "default",
			obj: unstructuredObject(
				schema.GroupVersionKind{Group: "tekton.dev", Version: "v1", Kind: "TaskRun"},
				"default", "container-info-taskrun",
				map[string]interface{}{
					"taskSpec": map[string]interface{}{
						"steps": []interface{}{containerEntry("main", img.ref)},
					},
				},
			),
		},
		{
			kind:      "Service",
			group:     "serving.knative.dev",
			version:   "v1",
			namespace: "default",
			obj: unstructuredObject(
				schema.GroupVersionKind{Group: "serving.knative.dev", Version: "v1", Kind: "Service"},
				"default", "container-info-ksvc",
				map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{
							"containers": []interface{}{containerEntry("main", img.ref)},
						},
					},
				},
			),
		},
		{
			kind:      "Revision",
			group:     "serving.knative.dev",
			version:   "v1",
			namespace: "default",
			obj: unstructuredObject(
				schema.GroupVersionKind{Group: "serving.knative.dev", Version: "v1", Kind: "Revision"},
				"default", "container-info-rev",
				map[string]interface{}{
					"containers": []interface{}{containerEntry("main", img.ref)},
				},
			),
		},
		{
			kind:      "Workflow",
			group:     "argoproj.io",
			version:   "v1alpha1",
			namespace: "default",
			obj: unstructuredObject(
				schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Workflow"},
				"default", "container-info-workflow",
				map[string]interface{}{
					"templates": []interface{}{
						map[string]interface{}{
							"name":      "main",
							"container": map[string]interface{}{"image": img.ref},
						},
					},
				},
			),
		},
		{
			kind:      "WorkflowTemplate",
			group:     "argoproj.io",
			version:   "v1alpha1",
			namespace: "default",
			obj: unstructuredObject(
				schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "WorkflowTemplate"},
				"default", "container-info-wftmpl",
				map[string]interface{}{
					"templates": []interface{}{
						map[string]interface{}{
							"name":      "main",
							"container": map[string]interface{}{"image": img.ref},
						},
					},
				},
			),
		},
		{
			kind:    "ClusterWorkflowTemplate",
			group:   "argoproj.io",
			version: "v1alpha1",
			// cluster-scoped: namespace is empty
			namespace: "",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "argoproj.io/v1alpha1",
					"kind":       "ClusterWorkflowTemplate",
					"metadata":   map[string]interface{}{"name": "container-info-cwftmpl"},
					"spec": map[string]interface{}{
						"templates": []interface{}{
							map[string]interface{}{
								"name":      "main",
								"container": map[string]interface{}{"image": img.ref},
							},
						},
					},
				},
			},
		},
		{
			kind:      "CronWorkflow",
			group:     "argoproj.io",
			version:   "v1alpha1",
			namespace: "default",
			obj: unstructuredObject(
				schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "CronWorkflow"},
				"default", "container-info-cronwf",
				map[string]interface{}{
					"schedule": "0 * * * *",
					"workflowSpec": map[string]interface{}{
						"templates": []interface{}{
							map[string]interface{}{
								"name":      "main",
								"container": map[string]interface{}{"image": img.ref},
							},
						},
					},
				},
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			if err := testK8sClient.Create(testCtx, tt.obj); err != nil {
				t.Fatalf("creating %s: %v", tt.kind, err)
			}
			t.Cleanup(func() { testK8sClient.Delete(testCtx, tt.obj) })

			m := waitForMetric(t, "container_image_cluster_container_info", map[string]string{
				"group":     tt.group,
				"version":   tt.version,
				"kind":      tt.kind,
				"namespace": tt.namespace,
				"image":     img.ref,
				"digest":    img.digest,
			})

			if got := m.GetGauge().GetValue(); got != 1.0 {
				t.Errorf("expected gauge 1.0, got %v", got)
			}
		})
	}
}

// TestExporter_InitContainers checks that init containers are tracked as well
// as regular containers, each with the correct JSONPath label.
func TestExporter_InitContainers(t *testing.T) {
	main := pushImage(t, "test/init-main", nil, nil, time.Time{})
	init_ := pushImage(t, "test/init-init", nil, nil, time.Time{})

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "init-container-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init", Image: init_.ref}},
			Containers:     []corev1.Container{{Name: "main", Image: main.ref}},
		},
	}
	if err := testK8sClient.Create(testCtx, pod); err != nil {
		t.Fatalf("creating pod: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, pod) })

	waitForMetric(t, "container_image_cluster_container_info", map[string]string{
		"kind":     "Pod",
		"name":     "init-container-pod",
		"jsonpath": "{.spec.containers[0]}",
		"image":    main.ref,
	})
	waitForMetric(t, "container_image_cluster_container_info", map[string]string{
		"kind":     "Pod",
		"name":     "init-container-pod",
		"jsonpath": "{.spec.initContainers[0]}",
		"image":    init_.ref,
	})
}

// TestExporter_EphemeralContainers checks that ephemeral containers in Pods
// are tracked with the correct JSONPath label.
func TestExporter_EphemeralContainers(t *testing.T) {
	main := pushImage(t, "test/ephemeral-main", nil, nil, time.Time{})
	debugger := pushImage(t, "test/ephemeral-debugger", nil, nil, time.Time{})

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ephemeral-container-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: main.ref}},
		},
	}
	if err := testK8sClient.Create(testCtx, pod); err != nil {
		t.Fatalf("creating pod: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, pod) })

	// Ephemeral containers cannot be set at creation time; they must be added
	// via the /ephemeralcontainers subresource.
	pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{
		{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger", Image: debugger.ref}},
	}
	if err := testK8sClient.SubResource("ephemeralcontainers").Update(testCtx, pod); err != nil {
		t.Fatalf("adding ephemeral container: %v", err)
	}

	waitForMetric(t, "container_image_cluster_container_info", map[string]string{
		"kind":     "Pod",
		"name":     "ephemeral-container-pod",
		"jsonpath": "{.spec.ephemeralContainers[0]}",
		"image":    debugger.ref,
	})
}

// TestExporter_TektonSidecars checks that Tekton Task sidecars are tracked
// in addition to steps.
func TestExporter_TektonSidecars(t *testing.T) {
	step := pushImage(t, "test/tekton-step", nil, nil, time.Time{})
	sidecar := pushImage(t, "test/tekton-sidecar", nil, nil, time.Time{})

	gvk := schema.GroupVersionKind{Group: "tekton.dev", Version: "v1", Kind: "Task"}
	obj := unstructuredObject(gvk, "default", "sidecar-task", map[string]interface{}{
		"steps":    []interface{}{containerEntry("build", step.ref)},
		"sidecars": []interface{}{containerEntry("helper", sidecar.ref)},
	})
	if err := testK8sClient.Create(testCtx, obj); err != nil {
		t.Fatalf("creating Task: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, obj) })

	waitForMetric(t, "container_image_cluster_container_info", map[string]string{
		"kind":  "Task",
		"name":  "sidecar-task",
		"image": step.ref,
	})
	waitForMetric(t, "container_image_cluster_container_info", map[string]string{
		"kind":  "Task",
		"name":  "sidecar-task",
		"image": sidecar.ref,
	})
}

// TestExporter_ArgoTemplateTypes checks that all four Argo container locations
// within a template — container (singleton), script (singleton), initContainers
// (list), and sidecars (list) — are each tracked correctly.
func TestExporter_ArgoTemplateTypes(t *testing.T) {
	containerImg := pushImage(t, "test/argo-container", nil, nil, time.Time{})
	scriptImg := pushImage(t, "test/argo-script", nil, nil, time.Time{})
	initImg := pushImage(t, "test/argo-init", nil, nil, time.Time{})
	sidecarImg := pushImage(t, "test/argo-sidecar", nil, nil, time.Time{})

	gvk := schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Workflow"}
	obj := unstructuredObject(gvk, "default", "argo-template-types", map[string]interface{}{
		"templates": []interface{}{
			map[string]interface{}{
				"name":      "step-with-container",
				"container": map[string]interface{}{"image": containerImg.ref},
				"initContainers": []interface{}{
					containerEntry("init", initImg.ref),
				},
				"sidecars": []interface{}{
					containerEntry("sidecar", sidecarImg.ref),
				},
			},
			map[string]interface{}{
				"name":   "step-with-script",
				"script": map[string]interface{}{"image": scriptImg.ref},
			},
		},
	})
	if err := testK8sClient.Create(testCtx, obj); err != nil {
		t.Fatalf("creating Workflow: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, obj) })

	// container singleton
	waitForMetric(t, "container_image_cluster_container_info", map[string]string{
		"kind": "Workflow", "name": "argo-template-types", "image": containerImg.ref,
	})
	// script singleton (no name field on the container)
	waitForMetric(t, "container_image_cluster_container_info", map[string]string{
		"kind": "Workflow", "name": "argo-template-types", "image": scriptImg.ref,
	})
	// initContainers list
	waitForMetric(t, "container_image_cluster_container_info", map[string]string{
		"kind": "Workflow", "name": "argo-template-types", "image": initImg.ref,
	})
	// sidecars list
	waitForMetric(t, "container_image_cluster_container_info", map[string]string{
		"kind": "Workflow", "name": "argo-template-types", "image": sidecarImg.ref,
	})
}

// TestExporter_ImageMetrics checks that image-specific metrics (labels, annotations,
// size, created) are emitted correctly once the image is resolved.
func TestExporter_ImageMetrics(t *testing.T) {
	createdAt := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	img := pushImage(t,
		"test/image-metrics",
		map[string]string{
			"org.example.team":    "platform",
			"org.example.version": "1.2.3",
		},
		map[string]string{
			"org.opencontainers.image.source": "https://github.com/example/repo",
		},
		createdAt,
	)

	deploy := deploymentSpec("image-metrics-deploy", img.ref)
	if err := testK8sClient.Create(testCtx, deploy); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, deploy) })

	// Config labels: wait for one to confirm reconciliation, then verify the
	// exact set of keys emitted for this digest.
	waitForMetric(t, "container_image_cluster_label", map[string]string{
		"digest": img.digest, "key": "org.example.team", "value": "platform",
	})
	assertLabelValueSet(t, gatherMetrics(t), "container_image_cluster_label",
		map[string]string{"digest": img.digest},
		"key",
		map[string]struct{}{
			"org.example.team":    {},
			"org.example.version": {},
		},
	)

	// Manifest annotations: same pattern.
	waitForMetric(t, "container_image_cluster_annotation", map[string]string{
		"digest": img.digest,
		"key":    "org.opencontainers.image.source",
		"value":  "https://github.com/example/repo",
	})
	assertLabelValueSet(t, gatherMetrics(t), "container_image_cluster_annotation",
		map[string]string{"digest": img.digest},
		"key",
		map[string]struct{}{
			"org.opencontainers.image.source": {},
		},
	)

	// Creation time
	m := waitForMetric(t, "container_image_cluster_created", map[string]string{"digest": img.digest})
	if got, want := m.GetGauge().GetValue(), float64(createdAt.Unix()); got != want {
		t.Errorf("container_image_cluster_created: got %v, want %v", got, want)
	}

	// Size should be positive
	m = waitForMetric(t, "container_image_cluster_size_bytes", map[string]string{"digest": img.digest})
	if m.GetGauge().GetValue() <= 0 {
		t.Errorf("container_image_cluster_size_bytes: expected positive value, got %v", m.GetGauge().GetValue())
	}
}

// TestExporter_CacheStaleness checks that the cache staleness metrics are
// emitted correctly once at least one image has been resolved.
func TestExporter_CacheStaleness(t *testing.T) {
	img := pushImage(t, "test/cache-staleness", nil, nil, time.Time{})
	deploy := deploymentSpec("cache-staleness-deploy", img.ref)
	if err := testK8sClient.Create(testCtx, deploy); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, deploy) })

	// Wait for the image to be reconciled into the cache.
	waitForMetric(t, "container_image_cluster_size_bytes", map[string]string{"digest": img.digest})

	mfs := gatherMetrics(t)

	// container_image_cluster_exporter_cache_duration_seconds should always be present and
	// match the WithCacheDuration(5*time.Minute) configured in TestMain.
	durationMF, ok := mfs["container_image_cluster_exporter_cache_duration_seconds"]
	if !ok {
		t.Fatal("container_image_cluster_exporter_cache_duration_seconds not found")
	}
	if got, want := durationMF.GetMetric()[0].GetGauge().GetValue(), (5 * time.Minute).Seconds(); got != want {
		t.Errorf("container_image_cluster_exporter_cache_duration_seconds: got %v, want %v", got, want)
	}

	// container_image_cluster_exporter_cache_oldest_entry_timestamp should be present. All
	// tests share a global cache, so the oldest entry may predate this test,
	// but it must fall within the last cacheDuration (5 minutes) — any older
	// entry would have been refetched by the reconciler.
	oldestMF, ok := mfs["container_image_cluster_exporter_cache_oldest_entry_timestamp"]
	if !ok {
		t.Fatal("container_image_cluster_exporter_cache_oldest_entry_timestamp not found")
	}
	ts := time.Unix(int64(oldestMF.GetMetric()[0].GetGauge().GetValue()), 0)
	cacheDuration := 5 * time.Minute
	earliest := time.Now().Add(-cacheDuration)
	if ts.Before(earliest) {
		t.Errorf("cache_oldest_entry_timestamp %v is older than cacheDuration (%v); earliest expected: %v", ts, cacheDuration, earliest)
	}
	if ts.After(time.Now()) {
		t.Errorf("cache_oldest_entry_timestamp %v is in the future", ts)
	}
}

// TestExporter_CacheEviction checks that metrics for an image disappear once
// the last workload referencing it is deleted.
func TestExporter_CacheEviction(t *testing.T) {
	img := pushImage(t, "test/cache-eviction", nil, nil, time.Time{})

	deploy := deploymentSpec("cache-eviction-deploy", img.ref)
	if err := testK8sClient.Create(testCtx, deploy); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, deploy) })

	// Wait for the metric to appear
	waitForMetric(t, "container_image_cluster_container_info", map[string]string{
		"kind":  "Deployment",
		"name":  "cache-eviction-deploy",
		"image": img.ref,
	})

	// Delete the deployment. The container_info metric is generated by listing
	// live resources, so it should disappear on the next Gather call once the
	// resource is gone. Image-specific metrics (size, created, etc.) disappear
	// once the cache eviction runs during Collect.
	if err := testK8sClient.Delete(testCtx, deploy); err != nil {
		t.Fatalf("deleting deployment: %v", err)
	}

	waitForNoMetric(t, "container_image_cluster_container_info", map[string]string{
		"kind": "Deployment",
		"name": "cache-eviction-deploy",
	})
	waitForNoMetric(t, "container_image_cluster_size_bytes", map[string]string{
		"digest": img.digest,
	})
}

// TestExporter_ContainerInfoBeforeCachePopulated verifies that
// container_image_cluster_container_info is emitted with an empty digest label when
// the image has not yet been fetched into the cache, and that the digest is
// populated once the reconciler successfully retrieves the image.
func TestExporter_ContainerInfoBeforeCachePopulated(t *testing.T) {
	// Use a ref that does not exist in the registry yet, so every reconcile
	// attempt returns a 404 and the cache stays empty.
	ref := fmt.Sprintf("%s/test/before-cache-populated:latest", testRegistryHost)

	gather := setupAllowlistManager(t)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "before-cache-populated-deploy", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "before-cache-populated"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "before-cache-populated"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: ref}}},
			},
		},
	}
	if err := testK8sClient.Create(testCtx, deploy); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, deploy) })

	// The image does not exist yet; every reconcile attempt will fail with a
	// registry 404. Poll until the Exporter has listed the deployment (so
	// the informer cache has synced) and emits container_info with digest="".
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if findMetric(gather(), "container_image_cluster_container_info", map[string]string{
			"image": ref, "digest": "",
		}) != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if findMetric(gather(), "container_image_cluster_container_info", map[string]string{
		"image": ref, "digest": "",
	}) == nil {
		t.Fatal("timed out waiting for container_image_cluster_container_info with empty digest")
	}

	// Now push the image. The reconciler retries failed reconciles via
	// controller-runtime's error requeue, so it will pick up the new image
	// without any manual intervention.
	img := pushImage(t, "test/before-cache-populated", nil, nil, time.Time{})

	// Wait for the digest to be populated in the metric.
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if findMetric(gather(), "container_image_cluster_container_info", map[string]string{
			"image": ref, "digest": img.digest,
		}) != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if findMetric(gather(), "container_image_cluster_container_info", map[string]string{
		"image": ref, "digest": img.digest,
	}) == nil {
		t.Errorf("timed out waiting for container_image_cluster_container_info with digest %s", img.digest)
	}
}

// TestAnnotationAllowlist verifies that only the allowed annotation keys are
// emitted when WithAnnotationAllowlist is configured.
func TestAnnotationAllowlist(t *testing.T) {
	img := pushImage(t, "test/annotation-allowlist",
		nil,
		map[string]string{
			"org.example.allowed":     "yes",
			"org.example.not-allowed": "no",
		},
		time.Time{},
	)

	gather := setupAllowlistManager(t,
		clusterexporter.WithAnnotationAllowlist([]string{"org.example.allowed"}),
	)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "annotation-allowlist-deploy", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "annotation-allowlist"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "annotation-allowlist"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: img.ref}}},
			},
		},
	}
	if err := testK8sClient.Create(testCtx, deploy); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, deploy) })

	// Wait until the allowed annotation appears.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if findMetric(gather(), "container_image_cluster_annotation", map[string]string{
			"digest": img.digest, "key": "org.example.allowed",
		}) != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if findMetric(gather(), "container_image_cluster_annotation", map[string]string{
		"digest": img.digest, "key": "org.example.allowed",
	}) == nil {
		t.Fatal("timed out waiting for allowed annotation metric")
	}

	// The non-allowed key must not appear.
	if findMetric(gather(), "container_image_cluster_annotation", map[string]string{
		"digest": img.digest, "key": "org.example.not-allowed",
	}) != nil {
		t.Error("unexpected annotation metric for non-allowed key org.example.not-allowed")
	}
}

// TestLabelAllowlist verifies that only the allowed label keys are emitted when
// WithLabelAllowlist is configured.
func TestLabelAllowlist(t *testing.T) {
	img := pushImage(t, "test/label-allowlist",
		map[string]string{
			"org.example.allowed":     "yes",
			"org.example.not-allowed": "no",
		},
		nil,
		time.Time{},
	)

	gather := setupAllowlistManager(t,
		clusterexporter.WithLabelAllowlist([]string{"org.example.allowed"}),
	)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "label-allowlist-deploy", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "label-allowlist"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "label-allowlist"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: img.ref}}},
			},
		},
	}
	if err := testK8sClient.Create(testCtx, deploy); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, deploy) })

	// Wait until the allowed label appears.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if findMetric(gather(), "container_image_cluster_label", map[string]string{
			"digest": img.digest, "key": "org.example.allowed",
		}) != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if findMetric(gather(), "container_image_cluster_label", map[string]string{
		"digest": img.digest, "key": "org.example.allowed",
	}) == nil {
		t.Fatal("timed out waiting for allowed label metric")
	}

	// The non-allowed key must not appear.
	if findMetric(gather(), "container_image_cluster_label", map[string]string{
		"digest": img.digest, "key": "org.example.not-allowed",
	}) != nil {
		t.Error("unexpected label metric for non-allowed key org.example.not-allowed")
	}
}

// TestAnnotationAllowlistMultipleKeys verifies that a multi-key allowlist
// emits all allowed keys and suppresses all others.
func TestAnnotationAllowlistMultipleKeys(t *testing.T) {
	img := pushImage(t, "test/annotation-allowlist-multi",
		nil,
		map[string]string{
			"org.example.first":       "1",
			"org.example.second":      "2",
			"org.example.not-allowed": "no",
		},
		time.Time{},
	)

	gather := setupAllowlistManager(t,
		clusterexporter.WithAnnotationAllowlist([]string{"org.example.first", "org.example.second"}),
	)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "annotation-allowlist-multi-deploy", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "annotation-allowlist-multi"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "annotation-allowlist-multi"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: img.ref}}},
			},
		},
	}
	if err := testK8sClient.Create(testCtx, deploy); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, deploy) })

	// Wait for both allowed keys to appear.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		mfs := gather()
		if findMetric(mfs, "container_image_cluster_annotation", map[string]string{"digest": img.digest, "key": "org.example.first"}) != nil &&
			findMetric(mfs, "container_image_cluster_annotation", map[string]string{"digest": img.digest, "key": "org.example.second"}) != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	assertLabelValueSet(t, gather(), "container_image_cluster_annotation",
		map[string]string{"digest": img.digest},
		"key",
		map[string]struct{}{"org.example.first": {}, "org.example.second": {}},
	)
}

// TestLabelAllowlistMultipleKeys verifies that a multi-key allowlist emits all
// allowed keys and suppresses all others.
func TestLabelAllowlistMultipleKeys(t *testing.T) {
	img := pushImage(t, "test/label-allowlist-multi",
		map[string]string{
			"org.example.first":       "1",
			"org.example.second":      "2",
			"org.example.not-allowed": "no",
		},
		nil,
		time.Time{},
	)

	gather := setupAllowlistManager(t,
		clusterexporter.WithLabelAllowlist([]string{"org.example.first", "org.example.second"}),
	)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "label-allowlist-multi-deploy", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "label-allowlist-multi"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "label-allowlist-multi"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: img.ref}}},
			},
		},
	}
	if err := testK8sClient.Create(testCtx, deploy); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}
	t.Cleanup(func() { testK8sClient.Delete(testCtx, deploy) })

	// Wait for both allowed keys to appear.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		mfs := gather()
		if findMetric(mfs, "container_image_cluster_label", map[string]string{"digest": img.digest, "key": "org.example.first"}) != nil &&
			findMetric(mfs, "container_image_cluster_label", map[string]string{"digest": img.digest, "key": "org.example.second"}) != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	assertLabelValueSet(t, gather(), "container_image_cluster_label",
		map[string]string{"digest": img.digest},
		"key",
		map[string]struct{}{"org.example.first": {}, "org.example.second": {}},
	)
}
