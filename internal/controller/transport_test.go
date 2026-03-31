package controller

import (
	"net/http"
	"sync"
	"testing"
	"time"
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

// TestRegistryTransport_RateLimiting verifies that a transport configured with
// rps=10 delays the second request by approximately 1/rps seconds.
func TestRegistryTransport_RateLimiting(t *testing.T) {
	const rps = 10.0
	const minDelay = 80 * time.Millisecond // 1/rps with some tolerance

	base := &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		return okResponse(), nil
	}}

	tr := newRegistryTransport(base, 0, rps)

	req1, _ := http.NewRequest("GET", "http://registry.example.com/v2/", nil)
	req2, _ := http.NewRequest("GET", "http://registry.example.com/v2/", nil)

	// First request consumes the burst token immediately.
	if _, err := tr.RoundTrip(req1); err != nil {
		t.Fatalf("first request: %v", err)
	}

	// Second request must wait for the next token (~1/rps seconds).
	start := time.Now()
	if _, err := tr.RoundTrip(req2); err != nil {
		t.Fatalf("second request: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < minDelay {
		t.Errorf("expected rate-limited delay >= %v, got %v", minDelay, elapsed)
	}
}

// TestRegistryTransport_Concurrency verifies that a transport configured with
// concurrency=1 holds the second request until the first has completed.
func TestRegistryTransport_Concurrency(t *testing.T) {
	block := make(chan struct{})

	base := &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		<-block
		return okResponse(), nil
	}}

	tr := newRegistryTransport(base, 1, 0)

	req1, _ := http.NewRequest("GET", "http://registry.example.com/v2/req1", nil)
	req2, _ := http.NewRequest("GET", "http://registry.example.com/v2/req2", nil)

	// Start the first request; it will block inside the base transport.
	req1Done := make(chan struct{})
	go func() {
		defer close(req1Done)
		if _, err := tr.RoundTrip(req1); err != nil {
			t.Errorf("req1: %v", err)
		}
	}()

	// Give req1 time to acquire the semaphore and reach the blocking handler.
	time.Sleep(50 * time.Millisecond)

	// Start the second request; it should be blocked by the semaphore.
	req2Done := make(chan struct{})
	go func() {
		defer close(req2Done)
		if _, err := tr.RoundTrip(req2); err != nil {
			t.Errorf("req2: %v", err)
		}
	}()

	// Verify req2 is still waiting after a brief pause.
	select {
	case <-req2Done:
		t.Fatal("second request completed before first was released")
	case <-time.After(100 * time.Millisecond):
		// expected: req2 is blocked
	}

	// Release req1.
	close(block)
	<-req1Done

	// Now req2 should complete promptly.
	select {
	case <-req2Done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("second request did not complete after first was released")
	}
}

// TestRegistryTransport_PerHostIsolation verifies that a transport configured
// with concurrency=1 uses separate semaphores per hostname, so that a blocked
// request to one host does not prevent requests to a different host.
func TestRegistryTransport_PerHostIsolation(t *testing.T) {
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

	tr := newRegistryTransport(base, 1, 0)

	reqA, _ := http.NewRequest("GET", "http://host-a.example.com/v2/", nil)
	reqB, _ := http.NewRequest("GET", "http://host-b.example.com/v2/", nil)

	// Start the host-a request; it will block inside the base transport,
	// holding the semaphore for host-a.
	reqADone := make(chan struct{})
	go func() {
		defer close(reqADone)
		tr.RoundTrip(reqA) //nolint:errcheck
	}()

	// Give the host-a request time to acquire the semaphore.
	time.Sleep(50 * time.Millisecond)

	// The host-b request should complete independently without waiting for host-a.
	reqBDone := make(chan struct{})
	go func() {
		defer close(reqBDone)
		tr.RoundTrip(reqB) //nolint:errcheck
	}()

	select {
	case <-reqBDone:
		// expected: host-b is not blocked by host-a's semaphore
	case <-time.After(1 * time.Second):
		t.Fatal("request to host-b was blocked by host-a's concurrency slot")
	}

	// Clean up by releasing host-a.
	close(blockA)
	<-reqADone
}
