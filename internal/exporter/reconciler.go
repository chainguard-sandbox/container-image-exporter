package exporter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/awslabs/amazon-ecr-credential-helper/ecr-login"
	"github.com/chrismellard/docker-credential-acr-env/pkg/credhelper"
	"github.com/google/go-containerregistry/pkg/authn"
	k8schain "github.com/google/go-containerregistry/pkg/authn/kubernetes"
	kauth "github.com/google/go-containerregistry/pkg/authn/kubernetes"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"golang.org/x/sync/singleflight"
)

// ContainerImage describes a container image
type ContainerImage struct {
	// Digest is the digest of the image
	Digest string

	// Annotations are the annotations on the image manifest
	Annotations map[string]string

	// Labels are the labels in the image config
	Labels map[string]string

	// Size is the size of the image in the registry
	Size int64

	// Created is created time from the image config
	Created time.Time
}

// ContainerImageReconciler reconciles container images described in a
// Kubernetes object
type ContainerImageReconciler struct {
	client.Client
	KubeClient              kubernetes.Interface
	GroupVersionKind         schema.GroupVersionKind
	ContainerPaths          [][]string
	ImagePullSecretPaths    [][]string
	ServiceAccountNamePaths [][]string
	Cache                   ContainerImageCache
	CacheDuration           time.Duration
	Platform                *v1.Platform
	K8sKeychain             bool
	Transport               http.RoundTripper
	Inflight                *singleflight.Group
}

// Reconcile reconciles objects that define containers
func (r *ContainerImageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithValues(
		"group", r.GroupVersionKind.Group,
		"version", r.GroupVersionKind.Version,
		"kind", r.GroupVersionKind.Kind,
		"namespace", req.Namespace,
		"name", req.Name,
	)
	logger.Info("Reconciling")

	// Get the object
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.GroupVersionKind)
	if err := r.Client.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Construct a keychain for retrieving credentials
	kc, err := r.newKeychain(ctx, obj)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("constructing keychain: %w", err)
	}

	// Iterate over every container spec in the object, fetching the image
	// metadata. This populates the cache that we export metrics from.
	// Deduplicate by image reference so that the same image appearing in
	// multiple container paths (e.g. init container and sidecar) is only
	// fetched once per reconcile.
	seen := map[string]struct{}{}
	for _, container := range containerSpecs(obj, r.ContainerPaths) {
		if _, ok := seen[container.Image]; ok {
			continue
		}
		seen[container.Image] = struct{}{}
		logger.Info("Fetching image metadata", "image", container.Image)
		img, err := r.getImage(ctx, container.Image, remote.WithAuthFromKeychain(kc))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("fetching image details: %w", err)
		}
		logger.Info("Fetched image metadata", "image", container.Image, "digest", img.Digest)
	}

	// Tags are mutable so we should periodically check to see if the digest
	// of any of the container images has changed by requeueing the object.
	d := addJitter(r.CacheDuration)
	logger.Info("Reconciled", "requeue_after", d)
	return ctrl.Result{
		RequeueAfter: d,
	}, nil
}

// addJitter adds random jitter to a duration, extending it by up to 1/6 of
// the original. This spreads out registry fetches across reconcilers to avoid
// thundering herd while ensuring the requeue fires AFTER the cache entry has
// expired, so the reconcile actually triggers a refresh.
func addJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	maxJitter := d / 6
	jitter := time.Duration(rand.Int64N(int64(maxJitter)))
	return d + jitter
}

func (r *ContainerImageReconciler) getImage(ctx context.Context, imgRef string, opts ...remote.Option) (*ContainerImage, error) {
	ref, err := name.ParseReference(imgRef)
	if err != nil {
		return nil, fmt.Errorf("parsing image reference: %w", err)
	}

	// If the cache is configured, attempt to get the details from that
	// first
	if r.Cache != nil {
		cimg, err := r.Cache.Get(ctx, ref)
		if err == nil && time.Now().Before(cimg.Time.Add(r.CacheDuration)) {
			return cimg.ContainerImage, nil
		}
		if !errors.Is(err, ErrContainerImageNotFound) {
			return nil, fmt.Errorf("fetching image details from cache: %w", err)
		}
	}

	// Coalesce concurrent fetches for the same image reference so that a cold
	// cache at startup does not cause multiple reconcilers to hit the registry
	// simultaneously for the same image.
	if r.Inflight != nil {
		v, err, _ := r.Inflight.Do(ref.String(), func() (any, error) {
			return r.fetchImage(ctx, ref, opts...)
		})
		if err != nil {
			return nil, err
		}
		return v.(*ContainerImage), nil
	}
	return r.fetchImage(ctx, ref, opts...)
}

