package clusterexporter

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// AddToScheme registers all types required by the controller with s.
// Both the main entrypoint and tests should call this to ensure the scheme
// is consistent.
func AddToScheme(s *runtime.Scheme) error {
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return err
	}
	if err := corev1.AddToScheme(s); err != nil {
		return err
	}
	if err := appsv1.AddToScheme(s); err != nil {
		return err
	}
	return batchv1.AddToScheme(s)
}

// watchedResource describes a Kubernetes resource type to watch for container images.
type watchedResource struct {
	object                  client.Object
	groupVersionKind         schema.GroupVersionKind
	containerPaths          [][]string
	imagePullSecretPaths    [][]string
	serviceAccountNamePaths [][]string
}

// podContainerPaths covers the container locations in a Pod spec.
var podContainerPaths = [][]string{
	{"spec", "initContainers"},
	{"spec", "containers"},
	{"spec", "ephemeralContainers"},
}

// podTemplateContainerPaths covers the container locations in resources that
// embed a pod template (Deployment, StatefulSet, DaemonSet, Job).
var podTemplateContainerPaths = [][]string{
	{"spec", "template", "spec", "initContainers"},
	{"spec", "template", "spec", "containers"},
}

// cronJobContainerPaths covers the container locations in a CronJob, which
// nests a job template inside its spec.
var cronJobContainerPaths = [][]string{
	{"spec", "jobTemplate", "spec", "template", "spec", "initContainers"},
	{"spec", "jobTemplate", "spec", "template", "spec", "containers"},
}

// podCredentialPaths covers imagePullSecrets and serviceAccountName in a bare
// Pod spec (also used by Knative Revision and Argo Workflow/WorkflowTemplate/
// ClusterWorkflowTemplate).
var podImagePullSecretPaths = [][]string{
	{"spec", "imagePullSecrets"},
}
var podServiceAccountNamePaths = [][]string{
	{"spec", "serviceAccountName"},
}

// podTemplateCredentialPaths covers credentials in resources that embed a pod
// template (Deployment, StatefulSet, DaemonSet, Job, Knative Service).
var podTemplateImagePullSecretPaths = [][]string{
	{"spec", "template", "spec", "imagePullSecrets"},
}
var podTemplateServiceAccountNamePaths = [][]string{
	{"spec", "template", "spec", "serviceAccountName"},
}

// cronJobCredentialPaths covers credentials in a CronJob.
var cronJobImagePullSecretPaths = [][]string{
	{"spec", "jobTemplate", "spec", "template", "spec", "imagePullSecrets"},
}
var cronJobServiceAccountNamePaths = [][]string{
	{"spec", "jobTemplate", "spec", "template", "spec", "serviceAccountName"},
}

// builtinResources are the standard Kubernetes resource types always watched.
var builtinResources = []watchedResource{
	{
		object:                  &corev1.Pod{},
		groupVersionKind:         schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		containerPaths:          podContainerPaths,
		imagePullSecretPaths:    podImagePullSecretPaths,
		serviceAccountNamePaths: podServiceAccountNamePaths,
	},
	{
		object:                  &appsv1.Deployment{},
		groupVersionKind:         schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
		containerPaths:          podTemplateContainerPaths,
		imagePullSecretPaths:    podTemplateImagePullSecretPaths,
		serviceAccountNamePaths: podTemplateServiceAccountNamePaths,
	},
	{
		object:                  &appsv1.StatefulSet{},
		groupVersionKind:         schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"},
		containerPaths:          podTemplateContainerPaths,
		imagePullSecretPaths:    podTemplateImagePullSecretPaths,
		serviceAccountNamePaths: podTemplateServiceAccountNamePaths,
	},
	{
		object:                  &appsv1.DaemonSet{},
		groupVersionKind:         schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"},
		containerPaths:          podTemplateContainerPaths,
		imagePullSecretPaths:    podTemplateImagePullSecretPaths,
		serviceAccountNamePaths: podTemplateServiceAccountNamePaths,
	},
	{
		object:                  &batchv1.Job{},
		groupVersionKind:         schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"},
		containerPaths:          podTemplateContainerPaths,
		imagePullSecretPaths:    podTemplateImagePullSecretPaths,
		serviceAccountNamePaths: podTemplateServiceAccountNamePaths,
	},
	{
		object:                  &batchv1.CronJob{},
		groupVersionKind:         schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"},
		containerPaths:          cronJobContainerPaths,
		imagePullSecretPaths:    cronJobImagePullSecretPaths,
		serviceAccountNamePaths: cronJobServiceAccountNamePaths,
	},
}

