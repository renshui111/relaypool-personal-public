package api

// The catalogue is public information. Its requests never carry local account
// data or credentials, and its records cannot select a claim handler.
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const benefitOrigin = "https://ai-yangmao.com"

type benefitCacheEntry struct {
	body json.RawMessage
	at   time.Time
}

// Publish the complete result before closing done, including failures and
// stale fallbacks, so waiting callers do not retry the same failed request.
type benefitFlight struct {
	done  chan struct{}
	entry benefitCacheEntry
	stale bool
	err   error
	retry bool // Only the caller's cancellation, not an upstream timeout.
}

type benefitFeed struct {
	mu      sync.Mutex
	cache   map[string]benefitCacheEntry
	flights map[string]*benefitFlight
	client  *http.Client
}

var benefitID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

func benefitSearchQuery(q url.Values) (url.Values, error) {
	out := url.Values{"status": {"all"}, "limit": {"12"}, "offset": {"0"}}
	for k, vs := range q {
		if len(vs) != 1 {
			return nil, errors.New("参数重复")
		}
		v := vs[0]
		switch k {
		case "q":
			if len([]rune(v)) > 120 {
				return nil, errors.New("搜索词过长")
			}
		case "status":
			if v != "all" && v != "claimable" && v != "stale" {
				return nil, errors.New("状态无效")
			}
		case "modality":
			if v != "" && v != "text" && v != "code" && v != "image" && v != "video" && v != "audio" {
				return nil, errors.New("用途无效")
			}
		case "offset":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > 10000 {
				return nil, errors.New("页码无效")
			}
		default:
			return nil, errors.New("不支持的查询参数")
		}
		if v != "" {
			out.Set(k, v)
		}
	}
	return out, nil
}

func (f *benefitFeed) read(ctx context.Context, path string) (benefitCacheEntry, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return benefitCacheEntry{}, false, err
		}
		f.mu.Lock()
		if f.cache == nil {
			f.cache = make(map[string]benefitCacheEntry)
			f.flights = make(map[string]*benefitFlight)
		}
		old, exists := f.cache[path]
		if exists && time.Since(old.at) < 15*time.Minute {
			f.mu.Unlock()
			return old, false, nil
		}
		if flight := f.flights[path]; flight != nil {
			f.mu.Unlock()
			select {
			case <-flight.done:
				if err := ctx.Err(); err != nil {
					return benefitCacheEntry{}, false, err
				}
				// A disconnected leader must not cancel other callers.
				if flight.retry {
					continue
				}
				return flight.entry, flight.stale, flight.err
			case <-ctx.Done():
				return benefitCacheEntry{}, false, ctx.Err()
			}
		}
		flight := &benefitFlight{done: make(chan struct{})}
		f.flights[path] = flight
		f.mu.Unlock()
		entry, err := f.fetch(ctx, path)
		stale := false
		if ctx.Err() != nil {
			entry, err = benefitCacheEntry{}, ctx.Err()
			flight.retry = true
		} else if err != nil && exists && time.Since(old.at) < 24*time.Hour {
			entry, stale, err = old, true, nil
		}
		f.mu.Lock()
		if err == nil && !stale {
			if _, replacing := f.cache[path]; !replacing && len(f.cache) >= 64 {
				var oldest string
				var oldestAt time.Time
				for key, value := range f.cache {
					if oldest == "" || value.at.Before(oldestAt) {
						oldest, oldestAt = key, value.at
					}
				}
				delete(f.cache, oldest)
			}
			f.cache[path] = entry
		}
		flight.entry, flight.stale, flight.err = entry, stale, err
		delete(f.flights, path)
		close(flight.done)
		f.mu.Unlock()
		return entry, stale, err
	}
}

func (f *benefitFeed) fetch(ctx context.Context, path string) (benefitCacheEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, benefitOrigin+path, nil)
	if err != nil {
		return benefitCacheEntry{}, err
	}
	req.Header.Set("Accept", "application/json")
	client := f.client
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return benefitCacheEntry{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return benefitCacheEntry{}, errors.New("catalogue unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return benefitCacheEntry{}, errors.New("catalogue response too large")
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(body, &data) != nil {
		return benefitCacheEntry{}, errors.New("invalid catalogue response")
	}
	if len(data["offers"]) == 0 && len(data["offer"]) == 0 {
		return benefitCacheEntry{}, errors.New("missing catalogue data")
	}
	return benefitCacheEntry{body: body, at: time.Now().UTC()}, nil
}

func (s *Server) handleBenefits(w http.ResponseWriter, r *http.Request) {
	query, err := benefitSearchQuery(r.URL.Query())
	if err != nil {
		writeError(w, 400, "invalid_benefit_query", err.Error())
		return
	}
	s.writeBenefits(w, r, "/api/agent/search?"+query.Encode())
}

func (s *Server) handleBenefitDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !benefitID.MatchString(id) {
		writeError(w, 400, "invalid_benefit_id", "福利编号无效")
		return
	}
	s.writeBenefits(w, r, "/api/agent/offers/"+id)
}

func (s *Server) writeBenefits(w http.ResponseWriter, r *http.Request, path string) {
	entry, stale, err := s.benefits.read(r.Context(), path)
	if err != nil {
		w.Header().Set("Retry-After", "60")
		writeError(w, 502, "benefit_source_unavailable", "福利目录暂时无法连接，已接入账号的领取仍可使用。请稍后重试。")
		return
	}
	writeJSON(w, 200, map[string]any{"source": "AI 羊毛牧场", "source_url": benefitOrigin, "fetched_at": entry.at, "stale": stale, "data": entry.body})
}
