package main

import "testing"

func TestLoadConfig(t *testing.T) {
	t.Setenv("DB_DSN", "postgres://collector@example/db")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("SPREAD_ADDR", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.DSN != "postgres://collector@example/db" || cfg.Addr != defaultAddr {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfigFallbackAndOverride(t *testing.T) {
	t.Setenv("DB_DSN", "")
	t.Setenv("DATABASE_URL", "postgres://collector@fallback/db")
	t.Setenv("SPREAD_ADDR", "127.0.0.1:9999")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.DSN != "postgres://collector@fallback/db" || cfg.Addr != "127.0.0.1:9999" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfigRequiresDSN(t *testing.T) {
	t.Setenv("DB_DSN", "")
	t.Setenv("DATABASE_URL", "")
	if _, err := loadConfig(); err == nil {
		t.Error("loadConfig accepted a missing DSN")
	}
}
