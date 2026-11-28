// Package spread serves downsampled ask-to-ask pair series from book_l1.
//
// It is read-only: it never writes to the collector's table and never runs
// migrations. The pairs, presets and JSON shapes in this file are the v1
// contract; see tools/spread-view/SPEC.md sections 5 and 12.
package spread

import (
	"fmt"
	"time"
)

// Pair is one ask-to-ask ratio: Num / Den.
type Pair struct {
	ID  string
	Num string
	Den string
}

// Pairs is the complete v1 surface. Adding a pair is additive; changing an
// existing pair's legs is a coordinated breaking change.
var Pairs = map[string]Pair{
	"XAU-XAG": {ID: "XAU-XAG", Num: "XAUUSDT", Den: "XAGUSDT"},
	"XPT-XPD": {ID: "XPT-XPD", Num: "XPTUSDT", Den: "XPDUSDT"},
}

// ParsePair validates and resolves a pair identifier.
func ParsePair(id string) (Pair, error) {
	p, ok := Pairs[id]
	if !ok {
		return Pair{}, fmt.Errorf("unknown pair: %s", id)
	}
	return p, nil
}

// Preset is one fixed history window and its bucket size.
type Preset struct {
	Name      string
	Window    time.Duration
	Bucket    time.Duration
	BucketSQL string
}

// Presets is the complete v1 set of range values.
var Presets = []Preset{
	{Name: "45d", Window: 45 * 24 * time.Hour, Bucket: 15 * time.Minute, BucketSQL: "15 minutes"},
	{Name: "7d", Window: 7 * 24 * time.Hour, Bucket: 5 * time.Minute, BucketSQL: "5 minutes"},
	{Name: "1d", Window: 24 * time.Hour, Bucket: time.Minute, BucketSQL: "1 minute"},
	{Name: "8h", Window: 8 * time.Hour, Bucket: 15 * time.Second, BucketSQL: "15 seconds"},
	{Name: "1h", Window: time.Hour, Bucket: time.Second, BucketSQL: "1 second"},
}

var presetsByName = func() map[string]Preset {
	m := make(map[string]Preset, len(Presets))
	for _, p := range Presets {
		m[p.Name] = p
	}
	return m
}()

// ParsePreset validates and resolves a range name.
func ParsePreset(name string) (Preset, error) {
	p, ok := presetsByName[name]
	if !ok {
		return Preset{}, fmt.Errorf("unknown range: %s", name)
	}
	return p, nil
}

// BucketSec returns the bucket duration in whole seconds.
func (p Preset) BucketSec() int64 { return int64(p.Bucket / time.Second) }

// Point is one bucket of the history series. All timestamps are epoch seconds.
type Point struct {
	T     int64   `json:"t"`
	N     int64   `json:"n"`
	Ratio float64 `json:"ratio"`
	Lo    float64 `json:"lo"`
	Hi    float64 `json:"hi"`
	Last  float64 `json:"last"`
	AskA  float64 `json:"askA"`
	AskB  float64 `json:"askB"`
}

// HistoryResponse is the v1 response of GET /api/v1/spread.
type HistoryResponse struct {
	Pair      string  `json:"pair"`
	Range     string  `json:"range"`
	BucketSec int64   `json:"bucketSec"`
	AsOf      int64   `json:"asOf"`
	Points    []Point `json:"points"`
}

// LivePoint is one second of the incremental live series.
type LivePoint struct {
	T     int64   `json:"t"`
	AskA  float64 `json:"askA"`
	AskB  float64 `json:"askB"`
	Ratio float64 `json:"ratio"`
}

// Latest is the most recent observed second, used by the page cards.
type Latest struct {
	T     int64   `json:"t"`
	AskA  float64 `json:"askA"`
	AskB  float64 `json:"askB"`
	Ratio float64 `json:"ratio"`
}

// LiveResponse is the v1 response of GET /api/v1/spread/live.
type LiveResponse struct {
	Pair   string      `json:"pair"`
	Cursor int64       `json:"cursor"`
	PollMS int         `json:"pollMs"`
	Latest *Latest     `json:"latest"`
	Points []LivePoint `json:"points"`
}

// ErrorResponse is the v1 error body.
type ErrorResponse struct {
	Error string `json:"error"`
}
