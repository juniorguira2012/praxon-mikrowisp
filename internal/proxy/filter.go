package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/axentra/mikrowisp-proxy/internal/config"
)

// Filter is the rule engine that decides whether a given customer should
// be excluded from upstream responses. Construct with NewFilter and call
// IsExcluded(...) per customer id.
//
// The engine has two layers:
//
//  1. Rule evaluator — pure function from "extras map" to bool. Each rule
//     is checked in declaration order; first match wins (the customer is
//     excluded). Encapsulated in evalRules.
//
//  2. Verdict cache — an LRU + TTL map keyed by customer id holding only
//     the bool verdict (excluded or not) plus the timestamp the verdict
//     was computed. The cache exists so that a request for 100 invoices
//     belonging to 30 distinct clients doesn't trigger 100 upstream
//     GetClientsDetails calls; instead we hit upstream once per distinct
//     client and reuse the verdict for the rest.
//
// The cache stores VERDICTS only, not the underlying extras data. That
// keeps the proxy's memory footprint bounded and avoids accidentally
// becoming a cache of customer PII.
type Filter struct {
	rules    []config.ExcludeRule
	upstream *Upstream
	cache    *verdictCache
}

// NewFilter wires a Filter together. If `rules` is empty, the filter is
// a no-op — IsExcluded always returns false and the cache is never used.
func NewFilter(rules []config.ExcludeRule, upstream *Upstream, cacheTTL time.Duration, cacheMax int) *Filter {
	return &Filter{
		rules:    rules,
		upstream: upstream,
		cache:    newVerdictCache(cacheTTL, cacheMax),
	}
}

// IsExcluded returns true if the given customer id matches any exclusion
// rule. Cache hit returns immediately; cache miss triggers an upstream
// GetClientsDetails call to fetch the customer's extras. On upstream
// failure (network error, etc.) the function returns (false, err) — the
// proxy then chooses to fail the request rather than over- or under-filter.
//
// The token argument is the caller's Mikrowisp API token; it's used for
// the upstream GetClientsDetails call so the proxy never holds tokens of
// its own. Token-on-token-forwarding keeps the proxy stateless from a
// credentials standpoint.
func (f *Filter) IsExcluded(ctx context.Context, token string, idCliente int) (bool, error) {
	if len(f.rules) == 0 {
		return false, nil
	}
	if v, ok := f.cache.Get(idCliente); ok {
		return v, nil
	}
	extras, err := f.upstream.GetClientDetails(ctx, token, idCliente)
	if err != nil {
		// "Client not found" is a soft answer: nothing to exclude, and
		// the invoice that referenced this id is probably stale or
		// malformed. The caller decides what to do with it.
		if errors.Is(err, ErrClientNotFound) {
			f.cache.Put(idCliente, false)
			return false, nil
		}
		return false, err
	}
	verdict := evalRules(f.rules, extras)
	f.cache.Put(idCliente, verdict)
	return verdict, nil
}

// FilterClient is a synchronous variant for the GetClientsDetails endpoint
// where we already have the full extras map in hand from the upstream
// response — no need to round-trip back to upstream just to decide.
//
// We still record the verdict in the cache so a subsequent invoice
// request can short-circuit on the same id.
func (f *Filter) FilterClient(idCliente int, extras map[string]json.RawMessage) bool {
	if len(f.rules) == 0 {
		return false
	}
	verdict := evalRules(f.rules, extras)
	f.cache.Put(idCliente, verdict)
	return verdict
}

// evalRules walks the rule list against an extras map and returns true on
// the first match. Pure function — no I/O, suitable for table-driven tests.
func evalRules(rules []config.ExcludeRule, extras map[string]json.RawMessage) bool {
	for _, r := range rules {
		raw, present := extras[r.Field]

		// Presence rule: { field, present = false }. Match when the
		// key is missing entirely. The validator guarantees `present`
		// is non-nil here when `value` is nil.
		if r.Present != nil {
			want := *r.Present
			if !want && !present {
				return true // rule says "field absent"; field IS absent
			}
			if want && present {
				return true // rule says "field present"; field IS present
			}
			continue
		}

		// Value rule: { field, value = "X" }. Match when the field is
		// present and its canonical string form equals X.
		if !present || r.Value == nil {
			continue
		}
		if canonString(raw) == *r.Value {
			return true
		}
	}
	return false
}

// canonString reduces a JSON value to its canonical string form for
// equality comparison. Booleans -> "true"/"false"; numbers -> their
// numeric string ("100", not "100.0"); strings -> their unquoted
// content; null/objects/arrays -> the raw JSON literal.
//
// The intent is operator-friendly equality: an admin who writes
// `value = "false"` should match the boolean false, the string "false",
// and the number 0 only via explicit "0" — not via clever coercion. We
// stay strict to keep the rule outcome predictable.
func canonString(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	// Try string first (the most common case for admin-defined extras).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Then bool.
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		if b {
			return "true"
		}
		return "false"
	}
	// Then number — leave the raw JSON which is already a numeric
	// literal like "100" or "3.14".
	if len(trimmed) > 0 && (trimmed[0] == '-' || (trimmed[0] >= '0' && trimmed[0] <= '9')) {
		return trimmed
	}
	// Fallback: the raw literal (covers null, [..], {..}). Operators
	// shouldn't be filtering on these but we don't want to crash.
	return trimmed
}

// ─── verdict cache (LRU + TTL) ────────────────────────────────────────────

// verdictEntry holds one cached decision.
type verdictEntry struct {
	verdict bool
	at      time.Time
}

// verdictCache is a tiny TTL+capacity-bounded map. For our scale (single
// digit thousands of customer ids per ISP) the simple `sync.RWMutex + map`
// shape is fast enough; we don't need a fancy LRU library. When the map
// hits MaxEntries we drop the oldest TTL-expired entries first; if there
// are none expired, we drop a random ~10% to make room.
type verdictCache struct {
	mu      sync.RWMutex
	entries map[int]verdictEntry
	ttl     time.Duration
	max     int
}

func newVerdictCache(ttl time.Duration, max int) *verdictCache {
	if max <= 0 {
		max = 10000
	}
	return &verdictCache{
		entries: make(map[int]verdictEntry),
		ttl:     ttl,
		max:     max,
	}
}

func (c *verdictCache) Get(id int) (bool, bool) {
	c.mu.RLock()
	e, ok := c.entries[id]
	c.mu.RUnlock()
	if !ok {
		return false, false
	}
	if time.Since(e.at) > c.ttl {
		// Expired. Drop and treat as miss; we re-check upstream on
		// the next call.
		c.mu.Lock()
		delete(c.entries, id)
		c.mu.Unlock()
		return false, false
	}
	return e.verdict, true
}

func (c *verdictCache) Put(id int, verdict bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		c.evictLocked()
	}
	c.entries[id] = verdictEntry{verdict: verdict, at: time.Now()}
}

// evictLocked drops expired entries first; if none are expired, drops a
// rough 10% of the map to make room. Must be called with c.mu held.
func (c *verdictCache) evictLocked() {
	now := time.Now()
	dropped := 0
	for k, e := range c.entries {
		if now.Sub(e.at) > c.ttl {
			delete(c.entries, k)
			dropped++
		}
	}
	if dropped > 0 {
		return
	}
	// Nothing expired yet; trim ~10% by iteration order. Map iteration
	// is randomized in Go, so this is effectively random eviction.
	target := c.max / 10
	if target < 1 {
		target = 1
	}
	for k := range c.entries {
		delete(c.entries, k)
		target--
		if target <= 0 {
			break
		}
	}
}
