// Package ops provides the process health endpoint and a minimal plain-text
// metrics registry. No third-party Prometheus client is used in v1.0.
package ops

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metric is a counter or gauge with a fixed label set.
type Metric struct {
	name   string
	labels map[string]string
	kind   string
	value  atomic.Int64
	scale  atomic.Int64 // >1 renders value/scale as a float (e.g. nanoseconds -> seconds)
}

func (m *Metric) Add(delta int64) { m.value.Add(delta) }
func (m *Metric) Inc()            { m.Add(1) }
func (m *Metric) Set(v int64)     { m.scale.Store(0); m.value.Store(v) }
func (m *Metric) Get() int64      { return m.value.Load() }

// SetNanos stores a duration in nanoseconds rendered as seconds.
func (m *Metric) SetNanos(ns int64) {
	m.scale.Store(int64(time.Second))
	m.value.Store(ns)
}

func (m *Metric) format() string {
	v := m.value.Load()
	if scale := m.scale.Load(); scale > 1 {
		return strconv.FormatFloat(float64(v)/float64(scale), 'f', -1, 64)
	}
	return strconv.FormatInt(v, 10)
}

// Registry stores named, labelled metrics and renders them in Prometheus
// text exposition format.
type Registry struct {
	mu      sync.Mutex
	metrics map[string]*Metric
	sources map[string]func() string
}

func NewRegistry() *Registry {
	return &Registry{metrics: make(map[string]*Metric), sources: make(map[string]func() string)}
}

// AddSource appends an external metric registry's text to Render. Sources are
// used for the read-only APIs mounted on the collector's ops server; their
// metric names are distinct from the collector's own.
func (r *Registry) AddSource(name string, fn func() string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources[name] = fn
}

func (r *Registry) Counter(name string, labels map[string]string) *Metric {
	return r.metric(name, labels, "counter")
}

func (r *Registry) Gauge(name string, labels map[string]string) *Metric {
	return r.metric(name, labels, "gauge")
}

func (r *Registry) metric(name string, labels map[string]string, kind string) *Metric {
	key := metricKey(name, labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.metrics[key]; ok {
		return m
	}
	cp := make(map[string]string, len(labels))
	for k, v := range labels {
		cp[k] = v
	}
	m := &Metric{name: name, labels: cp, kind: kind}
	r.metrics[key] = m
	return m
}

// Handler serves /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		io.WriteString(w, r.Render())
	})
}

// Render returns the current metrics snapshot.
func (r *Registry) Render() string {
	r.mu.Lock()
	all := make([]*Metric, 0, len(r.metrics))
	for _, m := range r.metrics {
		all = append(all, m)
	}
	names := make([]string, 0, len(r.sources))
	for name := range r.sources {
		names = append(names, name)
	}
	sort.Strings(names)
	sources := make([]func() string, 0, len(names))
	for _, name := range names {
		sources = append(sources, r.sources[name])
	}
	r.mu.Unlock()
	sort.Slice(all, func(i, j int) bool {
		return metricKey(all[i].name, all[i].labels) < metricKey(all[j].name, all[j].labels)
	})

	var b strings.Builder
	seenType := make(map[string]bool)
	for _, m := range all {
		if !seenType[m.name] {
			fmt.Fprintf(&b, "# TYPE %s %s\n", m.name, m.kind)
			seenType[m.name] = true
		}
		b.WriteString(m.name)
		if len(m.labels) > 0 {
			b.WriteByte('{')
			keys := make([]string, 0, len(m.labels))
			for k := range m.labels {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for i, k := range keys {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, "%s=%q", k, m.labels[k])
			}
			b.WriteByte('}')
		}
		b.WriteByte(' ')
		b.WriteString(m.format())
		b.WriteByte('\n')
	}
	for _, source := range sources {
		if text := source(); text != "" {
			b.WriteString(text)
		}
	}
	return b.String()
}

func metricKey(name string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		b.WriteByte('\x00')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}
