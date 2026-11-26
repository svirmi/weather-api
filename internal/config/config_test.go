package config

import (
	"strings"
	"testing"
)

func TestWeightBudget(t *testing.T) {
	c := &Config{Symbols: []string{"a", "b", "c", "d"}}
	if got := c.WeightBudget(); got != 486 {
		t.Fatalf("WeightBudget = %d, want 486", got)
	}
}

func TestValidateRejectsTooManySymbols(t *testing.T) {
	symbols := make([]string, MaxSymbols+1)
	for i := range symbols {
		symbols[i] = "S" + string(rune('A'+i))
	}
	c := &Config{Symbols: symbols, PollInterval: 1e9, FlushInterval: 2e9, HTTPTimeout: 3e9,
		BufferMaxMinutes: 10, RetentionDays: 45, PartitionAheadDays: 7, SkewMaxMS: 250, MetricsAddr: ":9090"}
	err := c.Validate(false)
	if err == nil || !strings.Contains(err.Error(), "maximum is 16") {
		t.Fatalf("expected max-symbols error, got %v", err)
	}
}

func TestValidateRejectsDuplicatesAndMissingDSN(t *testing.T) {
	c := &Config{Symbols: []string{"XPTUSDT", "XPTUSDT"}, PollInterval: 1e9, FlushInterval: 2e9,
		HTTPTimeout: 3e9, BufferMaxMinutes: 10, RetentionDays: 45, PartitionAheadDays: 7,
		SkewMaxMS: 250, MetricsAddr: ":9090"}
	if err := c.Validate(true); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
	c.Symbols = []string{"XPTUSDT"}
	if err := c.Validate(true); err == nil || !strings.Contains(err.Error(), "DB_DSN") {
		t.Fatalf("expected DSN error, got %v", err)
	}
}
