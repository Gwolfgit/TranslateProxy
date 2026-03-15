package main

import (
	"container/list"
	"hash/fnv"
	"log"
	"sync"
	"sync/atomic"
)

// translationCache is a concurrent-safe LRU cache for translated strings,
// keyed by FNV-64a hash of the source text.
type translationCache struct {
	mu       sync.RWMutex
	maxSize  int
	items    map[uint64]*list.Element
	order    *list.List // front = most recently used

	// Stats
	hits   atomic.Int64
	misses atomic.Int64
	evicts atomic.Int64
}

type cacheEntry struct {
	key    uint64
	source string
	result string
}

func newTranslationCache(maxSize int) *translationCache {
	return &translationCache{
		maxSize: maxSize,
		items:   make(map[uint64]*list.Element, maxSize),
		order:   list.New(),
	}
}

// hashKey returns the FNV-64a hash of a string.
func hashKey(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// Get looks up a translation by source text. Returns ("", false) on miss.
func (c *translationCache) Get(source string) (string, bool) {
	key := hashKey(source)

	c.mu.RLock()
	elem, ok := c.items[key]
	c.mu.RUnlock()

	if !ok {
		c.misses.Add(1)
		return "", false
	}

	entry := elem.Value.(*cacheEntry)
	// Verify full string match (hash collision guard)
	if entry.source != source {
		c.misses.Add(1)
		return "", false
	}

	// Promote to front (most recently used)
	c.mu.Lock()
	c.order.MoveToFront(elem)
	c.mu.Unlock()

	c.hits.Add(1)
	return entry.result, true
}

// Put stores a translation in the cache, evicting the LRU entry if full.
func (c *translationCache) Put(source, result string) {
	key := hashKey(source)

	c.mu.Lock()
	defer c.mu.Unlock()

	// Update existing entry
	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem)
		elem.Value.(*cacheEntry).result = result
		return
	}

	// Evict LRU if at capacity
	if c.order.Len() >= c.maxSize {
		oldest := c.order.Back()
		if oldest != nil {
			c.order.Remove(oldest)
			delete(c.items, oldest.Value.(*cacheEntry).key)
			c.evicts.Add(1)
		}
	}

	// Insert new entry
	entry := &cacheEntry{key: key, source: source, result: result}
	elem := c.order.PushFront(entry)
	c.items[key] = elem
}

// Stats returns current cache statistics.
func (c *translationCache) Stats() (size int, hits, misses, evicts int64) {
	c.mu.RLock()
	size = c.order.Len()
	c.mu.RUnlock()
	return size, c.hits.Load(), c.misses.Load(), c.evicts.Load()
}

// LogStats logs cache statistics.
func (c *translationCache) LogStats() {
	size, hits, misses, evicts := c.Stats()
	total := hits + misses
	hitRate := 0.0
	if total > 0 {
		hitRate = float64(hits) / float64(total) * 100
	}
	log.Printf("[CACHE] entries=%d hits=%d misses=%d evicts=%d hit_rate=%.1f%%",
		size, hits, misses, evicts, hitRate)
}
