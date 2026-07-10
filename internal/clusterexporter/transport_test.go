package clusterexporter

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// fakeTransport is a minimal http.RoundTripper backed by a function.
type fakeTransport struct {
	fn func(*http.Request) (*http.Response, error)
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f.fn(req)
}

// okResponse returns a minimal 200 response suitable for use in transport tests.
func okResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
	}
}

// TestRateLimitTransport verifies a transport with rps=10 delays the second request by ~1/rps seconds.
func TestRateLimitTransport(t *testing.T) {
	const rps = 10.0
	const minDelay = 80 * time.Millisecond // 1/rps with some tolerance

	base := &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		return okResponse(), nil
	}}

	tr := newRateLimitTransport(base, rps, 1)

	req1, _ := http.NewRequest("GET", "http://registry.example.com/v2/", nil)
	req2, _ := http.NewRequest("GET", "http://registry.example.com/v2/", nil)

	if _, err := tr.RoundTrip(req1); err != nil {
		t.Fatalf("first request: %v", err)
	}

	start := time.Now()
	if _, err := tr.RoundTrip(req2); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if elapsed := time.Since(start); elapsed < minDelay {
		t.Errorf("expected rate-limited delay >= %v, got %v", minDelay, elapsed)
	}
}

// TestConcurrencyTransport verifies a transport with max=1 holds the second request until the first finishes.
func TestConcurrencyTransport(t *testing.T) {
	block := make(chan struct{})

	base := &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		<-block
		return okResponse(), nil
	}}

	tr := newConcurrencyTransport(base, 1)

	req1, _ := http.NewRequest("GET", "http://registry.example.com/v2/req1", nil)
	req2, _ := http.NewRequest("GET", "http://registry.example.com/v2/req2", nil)

	req1Done := make(chan struct{})
	go func() {
		defer close(req1Done)
		if _, err := tr.RoundTrip(req1); err != nil {
			t.Errorf("req1: %v", err)
		}
	}()

	// Let req1 acquire the semaphore and enter the blocking handler.
	time.Sleep(50 * time.Millisecond)

	req2Done := make(chan struct{})
	go func() {
		defer close(req2Done)
		if _, err := tr.RoundTrip(req2); err != nil {
			t.Errorf("req2: %v", err)
		}
	}()

	select {
	case <-req2Done:
		t.Fatal("second request completed before first was released")
	case <-time.After(100 * time.Millisecond):
	}

	close(block)
	<-req1Done

	select {
	case <-req2Done:
	case <-time.After(2 * time.Second):
		t.Fatal("second request did not complete after first was released")
	}
}

// TestConcurrencyTransport_PerHostIsolation verifies the semaphore is per-host: a block on one host doesn't stall another.
func TestConcurrencyTransport_PerHostIsolation(t *testing.T) {
	blockA := make(chan struct{})
	var mu sync.Mutex
	var completed []string

	base := &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "host-a.example.com" {
			<-blockA
		}
		mu.Lock()
		completed = append(completed, req.URL.Hostname())
		mu.Unlock()
		return okResponse(), nil
	}}

	tr := newConcurrencyTransport(base, 1)

	reqA, _ := http.NewRequest("GET", "http://host-a.example.com/v2/", nil)
	reqB, _ := http.NewRequest("GET", "http://host-b.example.com/v2/", nil)

	reqADone := make(chan struct{})
	go func() {
		defer close(reqADone)
		tr.RoundTrip(reqA) //nolint:errcheck
	}()

	time.Sleep(50 * time.Millisecond)

	reqBDone := make(chan struct{})
	go func() {
		defer close(reqBDone)
		tr.RoundTrip(reqB) //nolint:errcheck
	}()

	select {
	case <-reqBDone:
	case <-time.After(1 * time.Second):
		t.Fatal("request to host-b was blocked by host-a's concurrency slot")
	}

	close(blockA)
	<-reqADone
}

// TestMetricsTransport verifies the counter and histogram are incremented per (host, method, code), including "0" on transport error.
func TestMetricsTransport(t *testing.T) {
	base := &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/ok":
			return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
		case "/notfound":
			return &http.Response{StatusCode: 404, Body: http.NoBody}, nil
		default:
			return nil, errors.New("simulated transport error")
		}
	}}

	reg := prometheus.NewRegistry()
	tr := newMetricsTransport(base, reg)

	for _, path := range []string{"/ok", "/notfound", "/boom"} {
		req, _ := http.NewRequest("GET", "http://registry.example.com"+path, nil)
		_, _ = tr.RoundTrip(req) //nolint:errcheck
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	counter := findFamily(mfs, "container_image_cluster_exporter_registry_requests_total")
	if counter == nil {
		t.Fatal("container_image_cluster_exporter_registry_requests_total not registered")
	}
	for _, want := range []map[string]string{
		{"host": "registry.example.com", "method": "GET", "code": "200"},
		{"host": "registry.example.com", "method": "GET", "code": "404"},
		{"host": "registry.example.com", "method": "GET", "code": "0"},
	} {
		m := findByLabels(counter.GetMetric(), want)
		if m == nil {
			t.Errorf("counter missing series for %v", want)
			continue
		}
		if v := m.GetCounter().GetValue(); v != 1 {
			t.Errorf("counter %v = %v, want 1", want, v)
		}
	}

	hist := findFamily(mfs, "container_image_cluster_exporter_registry_request_duration_seconds")
	if hist == nil {
		t.Fatal("container_image_cluster_exporter_registry_request_duration_seconds not registered")
	}
	if got, want := len(hist.GetMetric()), 3; got != want {
		t.Errorf("histogram series count = %d, want %d", got, want)
	}
}

// TestNewRegistryTransport_Composition verifies the composer reaches base and registers the metrics layer.
func TestNewRegistryTransport_Composition(t *testing.T) {
	var hits int
	base := &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		hits++
		return okResponse(), nil
	}}

	reg := prometheus.NewRegistry()
	tr := newRegistryTransport(base, 2, 100, reg)

	req, _ := http.NewRequest("GET", "http://registry.example.com/v2/", nil)
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if hits != 1 {
		t.Errorf("base hits = %d, want 1", hits)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if findFamily(mfs, "container_image_cluster_exporter_registry_requests_total") == nil {
		t.Error("composed transport did not register the metrics layer")
	}
}

// findFamily returns the named MetricFamily from mfs, or nil.
func findFamily(mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

// findByLabels returns the first dto.Metric whose labels match want exactly, or nil.
func findByLabels(ms []*dto.Metric, want map[string]string) *dto.Metric {
	for _, m := range ms {
		got := make(map[string]string, len(m.GetLabel()))
		for _, lp := range m.GetLabel() {
			got[lp.GetName()] = lp.GetValue()
		}
		match := len(got) == len(want)
		for k, v := range want {
			if got[k] != v {
				match = false
				break
			}
		}
		if match {
			return m
		}
	}
	return nil
}
