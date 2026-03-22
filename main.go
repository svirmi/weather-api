package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// --- Configuration ---
const (
	Max15mSlots    = 124 // 31 hours  @ 15-min intervals
	MaxHrSlots     = 72  // 72 hours  @ 1-hour intervals
	MaxObsSlots    = 96  // holds ~16h of observations at ~10-min station cadence
	ObsDupWindow   = 60  // seconds — timestamps within this window are duplicates
	PollInterval   = 3 * time.Minute
	MaxConcurrency = 6
	ListenAddr     = ":8080"
)

type Slot struct {
	Temp      float32
	Timestamp int64
}

type LocationStore struct {
	ID            string
	Buffer15m     [Max15mSlots]Slot
	BufferHr      [MaxHrSlots]Slot
	BufferObs     [MaxObsSlots]Slot // observation history, written by monotonic counter
	ObsWriteIdx   int               // next slot to write; wraps at MaxObsSlots
	Current       Slot              // latest observation for the "now" marker
	LastSync15m   time.Time
	LastSyncHr    time.Time
	LastSyncCurr  time.Time
	SyncCount15m  int
	SyncCountHr   int
	SyncCountCurr int
	mu            sync.RWMutex
}

type WeatherRegistry struct {
	Locations map[string]*LocationStore
	ApiKey    string
}

// --- Timezone-robust local time parser ---
// weather.com returns validTimeLocal with timezone offsets in two formats
// depending on region:
//   - "2026-03-21T12:30:00-0400"  (no colon — most locations)
//   - "2026-03-21T12:30:00-04:00" (with colon — some regions e.g. Canada)
//
// time.Parse is strict, so we try both layouts. The offset is only needed
// for correct UTC conversion — it never leaks into stored data.
var localTimeLayouts = []string{
	"2006-01-02T15:04:05-0700",  // no colon:   -0400
	"2006-01-02T15:04:05-07:00", // with colon: -04:00
}

