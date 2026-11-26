package store

import (
	"testing"
	"time"

	"metals-collector/internal/model"
)

func TestRingDropOldestAndAck(t *testing.T) {
	r := NewRing(3)
	for i := 1; i <= 4; i++ {
		r.Push(model.Row{Symbol: "S", TS: time.Unix(int64(i), 0)})
	}
	if got := r.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	if got := r.Dropped(); got != 1 {
		t.Fatalf("Dropped = %d, want 1", got)
	}
	rows := r.Snapshot()
	if rows[0].TS.Unix() != 2 || rows[2].TS.Unix() != 4 {
		t.Fatalf("snapshot = %v", rows)
	}
	r.Ack(2)
	if got := r.Len(); got != 1 {
		t.Fatalf("Len after Ack = %d, want 1", got)
	}
	if r.Snapshot()[0].TS.Unix() != 4 {
		t.Fatalf("remaining row = %v", r.Snapshot())
	}
	r.Ack(1)
	if r.Len() != 0 || len(r.Snapshot()) != 0 {
		t.Fatalf("ring not empty after full Ack")
	}
}