// knownAdditionalResources describes well-known CRDs that are watched
// automatically if discovered in the cluster.
var knownAdditionalResources = []struct {
	group                   string
	resource                string // plural name used for discovery
	kind                    string
	containerPaths          [][]string
	imagePullSecretPaths    [][]string
	serviceAccountNamePaths [][]string
}{
	{
		group:    "tekton.dev",
		resource: "tasks",
		kind:     "Task",
		containerPaths: [][]string{
			{"spec", "steps"},
			{"spec", "sidecars"},
		},
		// Tasks don't carry imagePullSecrets; credentials come from the
		// service account bound to the TaskRun at runtime.
		serviceAccountNamePaths: [][]string{
			{"spec", "serviceAccountName"},
		},
	},
	{
		group:    "tekton.dev",
		resource: "taskruns",
		kind:     "TaskRun",
		containerPaths: [][]string{
			{"spec", "taskSpec", "steps"},
			{"spec", "taskSpec", "sidecars"},
		},
		imagePullSecretPaths: [][]string{
			{"spec", "podTemplate", "imagePullSecrets"},
		},
		serviceAccountNamePaths: [][]string{
			{"spec", "serviceAccountName"},
		},
	},
	{
		group:    "serving.knative.dev",
		resource: "services",
		kind:     "Service",
		containerPaths: [][]string{
			{"spec", "template", "spec", "containers"},
			{"spec", "template", "spec", "initContainers"},
		},
		imagePullSecretPaths:    podTemplateImagePullSecretPaths,
		serviceAccountNamePaths: podTemplateServiceAccountNamePaths,
	},
	{
		group:    "serving.knative.dev",
		resource: "revisions",
		kind:     "Revision",
		containerPaths: [][]string{
			{"spec", "containers"},
			{"spec", "initContainers"},
		},
		imagePullSecretPaths:    podImagePullSecretPaths,
		serviceAccountNamePaths: podServiceAccountNamePaths,
	},
	{
		group:    "argoproj.io",
		resource: "workflows",
		kind:     "Workflow",
		containerPaths: [][]string{
			{"spec", "templates", "*", "container"},
			{"spec", "templates", "*", "initContainers"},
			{"spec", "templates", "*", "sidecars"},
			{"spec", "templates", "*", "script"},
		},
		imagePullSecretPaths:    podImagePullSecretPaths,
		serviceAccountNamePaths: podServiceAccountNamePaths,
	},
	{
		group:    "argoproj.io",
		resource: "workflowtemplates",
		kind:     "WorkflowTemplate",
		containerPaths: [][]string{
			{"spec", "templates", "*", "container"},
			{"spec", "templates", "*", "initContainers"},
			{"spec", "templates", "*", "sidecars"},
			{"spec", "templates", "*", "script"},
		},
		imagePullSecretPaths:    podImagePullSecretPaths,
		serviceAccountNamePaths: podServiceAccountNamePaths,
	},
	{
		group:    "argoproj.io",
		resource: "clusterworkflowtemplates",
		kind:     "ClusterWorkflowTemplate",
		containerPaths: [][]string{
			{"spec", "templates", "*", "container"},
			{"spec", "templates", "*", "initContainers"},
			{"spec", "templates", "*", "sidecars"},
			{"spec", "templates", "*", "script"},
		},
		imagePullSecretPaths:    podImagePullSecretPaths,
		serviceAccountNamePaths: podServiceAccountNamePaths,
	},
	{
		group:    "argoproj.io",
		resource: "cronworkflows",
		kind:     "CronWorkflow",
		containerPaths: [][]string{
			{"spec", "workflowSpec", "templates", "*", "container"},
			{"spec", "workflowSpec", "templates", "*", "initContainers"},
			{"spec", "workflowSpec", "templates", "*", "sidecars"},
			{"spec", "workflowSpec", "templates", "*", "script"},
		},
		imagePullSecretPaths: [][]string{
			{"spec", "workflowSpec", "imagePullSecrets"},
		},
		serviceAccountNamePaths: [][]string{
			{"spec", "workflowSpec", "serviceAccountName"},
		},
	},
}