// fetchImage fetches image metadata from the registry and writes the result to
// the cache.
func (r *ContainerImageReconciler) fetchImage(ctx context.Context, ref name.Reference, opts ...remote.Option) (*ContainerImage, error) {
	if r.Transport != nil {
		opts = append(opts, remote.WithTransport(r.Transport))
	}
	desc, err := remote.Get(ref, append(opts, remote.WithContext(ctx))...)
	if err != nil {
		return nil, fmt.Errorf("getting descriptor: %s: %w", ref, err)
	}

	img, err := resolveImageFromDescriptor(desc, r.Platform)
	if err != nil {
		return nil, fmt.Errorf("getting image: %w", err)
	}

	manifest, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("getting manifest: %w", err)
	}

	configFile, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("getting config: %w", err)
	}

	sz := manifest.Config.Size
	for _, layer := range manifest.Layers {
		sz = sz + layer.Size
	}

	cimg := &ContainerImage{
		Digest:      desc.Digest.String(),
		Annotations: manifest.Annotations,
		Labels:      configFile.Config.Labels,
		Size:        sz,
		Created:     configFile.Created.Time,
	}

	if r.Cache != nil {
		if err := r.Cache.Put(ctx, ref, cimg); err != nil {
			return nil, fmt.Errorf("putting details for %s into the cache: %w", desc.Digest, err)
		}
	}

	return cimg, nil
}

var (
	amazonKeychain authn.Keychain = authn.NewKeychainFromHelper(ecr.NewECRHelper(ecr.WithLogger(io.Discard)))
	azureKeychain  authn.Keychain = authn.NewKeychainFromHelper(credhelper.NewACRCredentialsHelper())
)

func (r *ContainerImageReconciler) newKeychain(ctx context.Context, obj *unstructured.Unstructured) (authn.Keychain, error) {
	// Fetch credentials from ~/.docker/config.json and any ambient cloud credentials
	// configured in the environment
	keychains := []authn.Keychain{
		authn.DefaultKeychain,
		google.Keychain,
		amazonKeychain,
		azureKeychain,
	}

	// If enabled, construct a keychain which uses the pull secrets
	// attached to the object and the object's service account.
	if r.K8sKeychain {
		opts := k8schain.Options{
			Namespace:          obj.GetNamespace(),
			ServiceAccountName: serviceAccountName(obj, r.ServiceAccountNamePaths),
			ImagePullSecrets:   imagePullSecrets(obj, r.ImagePullSecretPaths),
		}
		k8s, err := kauth.New(ctx, r.KubeClient, kauth.Options(opts))
		if err != nil {
			return nil, fmt.Errorf("constructing k8s keychain: %w", err)
		}
		keychains = append([]authn.Keychain{k8s}, keychains...)
	}

	return authn.NewMultiKeychain(keychains...), nil
}

func resolveImageFromDescriptor(desc *remote.Descriptor, platform *v1.Platform) (v1.Image, error) {
	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, fmt.Errorf("fetching index: %w", err)
		}
		indexManifest, err := idx.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("fetching manifest: %w", err)
		}
		if len(indexManifest.Manifests) == 0 {
			return nil, fmt.Errorf("no manifests in index")
		}

		// If a platform is configured then look for it in the manifests
		if platform != nil {
			for _, manifest := range indexManifest.Manifests {
				if manifest.Platform.Equals(*platform) {
					return idx.Image(manifest.Digest)
				}
			}
		}

		// If not, or if the platform doesn't exist in the manifests,
		// just return the first one in the list.
		return idx.Image(indexManifest.Manifests[0].Digest)
	}

	return desc.Image()
}

