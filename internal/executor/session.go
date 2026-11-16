// Package executor turns a client request into an upstream call, including the
// failover loop that walks the account pool until one succeeds.
package executor

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// DefaultSessionTTL is how long an idle conversation binding survives.
const DefaultSessionTTL = 30 * time.Minute

// DefaultSessionCapacity bounds the binding table.
//
// The bound is mandatory, not a tuning knob: an unbounded map keyed by
// conversation would grow forever on a long-running gateway and eventually
// exhaust memory. An LRU makes the memory ceiling a constant.
const DefaultSessionCapacity = 10_000

// sessionBinding is one conversation's account assignment. List elements hold
// pointers so TTL/account refreshes reuse the record without interface boxing.
// Bindings never escape SessionAffinity and are only accessed under its mutex.
type sessionBinding struct {
	key       string
	accountID string
	expiresAt time.Time
}

// SessionAffinity keeps a conversation on one account.
//
// WHY THIS EXISTS: a multi-turn conversation spread across different accounts
// loses the upstream's prompt cache on every turn, and on channels that track
// conversation state it can look like the same user switching devices, which
// invites risk-control action.
//
// This is a ROUTING CACHE, not durable state. It is deliberately process-local
// and cleared on restart: a binding is only an optimization, and persisting it
// would let a stale assignment outlive the reason it was made.
type SessionAffinity struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int

	entries map[string]*list.Element
	// lru orders entries front (most recent) to back (eviction candidate).
	lru *list.List

	// Counters for the admin API. They are the only way to tell a 0% hit rate
	// (sticky routing silently disabled) from a healthy one.
	hits       uint64
	misses     uint64
	escapes    uint64
	rebindings uint64

	lastEscapeAt     time.Time
	lastEscapeReason string
	lastMissReason   string
}

// SessionStats is the diagnostic snapshot.
type SessionStats struct {
	TTLSeconds       int64  `json:"ttl_seconds"`
	Capacity         int    `json:"capacity"`
	Bindings         int    `json:"bindings"`
	Hits             uint64 `json:"hits"`
	Misses           uint64 `json:"misses"`
	Escapes          uint64 `json:"escapes"`
	Rebindings       uint64 `json:"rebindings"`
	LastEscapeAt     string `json:"last_escape_at,omitempty"`
	LastEscapeReason string `json:"last_escape_reason,omitempty"`
	LastMissReason   string `json:"last_miss_reason,omitempty"`
}

// NewSessionAffinity builds a binding table.
func NewSessionAffinity(ttl time.Duration, capacity int) *SessionAffinity {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	if capacity <= 0 {
		capacity = DefaultSessionCapacity
	}
	return &SessionAffinity{
		ttl:      ttl,
		capacity: capacity,
		entries:  make(map[string]*list.Element, capacity),
		lru:      list.New(),
	}
}

// Get resolves a binding, renewing its TTL on a hit.
func (a *SessionAffinity) Get(key string) (string, bool) {
	if a == nil {
		return "", false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	element, ok := a.entries[key]
	if !ok {
		a.misses++
		a.lastMissReason = "not_found"
		return "", false
	}
	binding := element.Value.(*sessionBinding)
	now := time.Now()
	if !binding.expiresAt.After(now) {
		// Expired entries are removed on access so a stale binding cannot
		// pin traffic to an account forever.
		a.removeLocked(element)
		a.misses++
		a.lastMissReason = "expired"
		return "", false
	}
	a.hits++
	binding.expiresAt = now.Add(a.ttl)
	a.lru.MoveToFront(element)
	return binding.accountID, true
}

// Bind records or refreshes a binding.
func (a *SessionAffinity) Bind(key, accountID string) {
	if a == nil {
		return
	}
	key = strings.TrimSpace(key)
	accountID = strings.TrimSpace(accountID)
	if key == "" || accountID == "" {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if element, ok := a.entries[key]; ok {
		binding := element.Value.(*sessionBinding)
		if binding.accountID != accountID {
			// The conversation moved accounts, which is what a failover looks
			// like. Counting it distinguishes "sticky is working" from
			// "everything is bouncing".
			a.rebindings++
		}
		binding.accountID = accountID
		binding.expiresAt = time.Now().Add(a.ttl)
		a.lru.MoveToFront(element)
		return
	}

	element := a.lru.PushFront(&sessionBinding{key: key, accountID: accountID, expiresAt: time.Now().Add(a.ttl)})
	a.entries[key] = element
	for a.lru.Len() > a.capacity {
		a.removeLocked(a.lru.Back())
	}
}

// RecordEscape notes that a binding had to be abandoned because its account
// became unusable. The reason is the useful part: without it, a high escape
// count gives no clue whether accounts are dying or being misconfigured.
func (a *SessionAffinity) RecordEscape(reason string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.escapes++
	a.lastEscapeAt = time.Now().UTC()
	a.lastEscapeReason = strings.TrimSpace(reason)
	a.mu.Unlock()
}

// Forget drops a binding.
func (a *SessionAffinity) Forget(key string) {
	if a == nil {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if element, ok := a.entries[key]; ok {
		a.removeLocked(element)
	}
}

// Stats reports the counters.
func (a *SessionAffinity) Stats() SessionStats {
	if a == nil {
		return SessionStats{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	stats := SessionStats{
		TTLSeconds:       int64(a.ttl / time.Second),
		Capacity:         a.capacity,
		Bindings:         a.lru.Len(),
		Hits:             a.hits,
		Misses:           a.misses,
		Escapes:          a.escapes,
		Rebindings:       a.rebindings,
		LastEscapeReason: a.lastEscapeReason,
		LastMissReason:   a.lastMissReason,
	}
	if !a.lastEscapeAt.IsZero() {
		stats.LastEscapeAt = a.lastEscapeAt.Format(time.RFC3339)
	}
	return stats
}

// Prune removes expired bindings. It exists so a long-running process reclaims
// memory from conversations that ended, rather than waiting for the LRU to
// evict them under pressure.
func (a *SessionAffinity) Prune() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	removed := 0
	// Both Bind and Get renew the TTL when moving an entry to the front.
	// Expiration therefore follows LRU order, so stop at the first live entry.
	for element := a.lru.Back(); element != nil; {
		binding := element.Value.(*sessionBinding)
		previous := element.Prev()
		if binding.expiresAt.After(now) {
			break
		}
		a.removeLocked(element)
		removed++
		element = previous
	}
	return removed
}

func (a *SessionAffinity) removeLocked(element *list.Element) {
	if element == nil {
		return
	}
	binding := element.Value.(*sessionBinding)
	delete(a.entries, binding.key)
	a.lru.Remove(element)
}

// ContentSessionKey derives a binding key from conversation content.
//
// Hashing the first user message means a client that sends its whole history
// every turn -- which is the norm for stateless OpenAI-shaped clients -- gets
// sticky routing with no client-side change. The separator keeps a prefix
// collision from making two different seeds hash alike.
func ContentSessionKey(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("content\x00" + seed))
	return hex.EncodeToString(sum[:])
}
