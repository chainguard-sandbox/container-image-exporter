package controller

import (
	"net/http"
	"sync"

	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

// registryTransport is an http.RoundTripper that enforces per-registry
// concurrency and rate limits. Each registry hostname gets its own semaphore
// and token bucket so that a slow or throttled registry cannot starve requests
// to other registries.
//
// The rate limiter is applied first (waiting for a token), then the semaphore
// is acquired. This ensures the concurrency slot is not held while waiting for
// a rate limit token.
type registryTransport struct {
	base        http.RoundTripper
	concurrency int64
	rps         float64
	mu          sync.Mutex
	semaphores  map[string]*semaphore.Weighted
	limiters    map[string]*rate.Limiter
}

func newRegistryTransport(base http.RoundTripper, concurrency int64, rps float64) *registryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &registryTransport{
		base:        base,
		concurrency: concurrency,
		rps:         rps,
		semaphores:  make(map[string]*semaphore.Weighted),
		limiters:    make(map[string]*rate.Limiter),
	}
}

func (t *registryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()

	// Wait for a rate limit token before acquiring the concurrency slot.
	if t.rps > 0 {
		if err := t.limiterFor(host).Wait(req.Context()); err != nil {
			return nil, err
		}
	}

	// Acquire the concurrency slot.
	if t.concurrency > 0 {
		sem := t.semaphoreFor(host)
		if err := sem.Acquire(req.Context(), 1); err != nil {
			return nil, err
		}
		defer sem.Release(1)
	}

	return t.base.RoundTrip(req)
}

func (t *registryTransport) semaphoreFor(host string) *semaphore.Weighted {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.semaphores[host]; !ok {
		t.semaphores[host] = semaphore.NewWeighted(t.concurrency)
	}
	return t.semaphores[host]
}

func (t *registryTransport) limiterFor(host string) *rate.Limiter {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.limiters[host]; !ok {
		// Burst is set to the concurrency limit so that up to N requests can
		// fire immediately when the system has been idle, before the sustained
		// rate limit takes effect. Falls back to 1 if concurrency is disabled.
		burst := int(t.concurrency)
		if burst < 1 {
			burst = 1
		}
		t.limiters[host] = rate.NewLimiter(rate.Limit(t.rps), burst)
	}
	return t.limiters[host]
}
