package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// --- Configuration ---

const (
	Max15mSlots    = 124 // 31 hours
	MaxHrSlots     = 72  // 72 hours
	PollInterval   = 3 * time.Minute
	MaxConcurrency = 4 // Gentle API limit
	ListenAddr     = ":8080"
)

type Slot struct {
	Temp      float32
	Timestamp int64
}

type LocationStore struct {
	ID           string
	Buffer15m    [Max15mSlots]Slot
	BufferHr     [MaxHrSlots]Slot
	LastSync15m  time.Time
	LastSyncHr   time.Time
	SyncCount15m int
	SyncCountHr  int
	mu           sync.RWMutex
}

type WeatherRegistry struct {
	Locations map[string]*LocationStore
	ApiKey    string
}

// --- Logic Engine ---

func (r *WeatherRegistry) Update(id string, data WeatherResponse, is15m bool) {
	store, ok := r.Locations[id]
	if !ok {
		return
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(data.Temperature) == 0 {
		log.Printf("⚠️  [%s] API returned empty arrays", id)
		return
	}

	for i, temp := range data.Temperature {
		var ts int64
		if is15m {
			// External API provides string for 15m; we convert to Unix here
			t, err := time.Parse("2006-01-02T15:04:05-0700", data.ValidTimeLocal[i])
			if err != nil {
				continue
			}
			ts = t.Unix()
			idx := (ts / 900) % Max15mSlots
			store.Buffer15m[idx] = Slot{Temp: temp, Timestamp: ts}
		} else {
			// External API provides Unix for Hourly
			if i >= len(data.ValidTimeUtc) {
				break
			}
			ts = data.ValidTimeUtc[i]
			idx := (ts / 3600) % MaxHrSlots
			store.BufferHr[idx] = Slot{Temp: temp, Timestamp: ts}
		}
	}

	if is15m {
		store.LastSync15m = time.Now()
		store.SyncCount15m++
	} else {
		store.LastSyncHr = time.Now()
		store.SyncCountHr++
	}
	log.Printf("✅ [%s] Updated %s (%d points)", id, map[bool]string{true: "15m", false: "Hr"}[is15m], len(data.Temperature))
}

func (s *LocationStore) GetData(startTs, endTs int64) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := []map[string]any{}

	for t := (startTs / 900) * 900; t <= endTs; t += 900 {
		entry := map[string]any{
			"t":       time.Unix(t, 0).UTC().Format(time.RFC3339),
			"unix_ts": t,
		}

		found := false

		// Check 15m buffer — higher resolution, takes priority
		idx15 := (t / 900) % Max15mSlots
		if s.Buffer15m[idx15].Timestamp == t {
			entry["v"] = s.Buffer15m[idx15].Temp
			entry["res"] = "15m"
			found = true
		}

		// Check Hourly buffer (only on full hours)
		if t%3600 == 0 {
			idxHr := (t / 3600) % MaxHrSlots
			if s.BufferHr[idxHr].Timestamp == t {
				// Only set v/res from hourly if 15m didn't already claim this slot
				if !found {
					entry["v"] = s.BufferHr[idxHr].Temp
					entry["res"] = "hr"
				}
				found = true
			}
		}

		if found {
			result = append(result, entry)
		}
	}

	return result
}

// --- Background Sync ---

func startPoller(reg *WeatherRegistry) {
	sem := make(chan struct{}, MaxConcurrency)
	ticker := time.NewTicker(PollInterval)

	run := func() {
		log.Println("🔄 Starting batch sync...")
		var wg sync.WaitGroup
		for id := range reg.Locations {
			// 15 Min forecast data Fetch
			wg.Add(1)
			go func(locID string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				url := fmt.Sprintf("https://api.weather.com/v3/wx/forecast/fifteenminute?icaoCode=%s&units=m&language=en-US&apiKey=%s&format=json", locID, reg.ApiKey)
				fetchAndUpdate(reg, locID, url, true)
			}(id)

			// Hourly forecast data Fetch
			wg.Add(1)
			go func(locID string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				url := fmt.Sprintf("https://api.weather.com/v3/wx/forecast/hourly/2day?icaoCode=%s&units=m&language=en-US&apiKey=%s&format=json", locID, reg.ApiKey)
				fetchAndUpdate(reg, locID, url, false)
			}(id)
		}
		wg.Wait()
		log.Println("✨ Batch sync finished.")
	}

	go func() {
		run()
		for range ticker.C {
			run()
		}
	}()
}

func fetchAndUpdate(reg *WeatherRegistry, id, url string, is15m bool) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("❌ [%s] Request Error: %v", id, err)
		return
	}
	// Mimic browser to avoid potential bot blocking
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("❌ [%s] Network Error: %v", id, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("❌ [%s] API Error: Status %d", id, resp.StatusCode)
		return
	}

	var data WeatherResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("❌ [%s] JSON Error: %v", id, err)
		return
	}
	reg.Update(id, data, is15m)
}

// --- Handlers ---

func main() {
	apiKey := os.Getenv("WEATHER_DOT_COM")
	if apiKey == "" {
		log.Println("⚠️ Set WEATHER_DOT_COM environment variable. Default used.")
		apiKey = "e1f10a1e78da46f5b10a1e78da96f525"
	}

	ids := []string{"RJTT", "LLBG", "LFPG", "EGLC", "LTAC", "EPWA"}
	registry := &WeatherRegistry{
		Locations: make(map[string]*LocationStore),
		ApiKey:    apiKey,
	}

	for _, id := range ids {
		registry.Locations[id] = &LocationStore{ID: id}
	}

	startPoller(registry)

	// GET /weather/{id}
	http.HandleFunc("/weather/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/weather/"):]
		store, ok := registry.Locations[id]
		if !ok {
			http.Error(w, "Unknown Location", 404)
			return
		}
		start, end := getTimeBounds(r)
		renderJSON(w, store.GetData(start, end))
	})

	// GET /weather/all
	http.HandleFunc("/weather/all", func(w http.ResponseWriter, r *http.Request) {
		start, end := getTimeBounds(r)
		out := make(map[string]any)
		for id, store := range registry.Locations {
			out[id] = store.GetData(start, end)
		}
		renderJSON(w, out)
	})

	// GET /health
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		stats := make(map[string]any)
		for id, s := range registry.Locations {
			s.mu.RLock()
			stats[id] = map[string]any{
				"last_15m":    s.LastSync15m.Format(time.RFC1123),
				"last_hr":     s.LastSyncHr.Format(time.RFC1123),
				"total_syncs": s.SyncCount15m + s.SyncCountHr,
			}
			s.mu.RUnlock()
		}
		renderJSON(w, stats)
	})

	log.Printf("Weather Engine LIVE on %s", ListenAddr)
	log.Fatal(http.ListenAndServe(ListenAddr, nil))
}

func getTimeBounds(r *http.Request) (int64, int64) {
	now := time.Now().Unix()
	s, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
	if s == 0 {
		s = now - 86400
	} // -24h
	e, _ := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
	if e == 0 {
		e = now + 172800
	} // +48h
	return s, e
}

func renderJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

type WeatherResponse struct {
	Temperature    []float32 `json:"temperature"`
	ValidTimeLocal []string  `json:"validTimeLocal"`
	ValidTimeUtc   []int64   `json:"validTimeUtc"`
}