// ContainerSpec is information about a container
type ContainerSpec struct {
	// JSONPath is the path to the container in the object
	JSONPath string

	// Name is the name of the container
	Name string

	// Image is the image reference
	Image string
}

func containerSpecs(obj *unstructured.Unstructured, paths [][]string) []ContainerSpec {
	var result []ContainerSpec
	for _, path := range paths {
		result = append(result, specsForPath(obj.Object, path)...)
	}
	return result
}

// specsForPath extracts ContainerSpecs from obj for a single path. The path
// may contain a single "*" wildcard segment, which causes the list before it
// to be iterated and the suffix applied to each element. The resulting value
// is handled as either a singleton map (e.g. .container, .script) or a list
// (e.g. .initContainers, .sidecars). The name field is treated as optional
// so that containers without names (e.g. Argo script templates) are included.
//
// Only one wildcard per path is supported. Paths with multiple wildcards
// (e.g. spec.templates.*.steps.*.container) will only expand the first one;
// the suffix after it is applied literally, not recursively.
func specsForPath(obj map[string]interface{}, path []string) []ContainerSpec {
	// Find the first (and only) wildcard segment.
	wildcardIdx := -1
	for i, seg := range path {
		if seg == "*" {
			wildcardIdx = i
			break
		}
	}

	if wildcardIdx == -1 {
		// No wildcard — iterate over the slice at path as a container list.
		items, _, _ := unstructured.NestedSlice(obj, path...)
		var result []ContainerSpec
		for i, item := range items {
			data, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			image, ok := data["image"].(string)
			if !ok {
				continue
			}
			name, _ := data["name"].(string)
			result = append(result, ContainerSpec{
				JSONPath: fmt.Sprintf("{.%s[%d]}", strings.Join(path, "."), i),
				Name:     name,
				Image:    image,
			})
		}
		return result
	}

	// Wildcard: iterate over the list at prefix, apply suffix to each element,
	// then dispatch on whether the result is a singleton map or a list.
	prefix := path[:wildcardIdx]
	suffix := path[wildcardIdx+1:]

	items, _, _ := unstructured.NestedSlice(obj, prefix...)
	var result []ContainerSpec
	for i, item := range items {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		val, ok, _ := unstructured.NestedFieldNoCopy(itemMap, suffix...)
		if !ok {
			continue
		}
		switch v := val.(type) {
		case map[string]interface{}:
			// Singleton container field (e.g. .container, .script).
			image, ok := v["image"].(string)
			if !ok {
				continue
			}
			name, _ := v["name"].(string)
			result = append(result, ContainerSpec{
				JSONPath: fmt.Sprintf("{.%s[%d].%s}", strings.Join(prefix, "."), i, strings.Join(suffix, ".")),
				Name:     name,
				Image:    image,
			})
		case []interface{}:
			// List of containers (e.g. .initContainers, .sidecars).
			for j, elem := range v {
				elemMap, ok := elem.(map[string]interface{})
				if !ok {
					continue
				}
				image, ok := elemMap["image"].(string)
				if !ok {
					continue
				}
				name, _ := elemMap["name"].(string)
				result = append(result, ContainerSpec{
					JSONPath: fmt.Sprintf("{.%s[%d].%s[%d]}", strings.Join(prefix, "."), i, strings.Join(suffix, "."), j),
					Name:     name,
					Image:    image,
				})
			}
		}
	}
	return result
}

func imagePullSecrets(obj *unstructured.Unstructured, paths [][]string) []string {
	var secrets []string
	for _, imagePullSecretsPath := range paths {
		pullSecrets, _, _ := unstructured.NestedSlice(obj.Object, imagePullSecretsPath...)
		for _, pullSecret := range pullSecrets {
			data, ok := pullSecret.(map[string]interface{})
			if !ok {
				continue
			}
			name, ok := data["name"].(string)
			if !ok {
				continue
			}

			secrets = append(secrets, name)
		}
	}

	return secrets
}

func serviceAccountName(obj *unstructured.Unstructured, paths [][]string) string {
	for _, serviceAccountNamePath := range paths {
		serviceAccountName, _, _ := unstructured.NestedString(obj.Object, serviceAccountNamePath...)
		if serviceAccountName != "" {
			return serviceAccountName
		}
	}

	return ""
}
