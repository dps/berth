package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	sessionTTL    = 30 * 24 * time.Hour
	extendAfter   = 24 * time.Hour // extend (and write) at most daily per session
	maxSessions   = 100_000
	sessionPrefix = "s1."
)

// Session is one browser signed in on one host. Only the hash of the
// cookie value is stored.
type Session struct {
	Hash     string    `json:"hash"`
	Host     string    `json:"host"`
	User     Identity  `json:"user"`
	Created  time.Time `json:"created"`
	Extended time.Time `json:"extended"`
	Expires  time.Time `json:"expires"`
}

// Sessions is the session table, persisted to one JSON file.
type Sessions struct {
	path string
	mu   sync.Mutex
	m    map[string]*Session
}

func OpenSessions(path string) *Sessions {
	s := &Sessions{path: path, m: map[string]*Session{}}
	if b, err := os.ReadFile(path); err == nil {
		var list []*Session
		if json.Unmarshal(b, &list) == nil {
			now := time.Now()
			for _, x := range list {
				if x.Expires.After(now) {
					s.m[x.Hash] = x
				}
			}
		}
	}
	return s
}

func token() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashOf(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// Create starts a session and returns the cookie value.
func (s *Sessions) Create(host string, id Identity) string {
	v := sessionPrefix + token()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) >= maxSessions {
		s.pruneLocked(now, true)
	}
	s.m[hashOf(v)] = &Session{Hash: hashOf(v), Host: host, User: id, Created: now, Extended: now, Expires: now.Add(sessionTTL)}
	s.saveLocked()
	return v
}

// Get returns the live session for cookie value v on host, extending it
// when it's been a day.
func (s *Sessions) Get(host, v string) *Session {
	if v == "" {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.m[hashOf(v)]
	if x == nil || x.Host != host {
		return nil
	}
	if now.After(x.Expires) {
		delete(s.m, x.Hash)
		return nil
	}
	if now.Sub(x.Extended) > extendAfter {
		x.Extended, x.Expires = now, now.Add(sessionTTL)
		s.saveLocked()
	}
	c := *x
	return &c
}

// EndUser deletes every session of the user with this id (or, with email,
// of that email address) and returns how many ended.
func (s *Sessions) EndUser(id, email string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, x := range s.m {
		if (id != "" && x.User.ID() == id) || (email != "" && x.User.Email == email) {
			delete(s.m, k)
			n++
		}
	}
	if n > 0 {
		s.saveLocked()
	}
	return n
}

// List returns live sessions, newest first.
func (s *Sessions) List() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now(), false)
	out := make([]Session, 0, len(s.m))
	for _, x := range s.m {
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// pruneLocked drops expired sessions; with full, also the oldest tenth.
func (s *Sessions) pruneLocked(now time.Time, full bool) {
	for k, x := range s.m {
		if now.After(x.Expires) {
			delete(s.m, k)
		}
	}
	if full && len(s.m) >= maxSessions {
		all := make([]*Session, 0, len(s.m))
		for _, x := range s.m {
			all = append(all, x)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Extended.Before(all[j].Extended) })
		for _, x := range all[:len(all)/10] {
			delete(s.m, x.Hash)
		}
	}
}

func (s *Sessions) saveLocked() {
	list := make([]*Session, 0, len(s.m))
	for _, x := range s.m {
		list = append(list, x)
	}
	b, _ := json.Marshal(list)
	os.MkdirAll(filepath.Dir(s.path), 0o700)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, s.path)
	}
}
