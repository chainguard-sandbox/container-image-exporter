package clusterexporter

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

func mustParseRef(t *testing.T, s string) name.Reference {
	t.Helper()
	ref, err := name.ParseReference(s)
	if err != nil {
		t.Fatalf("parsing reference %q: %v", s, err)
	}
	return ref
}

// TestEvict_RemovesStaleRefs verifies the baseline behaviour: refs not in the
// active set that were cached before olderThan are evicted.
func TestEvict_RemovesStaleRefs(t *testing.T) {
	ctx := context.Background()
	c := NewContainerImageCache()

	ref := mustParseRef(t, "example.com/app:latest")
	if err := c.Put(ctx, ref, &ContainerImage{Digest: "sha256:aaa"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Evict with an empty active set and olderThan=now — entry is eligible.
	if err := c.Evict(ctx, nil, time.Now()); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if _, err := c.Get(ctx, ref); err == nil {
		t.Error("expected entry to be evicted, but Get succeeded")
	}
}

// TestEvict_ProtectsRecentlyPutEntry verifies that an entry Put after the
// olderThan epoch is protected even if its ref is not in the active set.
// This covers the race where a reconciler caches an image mid-Collect.
func TestEvict_ProtectsRecentlyPutEntry(t *testing.T) {
	ctx := context.Background()
	c := NewContainerImageCache()

	// Capture the epoch before the Put — simulating Collect starting before
	// the reconciler writes the entry.
	epoch := time.Now()

	ref := mustParseRef(t, "example.com/new:latest")
	if err := c.Put(ctx, ref, &ContainerImage{Digest: "sha256:bbb"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Evict with an empty active set but olderThan=epoch (before the Put).
	if err := c.Evict(ctx, nil, epoch); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if _, err := c.Get(ctx, ref); err != nil {
		t.Errorf("expected recently-Put entry to survive eviction, but Get returned: %v", err)
	}
}