func parseLocalTime(s string) (time.Time, error) {
	var t time.Time
	var err error
	for _, layout := range localTimeLayouts {
		t, err = time.Parse(layout, s)
		if err == nil {
			return t, nil
		}
	}
	return t, fmt.Errorf("parseLocalTime: cannot parse %q with any known layout", s)
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
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

	skipped := 0
	for i, temp := range data.Temperature {
		var ts int64
		if is15m {
			t, err := parseLocalTime(data.ValidTimeLocal[i])
			if err != nil {
				skipped++
				log.Printf("⚠️  [%s] Skipping point %d: %v", id, i, err)
				continue
			}
			ts = t.Unix()
			idx := (ts / 900) % Max15mSlots
			store.Buffer15m[idx] = Slot{Temp: temp, Timestamp: ts}
		} else {
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
		if skipped > 0 {
			log.Printf("⚠️  [%s] 15m: %d/%d points skipped (parse failure)", id, skipped, len(data.Temperature))
		}
	} else {
		store.LastSyncHr = time.Now()
		store.SyncCountHr++
	}
	log.Printf("✅ [%s] Updated %s (%d points)", id,
		map[bool]string{true: "15m", false: "Hr"}[is15m], len(data.Temperature)-skipped)
}

func (r *WeatherRegistry) UpdateCurrent(id string, data ObservationResponse) {
	store, ok := r.Locations[id]
	if !ok {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	incoming := data.ValidTimeUtc

	// Deduplicate: if the incoming timestamp is within ObsDupWindow seconds
	// of the last stored observation, the station hasn't reported a new
	// measurement yet — just update Current in place and skip the buffer write.
	// The threshold absorbs API timestamp jitter (e.g. :57:07 vs :57:09 for
	// the same physical reading) while correctly detecting new measurements.
	if store.Current.Timestamp > 0 && abs64(incoming-store.Current.Timestamp) < ObsDupWindow {
		// Same measurement, just refresh Current
		store.Current = Slot{Temp: float32(data.Temperature), Timestamp: incoming}
		log.Printf("⏭  [%s] Obs duplicate skipped (Δ%ds)", id, abs64(incoming-store.Current.Timestamp))
		return
	}

	slot := Slot{
		Temp:      float32(data.Temperature),
		Timestamp: incoming,
	}

	// Update the "now" marker
	store.Current = slot

	// Write into ring buffer using monotonic write counter — no timestamp-
	// derived indexing. Oldest entry is naturally overwritten when the
	// counter wraps after MaxObsSlots observations.
	store.BufferObs[store.ObsWriteIdx] = slot
	store.ObsWriteIdx = (store.ObsWriteIdx + 1) % MaxObsSlots

	store.LastSyncCurr = time.Now()
	store.SyncCountCurr++
	log.Printf("✅ [%s] Updated obs (%.1f°C @ %s)", id,
		float32(data.Temperature),
		time.Unix(incoming, 0).UTC().Format(time.RFC3339))
}

func (s *LocationStore) GetData(startTs, endTs int64) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Current observation — nil if never fetched
	var current any
	if s.Current.Timestamp > 0 {
		current = map[string]any{
			"t":       time.Unix(s.Current.Timestamp, 0).UTC().Format(time.RFC3339),
			"unix_ts": s.Current.Timestamp,
			"v":       s.Current.Temp,
		}
	}

	// Observation history — scan all slots, collect those in range, sort.
	// No grid assumption: station timestamps are irregular and stored as-is.
	observed := []map[string]any{}
	for i := 0; i < MaxObsSlots; i++ {
		slot := s.BufferObs[i]
		if slot.Timestamp > 0 && slot.Timestamp >= startTs && slot.Timestamp <= endTs {
			observed = append(observed, map[string]any{
				"t":       time.Unix(slot.Timestamp, 0).UTC().Format(time.RFC3339),
				"unix_ts": slot.Timestamp,
				"v":       slot.Temp,
			})
		}
	}
	sort.Slice(observed, func(i, j int) bool {
		return observed[i]["unix_ts"].(int64) < observed[j]["unix_ts"].(int64)
	})

	// Forecast points — walk 15-min slots within requested range
	forecast := []map[string]any{}
	for t := (startTs / 900) * 900; t <= endTs; t += 900 {
		entry := map[string]any{
			"t":       time.Unix(t, 0).UTC().Format(time.RFC3339),
			"unix_ts": t,
		}
		found := false

		// 15m takes priority
		idx15 := (t / 900) % Max15mSlots
		if s.Buffer15m[idx15].Timestamp == t {
			entry["v"] = s.Buffer15m[idx15].Temp
			entry["res"] = "15m"
			found = true
		}

		// Hourly — only on full hours, only if 15m didn't claim this slot
		if t%3600 == 0 {
			idxHr := (t / 3600) % MaxHrSlots
			if s.BufferHr[idxHr].Timestamp == t {
				if !found {
					entry["v"] = s.BufferHr[idxHr].Temp
					entry["res"] = "hr"
				}
				found = true
			}
		}

		if found {
			forecast = append(forecast, entry)
		}
	}

	return map[string]any{
		"current":  current,
		"observed": observed,
		"forecast": forecast,
	}
}

// --- Background Sync ---

func startPoller(reg *WeatherRegistry) {
	sem := make(chan struct{}, MaxConcurrency)

	runForecasts := func() {
		log.Println("🔄 Forecast sync...")
		var wg sync.WaitGroup
		for id := range reg.Locations {
			wg.Add(1)
			go func(locID string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				url := fmt.Sprintf(
					"https://api.weather.com/v3/wx/forecast/fifteenminute?icaoCode=%s&units=m&language=en-US&apiKey=%s&format=json",
					locID, reg.ApiKey)
				fetchAndUpdate(reg, locID, url, true)
			}(id)

			wg.Add(1)
			go func(locID string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				url := fmt.Sprintf(
					"https://api.weather.com/v3/wx/forecast/hourly/2day?icaoCode=%s&units=m&language=en-US&apiKey=%s&format=json",
					locID, reg.ApiKey)
				fetchAndUpdate(reg, locID, url, false)
			}(id)
		}
		wg.Wait()
		log.Println("✨ Forecast sync done.")
	}

	runObservations := func() {
		log.Println("🔄 Observation sync...")
		var wg sync.WaitGroup
		for id := range reg.Locations {
			wg.Add(1)
			go func(locID string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				url := fmt.Sprintf(
					"https://api.weather.com/v3/wx/observations/current?icaoCode=%s&units=m&language=en-US&apiKey=%s&format=json",
					locID, reg.ApiKey)
				fetchAndUpdateCurrent(reg, locID, url)
			}(id)
		}
		wg.Wait()
		log.Println("✨ Observation sync done.")
	}

	go func() {
		// Run both immediately on startup
		runForecasts()
		runObservations()

		// Forecast ticker: every 3 minutes
		go func() {
			ticker := time.NewTicker(PollInterval)
			for range ticker.C {
				runForecasts()
			}
		}()

		// Observation ticker: wall-clock driven, fires at :00/:05/:10.../:55
		// Checks every 30s whether we've crossed a 5-min boundary. Self-
		// correcting — no drift possible regardless of how long a run takes.
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			var lastFired time.Time
			for tick := range ticker.C {
				boundary := tick.Truncate(5 * time.Minute)
				if tick.Sub(boundary) < 30*time.Second && boundary.After(lastFired) {
					lastFired = boundary
					log.Printf("⏱  Observation tick at boundary %s UTC",
						boundary.UTC().Format("15:04:05"))
					runObservations()
				}
			}
		}()
	}()
}