// discoverAdditionalResources uses the API server's discovery endpoint to find
// which of the knownAdditionalResources are installed in the cluster and
// returns a watchedResource for each one found.
func discoverAdditionalResources(mgr ctrl.Manager) ([]watchedResource, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("creating discovery client: %w", err)
	}

	// ServerPreferredResources returns the server's preferred version for each
	// resource. Partial failures (e.g. a CRD group temporarily unavailable)
	// are logged but do not abort discovery.
	resourceLists, err := dc.ServerPreferredResources()
	if err != nil {
		ctrl.Log.Info("partial failure during resource discovery, some CRDs may not be watched", "err", err)
		if len(resourceLists) == 0 {
			return nil, fmt.Errorf("fetching server resources: %w", err)
		}
	}

	// Build a lookup: group → resource name → (version, kind)
	type vk struct{ version, kind string }
	available := map[string]map[string]vk{}
	for _, list := range resourceLists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		if available[gv.Group] == nil {
			available[gv.Group] = map[string]vk{}
		}
		for _, r := range list.APIResources {
			available[gv.Group][r.Name] = vk{gv.Version, r.Kind}
		}
	}

	// Create a direct (non-cache-backed) client to verify list permissions for
	// each discovered CRD. The cache hasn't started yet so we can't use the
	// manager's client here.
	directClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return nil, fmt.Errorf("creating direct client: %w", err)
	}

	var result []watchedResource
	for _, known := range knownAdditionalResources {
		groupResources, ok := available[known.group]
		if !ok {
			continue
		}
		found, ok := groupResources[known.resource]
		if !ok {
			continue
		}
		gvk := schema.GroupVersionKind{
			Group:   known.group,
			Version: found.version,
			Kind:    found.kind,
		}

		// Verify we can list this resource before registering a watcher. If
		// the service account lacks permission, skip it so that missing RBAC
		// for a CRD type doesn't poison every scrape with up=0.
		ul := &unstructured.UnstructuredList{}
		ul.SetGroupVersionKind(gvk)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		listErr := directClient.List(ctx, ul, client.Limit(1))
		cancel()
		if listErr != nil {
			if apierrors.IsForbidden(listErr) || apierrors.IsUnauthorized(listErr) {
				ctrl.Log.Info("skipping resource type: insufficient permissions to list", "group", known.group, "kind", found.kind)
			} else {
				ctrl.Log.Info("skipping resource type: list check failed", "group", known.group, "kind", found.kind, "err", listErr)
			}
			continue
		}

		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		result = append(result, watchedResource{
			object:                  u,
			groupVersionKind:         gvk,
			containerPaths:          known.containerPaths,
			imagePullSecretPaths:    known.imagePullSecretPaths,
			serviceAccountNamePaths: known.serviceAccountNamePaths,
		})
	}
	return result, nil
}

