package workbuddy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

const maxCatalogEntries = 1024

type catalogCall struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	waiters int                  // protected by Client.mu
	models  []provider.ModelInfo // immutable after done closes
	err     error
}

func catalogKey(cred Credential) string {
	// Include token and tenant changes without retaining plaintext credentials
	// (or token suffixes) in cache keys. Structured encoding prevents ambiguity.
	raw, _ := json.Marshal([]string{cred.ChatBase(), cred.Domain, cred.UID, cred.EnterpriseID, cred.AccessToken})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func cloneCatalog(models []provider.ModelInfo) []provider.ModelInfo {
	if models == nil {
		return nil
	}
	out := make([]provider.ModelInfo, len(models))
	copy(out, models)
	for i := range out {
		out[i].ReasoningOptions = append([]string(nil), models[i].ReasoningOptions...)
	}
	return out
}

func (c *Client) cachedCatalog(key string) ([]provider.ModelInfo, bool) {
	c.mu.RLock()
	entry, ok := c.catalog[key]
	c.mu.RUnlock()
	if !ok || time.Since(entry.fetched) >= catalogTTL {
		return nil, false
	}
	return cloneCatalog(entry.models), true
}

func (c *Client) storeCatalog(key string, models []provider.ModelInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeCatalogLocked(key, models)
}

func (c *Client) storeCatalogLocked(key string, models []provider.ModelInfo) {
	if c.catalog == nil {
		c.catalog = map[string]catalogEntry{}
	}
	now := time.Now()
	for k, entry := range c.catalog {
		if now.Sub(entry.fetched) >= catalogTTL {
			delete(c.catalog, k)
		}
	}
	if _, exists := c.catalog[key]; !exists && len(c.catalog) >= maxCatalogEntries {
		var oldestKey string
		var oldest time.Time
		for k, entry := range c.catalog {
			if oldest.IsZero() || entry.fetched.Before(oldest) {
				oldestKey, oldest = k, entry.fetched
			}
		}
		delete(c.catalog, oldestKey)
	}
	c.catalog[key] = catalogEntry{models: cloneCatalog(models), fetched: now}
}

func (c *Client) sharedCatalog(ctx context.Context, cred Credential) ([]provider.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := catalogKey(cred)
	c.mu.Lock()
	if entry, ok := c.catalog[key]; ok && time.Since(entry.fetched) < catalogTTL {
		c.mu.Unlock()
		return cloneCatalog(entry.models), nil
	}
	if c.catalogPending == nil {
		c.catalogPending = map[string]*catalogCall{}
	}
	call := c.catalogPending[key]
	if call == nil {
		// One caller leaving must not abort another caller's query. The shared
		// request has its own deadline and is canceled when the last waiter leaves.
		shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogTimeout)
		call = &catalogCall{ctx: shared, cancel: cancel, done: make(chan struct{})}
		c.catalogPending[key] = call
		go c.runCatalog(key, call, cred)
	}
	call.waiters++
	c.mu.Unlock()
	select {
	case <-call.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return cloneCatalog(call.models), call.err
	case <-ctx.Done():
		c.mu.Lock()
		call.waiters--
		if call.waiters == 0 && c.catalogPending[key] == call {
			delete(c.catalogPending, key)
			call.cancel()
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *Client) runCatalog(key string, call *catalogCall, cred Credential) {
	defer call.cancel()
	models, err := c.fetchCatalogHTTP(call.ctx, cred)
	c.mu.Lock()
	if err == nil && call.ctx.Err() != nil {
		models, err = nil, call.ctx.Err()
	}
	// Invalidation or all callers leaving may have replaced this flight.
	// Its late result must never refill the cache or erase the newer request.
	if c.catalogPending[key] == call {
		delete(c.catalogPending, key)
		if err == nil {
			c.storeCatalogLocked(key, models)
		}
	}
	call.models, call.err = models, err
	close(call.done)
	c.mu.Unlock()
}
