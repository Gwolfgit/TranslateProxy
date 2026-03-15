package main

import (
	"container/list"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"log"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

var translationBucket = []byte("translations")

// tieredCache implements a two-tier cache: in-memory LRU (fast, bounded)
// backed by a BoltDB persistent store (large, survives restarts).
// Lookup order: memory → disk → API.
type tieredCache struct {
	mem  *memCache
	db   *bolt.DB
	ttl  time.Duration

	// Aggregate stats
	memHits  atomic.Int64
	diskHits atomic.Int64
	misses   atomic.Int64
}

func newTieredCache(memSize int, dbPath string, ttl time.Duration) (*tieredCache, error) {
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}

	// Create bucket
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(translationBucket)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	// Count existing entries
	var count int
	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(translationBucket)
		count = b.Stats().KeyN
		return nil
	})
	log.Printf("[CACHE] BoltDB opened: %s (%d existing entries, TTL=%v)", dbPath, count, ttl)

	return &tieredCache{
		mem: newMemCache(memSize),
		db:  db,
		ttl: ttl,
	}, nil
}

// Get looks up: memory first, then disk. Promotes disk hits to memory.
func (c *tieredCache) Get(source string) (string, bool) {
	normalized := normalizeForCache(source)
	key := hashKey(normalized)

	// Tier 1: memory
	if result, ok := c.mem.Get(key, normalized); ok {
		c.memHits.Add(1)
		return result, true
	}

	// Tier 2: disk
	if result, ok := c.diskGet(key, normalized); ok {
		c.diskHits.Add(1)
		// Promote to memory
		c.mem.Put(key, normalized, result)
		return result, true
	}

	c.misses.Add(1)
	return "", false
}

// Put stores in both memory and disk.
func (c *tieredCache) Put(source, result string) {
	normalized := normalizeForCache(source)
	key := hashKey(normalized)

	c.mem.Put(key, normalized, result)
	c.diskPut(key, normalized, result)
}

// diskGet reads from BoltDB, checking TTL.
func (c *tieredCache) diskGet(key uint64, normalized string) (string, bool) {
	var result string
	var found bool

	c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(translationBucket)
		keyBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(keyBytes, key)
		val := b.Get(keyBytes)
		if val == nil {
			return nil
		}

		entry, err := decodeDiskEntry(val)
		if err != nil {
			return nil
		}

		// TTL check
		if c.ttl > 0 && time.Since(entry.created) > c.ttl {
			return nil // expired
		}

		// Collision guard
		if entry.source != normalized {
			return nil
		}

		result = entry.result
		found = true
		return nil
	})

	return result, found
}

// diskPut writes to BoltDB.
func (c *tieredCache) diskPut(key uint64, normalized, result string) {
	c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(translationBucket)
		keyBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(keyBytes, key)

		val := encodeDiskEntry(diskEntry{
			source:  normalized,
			result:  result,
			created: time.Now(),
		})
		return b.Put(keyBytes, val)
	})
}

// Close flushes and closes the BoltDB.
func (c *tieredCache) Close() error {
	return c.db.Close()
}

// LogStats logs statistics for both tiers.
func (c *tieredCache) LogStats() {
	memSize := c.mem.Len()
	memHits := c.memHits.Load()
	diskHits := c.diskHits.Load()
	misses := c.misses.Load()
	total := memHits + diskHits + misses
	hitRate := 0.0
	if total > 0 {
		hitRate = float64(memHits+diskHits) / float64(total) * 100
	}

	var diskSize int
	c.db.View(func(tx *bolt.Tx) error {
		diskSize = tx.Bucket(translationBucket).Stats().KeyN
		return nil
	})

	log.Printf("[CACHE] mem=%d disk=%d | mem_hits=%d disk_hits=%d misses=%d hit_rate=%.1f%%",
		memSize, diskSize, memHits, diskHits, misses, hitRate)
}

// --- Disk entry encoding ---
// Format: 8-byte unix timestamp (seconds) | 4-byte source len | source | result

type diskEntry struct {
	source  string
	result  string
	created time.Time
}

func encodeDiskEntry(e diskEntry) []byte {
	srcBytes := []byte(e.source)
	resBytes := []byte(e.result)
	buf := make([]byte, 8+4+len(srcBytes)+len(resBytes))
	binary.BigEndian.PutUint64(buf[0:8], uint64(e.created.Unix()))
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(srcBytes)))
	copy(buf[12:12+len(srcBytes)], srcBytes)
	copy(buf[12+len(srcBytes):], resBytes)
	return buf
}

func decodeDiskEntry(data []byte) (diskEntry, error) {
	if len(data) < 12 {
		return diskEntry{}, fmt.Errorf("entry too short")
	}
	ts := binary.BigEndian.Uint64(data[0:8])
	srcLen := binary.BigEndian.Uint32(data[8:12])
	if int(12+srcLen) > len(data) {
		return diskEntry{}, fmt.Errorf("invalid source length")
	}
	return diskEntry{
		source:  string(data[12 : 12+srcLen]),
		result:  string(data[12+srcLen:]),
		created: time.Unix(int64(ts), 0),
	}, nil
}

// --- In-memory LRU tier ---

type memCache struct {
	mu      sync.RWMutex
	maxSize int
	items   map[uint64]*list.Element
	order   *list.List
}

type memEntry struct {
	key        uint64
	normalized string
	result     string
}

func newMemCache(maxSize int) *memCache {
	return &memCache{
		maxSize: maxSize,
		items:   make(map[uint64]*list.Element, maxSize),
		order:   list.New(),
	}
}

func (c *memCache) Get(key uint64, normalized string) (string, bool) {
	c.mu.RLock()
	elem, ok := c.items[key]
	c.mu.RUnlock()

	if !ok {
		return "", false
	}

	entry := elem.Value.(*memEntry)
	if entry.normalized != normalized {
		return "", false
	}

	c.mu.Lock()
	c.order.MoveToFront(elem)
	c.mu.Unlock()

	return entry.result, true
}

func (c *memCache) Put(key uint64, normalized, result string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem)
		e := elem.Value.(*memEntry)
		e.result = result
		return
	}

	if c.order.Len() >= c.maxSize {
		oldest := c.order.Back()
		if oldest != nil {
			c.order.Remove(oldest)
			delete(c.items, oldest.Value.(*memEntry).key)
		}
	}

	entry := &memEntry{key: key, normalized: normalized, result: result}
	elem := c.order.PushFront(entry)
	c.items[key] = elem
}

func (c *memCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.order.Len()
}

// hashKey returns the FNV-64a hash of a string.
func hashKey(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}
