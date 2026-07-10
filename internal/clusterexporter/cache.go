package clusterexporter

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

// ErrContainerImageNotFound is returned when an item isn't in the cache
var ErrContainerImageNotFound = fmt.Errorf("not found")

// CachedContainerImage is a container image that we've cached
type CachedContainerImage struct {
	*ContainerImage
	Time time.Time
}

// ContainerImageCache caches details about container images
type ContainerImageCache interface {
	Get(ctx context.Context, ref name.Reference) (*CachedContainerImage, error)
	Put(ctx context.Context, ref name.Reference, img *ContainerImage) error
	// Evict removes cached entries not in refs AND cached before olderThan.
	// Entries cached at or after olderThan are protected so that reconcilers
	// running concurrently with Collect don't have their writes immediately
	// dropped.
	Evict(ctx context.Context, refs []name.Reference, olderThan time.Time) error
}

type cacheImpl struct {
	digestMap map[string]string
	imageMap  map[string]*CachedContainerImage
	lock      sync.Mutex
}

// NewContainerImageCache returns a new cache
func NewContainerImageCache() ContainerImageCache {
	return &cacheImpl{
		digestMap: map[string]string{},
		imageMap:  map[string]*CachedContainerImage{},
		lock:      sync.Mutex{},
	}
}

// Get an image from the cache
func (c *cacheImpl) Get(ctx context.Context, ref name.Reference) (*CachedContainerImage, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	var digestStr string
	if digest, ok := ref.(name.Digest); ok {
		digestStr = digest.DigestStr()
	} else {
		digest, ok := c.digestMap[ref.String()]
		if ok {
			digestStr = digest
		}
	}
	if digestStr == "" {
		return nil, ErrContainerImageNotFound
	}

	img, ok := c.imageMap[digestStr]
	if !ok {
		return nil, ErrContainerImageNotFound
	}

	return img, nil
}

// Put an image into the cache
func (c *cacheImpl) Put(ctx context.Context, ref name.Reference, img *ContainerImage) error {
	if img == nil {
		return nil
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	c.digestMap[ref.String()] = img.Digest
	c.imageMap[img.Digest] = &CachedContainerImage{
		ContainerImage: img,
		Time:           time.Now(),
	}

	return nil
}

// Evict removes cached entries for any image references not present in refs
// that were also cached before olderThan. A digest is only removed from the
// image map once all references pointing to it have been evicted.
func (c *cacheImpl) Evict(ctx context.Context, refs []name.Reference, olderThan time.Time) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	activeRefs := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		activeRefs[ref.String()] = struct{}{}
	}

	// Remove stale ref→digest mappings, tracking which digests remain referenced.
	activeDigests := map[string]struct{}{}
	for refStr, digestStr := range c.digestMap {
		if _, active := activeRefs[refStr]; active {
			activeDigests[digestStr] = struct{}{}
			continue
		}
		img, ok := c.imageMap[digestStr]
		if ok && !img.Time.Before(olderThan) {
			// Cached during or after the current Collect pass — protect it.
			activeDigests[digestStr] = struct{}{}
			continue
		}
		delete(c.digestMap, refStr)
	}

	// Remove digest→image entries that are no longer pointed to by any ref.
	for digestStr := range c.imageMap {
		if _, ok := activeDigests[digestStr]; !ok {
			delete(c.imageMap, digestStr)
		}
	}

	return nil
}
