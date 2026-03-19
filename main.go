package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	upstreamURL = "https://api.weather.com/v3/wx/forecast/hourly/2day" +
		"?icaoCode=RJTT&units=m&language=en-US&format=json" +
		"&apiKey=e1f10a1e78da46f5b10a1e78da96f525"
	cacheTTL   = 5 * time.Minute
	listenAddr = ":8080"
)

type cache struct {
	mu        sync.RWMutex
	body      []byte
	fetchedAt time.Time
}

func (c *cache) get() ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.body == nil || time.Since(c.fetchedAt) > cacheTTL {
		return nil, false
	}
	return c.body, true
}

func (c *cache) set(body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = body
	c.fetchedAt = time.Now()
}

func (c *cache) status() (fetchedAt time.Time, age time.Duration, valid bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.body == nil {
		return time.Time{}, 0, false
	}
	age = time.Since(c.fetchedAt)
	return c.fetchedAt, age, age <= cacheTTL
}

var globalCache = &cache{}

func fetchUpstream() ([]byte, error) {
	resp, err := http.Get(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading upstream body: %w", err)
	}
	return body, nil
}

// GET /weather — proxied + cached response
func weatherHandler(w http.ResponseWriter, r *http.Request) {
	cacheHit := true
	body, ok := globalCache.get()

	if !ok {
		cacheHit = false
		log.Println("cache miss — fetching from upstream")

		var err error
		body, err = fetchUpstream()
		if err != nil {
			log.Printf("upstream error: %v", err)
			http.Error(w, `{"error":"upstream fetch failed"}`, http.StatusBadGateway)
			return
		}
		globalCache.set(body)
		log.Println("cache populated")
	} else {
		log.Println("cache hit")
	}

	w.Header().Set("Content-Type", "application/json")
	if cacheHit {
		w.Header().Set("X-Cache", "HIT")
	} else {
		w.Header().Set("X-Cache", "MISS")
	}

	_, fetchedAt, age, _ := func() ([]byte, time.Time, time.Duration, bool) {
		fa, a, v := globalCache.status()
		return nil, fa, a, v
	}()
	w.Header().Set("X-Cache-Age", age.Round(time.Second).String())
	w.Header().Set("X-Cache-Fetched-At", fetchedAt.UTC().Format(time.RFC3339))
	w.Header().Set("X-Cache-Expires-In", (cacheTTL - age).Round(time.Second).String())

	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// GET /status — cache diagnostics
func statusHandler(w http.ResponseWriter, r *http.Request) {
	fetchedAt, age, valid := globalCache.status()

	info := map[string]any{
		"cache_valid": valid,
		"ttl":         cacheTTL.String(),
	}
	if !fetchedAt.IsZero() {
		info["fetched_at"] = fetchedAt.UTC().Format(time.RFC3339)
		info["age"] = age.Round(time.Second).String()
		if valid {
			info["expires_in"] = (cacheTTL - age).Round(time.Second).String()
		} else {
			info["expires_in"] = "expired"
		}
	} else {
		info["fetched_at"] = nil
		info["age"] = nil
		info["expires_in"] = nil
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// DELETE /cache — force cache invalidation
func invalidateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	globalCache.mu.Lock()
	globalCache.body = nil
	globalCache.fetchedAt = time.Time{}
	globalCache.mu.Unlock()

	log.Println("cache invalidated via API")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"message":"cache cleared"}`))
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/weather", weatherHandler)
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/cache", invalidateHandler)

	log.Printf("weather proxy listening on %s", listenAddr)
	log.Printf("upstream: %s", upstreamURL)
	log.Printf("cache TTL: %s", cacheTTL)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
