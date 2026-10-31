package tools

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"
)

const (
	cacheEntries  = 4096
	cacheMaxValue = 1 << 20 // bigger answers aren't kept
)

type cached struct {
	body     []byte
	provider string
	expires  time.Time
}

// cache holds tool answers in memory, shared between apps, so a free tier
// goes further. It starts empty after a restart.
type cache struct {
	mu sync.Mutex
	m  map[[32]byte]cached
}

func newCache() *cache { return &cache{m: map[[32]byte]cached{}} }

// cacheKey hashes the tool and its cleaned args; json.Marshal sorts map
// keys, so the same arguments in any order share an entry.
func cacheKey(tool string, args map[string]any) [32]byte {
	b, _ := json.Marshal(args)
	return sha256.Sum256(append([]byte(tool+"\n"), b...))
}

func (c *cache) get(k [32]byte) (cached, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	if ok && time.Now().After(v.expires) {
		delete(c.m, k)
		return cached{}, false
	}
	return v, ok
}

func (c *cache) put(k [32]byte, v cached) {
	if len(v.body) > cacheMaxValue {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= cacheEntries {
		now := time.Now()
		for key, e := range c.m {
			if now.After(e.expires) {
				delete(c.m, key)
			}
		}
		// Still full of live answers: drop some. Map order is random,
		// which is good enough to make room.
		for key := range c.m {
			if len(c.m) < cacheEntries*9/10 {
				break
			}
			delete(c.m, key)
		}
	}
	c.m[k] = v
}