func fetchAndUpdate(reg *WeatherRegistry, id, url string, is15m bool) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("❌ [%s] Request Error: %v", id, err)
		return
	}
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

func fetchAndUpdateCurrent(reg *WeatherRegistry, id, url string) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("❌ [%s] Obs Request Error: %v", id, err)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("❌ [%s] Obs Network Error: %v", id, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("❌ [%s] Obs API Error: Status %d", id, resp.StatusCode)
		return
	}

	var data ObservationResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("❌ [%s] Obs JSON Error: %v", id, err)
		return
	}
	reg.UpdateCurrent(id, data)
}

// --- Handlers ---

func main() {
	apiKey := os.Getenv("WEATHER_DOT_COM")
	if apiKey == "" {
		log.Println("⚠️ Set WEATHER_DOT_COM environment variable. Default used.")
		apiKey = "e1f10a1e78da46f5b10a1e78da96f525"
	}

	ids := []string{"RJTT", "LLBG", "LFPG", "EGLC", "LTAC", "EPWA", "CYYZ"}
	registry := &WeatherRegistry{
		Locations: make(map[string]*LocationStore),
		ApiKey:    apiKey,
	}
	for _, id := range ids {
		registry.Locations[id] = &LocationStore{ID: id}
	}

	startPoller(registry)

	// GET /weather/{id}
	http.HandleFunc("/weather/", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/weather/"):]
		store, ok := registry.Locations[id]
		if !ok {
			http.Error(w, "Unknown Location", 404)
			return
		}
		start, end := getTimeBounds(r)
		renderJSON(w, store.GetData(start, end))
	}))

	// GET /weather/all
	http.HandleFunc("/weather/all", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		start, end := getTimeBounds(r)
		out := make(map[string]any)
		for id, store := range registry.Locations {
			out[id] = store.GetData(start, end)
		}
		renderJSON(w, out)
	}))

	// GET /health
	http.HandleFunc("/health", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		stats := make(map[string]any)
		for id, s := range registry.Locations {
			s.mu.RLock()
			stats[id] = map[string]any{
				"last_15m":      s.LastSync15m.Format(time.RFC1123),
				"last_hr":       s.LastSyncHr.Format(time.RFC1123),
				"last_obs":      s.LastSyncCurr.Format(time.RFC1123),
				"total_syncs":   s.SyncCount15m + s.SyncCountHr + s.SyncCountCurr,
				"obs_collected": s.SyncCountCurr,
			}
			s.mu.RUnlock()
		}
		renderJSON(w, stats)
	}))

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

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// --- API Response Types ---

type WeatherResponse struct {
	Temperature    []float32 `json:"temperature"`
	ValidTimeLocal []string  `json:"validTimeLocal"`
	ValidTimeUtc   []int64   `json:"validTimeUtc"`
}

type ObservationResponse struct {
	Temperature  int   `json:"temperature"`
	ValidTimeUtc int64 `json:"validTimeUtc"`
}
