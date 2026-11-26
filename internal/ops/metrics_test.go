package ops

import (
	"strings"
	"testing"
	"time"
)

func TestRegistryRender(t *testing.T) {
	r := NewRegistry()
	r.Counter("ticks_total", map[string]string{"symbol": "XPTUSDT"}).Inc()
	r.Gauge("db_flush_seconds", nil).SetNanos(int64(1500 * time.Millisecond))
	out := r.Render()
	if !strings.Contains(out, "# TYPE ticks_total counter") {
		t.Fatalf("missing TYPE line: %s", out)
	}
	if !strings.Contains(out, `ticks_total{symbol="XPTUSDT"} 1`) {
		t.Fatalf("missing counter value: %s", out)
	}
	if !strings.Contains(out, "db_flush_seconds 1.5") {
		t.Fatalf("seconds not rendered as float: %s", out)
	}
}
