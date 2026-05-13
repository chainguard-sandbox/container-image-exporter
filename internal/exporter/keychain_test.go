package exporter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// dockerConfigJSON returns a minimal .dockerconfigjson byte slice that
// k8schain.NewFromPullSecrets understands: a single auths entry keyed by
// registry hostname with username/password/auth.
func dockerConfigJSON(t *testing.T, registry, username, password string) []byte {
	t.Helper()
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	cfg := map[string]any{
		"auths": map[string]any{
			registry: map[string]string{
				"username": username,
				"password": password,
				"auth":     auth,
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal docker config: %v", err)
	}
	return b
}

func dockerConfigSecret(t *testing.T, name, namespace, registry, username, password string) *corev1.Secret {
	t.Helper()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: dockerConfigJSON(t, registry, username, password),
		},
	}
}

// resolveAuth parses ref, resolves it through kc, and returns the basic-auth
// pair. Uses ref.Context() (a name.Repository, which implements
// authn.Resource) — name.Reference itself does not.
func resolveAuth(t *testing.T, kc authn.Keychain, ref string) (string, string) {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse ref %q: %v", ref, err)
	}
	auth, err := kc.Resolve(r.Context())
	if err != nil {
		t.Fatalf("Resolve(%q): %v", ref, err)
	}
	cfg, err := auth.Authorization()
	if err != nil {
		t.Fatalf("Authorization(%q): %v", ref, err)
	}
	return cfg.Username, cfg.Password
}

// newReconciler wires up a pullSecretReconciler against a fake
// controller-runtime client seeded with objs.
func newReconciler(t *testing.T, namespace string, names []string, objs ...client.Object) (*pullSecretReconciler, *pullSecretKeychain) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	pk := newPullSecretKeychain()
	return &pullSecretReconciler{
		Client:    cl,
		namespace: namespace,
		names:     names,
		keychain:  pk,
	}, pk
}

func reconcile1(t *testing.T, rec *pullSecretReconciler) {
	t.Helper()
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func TestPullSecretKeychain_EmptyBeforeFirstReconcile(t *testing.T) {
	// A freshly constructed keychain should resolve to Anonymous so callers
	// never see nil before a reconcile fires.
	pk := newPullSecretKeychain()
	user, _ := resolveAuth(t, pk, "registry.example.com/foo:latest")
	if user != "" {
		t.Fatalf("got user %q, want anonymous (empty)", user)
	}
}

func TestPullSecretReconciler_BuildsKeychainFromNamedSecret(t *testing.T) {
	secret := dockerConfigSecret(t, "creds", "cie", "registry.example.com", "alice", "s3cret")
	rec, pk := newReconciler(t, "cie", []string{"creds"}, secret)
	reconcile1(t, rec)

	user, pass := resolveAuth(t, pk, "registry.example.com/foo:latest")
	if user != "alice" || pass != "s3cret" {
		t.Fatalf("got %q/%q, want alice/s3cret", user, pass)
	}
}

func TestPullSecretReconciler_RebuildsOnUpdate(t *testing.T) {
	secret := dockerConfigSecret(t, "creds", "cie", "registry.example.com", "alice", "old")
	rec, pk := newReconciler(t, "cie", []string{"creds"}, secret)
	reconcile1(t, rec)

	if _, p := resolveAuth(t, pk, "registry.example.com/foo:latest"); p != "old" {
		t.Fatalf("initial pass = %q, want old", p)
	}

	// Rotate the credential in the fake store. In the real system this
	// fires a watch event that the controller turns into a Reconcile call;
	// here we drive the Reconcile manually after the Update.
	updated := secret.DeepCopy()
	updated.Data[corev1.DockerConfigJsonKey] = dockerConfigJSON(t, "registry.example.com", "alice", "new")
	if err := rec.Update(context.Background(), updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reconcile1(t, rec)

	if _, p := resolveAuth(t, pk, "registry.example.com/foo:latest"); p != "new" {
		t.Fatalf("rebuilt pass = %q, want new", p)
	}
}

func TestPullSecretReconciler_PicksUpSecretCreatedLater(t *testing.T) {
	rec, pk := newReconciler(t, "cie", []string{"creds"})
	reconcile1(t, rec)
	if u, _ := resolveAuth(t, pk, "registry.example.com/foo:latest"); u != "" {
		t.Fatalf("pre-create user = %q, want anonymous", u)
	}

	secret := dockerConfigSecret(t, "creds", "cie", "registry.example.com", "alice", "s3cret")
	if err := rec.Create(context.Background(), secret); err != nil {
		t.Fatalf("Create: %v", err)
	}
	reconcile1(t, rec)

	user, pass := resolveAuth(t, pk, "registry.example.com/foo:latest")
	if user != "alice" || pass != "s3cret" {
		t.Fatalf("got %q/%q, want alice/s3cret", user, pass)
	}
}

func TestPullSecretReconciler_HandlesDeletion(t *testing.T) {
	secret := dockerConfigSecret(t, "creds", "cie", "registry.example.com", "alice", "s3cret")
	rec, pk := newReconciler(t, "cie", []string{"creds"}, secret)
	reconcile1(t, rec)
	if u, _ := resolveAuth(t, pk, "registry.example.com/foo:latest"); u != "alice" {
		t.Fatalf("initial user = %q, want alice", u)
	}

	if err := rec.Delete(context.Background(), secret); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	reconcile1(t, rec)

	if u, _ := resolveAuth(t, pk, "registry.example.com/foo:latest"); u != "" {
		t.Fatalf("post-delete user = %q, want anonymous", u)
	}
}

func TestPullSecretReconciler_IgnoresUnnamedSecret(t *testing.T) {
	// A Secret in the same namespace but not in our names list must not
	// leak into the keychain. (In the real system this is enforced by the
	// predicate filter; here we rely on the Reconciler's explicit name
	// list.)
	watched := dockerConfigSecret(t, "watched", "cie", "registry.example.com", "alice", "good")
	other := dockerConfigSecret(t, "other", "cie", "registry.example.com", "evil", "bad")
	rec, pk := newReconciler(t, "cie", []string{"watched"}, watched, other)
	reconcile1(t, rec)

	user, pass := resolveAuth(t, pk, "registry.example.com/foo:latest")
	if user != "alice" || pass != "good" {
		t.Fatalf("got %q/%q, want alice/good — unwatched secret leaked", user, pass)
	}
}