// SetupControllers constructs and registers controllers
func SetupControllers(mgr ctrl.Manager, opts ...Option) error {
	o := &options{
		cacheDuration: 1 * time.Hour,
	}
	for _, opt := range opts {
		opt(o)
	}

	// A direct (non-cache-backed) client is required here because k8schain
	// fetches Secrets and ServiceAccounts on demand at reconcile time, and
	// those objects may not be present in the manager's informer cache.
	kubeClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return err
	}

	additional, err := discoverAdditionalResources(mgr)
	if err != nil {
		return fmt.Errorf("discovering additional resources: %w", err)
	}

	allResources := append(builtinResources, additional...)

	// Avoid requesting information about the same images multiple times by
	// caching the responses.
	cache := NewContainerImageCache()

	// A single singleflight group shared across all reconcilers ensures that
	// concurrent cache misses for the same image ref (e.g. at startup when
	// multiple resource-type reconcilers all process their queues at once)
	// coalesce into a single registry request.
	inflight := &singleflight.Group{}

	// Determine the Prometheus registry up front: needed both for the
	// transport's request metrics and for the Exporter collector below.
	reg := prometheus.Registerer(metrics.Registry)
	if o.metricsRegistry != nil {
		reg = o.metricsRegistry
	}

	// All reconcilers share a single rate-limiting + instrumented transport
	// so per-registry limits are enforced globally and request count + latency
	// metrics aggregate across resource types. Constructed unconditionally:
	// even with no concurrency or rate limits the transport still records
	// per-host HTTP metrics, which are the only signal users have for
	// registry-side failures.
	transport := newRegistryTransport(nil, int64(o.registryConcurrency), o.registryRPS, reg)

	// If --image-pull-secret was provided, register a Secret reconciler with
	// the manager that rebuilds a shared keychain whenever any of the named
	// Secrets changes. Reuses the manager's cache (scoped to the install
	// namespace via cache.ByObject in cmd) and workqueue, so rotations
	// propagate without a pod restart and bursts of events get coalesced.
	var staticKeychain authn.Keychain
	if len(o.imagePullSecrets) > 0 {
		if o.installNamespace == "" {
			return fmt.Errorf("WithImagePullSecrets requires WithInstallNamespace")
		}
		pk := newPullSecretKeychain()
		nameSet := make(map[string]struct{}, len(o.imagePullSecrets))
		for _, n := range o.imagePullSecrets {
			nameSet[n] = struct{}{}
		}
		pred := predicate.NewPredicateFuncs(func(obj client.Object) bool {
			if obj.GetNamespace() != o.installNamespace {
				return false
			}
			_, ok := nameSet[obj.GetName()]
			return ok
		})
		if err := ctrl.NewControllerManagedBy(mgr).
			Named("pull-secret-keychain").
			For(&corev1.Secret{}, builder.WithPredicates(pred)).
			Complete(&pullSecretReconciler{
				Client:    mgr.GetClient(),
				namespace: o.installNamespace,
				names:     o.imagePullSecrets,
				keychain:  pk,
			}); err != nil {
			return fmt.Errorf("registering pull-secret reconciler: %w", err)
		}
		staticKeychain = pk
	}

	for _, r := range allResources {
		reconciler := &ContainerImageReconciler{
			Client:                  mgr.GetClient(),
			KubeClient:              kubeClient,
			GroupVersionKind:         r.groupVersionKind,
			ContainerPaths:          r.containerPaths,
			ImagePullSecretPaths:    r.imagePullSecretPaths,
			ServiceAccountNamePaths: r.serviceAccountNamePaths,
			Cache:                   cache,
			CacheDuration:           o.cacheDuration,
			Platform:                o.platform,
			K8sKeychain:             o.k8sKeychain,
			StaticKeychain:          staticKeychain,
			Transport:               transport,
			Inflight:                inflight,
			RegistryTimeout:         o.registryTimeout,
		}
		if err := ctrl.NewControllerManagedBy(mgr).For(r.object).Complete(reconciler); err != nil {
			return fmt.Errorf("unable to create controller for %s: %w", r.groupVersionKind, err)
		}
	}

	// Register the Exporter collector (reg was selected earlier so the
	// transport could share it).
	reg.Register(&Exporter{
		Client:              mgr.GetClient(),
		Cache:               cache,
		Resources:           allResources,
		CacheDuration:       o.cacheDuration,
		AnnotationAllowlist: o.annotationAllowlist,
		LabelAllowlist:      o.labelAllowlist,
	})

	return nil
}
