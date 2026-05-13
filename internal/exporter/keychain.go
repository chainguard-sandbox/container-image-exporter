package exporter

import (
	"context"
	"sync/atomic"

	"github.com/google/go-containerregistry/pkg/authn"
	k8schain "github.com/google/go-containerregistry/pkg/authn/kubernetes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// pullSecretKeychain is an authn.Keychain whose underlying credentials are
// rebuilt by pullSecretReconciler whenever a watched Secret changes.
// Resolve always observes the most recently built keychain via an atomic
// pointer.
type pullSecretKeychain struct {
	keychain atomic.Pointer[authn.Keychain]
}

// newPullSecretKeychain returns a keychain seeded with an empty (Anonymous)
// inner keychain so Resolve never observes nil before the first reconcile
// fires.
func newPullSecretKeychain() *pullSecretKeychain {
	pk := &pullSecretKeychain{}
	empty, _ := k8schain.NewFromPullSecrets(context.Background(), nil)
	pk.keychain.Store(&empty)
	return pk
}

// Resolve implements authn.Keychain.
func (k *pullSecretKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return (*k.keychain.Load()).Resolve(target)
}

// pullSecretReconciler watches a fixed list of Secrets in a single
// namespace and rebuilds the shared keychain on every change. Backed by
// the manager's cache (scoped to the install namespace via
// cache.ByObject so list/watch is namespaced too).
type pullSecretReconciler struct {
	client.Client
	namespace string
	names     []string
	keychain  *pullSecretKeychain
}

// Reconcile rebuilds the entire keychain from the current state of every
// named Secret. The workqueue dedupes bursts of events for free, and
// rebuilding wholesale keeps the keychain trivially consistent with the
// live Secret state.
func (r *pullSecretReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	secrets := make([]corev1.Secret, 0, len(r.names))
	for _, n := range r.names {
		var s corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: n}, &s); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return reconcile.Result{}, err
		}
		secrets = append(secrets, s)
	}
	kc, err := k8schain.NewFromPullSecrets(ctx, secrets)
	if err != nil {
		return reconcile.Result{}, err
	}
	r.keychain.keychain.Store(&kc)
	return reconcile.Result{}, nil
}
