package exporter

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

// newRegistryTransport composes per-host rate limiting, concurrency, and HTTP metrics onto base.
func newRegistryTransport(base http.RoundTripper, concurrency int64, rps float64, reg prometheus.Registerer) http.RoundTripper {
	if base == nil {
		base = remote.DefaultTransport
	}
	t := base
	if reg != nil {
		t = newMetricsTransport(t, reg)
	}
	if concurrency > 0 {
		t = newConcurrencyTransport(t, concurrency)
	}
	if rps > 0 {
		burst := int(concurrency)
		if burst < 1 {
			burst = 1
		}
		t = newRateLimitTransport(t, rps, burst)
	}
	return t
}

// rateLimitTransport applies a per-host token-bucket rate limit, then delegates to base.
type rateLimitTransport struct {
	base     http.RoundTripper
	rps      float64
	burst    int
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

func newRateLimitTransport(base http.RoundTripper, rps float64, burst int) *rateLimitTransport {
	return &rateLimitTransport{
		base:     base,
		rps:      rps,
		burst:    burst,
		limiters: make(map[string]*rate.Limiter),
	}
}

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.limiterFor(req.URL.Hostname()).Wait(req.Context()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func (t *rateLimitTransport) limiterFor(host string) *rate.Limiter {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.limiters[host]; !ok {
		t.limiters[host] = rate.NewLimiter(rate.Limit(t.rps), t.burst)
	}
	return t.limiters[host]
}

// concurrencyTransport bounds in-flight requests per host with a semaphore, then delegates to base.
type concurrencyTransport struct {
	base http.RoundTripper
	max  int64
	mu   sync.Mutex
	sems map[string]*semaphore.Weighted
}

func newConcurrencyTransport(base http.RoundTripper, max int64) *concurrencyTransport {
	return &concurrencyTransport{
		base: base,
		max:  max,
		sems: make(map[string]*semaphore.Weighted),
	}
}

func (t *concurrencyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sem := t.semFor(req.URL.Hostname())
	if err := sem.Acquire(req.Context(), 1); err != nil {
		return nil, err
	}
	defer sem.Release(1)
	return t.base.RoundTrip(req)
}

func (t *concurrencyTransport) semFor(host string) *semaphore.Weighted {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.sems[host]; !ok {
		t.sems[host] = semaphore.NewWeighted(t.max)
	}
	return t.sems[host]
}

// metricsTransport records request count and latency per (host, method, code).
type metricsTransport struct {
	base     http.RoundTripper
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newMetricsTransport(base http.RoundTripper, reg prometheus.Registerer) *metricsTransport {
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "container_image",
		Subsystem: "registry",
		Name:      "requests_total",
		Help:      "Count of HTTP requests issued to container registries, labelled by host, HTTP method, and response code (\"0\" when no response was received).",
	}, []string{"host", "method", "code"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "container_image",
		Subsystem: "registry",
		Name:      "request_duration_seconds",
		Help:      "Latency of HTTP requests to container registries, labelled by host, HTTP method, and response code.",
	}, []string{"host", "method", "code"})
	reg.MustRegister(requests, duration)
	return &metricsTransport{base: base, requests: requests, duration: duration}
}

func (t *metricsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	elapsed := time.Since(start).Seconds()
	code := "0"
	if resp != nil {
		code = strconv.Itoa(resp.StatusCode)
	}
	host := req.URL.Hostname()
	t.requests.WithLabelValues(host, req.Method, code).Inc()
	t.duration.WithLabelValues(host, req.Method, code).Observe(elapsed)
	return resp, err
}
